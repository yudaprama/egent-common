package policy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
)

// ---- registry tests ---------------------------------------------------------

func TestRegistryLookupDefault(t *testing.T) {
	def := XAgenticAccess{ActionClass: ActionConnected, Consequence: ConsequenceRead}
	r := NewRegistry(def)

	got := Lookup(r, "unknown_tool")
	if got.ActionClass != ActionConnected {
		t.Errorf("expected default, got %v", got)
	}
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	def := XAgenticAccess{ActionClass: ActionConnected, Consequence: ConsequenceRead}
	r := NewRegistry(def)

	sendMail := XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceWrite,
		Subject:     SubjectRequired,
		HITL:        HITLRequired,
		Audit:       AuditRequired,
	}
	r.Register("send_email", sendMail)

	got := Lookup(r, "send_email")
	if got.ActionClass != ActionActing {
		t.Errorf("expected acting, got %v", got)
	}
	if got.HITL != HITLRequired {
		t.Errorf("expected HITL required, got %v", got)
	}
}

func TestRegistryRegisterBatch(t *testing.T) {
	def := XAgenticAccess{ActionClass: ActionConnected, Consequence: ConsequenceRead}
	r := NewRegistry(def)

	mutating := XAgenticAccess{ActionClass: ActionActing, Consequence: ConsequenceWrite}
	r.RegisterBatch([]string{"tool_a", "tool_b", "tool_c"}, mutating)

	for _, name := range []string{"tool_a", "tool_b", "tool_c"} {
		got := Lookup(r, name)
		if got.ActionClass != ActionActing {
			t.Errorf("%s: expected acting, got %v", name, got)
		}
	}
}

// Lookup is a test helper that calls r.Lookup.
func Lookup(r *PolicyRegistry, name string) XAgenticAccess {
	return r.Lookup(name)
}

// ---- middleware tests --------------------------------------------------------

func TestMiddlewareAllowReadTool(t *testing.T) {
	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionConnected,
		Consequence: ConsequenceRead,
		Subject:     SubjectOptional,
		HITL:        HITLNone,
		Audit:       AuditNone,
	})
	mw := NewMiddleware(registry, nil)

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		return "result", nil
	}
	tCtx := &adk.ToolContext{Name: "get_weather", CallID: "c1"}

	endpoint, err := mw.WrapInvokableToolCall(context.Background(), inner, tCtx)
	if err != nil {
		t.Fatalf("WrapInvokableToolCall: %v", err)
	}

	got, err := endpoint(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if got != "result" {
		t.Errorf("expected result, got %q", got)
	}
}

func TestMiddlewareDenyWhenSubjectRequiredAndNoActor(t *testing.T) {
	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceWrite,
		Subject:     SubjectRequired,
		HITL:        HITLNone,
		Audit:       AuditRequired,
	})
	aud := &captureAuditor{}
	mw := NewMiddleware(registry, nil)

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		t.Fatal("inner should not be called")
		return "", nil
	}
	tCtx := &adk.ToolContext{Name: "send_email", CallID: "c2"}

	ctx := WithAuditor(context.Background(), aud) // no actor
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)

	got, err := endpoint(ctx, `{}`)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if !strings.Contains(got, "[policy: denied") {
		t.Errorf("expected deny content, got %q", got)
	}
	if len(aud.records) != 1 || aud.records[0].Verdict != "deny" {
		t.Errorf("expected deny audit record, got %+v", aud.records)
	}
}

func TestMiddlewareAllowWhenActorPresent(t *testing.T) {
	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceWrite,
		Subject:     SubjectRequired,
		HITL:        HITLNone,
		Audit:       AuditRequired,
	})
	aud := &captureAuditor{}
	mw := NewMiddleware(registry, nil)

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		return "sent", nil
	}
	tCtx := &adk.ToolContext{Name: "send_email", CallID: "c3"}

	ctx := WithAuditor(withActor(context.Background(), "user-1"), aud)
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)

	got, err := endpoint(ctx, `{}`)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if got != "sent" {
		t.Errorf("expected sent, got %q", got)
	}
	if len(aud.records) != 1 || aud.records[0].Verdict != "allow" {
		t.Errorf("expected allow audit record, got %+v", aud.records)
	}
	if aud.records[0].Actor != "user-1" {
		t.Errorf("expected actor=user-1, got %q", aud.records[0].Actor)
	}
}

