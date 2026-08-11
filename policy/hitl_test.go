package policy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
)

// ---- RiskLevel tests --------------------------------------------------------

func TestClassifyRiskSafetyCritical(t *testing.T) {
	p := XAgenticAccess{Consequence: ConsequenceSafetyCritical, HITL: HITLNone}
	if got := ClassifyRisk(p); got != RiskHigh {
		t.Errorf("expected high, got %v", got)
	}
}

func TestClassifyRiskHITLRequired(t *testing.T) {
	p := XAgenticAccess{Consequence: ConsequenceWrite, HITL: HITLRequired}
	if got := ClassifyRisk(p); got != RiskHigh {
		t.Errorf("expected high, got %v", got)
	}
}

func TestClassifyRiskHITLConditionalWrite(t *testing.T) {
	p := XAgenticAccess{Consequence: ConsequenceWrite, HITL: HITLConditional}
	if got := ClassifyRisk(p); got != RiskMedium {
		t.Errorf("expected medium, got %v", got)
	}
}

func TestClassifyRiskMutatingNoHITL(t *testing.T) {
	p := XAgenticAccess{ActionClass: ActionActing, Consequence: ConsequenceWrite, HITL: HITLNone}
	if got := ClassifyRisk(p); got != RiskMedium {
		t.Errorf("expected medium, got %v", got)
	}
}

func TestClassifyRiskReadOnly(t *testing.T) {
	p := XAgenticAccess{ActionClass: ActionConnected, Consequence: ConsequenceRead, HITL: HITLNone}
	if got := ClassifyRisk(p); got != RiskLow {
		t.Errorf("expected low, got %v", got)
	}
}

