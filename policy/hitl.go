package policy

import (
	"os"
	"sync"
	"time"
)

// UserPolicyPref controls per-user HITL gating aggressiveness.
type UserPolicyPref string

const (
	PrefStrict    UserPolicyPref = "strict"    // interrupt medium + high
	PrefBalanced  UserPolicyPref = "balanced"  // interrupt high only (default)
	PrefPermissive UserPolicyPref = "permissive" // interrupt high only, auto-execute medium without log
)

// ResolveUserPolicyPref returns the effective preference. Reads from the
// provided getter (typically user settings). Falls back to env var
// POLICY_HITL_DEFAULT_PREF, then "balanced".
func ResolveUserPolicyPref(getter func() string) UserPolicyPref {
	var raw string
	if getter != nil {
		raw = getter()
	}
	if raw == "" {
		raw = os.Getenv("POLICY_HITL_DEFAULT_PREF")
	}
	switch UserPolicyPref(raw) {
	case PrefStrict:
		return PrefStrict
	case PrefPermissive:
		return PrefPermissive
	default:
		return PrefBalanced
	}
}

// ShouldInterrupt decides whether a tool call should trigger an HITL
// interrupt based on risk level, user preference, and cooldown throttle.
//
// Returns (shouldInterrupt, reason).
func ShouldInterrupt(risk RiskLevel, pref UserPolicyPref, lastInterruptAt time.Time, cooldown time.Duration) (bool, string) {
	// Low-risk always passes through.
	if risk == RiskLow {
		return false, ""
	}

	// High-risk always interrupts regardless of preference.
	if risk == RiskHigh {
		if !lastInterruptAt.IsZero() && time.Since(lastInterruptAt) < cooldown {
			return false, "throttled_high"
		}
		return true, "risk_high"
	}

	// Medium-risk: depends on user preference.
	switch pref {
	case PrefStrict:
		if !lastInterruptAt.IsZero() && time.Since(lastInterruptAt) < cooldown {
			return false, "throttled_medium_strict"
		}
		return true, "risk_medium_strict"
	case PrefPermissive:
		return false, "permissive_skip"
	default: // PrefBalanced
		return false, "balanced_skip"
	}
}

// Throttle tracks the last interrupt timestamp per user for cooldown gating.
// It is safe for concurrent use.
type Throttle struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
	cooldown time.Duration
}

// NewThrottle creates a Throttle with the given cooldown duration.
func NewThrottle(cooldown time.Duration) *Throttle {
	return &Throttle{
		lastSeen: make(map[string]time.Time),
		cooldown: cooldown,
	}
}

// Record marks the current time as an interrupt for the given user.
func (t *Throttle) Record(userID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastSeen[userID] = time.Now()
}

// LastInterrupt returns the last interrupt time for the given user.
// Returns zero time if never recorded.
func (t *Throttle) LastInterrupt(userID string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastSeen[userID]
}

// Cooldown returns the configured cooldown duration.
func (t *Throttle) Cooldown() time.Duration {
	return t.cooldown
}

// Context key for per-request user ID (mirrors usage.WithActorID convention).
type ctxUserIDKey struct{}

// DefaultThrottleCooldown is the default cooldown between interrupts per user.
const DefaultThrottleCooldown = 30 * time.Second
