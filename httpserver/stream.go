package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"

	egentcommon "github.com/yudaprama/egent-common"
)

// EmitStreamErrorContent writes the conventional "\n[Error: <msg>]" delta
// chunk that all three egents already use, then terminates the SSE stream
// with the [DONE] marker.
//
// Background: the OpenAI-compatible stream contract ends every stream with
// `data: [DONE]\n\n`. The three egents historically emitted an error chunk
// inside the choices array but then `break`-ed the loop without sending
// [DONE], causing well-behaved clients (Vercel AI SDK, OpenAI SDK in stream
// mode) to hang waiting for the terminator. This helper is the single
// replacement for that pattern.
//
// We keep the error-as-content convention (rather than switching to
// `data: {"error":{...}}\n\n`) because LLM agent clients actually read the
// assistant delta content — switching to the OpenAI stream-error shape would
// change agent-observable behaviour. The [DONE] terminator is the only
// behaviour change, and it's a pure bug fix.
func EmitStreamErrorContent(w http.ResponseWriter, id, model string, err error) {
	flusher, _ := w.(http.Flusher)
	if flusher == nil {
		// Cannot stream — nothing useful to do; the caller has already
		// committed the SSE response. Log via slog when this happens; the
		// access log will pick up the request.
		return
	}
	chunk := egentcommon.ChatCompletionChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: 0,
		Model:   model,
		Choices: []egentcommon.ChatCompletionChunkChoice{
			{
				Index: 0,
				Delta: egentcommon.ChatCompletionMessage{
					Role:    "assistant",
					Content: fmt.Sprintf("\n[Error: %v]", err),
				},
				FinishReason: ptrTo("stop"),
			},
		},
	}
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
	// Terminator: signals to OpenAI/AI SDK clients that the stream is over.
	// Without this, clients hang waiting for more data after the error chunk.
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// EmitStreamDone writes just the [DONE] terminator. Use when a handler has
// already written the final content chunk but broken out of the loop before
// reaching the regular done-emission site.
func EmitStreamDone(w http.ResponseWriter) {
	flusher, _ := w.(http.Flusher)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func ptrTo(s string) *string { return &s }
