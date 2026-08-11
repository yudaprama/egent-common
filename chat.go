// Package egentcommon provides shared types and helpers for egent binaries
// that expose an OpenAI-compatible chat completion HTTP endpoint. All three
// agent binaries (egent-public-apis, egent-jigsawstack, egent-crew) duplicate
// these types and functions — this package is their single source of truth.
package egentcommon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
)

// ChatCompletionRequest is an OpenAI-compatible chat completion request body.
type ChatCompletionRequest struct {
	Model       string                  `json:"model"`
	Messages    []ChatCompletionMessage `json:"messages"`
	Stream      bool                    `json:"stream,omitempty"`
	Temperature float64                 `json:"temperature,omitempty"`
	MaxTokens   int                     `json:"max_tokens,omitempty"`
}

// ChatCompletionMessage is a single message in a chat completion request or
// response. Content is either a plain string or an array of typed parts
// ([{"type":"text","text":"hi"},...]) as emitted by AI SDK clients.
type ChatCompletionMessage struct {
	Role    string `json:"role,omitempty"`
	Content any    `json:"content,omitempty"`
	// ReasoningContent carries thinking-model "reasoning_content" deltas so the
	// client can render them as reasoning. Required for thinking models (e.g.
	// venice/nano-30b) that may emit reasoning without content; without this
	// field the stream silently drops the reasoning and the user sees nothing.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// ToolCalls carries the model's tool invocations (streamed name/arguments)
	// so the client renders a Tool component. eino's ToolCall JSON is already
	// OpenAI-compatible ({index,id,type,function}).
	ToolCalls []schema.ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a tool-result delta (Role="tool") back to its call so
	// the client can fill the Tool component's output.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Name carries the tool name on a tool-result delta (Role="tool") — Eino's
	// schema.Message.ToolName. Connector loops run some calls inside the egent and
	// forward only the result (no preceding tool_calls delta), so without this the
	// client can't recover the tool name and falls back to a generic "tool".
	Name string `json:"name,omitempty"`
}

// ChatCompletionResponse is a non-streaming chat completion response.
type ChatCompletionResponse struct {
	ID        string                   `json:"id"`
	Object    string                   `json:"object"`
	Created   int64                    `json:"created"`
	Model     string                   `json:"model"`
	Choices   []ChatCompletionChoice   `json:"choices"`
	Interrupt *ChatCompletionInterrupt `json:"interrupt,omitempty"`
}