func TestMiddlewareHITLObserveMode(t *testing.T) {
	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceSafetyCritical,
		Subject:     SubjectRequired,
		HITL:        HITLRequired,
		Audit:       AuditRequired,
	})
	aud := &captureAuditor{}
	mw := NewMiddleware(registry, nil)

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		return "executed", nil
	}
	tCtx := &adk.ToolContext{Name: "run_command", CallID: "c4"}

	ctx := WithAuditor(withActor(context.Background(), "u"), aud)
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)

	// Observe mode (default): HITL fires but doesn't block.
	got, err := endpoint(ctx, `{}`)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if got != "executed" {
		t.Errorf("observe mode: expected pass-through, got %q", got)
	}
	foundPending := false
	for _, r := range aud.records {
		if r.Verdict == "hitl_pending_observed" && strings.HasPrefix(r.Reason, "hitl_required") {
			foundPending = true
		}
	}
	if !foundPending {
		t.Errorf("expected hitl_pending_observed in audit: %+v", aud.records)
	}
}

func TestMiddlewareHITLEnforceMode(t *testing.T) {
	t.Setenv("POLICY_ENFORCE_HITL", "1")

	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceSafetyCritical,
		Subject:     SubjectRequired,
		HITL:        HITLRequired,
		Audit:       AuditRequired,
	})
	aud := &captureAuditor{}
	mw := NewMiddleware(registry, nil)

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		t.Fatal("inner should not be called in enforce mode")
		return "", nil
	}
	tCtx := &adk.ToolContext{Name: "run_command", CallID: "c5"}

	ctx := WithAuditor(withActor(context.Background(), "u"), aud)
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)

	_, err := endpoint(ctx, `{}`)
	if err == nil {
		t.Errorf("expected HITL interrupt, got %v", err)
	}
}

func TestMiddlewareConditionalHITLWithTrigger(t *testing.T) {
	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceWrite,
		Subject:     SubjectRequired,
		HITL:        HITLConditional,
		Audit:       AuditRequired,
		Triggers:    []string{TriggerHighValue},
	})
	aud := &captureAuditor{}
	mw := NewMiddleware(registry, nil)

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		return "ok", nil
	}
	tCtx := &adk.ToolContext{Name: "post_msg", CallID: "c6"}

	ctx := WithAuditor(withActor(context.Background(), "u"), aud)
	ctx = WithTriggers(ctx, []string{TriggerHighValue})
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)

	got, err := endpoint(ctx, `{}`)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if got != "ok" {
		t.Errorf("observe mode: expected pass-through, got %q", got)
	}
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

func TestMiddlewareAuthorizeAllow(t *testing.T) {
	t.Setenv("POLICY_ENFORCE_AUTHZ", "1")

	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceWrite,
		Subject:     SubjectRequired,
		HITL:        HITLNone,
		Audit:       AuditRequired,
	})
	authz := stubAuthorizer{allow: true, reason: "ok"}
	mw := NewMiddleware(registry, authz)

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		return "done", nil
	}
	tCtx := &adk.ToolContext{Name: "t", CallID: "c7"}

	ctx := WithAuditor(withActor(context.Background(), "u"), &captureAuditor{})
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)

	got, err := endpoint(ctx, `{}`)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if got != "done" {
		t.Errorf("expected done, got %q", got)
	}
}

