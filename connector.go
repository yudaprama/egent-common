package egentcommon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	usage "github.com/yudaprama/plano-usage"
)

// Connector tools proxy to the egent-connector sidecar (Composio + future
// providers; see ../CONNECTOR_FEDERATION_ARCHITECTURE.md). Two generic tools are
// registered ONCE and serve every user — the static-agent model can't register
// per-user Composio tools at startup, so instead the per-user identity is read
// from context (x-arch-actor-id → usage.ActorIDFromContext) at exec time and
// forwarded to the sidecar as body.userId. Billing happens IN THE SIDECAR
// (handleExecute wraps ExecuteTool with plano-usage → Talos) so the same call
// is billed once regardless of whether it came from an egent or a direct web
// UI hit. Do NOT re-bill here — that would double-charge the actor. Gated by
// CONNECTOR_URL.
//
// This is the single source of truth shared by egent-public-apis and egent-crew
// (previously each module carried a near-identical copy).

type connectorClient struct {
	baseURL     string // e.g. http://localhost:10560/.connector
	internalKey string
	http        *http.Client
}

// newConnectorClient returns nil when CONNECTOR_URL is unset (tools disabled).
func newConnectorClient() *connectorClient {
	base := strings.TrimRight(os.Getenv("CONNECTOR_URL"), "/")
	if base == "" {
		return nil
	}
	key := os.Getenv("CONNECTOR_INTERNAL_KEY")
	if key == "" {
		key = os.Getenv("PLANO_INTERNAL_KEY")
	}
	return &connectorClient{
		baseURL:     base,
		internalKey: key,
		http:        &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *connectorClient) do(ctx context.Context, method, path string, body any, userID string) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	if c.internalKey != "" {
		req.Header.Set("x-arch-internal-key", c.internalKey)
	}
	if userID != "" {
		req.Header.Set("x-user-id", userID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("connector %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// noConnectorTools reports whether a slimmed tool payload carries zero tools.
func noConnectorTools(slimmed string) bool {
	var p struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal([]byte(slimmed), &p); err != nil {
		return false // on doubt, don't override the real payload
	}
	return len(p.Tools) == 0
}

// --- connector_find_tools ---

type connectorFindTool struct{ c *connectorClient }

func (t *connectorFindTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "connector_find_tools",
		Desc: "Find and prepare the right tool for a specific ACTION on the user's connected apps (GitHub, " +
			"Gmail, etc.) via SEMANTIC search. Call this FIRST — with the user's TASK/INTENT as a full phrase " +
			"(e.g. \"create a GitHub issue\", \"send an email to someone\", \"open a pull request\", \"create a " +
			"calendar event\") — whenever the user wants to DO something (create / send / update / delete / " +
			"comment) with a connected app. NOT bare keywords. The connection already knows who the user is: " +
			"NEVER ask for a username, handle, or account. Returns the matching tools with their schemas; then " +
			"call connector_execute with the best match. Works for account facts/counts too (e.g. \"how many " +
			"public repos does the user have\" → a *_GET_THE_AUTHENTICATED_USER-style profile tool).",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"query": {
				Desc:     "The task or intent, described naturally as a full phrase (e.g. \"count public repositories for the authenticated user\", \"open a pull request\").",
				Type:     schema.String,
				Required: true,
			},
		}),
	}, nil
}

func (t *connectorFindTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("connector_find_tools: parse args: %w", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", fmt.Errorf("connector_find_tools: query is required")
	}
	userID := usage.ActorIDFromContext(ctx)
	if userID == "" {
		return "", fmt.Errorf("connector_find_tools: no user_id in context")
	}
	data, err := t.c.do(ctx, http.MethodPost, "/tools/search", map[string]any{
		"query": args.Query,
		"limit": 10,
	}, userID)
	if err != nil {
		return "", err
	}
	// Stash the rich guidance for the DISPLAY layer (recommended tool / pitfalls /
	// plan / connection / next steps). The LLM only gets slimToolList below; the
	// streaming layer pops this and merges it into the display-only result. See
	// GuidanceCollector.
	if coll := GuidanceCollectorFromContext(ctx); coll != nil {
		coll.Push(extractSearchGuidance(data))
	}
	slimmed := slimToolList(data)
	// No matches usually means the user has no connected app for this task. Return
	// an explicit instruction (ported from the removed connector_list_tools) so the
	// model tells the user to connect an app rather than looping on an empty result.
	if noConnectorTools(slimmed) {
		return `{"tools":[],"note":"No connected-app tool matched. The user likely hasn't connected the relevant app yet — tell them to connect it (e.g. GitHub) in Settings → Connections before you can use connector_execute. Do NOT call connector_execute."}`, nil
	}
	return slimmed, nil
}

// extractSearchGuidance pulls the `guidance` field out of the sidecar
// /tools/search response, returning nil when absent or unparseable.
func extractSearchGuidance(raw []byte) json.RawMessage {
	var p struct {
		Guidance json.RawMessage `json:"guidance"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	return p.Guidance
}

var _ tool.InvokableTool = (*connectorFindTool)(nil)

// --- connector_execute ---

type connectorExecuteTool struct{ c *connectorClient }

func (t *connectorExecuteTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "connector_execute",
		Desc: "Execute ONE connected third-party app tool on behalf of the current user (e.g. a GitHub " +
			"or Gmail action). First get the exact tool name from connector_find_tools (semantic search by " +
			"task/intent). When you only need a " +
			"count or a single fact about the user, PREFER a summary/profile tool that returns it directly " +
			"(e.g. a *_GET_THE_AUTHENTICATED_USER that includes a repo count) over a *_LIST_* tool that returns " +
			"many items. Copy argument names EXACTLY from the chosen tool's params.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"tool": {
				Desc:     "Exact tool name from connector_find_tools, e.g. GITHUB_GET_THE_AUTHENTICATED_USER.",
				Type:     schema.String,
				Required: true,
			},
			"arguments": {
				Desc:     `JSON object (as a string) of the tool's input arguments, e.g. {"owner":"x","repo":"y"}. Use {} if none.`,
				Type:     schema.String,
				Required: false,
			},
		}),
	}, nil
}

