package policy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	usage "github.com/yudaprama/plano-usage"
)

// stubTool is a minimal tool.InvokableTool for tests. It records its calls
// and returns whatever was configured.
type stubTool struct {
	name    string
	calls   int
	lastCtx context.Context
	lastArg string
	result  string
	err     error
}

func (s *stubTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: s.name, Desc: "stub"}, nil
}

func (s *stubTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	s.calls++
	s.lastCtx = ctx
	s.lastArg = argsJSON
	if s.err != nil {
		return "", s.err
	}
	if s.result != "" {
		return s.result, nil
	}
	return "ok", nil
}

// captureAuditor records every Decision so tests can assert on verdicts.
type captureAuditor struct {
	records []Decision
}

func (c *captureAuditor) Record(_ context.Context, d Decision) {
	c.records = append(c.records, d)
}

func withActor(ctx context.Context, actor string) context.Context {
	return usage.WithActorID(ctx, actor)
}

// ---- policy.go tests --------------------------------------------------------

func TestInferFromHTTPMethod(t *testing.T) {
	tests := []struct {
		method        string
		wantClass     ActionClass
		wantConseq    Consequence
		wantSubject   SubjectRequirement
		wantHITL      HITLRequirement
		wantAudit     AuditRequirement
	}{
		{"", ActionConnected, ConsequenceRead, SubjectOptional, HITLNone, AuditNone},
		{"GET", ActionConnected, ConsequenceRead, SubjectOptional, HITLNone, AuditNone},
		{"head", ActionConnected, ConsequenceRead, SubjectOptional, HITLNone, AuditNone},
		{"POST", ActionActing, ConsequenceWrite, SubjectRequired, HITLConditional, AuditRequired},
		{"put", ActionActing, ConsequenceWrite, SubjectRequired, HITLConditional, AuditRequired},
		{"DELETE", ActionActing, ConsequenceWrite, SubjectRequired, HITLConditional, AuditRequired},
	}
	for _, tc := range tests {
		t.Run("method_"+tc.method, func(t *testing.T) {
			p := InferFromHTTPMethod(tc.method)
			if p.ActionClass != tc.wantClass {
				t.Errorf("ActionClass: got %q, want %q", p.ActionClass, tc.wantClass)
			}
			if p.Consequence != tc.wantConseq {
				t.Errorf("Consequence: got %q, want %q", p.Consequence, tc.wantConseq)
			}
			if p.Subject != tc.wantSubject {
				t.Errorf("Subject: got %q, want %q", p.Subject, tc.wantSubject)
			}
			if p.HITL != tc.wantHITL {
				t.Errorf("HITL: got %q, want %q", p.HITL, tc.wantHITL)
			}
			if p.Audit != tc.wantAudit {
				t.Errorf("Audit: got %q, want %q", p.Audit, tc.wantAudit)
			}
		})
	}
}

func TestResolveExplicitOverridesInference(t *testing.T) {
	explicit := &XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceSafetyCritical,
		Subject:     SubjectRequired,
		HITL:        HITLRequired,
		Audit:       AuditRequired,
	}
	p := Resolve(explicit, "GET") // would normally infer connected/read
	if p.Consequence != ConsequenceSafetyCritical {
		t.Errorf("explicit consequence not respected: got %q", p.Consequence)
	}
	if !p.IsSafetyCritical() {
		t.Error("IsSafetyCritical should be true")
	}
	if !p.IsMutating() {
		t.Error("IsMutating should be true for safety-critical")
	}
}

func TestResolvePatchesZerosWithInferredDefaults(t *testing.T) {
	// Partial YAML: only the consequence is set.
	explicit := &XAgenticAccess{Consequence: ConsequenceWrite}
	p := Resolve(explicit, "POST")
	if p.ActionClass == "" {
		t.Error("ActionClass should have been patched from inference")
	}
	if p.Subject == "" {
		t.Error("Subject should have been patched from inference")
	}
	if p.HITL == "" {
		t.Error("HITL should have been patched from inference")
	}
	if p.HITL == HITLConditional && len(p.Triggers) == 0 {
		t.Error("conditional HITL should have inherited default triggers")
	}
}

// ---- decorator.go tests -----------------------------------------------------

