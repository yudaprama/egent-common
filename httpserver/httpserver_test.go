package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteErrorShapeAndStatus(t *testing.T) {
	// Every code should produce a parseable Envelope with the right HTTP
	// status from codeMeta, the code echoed, and request_id from context.
	cases := []struct {
		code       Code
		wantStatus int
		wantType   ErrorType
	}{
		{CodeMethodNotAllowed, 405, ErrorTypeInvalidRequest},
		{CodeInvalidJSON, 400, ErrorTypeInvalidRequest},
		{CodeEmptyMessages, 400, ErrorTypeInvalidRequest},
		{CodeMissingIdentity, 401, ErrorTypeAuth},
		{CodeForbidden, 403, ErrorTypeForbidden},
		{CodeInsufficientBalance, 402, ErrorTypePayment},
		{CodeProviderUnavailable, 503, ErrorTypeServer},
		{CodeProviderError, 502, ErrorTypeServer},
		{CodeUpstreamCallFailed, 502, ErrorTypeServer},
		{CodeStreamingNotSupported, 500, ErrorTypeServer},
		{CodeAgentError, 500, ErrorTypeServer},
		{CodeInternalError, 500, ErrorTypeServer},
	}
	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
			req = req.WithContext(context.WithValue(req.Context(), ctxRequestIDKey{}, "req-test-123"))
			rec := httptest.NewRecorder()

			WriteError(rec, req, tc.code, "detail-msg")

			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d", rec.Code, tc.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type: got %q, want application/json", ct)
			}
			var env Envelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("body not parseable: %v (body=%q)", err, rec.Body.String())
			}
			if env.Error.Code != tc.code {
				t.Errorf("code: got %q, want %q", env.Error.Code, tc.code)
			}
			if env.Error.Type != tc.wantType {
				t.Errorf("type: got %q, want %q", env.Error.Type, tc.wantType)
			}
			if env.Error.Status != tc.wantStatus {
				t.Errorf("body.status: got %d, want %d", env.Error.Status, tc.wantStatus)
			}
			if env.Error.Message != "detail-msg" {
				t.Errorf("message: got %q, want %q", env.Error.Message, "detail-msg")
			}
			if env.Error.RequestID != "req-test-123" {
				t.Errorf("request_id: got %q, want req-test-123", env.Error.RequestID)
			}
			if env.Error.Param != nil {
				t.Errorf("param: want nil, got %v", env.Error.Param)
			}
		})
	}
}

func TestWriteErrorFallsBackForUnknownCode(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	WriteError(rec, req, Code("totally_made_up"), "")
	if rec.Code != 500 {
		t.Errorf("unknown code should default to 500, got %d", rec.Code)
	}
	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body unparseable: %v", err)
	}
	// Unknown code preserved in body (operator can spot the gap) but title/
	// type/status come from the internal_error fallback.
	if env.Error.Code != "totally_made_up" {
		t.Errorf("code should be preserved, got %q", env.Error.Code)
	}
	if env.Error.Type != ErrorTypeServer {
		t.Errorf("type should fall back to server_error, got %q", env.Error.Type)
	}
}

func TestWriteErrorEmptyDetailUsesTitle(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	WriteError(rec, req, CodeMissingIdentity, "")
	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unparseable: %v", err)
	}
	if env.Error.Message != "Missing user identity" {
		t.Errorf("expected title fallback, got %q", env.Error.Message)
	}
}

func TestRequestIDMiddlewareEchoesAndStores(t *testing.T) {
	var seenID string
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = RequestIDFromContext(r.Context())
		w.WriteHeader(200)
	}))

	// 1. Client-supplied id is preserved.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "client-supplied-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seenID != "client-supplied-1" {
		t.Errorf("context: got %q, want client-supplied-1", seenID)
	}
	if got := rec.Header().Get(RequestIDHeader); got != "client-supplied-1" {
		t.Errorf("echo: got %q, want client-supplied-1", got)
	}

	// 2. Missing id is minted.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	minted := rec2.Header().Get(RequestIDHeader)
	if !strings.HasPrefix(minted, "req-") {
		t.Errorf("minted id should start with 'req-', got %q", minted)
	}
	if minted != seenID {
		t.Errorf("stored id (%q) != echoed (%q)", seenID, minted)
	}
}

func TestRecoverMiddleware(t *testing.T) {
	h := RecoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	// Must NOT panic out of the handler.
	h.ServeHTTP(rec, req)

	if rec.Code != 500 {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body unparseable: %v", err)
	}
	if env.Error.Code != CodeInternalError {
		t.Errorf("code: got %q, want internal_error", env.Error.Code)
	}
	if env.Error.Message != "internal panic" {
		t.Errorf("message: got %q, want 'internal panic'", env.Error.Message)
	}
}

func TestChainRunsInOrder(t *testing.T) {
	// End-to-end: Chain = recover → requestID → accessLog → handler.
	// Verify requestID propagates into the handler and into a WriteError body.
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, CodeAgentError, "agent failed")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 500 {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body unparseable: %v", err)
	}
	// request_id was minted by middleware and threaded into the envelope.
	if !strings.HasPrefix(env.Error.RequestID, "req-") {
		t.Errorf("expected request_id minted, got %q", env.Error.RequestID)
	}
	// And echoed on the response header.
	if got := rec.Header().Get(RequestIDHeader); got != env.Error.RequestID {
		t.Errorf("header (%q) != body.request_id (%q)", got, env.Error.RequestID)
	}
}

func TestChainPanicCaughtAndRequestIDAttached(t *testing.T) {
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(errors.New("deep bug"))
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 500 {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	var env Envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	headerID := rec.Header().Get(RequestIDHeader)
	if env.Error.RequestID == "" {
		t.Error("expected request_id attached to body, got empty")
	}
	if env.Error.RequestID != headerID {
		t.Errorf("body.request_id (%q) != header (%q)", env.Error.RequestID, headerID)
	}
}

func TestStatusRecorderPreservesFlusher(t *testing.T) {
	// statusRecorder embeds http.ResponseWriter so http.Flusher (if present
	// on the underlying writer) must pass through. SSE handlers depend on it.
	// httptest.ResponseRecorder implements http.Flusher.
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	f, ok := interface{}(rec).(http.Flusher)
	if !ok {
		t.Fatal("statusRecorder must preserve http.Flusher from underlying writer")
	}
	_ = f
}

func TestEmitStreamDone(t *testing.T) {
	rec := httptest.NewRecorder()
	EmitStreamDone(rec)
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]\n\n") {
		t.Errorf("expected [DONE] terminator, got %q", body)
	}
}

func TestEmitStreamErrorContent(t *testing.T) {
	rec := httptest.NewRecorder()
	EmitStreamErrorContent(rec, "chatcmpl-1", "kawai-pro-max", errors.New("oops"))
	body := rec.Body.String()

	// Must contain an error chunk AND the terminator.
	if !strings.Contains(body, `[Error: oops]`) {
		t.Errorf("expected [Error: oops] in body, got %q", body)
	}
	if !strings.Contains(body, "data: [DONE]\n\n") {
		t.Errorf("expected [DONE] terminator after error chunk, got %q", body)
	}
	// Order matters: error chunk BEFORE terminator.
	errorIdx := strings.Index(body, `[Error: oops]`)
	doneIdx := strings.Index(body, "data: [DONE]")
	if errorIdx < 0 || doneIdx < 0 || errorIdx > doneIdx {
		t.Errorf("error chunk must come before [DONE]: errorIdx=%d doneIdx=%d", errorIdx, doneIdx)
	}
}