func (t *connectorExecuteTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	var args struct {
		Tool      string `json:"tool"`
		Arguments any    `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("connector_execute: parse args: %w", err)
	}
	if strings.TrimSpace(args.Tool) == "" {
		return "", fmt.Errorf("connector_execute: tool is required")
	}
	userID := usage.ActorIDFromContext(ctx)
	if userID == "" {
		return "", fmt.Errorf("connector_execute: no user_id in context")
	}
	// Billing moved into the sidecar (handleExecute) so the same call is
	// billed once regardless of caller (egent vs direct web UI). The sidecar
	// does CheckBalance (returns HTTP 402 with CodeInsufficientBalance) and
	// Record on success. Do NOT re-bill here — that would double-charge the actor.
	var rawArgs any = map[string]any{}
	switch v := args.Arguments.(type) {
	case nil:
		// no arguments — keep the empty object
	case string:
		// Documented form: a JSON object encoded as a string.
		if s := strings.TrimSpace(v); s != "" {
			if err := json.Unmarshal([]byte(s), &rawArgs); err != nil {
				return "", fmt.Errorf("connector_execute: arguments must be a JSON object string: %w", err)
			}
		}
	case map[string]any:
		// Models frequently emit a real object despite the string schema; accept it.
		rawArgs = v
	default:
		return "", fmt.Errorf("connector_execute: arguments must be a JSON object, got %T", args.Arguments)
	}
	data, err := t.c.do(ctx, http.MethodPost, "/tools/execute", map[string]any{
		"userId": userID,
		"tool":   args.Tool,
		"args":   rawArgs,
	}, userID)
	if err != nil {
		// Return the failure AS tool output (not a hard error) so the ReAct loop
		// continues and the model can fix its arguments or pick another tool,
		// instead of the whole turn crashing with NodeRunError. A small model that
		// passes wrong args (the observed intermittent execute 500s) then gets a
		// chance to self-correct rather than aborting the conversation. A 402
		// (insufficient_balance) from the sidecar surfaces here with the same
		// treatment — the model can tell the user to top up.
		return fmt.Sprintf(
			`{"error":%q,"tool":%q,"hint":"That call failed. Re-check the arguments against the tool's params in connector_find_tools and retry, or choose a different tool."}`,
			err.Error(), args.Tool), nil
	}
	out := capConnectorResponse(data)
	slog.Debug("connector_execute", "tool", args.Tool, "raw_bytes", len(data), "llm_bytes", len(out))
	return out, nil
}

var _ tool.InvokableTool = (*connectorExecuteTool)(nil)

// BuildConnectorTools returns the connector tools when CONNECTOR_URL is set, else
// nil. It returns []tool.InvokableTool (both tools are invokable); callers that
// need []tool.BaseTool adapt with a per-element loop (InvokableTool satisfies
// BaseTool). egent-crew appends the slice directly; egent-public-apis loops.
//
// Discovery is intent-search only: connector_find_tools is the sole discovery tool
// (connector_list_tools was removed 2026-07-22). A no-arg curated list was the
// path of least resistance for the model — being zero-arg, first-listed, and
// already sufficient — so it was chosen even for action prompts and the rich
// discovery guidance (which only comes from a search) never surfaced. Forcing all
// discovery through search fixes that.
func BuildConnectorTools() []tool.InvokableTool {
	c := newConnectorClient()
	if c == nil {
		return nil
	}
	return []tool.InvokableTool{
		&connectorFindTool{c: c},
		&connectorExecuteTool{c: c},
	}
}

// --- token-cost control (LLM-facing response slimming) ---

// connectorExecuteMaxBytes caps the raw execute payload returned to the LLM. The
// sidecar returns the provider's full response verbatim; list/search tools can
// emit tens of KB, so this is a safety valve. The returned string need not be
// valid JSON — the model reads it as text.
const connectorExecuteMaxBytes = 6 * 1024

// slimToolList rewrites the sidecar /tools payload for the LLM: it DROPS each
// tool's full JSON Schema (inputSchema) — ~29k tokens for 20 GitHub tools — and
// keeps name/description/app plus a compact param-name summary (required/optional),
// which is enough for the model to construct connector_execute arguments while
// cutting ~94% of the tokens. Fail-safe: on any parse error it returns the raw
// payload unchanged.
func slimToolList(raw []byte) string {
	var in struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			App         string          `json:"app"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || len(in.Tools) == 0 {
		return string(raw)
	}
	type slimParams struct {
		Required []string `json:"required,omitempty"`
		Optional []string `json:"optional,omitempty"`
	}
	type slimTool struct {
		Name        string     `json:"name"`
		Description string     `json:"description,omitempty"`
		App         string     `json:"app,omitempty"`
		Params      slimParams `json:"params"`
	}
	out := struct {
		Tools []slimTool `json:"tools"`
	}{Tools: make([]slimTool, 0, len(in.Tools))}
	for _, t := range in.Tools {
		req, opt := schemaParamNames(t.InputSchema)
		out.Tools = append(out.Tools, slimTool{
			Name: t.Name, Description: t.Description, App: t.App,
			Params: slimParams{Required: req, Optional: opt},
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// schemaParamNames extracts required + optional property names from a JSON Schema
// object, dropping the (large) per-property type/description/examples detail.
func schemaParamNames(rawSchema json.RawMessage) (required, optional []string) {
	if len(rawSchema) == 0 {
		return nil, nil
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(rawSchema, &s); err != nil {
		return nil, nil
	}
	req := make(map[string]bool, len(s.Required))
	for _, r := range s.Required {
		req[r] = true
	}
	required = append(required, s.Required...)
	for k := range s.Properties {
		if !req[k] {
			optional = append(optional, k)
		}
	}
	sort.Strings(optional)
	return required, optional
}

// capConnectorResponse bounds an execute payload sent to the LLM. The common
// overflow is an array of large objects (e.g. GitHub "list repos" returns full
// repo objects, ~2-3KB each). A blind byte-cut there yields INVALID JSON mid-
// object: the model can't parse it, loses the item count it was asked for, and
// retries with other tools — the observed multi-call thrash. So we first try a
// STRUCTURAL slim that caps array lengths while keeping the JSON valid AND
// reporting the true item count (often the literal answer, e.g. repo count).
// Only if that fails (non-JSON, or a single giant object) do we fall back to the
// old rune-boundary byte-cut.
func capConnectorResponse(raw []byte) string {
	if len(raw) <= connectorExecuteMaxBytes {
		return string(raw)
	}
	if slim, ok := slimConnectorArrays(raw, connectorExecuteMaxBytes); ok {
		return slim
	}
	cut := connectorExecuteMaxBytes
	for cut > 0 && !utf8.RuneStart(raw[cut]) {
		cut--
	}
	return string(raw[:cut]) + fmt.Sprintf(
		"\n…[truncated: full response was %d bytes; narrow the request or fetch fewer results]", len(raw))
}

// slimConnectorArrays parses raw JSON and caps every array (at any depth) to a
// small element count, appending a marker element that states the original
// length so the model still learns the true count. It shrinks the per-array
// limit until the re-marshaled result fits budget. Returns ok=false when raw is
// not JSON or can't be brought under budget by capping arrays alone (e.g. one
// oversized object), leaving the caller to fall back to a byte-cut.
func slimConnectorArrays(raw []byte, budget int) (string, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	for _, limit := range []int{20, 10, 5, 3, 1} {
		b, err := json.Marshal(capArrays(v, limit))
		if err != nil {
			return "", false
		}
		if len(b) <= budget {
			return string(b), true
		}
	}
	return "", false
}

// capArrays returns a deep copy of v with every array truncated to limit
// elements plus a trailing string marker naming the dropped count. Objects are
// walked recursively; scalars pass through untouched.
func capArrays(v any, limit int) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = capArrays(val, limit)
		}
		return out
	case []any:
		if len(t) > limit {
			out := make([]any, 0, limit+1)
			for i := range limit {
				out = append(out, capArrays(t[i], limit))
			}
			out = append(out, fmt.Sprintf(
				"…[showing %d of %d items; narrow the request or paginate for the rest]", limit, len(t)))
			return out
		}
		out := make([]any, len(t))
		for i := range t {
			out[i] = capArrays(t[i], limit)
		}
		return out
	default:
		return v
	}
}
