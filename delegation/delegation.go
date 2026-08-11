// Package delegation provides the shared cross-cutting concerns for supervisor
// agents that delegate sub-tasks to specialists. Two supervisors consume it:
//
//   - egent-crew's Crew Lead — delegates in-process to co-located personas via
//     adk.NewAgentTool (the personas share one process on :10550).
//   - egent-public-apis' knowledge agent — delegates over loopback HTTP to the
//     other 11 category egents (separate processes on :10501–:10512) using
//     HTTPDelegateTool.
//
// This package holds what is genuinely common to both so a future supervisor
// (e.g. a jigsawstack multimodal coordinator) can delegate either way without
// re-deriving the target descriptor, identity forwarding, delegate policy, or
// instruction preamble. The transport-specific tool construction stays with
// each caller: in-process supervisors call adk.NewAgentTool directly (a one
// liner); HTTP supervisors use HTTPDelegateTool from this package.
package delegation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	egentcommon "github.com/yudaprama/egent-common"
)

// Target describes one specialist a supervisor can delegate to. ID is the
// stable agent/persona identifier (e.g. "egent_crew_engineer" or
// "egent_finance"); Description is the one-line capability surfaced to the
// supervisor's LLM as the tool's description, so it must be informative enough
// for the model to pick the right specialist.
type Target struct {
	ID          string
	Description string
}

// Identity carries the edge-injected headers that must be forwarded when a
// supervisor delegates to a specialist over HTTP, so the sub-call runs as the
// same user (billing, memory, knowledge scoping). In-process delegation
// (adk.NewAgentTool) inherits identity via the parent agent's context and does
// NOT need this — it is only for HTTP/federated delegation.
type Identity struct {
	ActorID   string
	TenantID  string
	SessionID string // x-model-affinity
	ProjectID string
}

// ForwardHeaders stamps the identity onto an outbound HTTP request to a
// sibling egent. Empty fields are skipped. The egent ports are loopback-only
// with no auth gate, so no internal-key/auth header is required.
func (i Identity) ForwardHeaders(req *http.Request) {
	if i.ActorID != "" {
		req.Header.Set("x-arch-actor-id", i.ActorID)
	}
	if i.TenantID != "" {
		req.Header.Set("x-tenant-id", i.TenantID)
	}
	if i.SessionID != "" {
		req.Header.Set("x-model-affinity", i.SessionID)
	}
	if i.ProjectID != "" {
		req.Header.Set("x-project-id", i.ProjectID)
	}
}

type identityCtxKey struct{}

// WithIdentity stamps a delegate Identity into the request context so an
// HTTPDelegateTool running deep in the agent tool loop can recover and forward
// it. Call once per request in the HTTP handler.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFromContext recovers the Identity stamped by WithIdentity. Returns
// the zero value when unset (in-process delegation, tests, etc.).
func IdentityFromContext(ctx context.Context) Identity {
	if v, ok := ctx.Value(identityCtxKey{}).(Identity); ok {
		return v
	}
	return Identity{}
}

// Instruction builds a delegation preamble appended to a supervisor's system
// prompt. specialistNoun names what the supervisor delegates to ("crew
// specialist", "category agent", …) and is pluralized automatically. The
// targets' IDs + descriptions are listed so the model knows who it can call.
//
// Supervisors with rich, hand-tuned instructions (e.g. egent-crew's Crew Lead,
// whose prompt lives in a persona YAML) may skip this and keep their bespoke
// prose; it is provided for supervisors that want a consistent default.
func Instruction(targets []Target, specialistNoun string) string {
	var b strings.Builder
	b.WriteString("\n\nSUPERVISOR — CROSS-DOMAIN DELEGATION\n")
	fmt.Fprintf(&b, "You coordinate %ss. You have one tool per %s and delegate focused sub-tasks to them, then synthesize their answers. When a request spans work no single %s can fully handle:\n",
		specialistNoun, specialistNoun, specialistNoun)
	b.WriteString("- Split it into focused, self-contained sub-tasks.\n")
	fmt.Fprintf(&b, "- Call the matching tool once per sub-task. The %s does not see this conversation, so give it everything it needs.\n",
		specialistNoun)
	b.WriteString("- Synthesize the answers into one coherent reply.\n")
	fmt.Fprintf(&b, "Do NOT delegate a sub-task you can already answer directly, and do NOT call a %s for a single-domain request that should route to it directly.\n\n",
		specialistNoun)
	fmt.Fprintf(&b, "Your %ss:\n", specialistNoun)
	for _, t := range targets {
		fmt.Fprintf(&b, "  - %s: %s\n", t.ID, t.Description)
	}
	return b.String()
}

