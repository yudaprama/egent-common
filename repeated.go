package egentcommon

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// ReuseRepeatedQuestion reports whether the within-session repeated-question
// cache is enabled. Enabled by default; disable with REUSE_REPEATED_QUESTION=0
// (no existing env var serves this purpose).
//
// Rationale: web/ resends the full chat history on every turn, so each request
// already carries every prior question→answer pair. When the newest user
// message is a plain-text repeat of an earlier user message in the same
// history, the agent can replay the prior answer and skip the LLM entirely —
// zero new storage and zero token charge (billing only fires when the gateway
// is actually called).
func ReuseRepeatedQuestion() bool {
	return os.Getenv("REUSE_REPEATED_QUESTION") != "0"
}

// NormalizeQuestion collapses a user question to a stable comparison key:
// lowercase, trimmed, whitespace-collapsed, trailing punctuation stripped. The
// match stays exact enough to avoid false positives while tolerating trivial
// casing/whitespace drift across the client's resends.
func NormalizeQuestion(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimRight(s, "!?.,;:。！？ ")
	return s
}

// IsPlainText reports whether a message content is a plain string or text-only
// parts. Image/attachment turns are never candidates for replay.
func IsPlainText(content any) bool {
	switch v := content.(type) {
	case string:
		return true
	case []any:
		for _, part := range v {
			if p, ok := part.(map[string]any); ok {
				switch t, _ := p["type"].(string); t {
				case "image_url", "image", "input_image":
					return false
				}
			}
		}
		return true
	}
	return false
}

// FindRepeatedQuestion returns the prior assistant answer text when the newest
// user message is a plain-text repeat of an earlier user message in the same
// history, otherwise nil. The matched prior user turn must have a text
// assistant reply before the next user turn.
func FindRepeatedQuestion(messages []ChatCompletionMessage) *string {
	n := len(messages)
	if n < 3 {
		return nil
	}
	last := messages[n-1]
	if last.Role != "user" || !IsPlainText(last.Content) {
		return nil
	}
	target := NormalizeQuestion(MessageText(last.Content))
	if target == "" {
		return nil
	}
	for i := 0; i < n-1; i++ {
		m := messages[i]
		if m.Role != "user" || !IsPlainText(m.Content) {
			continue
		}
		if NormalizeQuestion(MessageText(m.Content)) != target {
			continue
		}
		for j := i + 1; j < n; j++ {
			switch messages[j].Role {
			case "user":
				return nil // no text reply before the next user turn
			case "assistant":
				if text := strings.TrimSpace(MessageText(messages[j].Content)); text != "" {
					return &text
				}
			}
		}
	}
	return nil
}

// WriteRepeatedResponse serves a cached answer as a non-streaming response.
func WriteRepeatedResponse(w http.ResponseWriter, req ChatCompletionRequest, answer string) {
	WriteNonStreamingResponse(w, BuildResponse(GenerateID(), req.Model, answer))
}

// WriteRepeatedStream serves a cached answer as a single content delta
// followed by a "stop" chunk and [DONE], mirroring the OpenAI-SSE shape the
// streaming handlers emit so AI SDK clients terminate normally. When the
// writer does not support streaming it falls back to a non-streaming response.
func WriteRepeatedStream(w http.ResponseWriter, req ChatCompletionRequest, answer string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteRepeatedResponse(w, req, answer)
		return
	}
	writer := bufio.NewWriter(w)
	id := GenerateID()
	if answer != "" {
		EmitDelta(writer, flusher, id, req.Model, ChatCompletionMessage{Role: "assistant", Content: answer})
	}
	WriteSSE(writer, BuildDoneChunk(id, req.Model))
	fmt.Fprint(writer, "data: [DONE]\n\n")
	writer.Flush()
	flusher.Flush()
}
