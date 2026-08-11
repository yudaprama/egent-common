package egentcommon

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/schema"
)

func TestBuildConversationMessagesPreservesRolesAndToolCalls(t *testing.T) {
	msgs := BuildConversationMessages([]ChatCompletionMessage{
		{Role: "user", Content: "find it"},
		{Role: "assistant", Content: "", ToolCalls: []schema.ToolCall{{ID: "call-1", Function: schema.FunctionCall{Name: "search", Arguments: "{}"}}}},
		{Role: "tool", Name: "search", ToolCallID: "call-1", Content: "result"},
	})

	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if msgs[1].Role != schema.Assistant || len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("assistant tool call was not preserved: %#v", msgs[1])
	}
	if msgs[2].Role != schema.Tool || msgs[2].ToolCallID != "call-1" {
		t.Fatalf("tool result linkage was not preserved: %#v", msgs[2])
	}
}

func TestBuildContextMiddlewares(t *testing.T) {
	mws, err := BuildContextMiddlewares(context.Background(), ContextMiddlewareConfig{
		Backend:          filesystem.NewInMemoryBackend(),
		MaxOutputLength:  100,
		MaxContextTokens: 1000,
		SummaryTrigger:   &summarization.TriggerCondition{ContextTokens: 2000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mws) != 2 {
		t.Fatalf("got %d middlewares with no summary model, want filesystem + reduction", len(mws))
	}
}