func TestReadToolPassesThroughWhenActorMissing(t *testing.T) {
	// A connected/read/optional tool: no actor, no audit, no HITL — should pass.
	inner := &stubTool{name: "get_weather"}
	p := InferFromHTTPMethod("GET")
	wrapped := Wrap(inner, p, nil)

	ctx := context.Background() // no auditor, no actor
	got, err := wrapped.InvokableRun(ctx, `{"q":"Jakarta"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ok" {
		t.Errorf("expected inner tool result, got %q", got)
	}
	if inner.calls != 1 {
		t.Errorf("inner tool should have been called once, got %d", inner.calls)
	}
}

func TestWriteToolDeniesWhenSubjectRequiredAndNoActor(t *testing.T) {
	inner := &stubTool{name: "send_email"}
	p := InferFromHTTPMethod("POST") // subject=required, audit=required
	wrapped := Wrap(inner, p, nil)

	aud := &captureAuditor{}
	ctx := WithAuditor(context.Background(), aud) // no actor

	got, err := wrapped.InvokableRun(ctx, `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "[policy: denied") {
		t.Errorf("expected deny content, got %q", got)
	}
	if inner.calls != 0 {
		t.Errorf("inner tool must NOT be called on deny, got %d calls", inner.calls)
	}
	if len(aud.records) != 1 {
		t.Fatalf("expected 1 audit record, got %d", len(aud.records))
	}
	if aud.records[0].Verdict != "deny" {
		t.Errorf("expected verdict=deny, got %q", aud.records[0].Verdict)
	}
	if aud.records[0].Reason != "no_actor" {
		t.Errorf("expected reason=no_actor, got %q", aud.records[0].Reason)
	}
}

func TestWriteToolAllowsWhenActorPresent(t *testing.T) {
	inner := &stubTool{name: "send_email"}
	p := InferFromHTTPMethod("POST")
	wrapped := Wrap(inner, p, nil)

	aud := &captureAuditor{}
	ctx := WithAuditor(withActor(context.Background(), "user-123"), aud)

	got, err := wrapped.InvokableRun(ctx, `{"to":"a@b.c"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ok" {
		t.Errorf("expected ok, got %q", got)
	}
	if inner.calls != 1 {
		t.Errorf("inner should be called once, got %d", inner.calls)
	}
	// POST policy has audit=required → one allow record after delegation.
	if len(aud.records) != 1 {
		t.Fatalf("expected 1 audit record, got %d", len(aud.records))
	}
	if aud.records[0].Verdict != "allow" {
		t.Errorf("expected verdict=allow, got %q", aud.records[0].Verdict)
	}
	if aud.records[0].Actor != "user-123" {
		t.Errorf("expected actor=user-123, got %q", aud.records[0].Actor)
	}
}

func TestHITLRequiredObservedByDefault(t *testing.T) {
	// Default mode: POLICY_ENFORCE_HITL unset → observe-and-log, don't block.
	inner := &stubTool{name: "delete_account"}
	p := XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceSafetyCritical,
		Subject:     SubjectRequired,
		HITL:        HITLRequired,
		Audit:       AuditRequired,
	}
	wrapped := Wrap(inner, p, nil)

	aud := &captureAuditor{}
	ctx := WithAuditor(withActor(context.Background(), "user-7"), aud)

	got, err := wrapped.InvokableRun(ctx, `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// PoC default: enforcement OFF, so the inner tool still runs.
	if got != "ok" {
		t.Errorf("expected pass-through (enforce off), got %q", got)
	}
	if inner.calls != 1 {
		t.Errorf("inner should still be called in observe mode, got %d", inner.calls)
	}
	// Audit should record the pending observation.
	foundPending := false
	for _, r := range aud.records {
		if r.Verdict == "hitl_pending_observed" && r.Reason == "hitl_required" {
			foundPending = true
		}
	}
	if !foundPending {
		t.Errorf("expected hitl_pending_observed in audit: %+v", aud.records)
	}
}

func TestHITLConditionEscalatesOnMatchingTrigger(t *testing.T) {
	inner := &stubTool{name: "post_message"}
	p := XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceWrite,
		Subject:     SubjectRequired,
		HITL:        HITLConditional,
		Audit:       AuditRequired,
		Triggers:    []string{TriggerHighValue, TriggerAbnormal},
	}
	wrapped := Wrap(inner, p, nil)

	aud := &captureAuditor{}
	ctx := WithAuditor(withActor(context.Background(), "u"), aud)
	ctx = WithTriggers(ctx, []string{TriggerHighValue})

	got, err := wrapped.InvokableRun(ctx, `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ok" {
		t.Errorf("observe mode: expected pass-through, got %q", got)
	}
	// The escalation should be recorded even though enforcement is off.
	foundEscalation := false
	for _, r := range aud.records {
		if strings.HasPrefix(r.Reason, "hitl_conditional:") {
			foundEscalation = true
		}
	}
	if !foundEscalation {
		t.Errorf("expected hitl_conditional:high-value in audit: %+v", aud.records)
	}
}

func TestHITLConditionDoesNotEscalateWithoutTrigger(t *testing.T) {
	inner := &stubTool{name: "post_message"}
	p := XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceWrite,
		Subject:     SubjectRequired,
		HITL:        HITLConditional,
		Audit:       AuditRequired,
		Triggers:    []string{TriggerHighValue},
	}
	wrapped := Wrap(inner, p, nil)

	aud := &captureAuditor{}
	// No triggers in context.
	ctx := WithAuditor(withActor(context.Background(), "u"), aud)

	if _, err := wrapped.InvokableRun(ctx, `{}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, r := range aud.records {
		if strings.HasPrefix(r.Reason, "hitl_") {
			t.Errorf("HITL should not fire without matching trigger, got record: %+v", r)
		}
	}
}

// ---- authorizer tests -------------------------------------------------------

type stubAuthorizer struct {
	allow bool
	reason string
	err   error
}

func (s stubAuthorizer) Authorize(context.Context, string, string, XAgenticAccess) (bool, string, error) {
	return s.allow, s.reason, s.err
}

func TestAuthorizerAllowWhenEnforceAuthz(t *testing.T) {
	inner := &stubTool{name: "t"}
	p := InferFromHTTPMethod("POST")
	wrapped := Wrap(inner, p, stubAuthorizer{allow: true, reason: "ok"})

	t.Setenv("POLICY_ENFORCE_AUTHZ", "1")
	ctx := WithAuditor(withActor(context.Background(), "u"), &captureAuditor{})

	if _, err := wrapped.InvokableRun(ctx, `{}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("allowed: inner should be called, got %d", inner.calls)
	}
}

func TestAuthorizerDenyWhenEnforceAuthz(t *testing.T) {
	inner := &stubTool{name: "t"}
	p := InferFromHTTPMethod("POST")
	wrapped := Wrap(inner, p, stubAuthorizer{allow: false, reason: "not_member"})

	t.Setenv("POLICY_ENFORCE_AUTHZ", "1")
	aud := &captureAuditor{}
	ctx := WithAuditor(withActor(context.Background(), "u"), aud)

	got, err := wrapped.InvokableRun(ctx, `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "not authorized") {
		t.Errorf("expected denial content, got %q", got)
	}
	if inner.calls != 0 {
		t.Errorf("denied: inner should NOT be called, got %d", inner.calls)
	}
	if aud.records[0].Verdict != "deny" || aud.records[0].Reason != "not_member" {
		t.Errorf("wrong audit record: %+v", aud.records[0])
	}
}

func TestAuthorizerFailClosedForSafetyCritical(t *testing.T) {
	inner := &stubTool{name: "delete_account"}
	p := XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceSafetyCritical,
		Subject:     SubjectRequired,
		HITL:        HITLNone, // bypass HITL, isolate authz behaviour
		Audit:       AuditRequired,
	}
	wrapped := Wrap(inner, p, stubAuthorizer{err: errors.New("authorizer timeout")})

	t.Setenv("POLICY_ENFORCE_AUTHZ", "1")
	aud := &captureAuditor{}
	ctx := WithAuditor(withActor(context.Background(), "u"), aud)

	got, _ := wrapped.InvokableRun(ctx, `{}`)
	if !strings.Contains(got, "fail-closed") {
		t.Errorf("safety-critical should fail-closed on authz error, got %q", got)
	}
	if inner.calls != 0 {
		t.Errorf("inner should NOT be called on fail-closed, got %d", inner.calls)
	}
	if aud.records[0].Verdict != "deny" {
		t.Errorf("expected verdict=deny, got %q", aud.records[0].Verdict)
	}
}