func TestRiskLevelString(t *testing.T) {
	tests := []struct {
		r    RiskLevel
		want string
	}{
		{RiskLow, "low"},
		{RiskMedium, "medium"},
		{RiskHigh, "high"},
		{RiskLevel(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.r.String(); got != tt.want {
			t.Errorf("RiskLevel(%d).String() = %q, want %q", tt.r, got, tt.want)
		}
	}
}

// ---- ShouldInterrupt tests --------------------------------------------------

func TestShouldInterruptLowRiskNeverInterrupts(t *testing.T) {
	interrupt, reason := ShouldInterrupt(RiskLow, PrefStrict, time.Time{}, 30*time.Second)
	if interrupt {
		t.Error("low risk should never interrupt")
	}
	if reason != "" {
		t.Errorf("expected empty reason, got %q", reason)
	}
}

func TestShouldInterruptHighRiskAlwaysInterrupts(t *testing.T) {
	interrupt, reason := ShouldInterrupt(RiskHigh, PrefBalanced, time.Time{}, 30*time.Second)
	if !interrupt {
		t.Error("high risk should always interrupt")
	}
	if reason != "risk_high" {
		t.Errorf("expected risk_high, got %q", reason)
	}
}

func TestShouldInterruptHighRiskThrottled(t *testing.T) {
	lastInterrupt := time.Now()
	interrupt, reason := ShouldInterrupt(RiskHigh, PrefBalanced, lastInterrupt, 30*time.Second)
	if interrupt {
		t.Error("should be throttled")
	}
	if reason != "throttled_high" {
		t.Errorf("expected throttled_high, got %q", reason)
	}
}

func TestShouldInterruptMediumRiskStrictMode(t *testing.T) {
	interrupt, reason := ShouldInterrupt(RiskMedium, PrefStrict, time.Time{}, 30*time.Second)
	if !interrupt {
		t.Error("medium risk in strict mode should interrupt")
	}
	if reason != "risk_medium_strict" {
		t.Errorf("expected risk_medium_strict, got %q", reason)
	}
}

func TestShouldInterruptMediumRiskStrictThrottled(t *testing.T) {
	lastInterrupt := time.Now()
	interrupt, reason := ShouldInterrupt(RiskMedium, PrefStrict, lastInterrupt, 30*time.Second)
	if interrupt {
		t.Error("should be throttled")
	}
	if reason != "throttled_medium_strict" {
		t.Errorf("expected throttled_medium_strict, got %q", reason)
	}
}

func TestShouldInterruptMediumRiskBalancedSkips(t *testing.T) {
	interrupt, reason := ShouldInterrupt(RiskMedium, PrefBalanced, time.Time{}, 30*time.Second)
	if interrupt {
		t.Error("medium risk in balanced mode should not interrupt")
	}
	if reason != "balanced_skip" {
		t.Errorf("expected balanced_skip, got %q", reason)
	}
}

func TestShouldInterruptMediumRiskPermissiveSkips(t *testing.T) {
	interrupt, reason := ShouldInterrupt(RiskMedium, PrefPermissive, time.Time{}, 30*time.Second)
	if interrupt {
		t.Error("medium risk in permissive mode should not interrupt")
	}
	if reason != "permissive_skip" {
		t.Errorf("expected permissive_skip, got %q", reason)
	}
}

func TestShouldInterruptCooldownExpired(t *testing.T) {
	lastInterrupt := time.Now().Add(-61 * time.Second)
	interrupt, _ := ShouldInterrupt(RiskHigh, PrefBalanced, lastInterrupt, 30*time.Second)
	if !interrupt {
		t.Error("should interrupt after cooldown expires")
	}
}

// ---- Throttle tests ---------------------------------------------------------

func TestThrottleRecordAndLookup(t *testing.T) {
	th := NewThrottle(30 * time.Second)

	if !th.LastInterrupt("user-1").IsZero() {
		t.Error("expected zero time before any record")
	}

	th.Record("user-1")
	last := th.LastInterrupt("user-1")
	if last.IsZero() {
		t.Error("expected non-zero time after record")
	}
	if time.Since(last) > time.Second {
		t.Error("recorded time should be recent")
	}

	// Different user should be independent.
	if !th.LastInterrupt("user-2").IsZero() {
		t.Error("user-2 should have no record")
	}
}

func TestThrottleCooldown(t *testing.T) {
	th := NewThrottle(5 * time.Second)
	if th.Cooldown() != 5*time.Second {
		t.Errorf("expected 5s cooldown, got %v", th.Cooldown())
	}
}

// ---- UserPolicyPref tests ---------------------------------------------------

func TestResolveUserPolicyPrefDefault(t *testing.T) {
	got := ResolveUserPolicyPref(nil)
	if got != PrefBalanced {
		t.Errorf("expected balanced default, got %q", got)
	}
}

func TestResolveUserPolicyPrefFromGetter(t *testing.T) {
	getter := func() string { return "strict" }
	got := ResolveUserPolicyPref(getter)
	if got != PrefStrict {
		t.Errorf("expected strict, got %q", got)
	}
}

func TestResolveUserPolicyPrefFromEnv(t *testing.T) {
	t.Setenv("POLICY_HITL_DEFAULT_PREF", "permissive")
	got := ResolveUserPolicyPref(nil)
	if got != PrefPermissive {
		t.Errorf("expected permissive from env, got %q", got)
	}
}

func TestResolveUserPolicyPrefInvalidFallsBack(t *testing.T) {
	getter := func() string { return "bogus" }
	got := ResolveUserPolicyPref(getter)
	if got != PrefBalanced {
		t.Errorf("expected balanced fallback, got %q", got)
	}
}

// ---- Smart gating integration tests (middleware level) ----------------------

func TestMiddlewareSmartGateLowRiskPassesThrough(t *testing.T) {
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

	endpoint, _ := mw.WrapInvokableToolCall(context.Background(), inner, tCtx)
	got, _ := endpoint(context.Background(), `{}`)
	if got != "result" {
		t.Errorf("expected result, got %q", got)
	}
}

func TestMiddlewareSmartGateHighRiskInterruptsInEnforceMode(t *testing.T) {
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
		t.Fatal("inner should not be called when interrupted")
		return "", nil
	}
	tCtx := &adk.ToolContext{Name: "run_command", CallID: "c2"}

	ctx := WithAuditor(withActor(context.Background(), "user-1"), aud)
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)

	_, err := endpoint(ctx, `{}`)
	if err == nil {
		t.Errorf("expected HITL interrupt, got %v", err)
	}
	if aud.records[0].Verdict != "hitl_blocked" {
		t.Errorf("expected hitl_blocked, got %q", aud.records[0].Verdict)
	}
}

func TestMiddlewareSmartGateMediumRiskBlockedInStrictMode(t *testing.T) {
	t.Setenv("POLICY_ENFORCE_HITL", "1")

	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceWrite,
		Subject:     SubjectRequired,
		HITL:        HITLConditional,
		Audit:       AuditRequired,
		Triggers:    []string{TriggerHighValue},
	})
	aud := &captureAuditor{}
	prefGetter := func() string { return "strict" }
	mw := NewMiddleware(registry, nil, WithUserPolicyPref(prefGetter))

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		t.Fatal("inner should not be called when interrupted in strict mode")
		return "", nil
	}
	tCtx := &adk.ToolContext{Name: "send_email", CallID: "c3"}

	ctx := WithAuditor(withActor(context.Background(), "user-1"), aud)
	ctx = WithTriggers(ctx, []string{TriggerHighValue})
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)

	_, err := endpoint(ctx, `{}`)
	if err == nil {
		t.Fatalf("expected HITL interrupt in strict mode, got %v", err)
	}
}

