package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	usage "github.com/yudaprama/plano-usage"
)

// PolicyMiddleware is an Eino-native ChatModelAgentMiddleware that enforces
// x-agentic-access policies on every tool call. It replaces the per-tool
// policy.Wrap decorator with a single middleware attached to the agent's
// handler chain.
//
// Usage:
//
//	registry := policy.NewRegistry(defaultMutatingPolicy)
//	registry.Register("send_email", policy.XAgenticAccess{...})
//	middleware := policy.NewMiddleware(registry, nil)
//	// attach to agent config:
//	agent, _ := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
//	    Handlers: []adk.ChatModelAgentMiddleware{middleware},
//	    ...
//	})
type PolicyMiddleware struct {
	*adk.TypedBaseChatModelAgentMiddleware[*schema.Message]
	registry   *PolicyRegistry
	authorizer Authorizer    // optional; nil = skip authz
	throttle   *Throttle     // cooldown gating per user
	prefGetter func() string // optional; per-user preference source
}

// MiddlewareOption configures the PolicyMiddleware.
type MiddlewareOption func(*PolicyMiddleware)

// WithThrottle sets the cooldown throttle for HITL interrupts.
func WithThrottle(t *Throttle) MiddlewareOption {
	return func(m *PolicyMiddleware) { m.throttle = t }
}

// WithUserPolicyPref sets a function that returns the per-user HITL preference.
// Called at interrupt-decision time, not at startup.
func WithUserPolicyPref(getter func() string) MiddlewareOption {
	return func(m *PolicyMiddleware) { m.prefGetter = getter }
}

// NewMiddleware creates a PolicyMiddleware backed by the given registry.
// Pass nil for authorizer to skip authorization checks (observe mode).
func NewMiddleware(registry *PolicyRegistry, authorizer Authorizer, opts ...MiddlewareOption) *PolicyMiddleware {
	m := &PolicyMiddleware{registry: registry, authorizer: authorizer}
	for _, opt := range opts {
		opt(m)
	}
	if m.throttle == nil {
		m.throttle = NewThrottle(DefaultThrottleCooldown)
	}
	return m
}