func TestMiddlewareAuthorizeDeny(t *testing.T) {
	t.Setenv("POLICY_ENFORCE_AUTHZ", "1")

	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceWrite,
		Subject:     SubjectRequired,
		HITL:        HITLNone,
		Audit:       AuditRequired,
	})
	authz := stubAuthorizer{allow: false, reason: "not_member"}
	mw := NewMiddleware(registry, authz)

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		t.Fatal("inner should not be called")
		return "", nil
	}
	tCtx := &adk.ToolContext{Name: "t", CallID: "c8"}

	aud := &captureAuditor{}
	ctx := WithAuditor(withActor(context.Background(), "u"), aud)
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)

	got, err := endpoint(ctx, `{}`)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if !strings.Contains(got, "not authorized") {
		t.Errorf("expected denial content, got %q", got)
	}
	if aud.records[0].Verdict != "deny" || aud.records[0].Reason != "not_member" {
		t.Errorf("wrong audit record: %+v", aud.records[0])
	}
}

func TestMiddlewareAuthorizeFailClosedSafetyCritical(t *testing.T) {
	t.Setenv("POLICY_ENFORCE_AUTHZ", "1")

	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceSafetyCritical,
		Subject:     SubjectRequired,
		HITL:        HITLNone,
		Audit:       AuditRequired,
	})
	authz := stubAuthorizer{err: errors.New("timeout")}
	mw := NewMiddleware(registry, authz)

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		return "should not reach", nil
	}
	tCtx := &adk.ToolContext{Name: "delete_account", CallID: "c9"}

	aud := &captureAuditor{}
	ctx := WithAuditor(withActor(context.Background(), "u"), aud)
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)

	got, err := endpoint(ctx, `{}`)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if !strings.Contains(got, "fail-closed") {
		t.Errorf("expected fail-closed content, got %q", got)
	}
	if aud.records[0].Verdict != "deny" {
		t.Errorf("expected deny verdict, got %q", aud.records[0].Verdict)
	}
}

func TestMiddlewareAuthorizeFailOpenNonCritical(t *testing.T) {
	t.Setenv("POLICY_ENFORCE_AUTHZ", "1")

	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceWrite,
		Subject:     SubjectRequired,
		HITL:        HITLNone,
		Audit:       AuditRequired,
	})
	authz := stubAuthorizer{err: errors.New("timeout")}
	mw := NewMiddleware(registry, authz)

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		return "done", nil
	}
	tCtx := &adk.ToolContext{Name: "post_msg", CallID: "c10"}

	aud := &captureAuditor{}
	ctx := WithAuditor(withActor(context.Background(), "u"), aud)
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)

	got, err := endpoint(ctx, `{}`)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if got != "done" {
		t.Errorf("fail-open: expected done, got %q", got)
	}
	if aud.records[0].Verdict == "deny" {
		t.Errorf("fail-open should not deny, got %+v", aud.records[0])
	}
}

func TestMiddlewareToolNotFoundUsesDefault(t *testing.T) {
	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionConnected,
		Consequence: ConsequenceRead,
		Subject:     SubjectOptional,
		HITL:        HITLNone,
		Audit:       AuditNone,
	})
	mw := NewMiddleware(registry, nil)

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		return "ok", nil
	}
	tCtx := &adk.ToolContext{Name: "unknown_tool_xyz", CallID: "c11"}

	endpoint, _ := mw.WrapInvokableToolCall(context.Background(), inner, tCtx)

	got, err := endpoint(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if got != "ok" {
		t.Errorf("expected ok, got %q", got)
	}
}

func TestMiddlewarePassesToolArgsAndError(t *testing.T) {
	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionConnected,
		Consequence: ConsequenceRead,
		Subject:     SubjectOptional,
		HITL:        HITLNone,
		Audit:       AuditNone,
	})
	mw := NewMiddleware(registry, nil)

	var gotArgs string
	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		gotArgs = args
		return "", errors.New("tool error")
	}
	tCtx := &adk.ToolContext{Name: "t", CallID: "c12"}

	endpoint, _ := mw.WrapInvokableToolCall(context.Background(), inner, tCtx)

	_, err := endpoint(context.Background(), `{"q":"test"}`)
	if err == nil {
		t.Fatal("expected error")
	}
	if gotArgs != `{"q":"test"}` {
		t.Errorf("expected args passthrough, got %q", gotArgs)
	}
}

// Ensure the middleware satisfies the ChatModelAgentMiddleware interface.
var _ adk.ChatModelAgentMiddleware = (*PolicyMiddleware)(nil)
