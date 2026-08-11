package egentcommon

import (
	"context"
	"encoding/json"
	"sync"
)

// GuidanceCollector buffers, in call order, the tool-router search guidance that
// connector_find_tools produces during one agent run. The streaming layer pops
// each entry when it emits the matching connector_find_tools result and merges it
// into that result's DISPLAY output.
//
// This is display-only: the guidance never enters the LLM context. The LLM saw
// only the slimmed tool list (connector_find_tools' return value) during the ReAct
// loop; the streamed-out result is not fed back to any model (buildConversationQuery
// forwards only user/assistant text), so enriching it costs zero tokens.
//
// Correlation is FIFO: results stream in the same order the tool ran. A guidance
// entry is popped only for a connector_find_tools result, so it never mismatches a
// connector_list_tools (guidance-less) result. The only imperfect case is two
// PARALLEL find calls in one turn with different queries — rare, low-impact (a card
// may show the sibling call's guidance).
type GuidanceCollector struct {
	mu    sync.Mutex
	queue []json.RawMessage
}

// NewGuidanceCollector returns an empty collector.
func NewGuidanceCollector() *GuidanceCollector { return &GuidanceCollector{} }

// Push appends a guidance payload (nil/empty is ignored).
func (c *GuidanceCollector) Push(g json.RawMessage) {
	if c == nil || len(g) == 0 {
		return
	}
	c.mu.Lock()
	c.queue = append(c.queue, g)
	c.mu.Unlock()
}

// Pop removes and returns the oldest guidance payload, or nil if empty.
func (c *GuidanceCollector) Pop() json.RawMessage {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		return nil
	}
	g := c.queue[0]
	c.queue = c.queue[1:]
	return g
}

type guidanceCollectorKey struct{}

// WithGuidanceCollector stores the collector on the context so connector_find_tools
// can push into it during a run.
func WithGuidanceCollector(ctx context.Context, c *GuidanceCollector) context.Context {
	return context.WithValue(ctx, guidanceCollectorKey{}, c)
}

// GuidanceCollectorFromContext returns the collector, or nil when absent.
func GuidanceCollectorFromContext(ctx context.Context) *GuidanceCollector {
	c, _ := ctx.Value(guidanceCollectorKey{}).(*GuidanceCollector)
	return c
}

// MergeGuidance merges a guidance payload into a display-only tool-result body.
// `content` is connector_find_tools' slim `{"tools":[…]}` JSON; the result is the
// same object plus a top-level `guidance` field. On any parse failure it returns
// content unchanged (fail-safe — never corrupt the tool output).
func MergeGuidance(content string, guidance json.RawMessage) string {
	if len(guidance) == 0 {
		return content
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &body); err != nil || body == nil {
		return content
	}
	body["guidance"] = guidance
	merged, err := json.Marshal(body)
	if err != nil {
		return content
	}
	return string(merged)
}