func TestAuthorizerFailOpenForNonCritical(t *testing.T) {
	inner := &stubTool{name: "post_msg"}
	p := InferFromHTTPMethod("POST") // consequence=write (not safety-critical)
	wrapped := Wrap(inner, p, stubAuthorizer{err: errors.New("authorizer timeout")})

	t.Setenv("POLICY_ENFORCE_AUTHZ", "1")
	aud := &captureAuditor{}
	ctx := WithAuditor(withActor(context.Background(), "u"), aud)

	if _, err := wrapped.InvokableRun(ctx, `{}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("fail-open: inner should still be called, got %d", inner.calls)
	}
	// Observe record, not a deny.
	if aud.records[0].Verdict == "deny" {
		t.Errorf("fail-open should not deny, got %+v", aud.records[0])
	}
}

// ---- audit redaction --------------------------------------------------------

func TestRedactArgsMasksSecrets(t *testing.T) {
	out := redactArgs(`{"name":"alice","api_key":"sk-xxx","Authorization":"Bearer y","q":"hi"}`)
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", out)
	}
	if m["api_key"] != "<redacted>" {
		t.Errorf("api_key should be redacted, got %v", m["api_key"])
	}
	if m["Authorization"] != "<redacted>" {
		t.Errorf("Authorization should be redacted, got %v", m["Authorization"])
	}
	if m["name"] != "alice" {
		t.Errorf("name should be intact, got %v", m["name"])
	}
	if m["q"] != "hi" {
		t.Errorf("q should be intact, got %v", m["q"])
	}
}

func TestRedactArgsHandlesInvalidJSON(t *testing.T) {
	out := redactArgs("not-json")
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", out)
	}
	if _, exists := m["bytes"]; !exists {
		t.Errorf("expected bytes field for unparseable input, got %v", m)
	}
}

// ---- auditor plumbing -------------------------------------------------------

func TestAuditorFromContextReturnsNoopWhenAbsent(t *testing.T) {
	// Should not panic.
	AuditorFromContext(context.Background()).Record(context.Background(), Decision{})
}

func TestNoopAuditorIsSafe(t *testing.T) {
	var n NoopAuditor
	n.Record(context.Background(), Decision{Tool: "x"})
}
