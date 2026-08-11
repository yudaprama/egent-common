package policy

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// Decision is the audit record emitted for every policy-gated tool call. The
// shape is intentionally close to the row that the production Postgres audit
// table will hold (see the design note in policy.go).
type Decision struct {
	Timestamp  time.Time      `json:"ts"`
	Tool       string         `json:"tool"`
	Actor      string         `json:"actor,omitempty"`
	ActionClass ActionClass   `json:"action_class"`
	Consequence Consequence   `json:"consequence"`
	Verdict    string         `json:"verdict"`            // allow / deny / hitl_pending / hitl_blocked
	Reason     string         `json:"reason,omitempty"`   // short machine slug, e.g. "no_actor", "authz_error", "hitl_required"
	DurationMS int64          `json:"duration_ms,omitempty"`
	Error      string         `json:"error,omitempty"`
	Extra      map[string]any `json:"extra,omitempty"`
}

// Auditor sinks Decision records. Implementations must be safe for concurrent
// use; Record must be non-blocking (drop on overflow rather than stall the
// tool call).
type Auditor interface {
	Record(ctx context.Context, d Decision)
}

// SlogAuditor emits Decisions as structured JSON via the default *slog.Logger.
// This is the PoC sink; the production sink is a Postgres writer mirroring
// plano-usage.Record → Talos.
type SlogAuditor struct {
	logger *slog.Logger
}

// NewSlogAuditor builds an auditor on top of the given logger. Pass nil to use
// slog.Default().
func NewSlogAuditor(logger *slog.Logger) *SlogAuditor {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlogAuditor{logger: logger}
}

// Record implements Auditor. It is non-blocking: a slow logger will be
// invoked synchronously here (slog is already async-friendly) but never panics
// on nil fields.
func (a *SlogAuditor) Record(ctx context.Context, d Decision) {
	if a == nil || a.logger == nil {
		return
	}
	if d.Timestamp.IsZero() {
		d.Timestamp = time.Now().UTC()
	}
	attrs := []any{
		slog.String("tool", d.Tool),
		slog.String("actor", d.Actor),
		slog.String("action_class", string(d.ActionClass)),
		slog.String("consequence", string(d.Consequence)),
		slog.String("verdict", d.Verdict),
		slog.String("reason", d.Reason),
		slog.Int64("duration_ms", d.DurationMS),
	}
	if d.Error != "" {
		attrs = append(attrs, slog.String("error", d.Error))
	}
	for k, v := range d.Extra {
		attrs = append(attrs, slog.Any(k, v))
	}
	a.logger.InfoContext(ctx, "policy.decision", attrs...)
}

// NoopAuditor discards every decision. Used when the egent runs without an
// auditor wired in (e.g. cmd/genlist, unit tests of tools).
type NoopAuditor struct{}

func (NoopAuditor) Record(context.Context, Decision) {}

// --- context plumbing --------------------------------------------------------

type ctxAuditorKey struct{}

// WithAuditor returns a context carrying the auditor. tool builders and the
// decorator read it back via AuditorFromContext. This mirrors the codebase's
// existing identity-in-context convention (usage.WithActorID,
// identity.WithUserID).
func WithAuditor(ctx context.Context, a Auditor) context.Context {
	return context.WithValue(ctx, ctxAuditorKey{}, a)
}

// AuditorFromContext returns the auditor set by WithAuditor, or a NoopAuditor
// when none is present. Never returns nil.
func AuditorFromContext(ctx context.Context) Auditor {
	if a, ok := ctx.Value(ctxAuditorKey{}).(Auditor); ok && a != nil {
		return a
	}
	return NoopAuditor{}
}

// EnforceHITL reports whether HITL enforcement (returning a synthetic pending
// message instead of letting the call through) is enabled. Default: false
// (observe-and-log only). Flip with POLICY_ENFORCE_HITL=1 so the PoC can ship
// in observability-only mode and be flipped on per-deployment.
func EnforceHITL() bool {
	return os.Getenv("POLICY_ENFORCE_HITL") == "1" ||
		os.Getenv("POLICY_ENFORCE_HITL") == "true"
}

// EnforceAuthz reports whether authorization denials actually block the call.
// Default: false. Flip with POLICY_ENFORCE_AUTHZ=1.
func EnforceAuthz() bool {
	return os.Getenv("POLICY_ENFORCE_AUTHZ") == "1" ||
		os.Getenv("POLICY_ENFORCE_AUTHZ") == "true"
}
