// Package httpserver is the unified error + middleware surface for the three
// OpenAI-compatible egents (egent-public-apis, egent-jigsawstack, egent-crew).
//
// The envelope is OpenAI-compatible on the outside (so OpenAI SDK / AI SDK /
// Vercel AI SDK clients parse it without changes) and carries Composio-style
// extension fields on the inside (so machine callers can branch on stable
// codes and operators can correlate via request_id):
//
//	{
//	  "error": {
//	    "message": "Missing user identity",
//	    "type": "authentication_error",
//	    "code": "missing_identity",
//	    "param": null,
//	    "status": 401,
//	    "request_id": "req-...",
//	    "suggested_fix": "Call via edge so X-User-Id is injected."
//	  }
//	}
//
// The middleware stack (request id, access log, panic recover) is the same
// pattern proven in egent-connector/internal/server/. Promoting it here gives
// every egent the same X-Request-Id propagation that plano trace-cmd already
// relies on (plano/cli/planoai/trace_cmd.go looks up by guid:x-request-id).
package httpserver

import (
	"encoding/json"
	"net/http"
)

// ErrorType is the OpenAI-style error classification (error.type field).
// Stable across the three egents.
type ErrorType string

const (
	ErrorTypeInvalidRequest ErrorType = "invalid_request_error" // 400 / 405 / 422
	ErrorTypeAuth           ErrorType = "authentication_error"  // 401
	ErrorTypeForbidden      ErrorType = "forbidden"             // 403
	ErrorTypePayment        ErrorType = "payment_required"      // 402
	ErrorTypeNotFound       ErrorType = "not_found"             // 404
	ErrorTypeRateLimit      ErrorType = "rate_limit_exceeded"   // 429
	ErrorTypeServer         ErrorType = "server_error"          // 500 / 502 / 503
)

// Code is a stable machine slug for the error (error.code field). Stable
// across releases so SDK callers can switch on them.
type Code string

const (
	CodeMethodNotAllowed      Code = "method_not_allowed"
	CodeInvalidJSON           Code = "invalid_json"
	CodeEmptyMessages         Code = "empty_messages"
	CodeMissingPrompt         Code = "missing_prompt"
	CodeMissingIdentity       Code = "missing_identity"
	CodeForbidden             Code = "forbidden"
	CodeInsufficientBalance   Code = "insufficient_balance"
	CodeProviderUnavailable   Code = "provider_unavailable"
	CodeProviderError         Code = "provider_error"
	CodeUpstreamCallFailed    Code = "upstream_call_failed"
	CodeStreamingNotSupported Code = "streaming_not_supported"
	CodeAgentError            Code = "agent_error"
	CodeInternalError         Code = "internal_error"
)

// codeMeta carries the per-code defaults so callers don't have to repeat the
// message/type/suggestion at every site. Add new codes here.
var codeMeta = map[Code]struct {
	Type         ErrorType
	Title        string // stable short summary; used when detail is empty
	Status       int
	SuggestedFix string
}{
	CodeMethodNotAllowed:      {ErrorTypeInvalidRequest, "Method not allowed", http.StatusMethodNotAllowed, "Check the route's allowed method (POST for /v1/chat/completions)."},
	CodeInvalidJSON:           {ErrorTypeInvalidRequest, "Invalid request body", http.StatusBadRequest, "Send a JSON body matching the OpenAI ChatCompletionRequest shape."},
	CodeEmptyMessages:         {ErrorTypeInvalidRequest, "Messages cannot be empty", http.StatusBadRequest, "Include at least one message in the messages array."},
	CodeMissingPrompt:         {ErrorTypeInvalidRequest, "Prompt is required", http.StatusBadRequest, `Send {"prompt":"..."} in the request body.`},
	CodeMissingIdentity:       {ErrorTypeAuth, "Missing user identity", http.StatusUnauthorized, "Call via the edge (Oathkeeper → Kratos) so X-User-Id is injected; or pass x-arch-actor-id for service-to-service."},
	CodeForbidden:             {ErrorTypeForbidden, "Forbidden", http.StatusForbidden, "The caller lacks the required role or workspace membership."},
	CodeInsufficientBalance:   {ErrorTypePayment, "Insufficient balance", http.StatusPaymentRequired, "Top up credits in the dashboard or contact billing."},
	CodeProviderUnavailable:   {ErrorTypeServer, "Provider unavailable", http.StatusServiceUnavailable, "Required upstream (LLM gateway, image API) is not configured or down. Retry with backoff."},
	CodeProviderError:         {ErrorTypeServer, "Upstream provider error", http.StatusBadGateway, "Provider returned an error; retry with backoff. Inspect detail for the upstream message."},
	CodeUpstreamCallFailed:    {ErrorTypeServer, "Upstream call failed", http.StatusBadGateway, "Tool/external API call failed; inspect detail."},
	CodeStreamingNotSupported: {ErrorTypeServer, "Streaming not supported", http.StatusInternalServerError, "The connection does not support streaming (http.Flusher missing); use a non-streaming client."},
	CodeAgentError:            {ErrorTypeServer, "Agent error", http.StatusInternalServerError, "The agent loop returned an error; inspect detail. Often transient — retry."},
	CodeInternalError:         {ErrorTypeServer, "Internal server error", http.StatusInternalServerError, "Unexpected server error. Quote request_id when reporting."},
}

// Envelope is the unified error response body. JSON-marshals to the
// OpenAI-compatible shape with Kawai/Composio-style extensions.
type Envelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inner object. Fields are ordered for OpenAI compatibility
// first (message/type/code/param) then Kawai extensions (status/request_id/
// suggested_fix).
type ErrorBody struct {
	Message      string    `json:"message"`
	Type         ErrorType `json:"type"`
	Code         Code      `json:"code"`
	Param        any       `json:"param"` // OpenAI field; always nil for egent errors
	Status       int       `json:"status"`
	RequestID    string    `json:"request_id,omitempty"`
	SuggestedFix string    `json:"suggested_fix,omitempty"`
}

// WriteError emits the unified error envelope. The code determines status/
// type/suggested-fix defaults; detail overrides the human message when
// non-empty. The X-Request-Id is attached from request context (set by
// RequestIDMiddleware); when context lookup is empty (e.g. inside
// RecoverMiddleware which sees the original request, not the inner
// r.WithContext), the response header is used as a fallback —
// RequestIDMiddleware sets that header before calling next, so it's always
// available by the time any WriteError site fires.
//
// Content-Type is application/json (OpenAI convention), NOT
// application/problem+json — these egents are OpenAI-compatible APIs.
func WriteError(w http.ResponseWriter, r *http.Request, code Code, detail string) {
	meta, ok := codeMeta[code]
	if !ok {
		// Unknown code: fall back to internal_error so we never write a body
		// the client cannot parse. The code itself is preserved in the body
		// so an operator can spot the gap.
		meta = codeMeta[CodeInternalError]
	}
	msg := detail
	if msg == "" {
		msg = meta.Title
	}
	rid := RequestIDFromContext(r.Context())
	if rid == "" {
		// Fallback: the response header is set by RequestIDMiddleware
		// before any inner handler runs, so it's visible even from
		// middlewares that wrap RequestIDMiddleware (e.g. Recover).
		rid = w.Header().Get(RequestIDHeader)
	}
	body := Envelope{Error: ErrorBody{
		Message:      msg,
		Type:         meta.Type,
		Code:         code,
		Param:        nil,
		Status:       meta.Status,
		RequestID:    rid,
		SuggestedFix: meta.SuggestedFix,
	}}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(meta.Status)
	_ = json.NewEncoder(w).Encode(body)
}