func TestMiddlewareSmartGateMediumRiskPassesInBalancedMode(t *testing.T) {
	t.Setenv("POLICY_ENFORCE_HITL", "1")

	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceWrite,
		Subject:     SubjectRequired,
		HITL:        HITLConditional,
		Audit:       AuditRequired,
		Triggers:    []string{TriggerHighValue},
	})
	aud := &captureAuditor{}
	prefGetter := func() string { return "balanced" }
	mw := NewMiddleware(registry, nil, WithUserPolicyPref(prefGetter))

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		return "sent", nil
	}
	tCtx := &adk.ToolContext{Name: "send_email", CallID: "c4"}

	ctx := WithAuditor(withActor(context.Background(), "user-1"), aud)
	ctx = WithTriggers(ctx, []string{TriggerHighValue})
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)

	got, err := endpoint(ctx, `{}`)
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	if got != "sent" {
		t.Errorf("balanced mode should pass through, got %q", got)
	}
}

func TestMiddlewareSmartGateThrottlePreventsDoubleInterrupt(t *testing.T) {
	t.Setenv("POLICY_ENFORCE_HITL", "1")

	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceSafetyCritical,
		Subject:     SubjectRequired,
		HITL:        HITLRequired,
		Audit:       AuditRequired,
	})
	aud := &captureAuditor{}
	th := NewThrottle(30 * time.Second)
	mw := NewMiddleware(registry, nil, WithThrottle(th))

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		return "executed", nil
	}
	tCtx := &adk.ToolContext{Name: "run_command", CallID: "c5"}

	ctx := WithAuditor(withActor(context.Background(), "user-1"), aud)

	// First call: should interrupt.
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)
	_, err := endpoint(ctx, `{}`)
	if err == nil {
		t.Errorf("first call: expected interrupt, got %v", err)
	}

	// Second call immediately: should be throttled (pass through).
	inner2 := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		return "throttled-pass", nil
	}
	endpoint2, _ := mw.WrapInvokableToolCall(ctx, inner2, tCtx)
	got2, err := endpoint2(ctx, `{}`)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got2 != "throttled-pass" {
		t.Errorf("second call: expected pass-through due to throttle, got %q", got2)
	}

	// Audit should show throttled reason.
	found := false
	for _, r := range aud.records {
		if strings.Contains(r.Reason, "throttled_high") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected throttled_high in audit: %+v", aud.records)
	}
}

func TestMiddlewareSmartGateCustomThrottleCooldown(t *testing.T) {
	t.Setenv("POLICY_ENFORCE_HITL", "1")

	registry := NewRegistry(XAgenticAccess{
		ActionClass: ActionActing,
		Consequence: ConsequenceSafetyCritical,
		Subject:     SubjectRequired,
		HITL:        HITLRequired,
		Audit:       AuditRequired,
	})
	aud := &captureAuditor{}
	// Very short cooldown for testing.
	th := NewThrottle(50 * time.Millisecond)
	mw := NewMiddleware(registry, nil, WithThrottle(th))

	inner := func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		return "ok", nil
	}
	tCtx := &adk.ToolContext{Name: "run_command", CallID: "c6"}
	ctx := WithAuditor(withActor(context.Background(), "user-1"), aud)

	// First call: interrupt.
	endpoint, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)
	_, err := endpoint(ctx, `{}`)
	if err == nil {
		t.Fatalf("expected interrupt, got %v", err)
	}

	// Immediately: throttled.
	endpoint2, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)
	got2, _ := endpoint2(ctx, `{}`)
	if got2 != "ok" {
		t.Errorf("expected pass-through (throttled), got %q", got2)
	}

	// After cooldown: interrupt again.
	time.Sleep(60 * time.Millisecond)
	endpoint3, _ := mw.WrapInvokableToolCall(ctx, inner, tCtx)
	_, err = endpoint3(ctx, `{}`)
	if err == nil {
		t.Errorf("expected interrupt after cooldown, got %v", err)
	}
}

// Ensure backward compatibility: existing NewMiddleware(registry, nil) still compiles.
var _ = NewMiddleware(nil, nil)