// WrapInvokableToolCall is the Eino middleware hook. It intercepts each
// invokable tool call, looks up the policy by name, and applies the same
// enforcement order as the legacy policyCheckedTool:
//
//  1. Subject check
//  2. HITL gate
//  3. Authorize
//  4. Delegate to inner tool
//  5. Audit outcome
func (m *PolicyMiddleware) WrapInvokableToolCall(
	ctx context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	tCtx *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	p := m.registry.Lookup(tCtx.Name)

	wrapped := func(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
		start := time.Now()
		auditor := AuditorFromContext(ctx)
		actor := usage.ActorIDFromContext(ctx)

		// A resumed tool call is allowed to pass through before evaluating the
		// normal HITL gate again. The resume payload is deliberately a bool so
		// approval cannot be confused with arbitrary tool input.
		if wasInterrupted, hasData, approved := compose.GetResumeContext[bool](ctx); wasInterrupted && hasData {
			if !approved {
				return fmt.Sprintf("tool %q was rejected by the user", tCtx.Name), nil
			}
		}

		// 1. Subject check.
		if p.Subject == SubjectRequired && actor == "" {
			auditor.Record(ctx, Decision{
				Timestamp:   start.UTC(),
				Tool:        tCtx.Name,
				Actor:       "",
				ActionClass: p.ActionClass,
				Consequence: p.Consequence,
				Verdict:     "deny",
				Reason:      "no_actor",
				DurationMS:  msSince(start),
			})
			return denyContent(tCtx.Name, "subject required but no actor in context"), nil
		}

		// 2. HITL gate — smart gating with risk classification, user
		// preference, and cooldown throttle.
		if escalates, why := hitlEscalates(ctx, p); escalates {
			risk := ClassifyRisk(p)
			pref := ResolveUserPolicyPref(m.prefGetter)
			actorID := actor // reuse from subject check
			lastAt := m.throttle.LastInterrupt(actorID)
			shouldInterrupt, reason := ShouldInterrupt(risk, pref, lastAt, m.throttle.Cooldown())

			if shouldInterrupt && EnforceHITL() {
				m.throttle.Record(actorID)
				auditor.Record(ctx, Decision{
					Timestamp:   start.UTC(),
					Tool:        tCtx.Name,
					Actor:       actor,
					ActionClass: p.ActionClass,
					Consequence: p.Consequence,
					Verdict:     "hitl_blocked",
					Reason:      why + ":" + reason,
					DurationMS:  msSince(start),
				})
				return "", compose.Interrupt(ctx, map[string]any{
					"kind":   "tool_approval",
					"tool":   tCtx.Name,
					"reason": why,
					"args":   json.RawMessage(argsJSON),
				})
			}
			auditor.Record(ctx, Decision{
				Timestamp:   start.UTC(),
				Tool:        tCtx.Name,
				Actor:       actor,
				ActionClass: p.ActionClass,
				Consequence: p.Consequence,
				Verdict:     "hitl_pending_observed",
				Reason:      why + ":" + reason,
			})
		}

		// 3. Authorize.
		if m.authorizer != nil && EnforceAuthz() {
			allowed, reason, err := m.authorizer.Authorize(ctx, actor, tCtx.Name, p)
			if err != nil {
				if p.IsSafetyCritical() {
					auditor.Record(ctx, Decision{
						Timestamp:   start.UTC(),
						Tool:        tCtx.Name,
						Actor:       actor,
						ActionClass: p.ActionClass,
						Consequence: p.Consequence,
						Verdict:     "deny",
						Reason:      "authz_error_failclosed",
						Error:       err.Error(),
						DurationMS:  msSince(start),
					})
					return denyContent(tCtx.Name, "authorization check failed (fail-closed for safety-critical)"), nil
				}
				auditor.Record(ctx, Decision{
					Timestamp:   start.UTC(),
					Tool:        tCtx.Name,
					Actor:       actor,
					ActionClass: p.ActionClass,
					Consequence: p.Consequence,
					Verdict:     "authz_error_observed",
					Reason:      reason,
					Error:       err.Error(),
				})
			} else if !allowed {
				auditor.Record(ctx, Decision{
					Timestamp:   start.UTC(),
					Tool:        tCtx.Name,
					Actor:       actor,
					ActionClass: p.ActionClass,
					Consequence: p.Consequence,
					Verdict:     "deny",
					Reason:      reason,
					DurationMS:  msSince(start),
				})
				return denyContent(tCtx.Name, "not authorized: "+reason), nil
			}
		}

		// 4. Delegate to inner tool.
		result, err := endpoint(ctx, argsJSON, opts...)

		// 5. Audit outcome.
		recordAudit(ctx, auditor, p, tCtx.Name, actor, argsJSON, start, err)
		return result, err
	}

	return wrapped, nil
}

// recordAudit emits a Decision for a completed tool call when the policy
// requires audit logging.
func recordAudit(ctx context.Context, auditor Auditor, p XAgenticAccess, toolName, actor, argsJSON string, start time.Time, err error) {
	if p.Audit != AuditRequired {
		return
	}
	d := Decision{
		Timestamp:   start.UTC(),
		Tool:        toolName,
		Actor:       actor,
		ActionClass: p.ActionClass,
		Consequence: p.Consequence,
		Verdict:     verdictFromResult(err),
		DurationMS:  msSince(start),
		Extra:       map[string]any{"args": redactArgs(argsJSON)},
	}
	if err != nil {
		d.Error = err.Error()
	}
	auditor.Record(ctx, d)
}

// hitlEscalates checks whether the policy triggers an HITL gate for this call.
func hitlEscalates(ctx context.Context, p XAgenticAccess) (bool, string) {
	switch p.HITL {
	case HITLRequired:
		return true, "hitl_required"
	case HITLConditional:
		firing := triggersFromContext(ctx)
		for _, want := range p.Triggers {
			for _, got := range firing {
				if strings.EqualFold(want, got) {
					return true, "hitl_conditional:" + want
				}
			}
		}
	}
	return false, ""
}
