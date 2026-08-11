package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	usage "github.com/yudaprama/plano-usage"
)

// Authorizer is the optional authorization hook. The PoC ships
// without one (nil = authorization skipped).
type Authorizer interface {
	// Authorize returns (allowed, reason, error). When error is non-nil the
	// decision is "unknown" and the caller chooses fail-open or fail-closed
	// based on the policy's Consequence.
	Authorize(ctx context.Context, subject, toolName string, p XAgenticAccess) (allowed bool, reason string, err error)
}

type ctxTriggersKey struct{}

// WithTriggers attaches the set of conditional-HITL triggers that are firing
// for the current request (e.g. {"abnormal"} if the call is off-hours). The
// decorator consults this to decide whether a conditional HITL policy
// escalates to a real block. An empty/missing set means "no triggers firing",
// so conditional HITL degrades to allow.
func WithTriggers(ctx context.Context, triggers []string) context.Context {
	return context.WithValue(ctx, ctxTriggersKey{}, triggers)
}

func triggersFromContext(ctx context.Context) []string {
	if v, ok := ctx.Value(ctxTriggersKey{}).([]string); ok {
		return v
	}
	return nil
}

// policyCheckedTool wraps a tool.InvokableTool and enforces its XAgenticAccess
// policy before delegating. It preserves the wrapped tool's Info() verbatim
// (LLM-facing schema unchanged) and forwards variadic tool.Option untouched.
type policyCheckedTool struct {
	inner      tool.InvokableTool
	policy     XAgenticAccess
	authorizer Authorizer // optional; nil = skip authz
}

// Wrap returns a tool.InvokableTool that enforces p before delegating to inner.
// If p is the zero value and the caller has not opted in via WrapWithPolicy,
// the inner tool is returned unchanged (zero-cost escape hatch for callers
// that want to gate wrapping externally).
func Wrap(inner tool.InvokableTool, p XAgenticAccess, authz Authorizer) tool.InvokableTool {
	if inner == nil {
		return nil
	}
	return &policyCheckedTool{inner: inner, policy: p, authorizer: authz}
}