// HTTPDelegateTool wraps a sibling egent running in a SEPARATE process as an
// InvokableTool. It POSTs a sub-task to that egent's /v1/chat/completions over
// loopback (non-streaming) and returns the specialist's answer. Identity is
// forwarded from the request context (see WithIdentity) so the sub-call runs
// as the same user.
//
// This is the federated counterpart to in-process adk.NewAgentTool: use it when
// the specialists are separate processes (different ports), as in
// egent-public-apis. When they share one process (egent-crew), prefer
// adk.NewAgentTool directly.
type HTTPDelegateTool struct {
	info   *schema.ToolInfo
	port   string
	client *http.Client
}

// NewHTTPDelegateTool creates a tool that delegates a sub-task to the sibling
// egent listening on port. The tool name is delegate_<target.ID>; its single
// parameter is a self-contained "request" string.
func NewHTTPDelegateTool(target Target, port string) *HTTPDelegateTool {
	return &HTTPDelegateTool{
		info: &schema.ToolInfo{
			Name: "delegate_" + target.ID,
			Desc: fmt.Sprintf(
				"Delegate a sub-task to the %s specialist and return its answer. "+
					"Give a clear, self-contained request. Capabilities: %s",
				target.ID, target.Description),
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"request": {
					Desc:     "The self-contained sub-task to delegate to this specialist",
					Type:     schema.String,
					Required: true,
				},
			}),
		},
		port:   port,
		client: &http.Client{Timeout: 90 * time.Second},
	}
}

// Info returns the tool metadata.
func (d *HTTPDelegateTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return d.info, nil
}

// InvokableRun POSTs the sub-task to the sibling egent and returns its answer.
// Errors are returned as content strings (not Go errors) so the supervisor's
// ReAct loop can recover — e.g. when a sibling isn't running in a partial
// `planoctl up --only` dev setup.
func (d *HTTPDelegateTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	var args struct {
		Request string `json:"request"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse delegate args: %w", err)
	}
	if strings.TrimSpace(args.Request) == "" {
		return "", fmt.Errorf("delegate request is empty")
	}

	// Non-streaming OpenAI-format request. The model field is cosmetic — the
	// sibling egent selects its model via MODEL_NAME, not this field.
	payload, err := json.Marshal(map[string]any{
		"model":  "delegated",
		"stream": false,
		"messages": []map[string]string{
			{"role": "user", "content": args.Request},
		},
	})
	if err != nil {
		return "", fmt.Errorf("marshal delegate request: %w", err)
	}

	url := "http://localhost:" + d.port + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return "", fmt.Errorf("create delegate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Forward edge identity so the sibling bills + scopes memory/knowledge as
	// the same user.
	IdentityFromContext(ctx).ForwardHeaders(req)

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Sprintf("delegate_%s: sibling egent unreachable (%v)", d.port, err), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Sprintf("delegate_%s: sibling egent returned HTTP %d", d.port, resp.StatusCode), nil
	}

	var cr egentcommon.ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return fmt.Sprintf("delegate_%s: failed to decode response (%v)", d.port, err), nil
	}
	if len(cr.Choices) == 0 {
		return "(delegate returned no answer)", nil
	}
	text := egentcommon.MessageText(cr.Choices[0].Message.Content)
	if text == "" {
		return "(delegate returned an empty answer)", nil
	}
	return text, nil
}
