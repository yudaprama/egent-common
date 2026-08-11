package egentcommon

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

type contextRegressionModel struct {
	t    *testing.T
	call int
}

func (m *contextRegressionModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *contextRegressionModel) Generate(_ context.Context, messages []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.call++
	switch m.call {
	case 1:
		return schema.AssistantMessage("run large tool", []schema.ToolCall{{
			ID:       "large-call",
			Function: schema.FunctionCall{Name: "large_output", Arguments: `{}`},
		}}), nil
	case 2:
		toolResult := lastToolMessage(messages)
		if !strings.Contains(toolResult, "Full output saved to:") {
			m.t.Fatalf("reduction did not replace large tool output: %q", toolResult)
		}
		path := regexp.MustCompile(`Full output saved to: ([^\n]+)`).FindStringSubmatch(toolResult)
		if len(path) != 2 {
			m.t.Fatalf("reduction notice did not contain an offload path: %q", toolResult)
		}
		return schema.AssistantMessage("read the original result", []schema.ToolCall{{
			ID:       "read-call",
			Function: schema.FunctionCall{Name: "read_file", Arguments: fmt.Sprintf(`{"file_path":%q}`, strings.TrimSpace(path[1]))},
		}}), nil
	case 3:
		toolResult := lastToolMessage(messages)
		if !strings.Contains(toolResult, "ORIGINAL-LARGE-OUTPUT") {
			m.t.Fatalf("read_file did not restore original tool output: %q", toolResult)
		}
		return schema.AssistantMessage("completed", nil), nil
	default:
		return nil, fmt.Errorf("unexpected model call %d", m.call)
	}
}

func (m *contextRegressionModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, fmt.Errorf("stream is not used by this regression test")
}

func lastToolMessage(messages []*schema.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == schema.Tool {
			return messages[i].Content
		}
	}
	return ""
}

type largeOutputTool struct{}

func (largeOutputTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "large_output", Desc: "returns a large output"}, nil
}

func (largeOutputTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	return "ORIGINAL-LARGE-OUTPUT-" + strings.Repeat("x", 200), nil
}

func TestContextMiddlewareAgentReductionAndReadFile(t *testing.T) {
	ctx := context.Background()
	model := &contextRegressionModel{t: t}
	middlewares, err := BuildContextMiddlewares(ctx, ContextMiddlewareConfig{
		Backend:          filesystem.NewInMemoryBackend(),
		RootDir:          "/regression",
		MaxOutputLength:  40,
		MaxContextTokens: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}

	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "context-regression",
		Instruction: "Use tools when requested.",
		Model:       model,
		Handlers:    middlewares,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools: []tool.BaseTool{largeOutputTool{}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	iter := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent}).Run(ctx, []adk.Message{schema.UserMessage("start")})
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			t.Fatal(event.Err)
		}
	}
	if model.call != 3 {
		t.Fatalf("model calls = %d, want 3", model.call)
	}
}