// Info passes through the inner tool's schema unchanged. The LLM must not see
// the policy layer — it only affects server-side execution gating.
func (t *policyCheckedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

// InvokableRun is the policy enforcement point. Order:
//
//  1. Subject check — subject=required and no actor → deny.
//  2. HITL gate — required or (conditional + trigger firing) → either return
//     a synthetic "[awaiting approval …]" content string (if EnforceHITL())
//     or just log "hitl_pending" and continue (PoC default).
//  3. Authorize (if authorizer wired + EnforceAuthz) — denied → deny; error
//     → fail-closed for safety-critical, fail-open otherwise.
//  4. Delegate to inner.
//  5. Audit the outcome (always, when AuditRequired).
//
// Non-fatal policy decisions return (string, nil) — matching the codebase's
// error-as-content convention (api_tool.go, connector.go) so the ReAct loop
// can recover instead of aborting the whole turn.
func (t *policyCheckedTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	start := time.Now()
	auditor := AuditorFromContext(ctx)
	actor := usage.ActorIDFromContext(ctx)
	toolName := t.toolName(ctx)

	// 1. Subject check.
	if t.policy.Subject == SubjectRequired && actor == "" {
		auditor.Record(ctx, Decision{
			Timestamp:   start.UTC(),
			Tool:        toolName,
			Actor:       "",
			ActionClass: t.policy.ActionClass,
			Consequence: t.policy.Consequence,
			Verdict:     "deny",
			Reason:      "no_actor",
			DurationMS:  msSince(start),
		})
		return denyContent(toolName, "subject required but no actor in context"), nil
	}

	// 2. HITL gate.
	if escalates, why := t.hitlEscalates(ctx); escalates {
		if EnforceHITL() {
			auditor.Record(ctx, Decision{
				Timestamp:   start.UTC(),
				Tool:        toolName,
				Actor:       actor,
				ActionClass: t.policy.ActionClass,
				Consequence: t.policy.Consequence,
				Verdict:     "hitl_blocked",
				Reason:      why,
				DurationMS:  msSince(start),
			})
			return hitlPendingContent(toolName, why), nil
		}
		// Observe-and-log mode: record the pending decision but let it through.
		// This is the default for the PoC so nothing breaks at runtime.
		auditor.Record(ctx, Decision{
			Timestamp:   start.UTC(),
			Tool:        toolName,
			Actor:       actor,
			ActionClass: t.policy.ActionClass,
			Consequence: t.policy.Consequence,
			Verdict:     "hitl_pending_observed",
			Reason:      why,
		})
	}

	// 3. Authorize.
	if t.authorizer != nil && EnforceAuthz() {
		allowed, reason, err := t.authorizer.Authorize(ctx, actor, toolName, t.policy)
		if err != nil {
			// Fail-closed for safety-critical; fail-open for everything else
			// (mirrors the existing plano-usage.CheckBalance posture).
			if t.policy.IsSafetyCritical() {
				auditor.Record(ctx, Decision{
					Timestamp:   start.UTC(),
					Tool:        toolName,
					Actor:       actor,
					ActionClass: t.policy.ActionClass,
					Consequence: t.policy.Consequence,
					Verdict:     "deny",
					Reason:      "authz_error_failclosed",
					Error:       err.Error(),
					DurationMS:  msSince(start),
				})
				return denyContent(toolName, "authorization check failed (fail-closed for safety-critical)"), nil
			}
			auditor.Record(ctx, Decision{
				Timestamp:   start.UTC(),
				Tool:        toolName,
				Actor:       actor,
				ActionClass: t.policy.ActionClass,
				Consequence: t.policy.Consequence,
				Verdict:     "authz_error_observed",
				Reason:      reason,
				Error:       err.Error(),
			})
		} else if !allowed {
			auditor.Record(ctx, Decision{
				Timestamp:   start.UTC(),
				Tool:        toolName,
				Actor:       actor,
				ActionClass: t.policy.ActionClass,
				Consequence: t.policy.Consequence,
				Verdict:     "deny",
				Reason:      reason,
				DurationMS:  msSince(start),
			})
			return denyContent(toolName, "not authorized: "+reason), nil
		}
	}

	// 4. Delegate.
	result, err := t.inner.InvokableRun(ctx, argsJSON, opts...)

	// 5. Audit (only when required — read-only tools stay quiet by default).
	recordAudit(ctx, auditor, t.policy, toolName, actor, argsJSON, start, err)
	return result, err
}

// hitlEscalates returns (true, reason) when the policy says HITL must fire for
// this call. For HITLRequired it always fires. For HITLConditional it fires
// only when at least one of the policy's declared triggers is present in the
// request context (set via WithTriggers).
func (t *policyCheckedTool) hitlEscalates(ctx context.Context) (bool, string) {
	return hitlEscalates(ctx, t.policy)
}

// toolName reads the inner tool's Info() once. Info is called by Eino at
// registration anyway, and implementations are expected to be cheap and cached.
// On error we fall back to a placeholder so the audit record is still emitted.
func (t *policyCheckedTool) toolName(ctx context.Context) string {
	info, err := t.inner.Info(ctx)
	if err != nil || info == nil {
		return "<unknown>"
	}
	return info.Name
}

// denyContent produces the (string, nil) shape the ReAct loop can recover from.
// Returning a Go error would abort the entire turn — see APITool.InvokableRun
// (egent-public-apis/tool/api_tool.go:144-146) for the same pattern.
func denyContent(toolName, reason string) string {
	return fmt.Sprintf("[policy: denied %q — %s]", toolName, reason)
}

func hitlPendingContent(toolName, reason string) string {
	return fmt.Sprintf("[awaiting human approval — tool %q requires sign-off (%s). Approve via the HITL surface to proceed.]", toolName, reason)
}

func verdictFromResult(err error) string {
	if err != nil {
		return "error"
	}
	return "allow"
}

func msSince(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

// redactArgs keeps the audit log small and PII-free. We unmarshal to a map and
// re-marshal only keys that look safe (no auth/token/password/secret). If
// parsing fails we emit just the byte length.
func redactArgs(argsJSON string) any {
	if argsJSON == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return map[string]any{"bytes": len(argsJSON)}
	}
	safe := make(map[string]any, len(m))
	for k, v := range m {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "token") ||
			strings.Contains(lk, "auth") ||
			strings.Contains(lk, "password") ||
			strings.Contains(lk, "secret") ||
			strings.Contains(lk, "key") {
			safe[k] = "<redacted>"
			continue
		}
		safe[k] = v
	}
	return safe
}
