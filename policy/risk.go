package policy

// RiskLevel classifies the operational risk of a tool call for smart HITL
// gating. Low-risk calls auto-execute; high-risk calls always interrupt.
type RiskLevel int

const (
	RiskLow    RiskLevel = 0 // auto-execute, never interrupt
	RiskMedium RiskLevel = 1 // interrupt only in strict mode
	RiskHigh   RiskLevel = 2 // always interrupt (safety-critical, financial, delete)
)

// String returns a human-readable label.
func (r RiskLevel) String() string {
	switch r {
	case RiskLow:
		return "low"
	case RiskMedium:
		return "medium"
	case RiskHigh:
		return "high"
	default:
		return "unknown"
	}
}

// ClassifyRisk derives a RiskLevel from the tool's XAgenticAccess policy.
// The mapping is:
//
//	safety-critical            → high
//	HITLRequired               → high
//	HITLConditional + write    → medium
//	HITLConditional + read     → low
//	HITLNone + write           → medium
//	HITLNone + read            → low
func ClassifyRisk(p XAgenticAccess) RiskLevel {
	if p.Consequence == ConsequenceSafetyCritical || p.HITL == HITLRequired {
		return RiskHigh
	}
	if p.HITL == HITLConditional {
		return RiskMedium
	}
	if p.IsMutating() {
		return RiskMedium
	}
	return RiskLow
}
