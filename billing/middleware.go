package billing

import (
	"context"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	usage "github.com/yudaprama/plano-usage"
)

type Tool struct {
	UsageType string
	Model     string
}

type Config struct {
	Tools map[string]Tool
}

type Middleware struct {
	*adk.TypedBaseChatModelAgentMiddleware[*schema.Message]
	config Config
}

func NewMiddleware(config Config) *Middleware {
	m := &Middleware{config: config}
	m.TypedBaseChatModelAgentMiddleware = &adk.TypedBaseChatModelAgentMiddleware[*schema.Message]{}
	return m
}

func (m *Middleware) WrapInvokableToolCall(
	_ context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	tCtx *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	bt, ok := m.config.Tools[tCtx.Name]
	if !ok {
		return endpoint, nil
	}

	return func(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
		if err := usage.CheckBalance(ctx, bt.UsageType); err != nil {
			return "", err
		}
		result, err := endpoint(ctx, argsJSON, opts...)
		if err != nil {
			return "", err
		}
		usage.Record(ctx, bt.UsageType, bt.Model)
		return result, nil
	}, nil
}
