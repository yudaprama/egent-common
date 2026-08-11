// Package policy implements an x-agentic-access style per-operation governance
// layer for egent tools, modelled on Composio's published contract
// (composio-api-evangelist/agentic-access/composio-agentic-access.yml).
//
// Each tool carries an XAgenticAccess policy describing:
//
//   - ActionClass  — connected (passive) vs acting (mutating)
//   - Consequence  — read / write / safety-critical
//   - Subject      - whether an authenticated subject is required
//   - HITL         — none / conditional / required human-in-the-loop gate
//   - Audit        — none / required decision audit
//
// The decorator in decorator.go enforces the policy inside the Eino tool
// pipeline. The Auditor interface in audit.go is the sink for decision records;
// the default slog implementation is good for a PoC, a Postgres-backed
// implementation is the production target (mirror plano-usage.Record → Talos).
//
// This PoC is observability-first: audit always fires; HITL enforcement and
// authorization are opt-in via env vars (POLICY_ENFORCE_HITL, POLICY_ENFORCE_AUTHZ)
// so the layer can land safely without changing any tool's runtime behaviour.
package policy

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// ActionClass classifies whether a tool passively reads state or acts on it.
type ActionClass string

const (
	ActionConnected ActionClass = "connected" // passive: read / no side effect
	ActionActing    ActionClass = "acting"    // mutating: writes, sends, charges, etc.
)

// Consequence grades the blast radius of a successful call. Safety-critical
// operations must use short-TTL tokens and require human-in-the-loop sign-off.
type Consequence string

const (
	ConsequenceRead           Consequence = "read"
	ConsequenceWrite          Consequence = "write"
	ConsequenceSafetyCritical Consequence = "safety-critical"
)

// SubjectRequirement captures whether the tool needs an authenticated caller.
type SubjectRequirement string

const (
	SubjectOptional SubjectRequirement = "optional"
	SubjectRequired SubjectRequirement = "required"
)

// HITLRequirement captures the human-in-the-loop policy for the tool.
type HITLRequirement string

const (
	HITLNone        HITLRequirement = "none"
	HITLConditional HITLRequirement = "conditional" // HITL only when a Trigger fires
	HITLRequired    HITLRequirement = "required"    // HITL on every invocation
)

// AuditRequirement captures whether each invocation must emit an audit record.
type AuditRequirement string

const (
	AuditNone     AuditRequirement = "none"
	AuditRequired AuditRequirement = "required"
)

// Conditional HITL triggers. Matches Composio's vocabulary; new triggers can
// be added without breaking the wire format.
const (
	TriggerAbnormal   = "abnormal"    // off-hours, new geo, unusual argument shape
	TriggerHighValue  = "high-value"  // calls that move money/data above a threshold
	TriggerDestructive = "destructive" // calls that delete or overwrite state
)

// XAgenticAccess is the per-tool policy contract. It mirrors Composio's
// x-agentic-access extension. Pointer fields in ToolDef make this optional.
type XAgenticAccess struct {
	ActionClass ActionClass        `yaml:"action-class" json:"action_class"`
	Consequence Consequence        `yaml:"consequence"   json:"consequence"`
	Subject     SubjectRequirement `yaml:"subject"       json:"subject"`
	HITL        HITLRequirement    `yaml:"hitl"          json:"hitl"`
	Audit       AuditRequirement   `yaml:"audit"         json:"audit"`
	Triggers    []string           `yaml:"triggers,omitempty" json:"triggers,omitempty"`
}

// InferFromHTTPMethod returns a conservative default policy for an HTTP tool
// based solely on its verb. This lets every existing category YAML get a
// sensible policy without any migration: GET/HEAD become connected/read,
// everything else becomes acting/write. Anything explicit in the YAML via
// tools[].x-agentic-access overrides these defaults.
func InferFromHTTPMethod(method string) XAgenticAccess {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "", "GET", "HEAD":
		return XAgenticAccess{
			ActionClass: ActionConnected,
			Consequence: ConsequenceRead,
			Subject:     SubjectOptional,
			HITL:        HITLNone,
			Audit:       AuditNone,
		}
	default: // POST, PUT, PATCH, DELETE
		return XAgenticAccess{
			ActionClass: ActionActing,
			Consequence: ConsequenceWrite,
			Subject:     SubjectRequired,
			HITL:        HITLConditional,
			Audit:       AuditRequired,
			Triggers:    []string{TriggerAbnormal, TriggerHighValue},
		}
	}
}

// Resolve returns the effective policy: explicit if set, otherwise inferred
// from the HTTP method. This is the single source of truth for "what policy
// applies to this tool" at decoration time.
func Resolve(explicit *XAgenticAccess, method string) XAgenticAccess {
	if explicit != nil {
		// Patch any zero-valued fields with inferred defaults so partial
		// YAML declarations still produce a complete policy.
		inferred := InferFromHTTPMethod(method)
		out := *explicit
		if out.ActionClass == "" {
			out.ActionClass = inferred.ActionClass
		}
		if out.Consequence == "" {
			out.Consequence = inferred.Consequence
		}
		if out.Subject == "" {
			out.Subject = inferred.Subject
		}
		if out.HITL == "" {
			out.HITL = inferred.HITL
		}
		if out.Audit == "" {
			out.Audit = inferred.Audit
		}
		if len(out.Triggers) == 0 && out.HITL == HITLConditional {
			out.Triggers = inferred.Triggers
		}
		return out
	}
	return InferFromHTTPMethod(method)
}

// IsMutating reports whether the policy implies state change. Used by the
// decorator to choose fail-closed vs fail-open behaviour on infra errors.
func (p XAgenticAccess) IsMutating() bool {
	return p.ActionClass == ActionActing ||
		p.Consequence == ConsequenceWrite ||
		p.Consequence == ConsequenceSafetyCritical
}

// IsSafetyCritical reports whether the policy requires the highest fence.
func (p XAgenticAccess) IsSafetyCritical() bool {
	return p.Consequence == ConsequenceSafetyCritical
}

// DelegateAccess is the canonical policy for a supervisor agent delegating a
// sub-task to a specialist — whether in-process (via adk.NewAgentTool, e.g.
// egent-crew's Crew Lead) or federated over loopback HTTP (e.g.
// egent-public-apis' knowledge supervisor). It is a light control/routing
// action, not a direct side effect: the specialist enforces its OWN HITL and
// safety policy on its real tools (e.g. run_command stays safety-critical +
// HITLRequired inside the engineer persona). Keeping the delegation itself
// read + no-HITL avoids double-gating the user on a single dangerous action.
// Both supervisors register this under each delegate tool name.
var DelegateAccess = XAgenticAccess{
	ActionClass: ActionConnected,
	Consequence: ConsequenceRead,
	Subject:     SubjectRequired,
	HITL:        HITLNone,
	Audit:       AuditRequired,
}

// MarshalYAML allows XAgenticAccess to round-trip through YAML configs.
func (p XAgenticAccess) MarshalYAML() (interface{}, error) {
	type plain XAgenticAccess
	return plain(p), nil
}

// UnmarshalYAML allows XAgenticAccess to be embedded under
// `tools[].x-agentic-access:` in the category YAMLs.
func (p *XAgenticAccess) UnmarshalYAML(value *yaml.Node) error {
	type plain XAgenticAccess
	return value.Decode((*plain)(p))
}