// ChatCompletionInterrupt is an additive wire representation of an Eino
// approval interrupt. OpenAI clients can ignore the field; richer clients can
// render an approval action and call the egent resume endpoint.
type ChatCompletionInterrupt struct {
	CheckpointID string `json:"checkpoint_id"`
	InterruptID  string `json:"interrupt_id"`
	AgentID      string `json:"agent_id,omitempty"`
	Tool         string `json:"tool,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Args         any    `json:"args,omitempty"`
}

// ChatCompletionChoice is a single choice in a non-streaming response.
type ChatCompletionChoice struct {
	Index        int                   `json:"index"`
	Message      ChatCompletionMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

// ChatCompletionChunk is a single SSE chunk in a streaming response.
type ChatCompletionChunk struct {
	ID        string                      `json:"id"`
	Object    string                      `json:"object"`
	Created   int64                       `json:"created"`
	Model     string                      `json:"model"`
	Choices   []ChatCompletionChunkChoice `json:"choices"`
	Interrupt *ChatCompletionInterrupt    `json:"interrupt,omitempty"`
}

// ChatCompletionChunkChoice is a single choice in a streaming chunk.
type ChatCompletionChunkChoice struct {
	Index        int                   `json:"index"`
	Delta        ChatCompletionMessage `json:"delta"`
	FinishReason *string               `json:"finish_reason"`
}

// MessageText extracts concatenated text from an OpenAI content field, which
// may be a plain string ("hi") or an array of typed parts
// ([{"type":"text","text":"hi"}, ...]) as emitted by multipart clients such as
// the Vercel AI SDK's convertToModelMessages.
func MessageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			switch p := part.(type) {
			case string:
				b.WriteString(p)
			case map[string]any:
				switch t, _ := p["type"].(string); t {
				case "text", "input_text", "output_text":
					if s, _ := p["text"].(string); s != "" {
						b.WriteString(s)
					}
				}
			}
		}
		return b.String()
	}
	return ""
}

// BuildConversationQuery formats the full conversation history so the agent
// has multi-turn context. When there is only a single user message, it is
// returned as-is for zero overhead.
func BuildConversationQuery(messages []ChatCompletionMessage) string {
	if len(messages) == 1 {
		return MessageText(messages[0].Content)
	}

	var b strings.Builder
	for _, m := range messages {
		switch m.Role {
		case "system":
			// system prompt is already in the agent instruction
		case "user":
			fmt.Fprintf(&b, "User: %s\n", MessageText(m.Content))
		case "assistant":
			fmt.Fprintf(&b, "Assistant: %s\n", MessageText(m.Content))
		}
	}
	return b.String()
}

// BuildConversationMessages preserves the OpenAI conversation as native Eino
// messages so agent middleware can reduce tool results and summarize history.
// System messages remain in the request for callers that need them, although
// ChatModelAgent instruction remains the canonical system prompt.
func BuildConversationMessages(messages []ChatCompletionMessage) []*schema.Message {
	out := make([]*schema.Message, 0, len(messages))
	for _, msg := range messages {
		role := schema.RoleType(msg.Role)
		if role == schema.System {
			continue
		}
		switch role {
		case schema.System, schema.User, schema.Assistant, schema.Tool:
		default:
			continue
		}
		out = append(out, &schema.Message{Role: role, Content: MessageText(msg.Content), ToolCallID: msg.ToolCallID, ToolName: msg.Name, ToolCalls: msg.ToolCalls})
	}
	return out
}

// PrefixLatestUserMessage adds server-side memory context without flattening
// the rest of the conversation into a single user prompt.
func PrefixLatestUserMessage(messages []*schema.Message, prefix string) []*schema.Message {
	if prefix == "" || len(messages) == 0 {
		return messages
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == schema.User {
			messages[i].Content = prefix
			return messages
		}
	}
	return messages
}

// LastUserQuestion returns the concatenated text of the most recent user
// message in the slice, or "" when there is none. Used by egent handlers to
// capture the current question (the memory "concept") for SaveTurn, since the
// web client no longer resends chat history.
func LastUserQuestion(messages []ChatCompletionMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return MessageText(messages[i].Content)
		}
	}
	return ""
}

// GenerateID returns a unique chat completion ID.
func GenerateID() string {
	return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
}

// WriteSSE marshals a ChatCompletionChunk and writes it as an SSE data frame
// to the given writer. The writer is NOT flushed — call Flush/Flusher after.
func WriteSSE(writer *bufio.Writer, chunk ChatCompletionChunk) {
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(writer, "data: %s\n\n", data)
}

// EmitDelta marshals a ChatCompletionChunk carrying the given delta, writes it
// as an SSE data frame, and flushes both the buffered writer and the HTTP flusher.
func EmitDelta(writer *bufio.Writer, flusher http.Flusher, id, model string, delta ChatCompletionMessage) {
	WriteSSE(writer, ChatCompletionChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatCompletionChunkChoice{{Index: 0, Delta: delta}},
	})
	writer.Flush()
	flusher.Flush()
}

// BuildDoneChunk builds the final SSE chunk with a "stop" finish reason.
func BuildDoneChunk(id, model string) ChatCompletionChunk {
	finishReason := "stop"
	return ChatCompletionChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatCompletionChunkChoice{{
			Index:        0,
			Delta:        ChatCompletionMessage{},
			FinishReason: &finishReason,
		}},
	}
}

// BuildErrorChunk builds an SSE chunk that carries an error message as
// assistant content so the client can display it.
func BuildErrorChunk(id, model string, err error) ChatCompletionChunk {
	return ChatCompletionChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatCompletionChunkChoice{{
			Index: 0,
			Delta: ChatCompletionMessage{
				Role:    "assistant",
				Content: fmt.Sprintf("\n[Error: %v]", err),
			},
		}},
	}
}

// BuildResponse builds a non-streaming ChatCompletionResponse from the final
// assistant content string.
func BuildResponse(id, model, content string) ChatCompletionResponse {
	return ChatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatCompletionChoice{{
			Index: 0,
			Message: ChatCompletionMessage{
				Role:    "assistant",
				Content: content,
			},
			FinishReason: "stop",
		}},
	}
}

// WriteNonStreamingResponse writes a ChatCompletionResponse as JSON.
func WriteNonStreamingResponse(w http.ResponseWriter, resp ChatCompletionResponse) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
