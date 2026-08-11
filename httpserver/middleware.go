// Package httpserver middleware: request id, structured access log, panic recover.
//
// Order matters and is applied by Chain (outermost first):
//
//	recover  →  requestID  →  accessLog  →  routed handler
//
// recover must be outermost so panics inside the inner middlewares are caught.
// requestID must wrap accessLog so the access log line carries the request id.
//
// This is a direct port of egent-connector/internal/server/middleware.go,
// generalised so the three OpenAI-compatible egents can share it via
// egent-common. Source of truth for the contract.
package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// RequestIDHeader is the canonical header name for end-to-end request
// correlation. Edge (envoy/oathkeeper) may set this on inbound requests; if
// absent, the middleware mints one and echoes it back on the response so
// clients can correlate. plano trace-cmd looks up by guid:x-request-id.
const RequestIDHeader = "X-Request-Id"

type ctxRequestIDKey struct{}

// RequestIDFromContext returns the X-Request-Id stored by
// RequestIDMiddleware, or "" when the chain was not applied.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestIDKey{}).(string)
	return v
}

// newRequestID mints a fresh id. Format: "req-" + 16 hex chars.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "req-" + hex.EncodeToString(b[:])
}

// statusRecorder wraps http.ResponseWriter to capture the status code and
// response size for the access log. It deliberately passes http.Flusher and
// http.Hijacker through from the wrapped writer so SSE streaming and
// websocket upgrades continue to work — the egents' /v1/chat/completions
// handler depends on Flusher for SSE.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

// Flush passes http.Flusher through if the underlying writer supports it.
// Without this, SSE handlers down the chain would see w.(http.Flusher)==nil
// and abort with "streaming not supported".
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// RequestIDMiddleware reads or mints an X-Request-Id, echoes it on the
// response, and stores it in the request context so handlers (and the
// access log) can read it via RequestIDFromContext.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		w.Header().Add(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), ctxRequestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AccessLogMiddleware writes one structured slog record per request at the
// end of the chain. Level is downgraded to Warn for 4xx and Error for 5xx so
// operators can alert on the noisy ones without missing anything actionable.
//
// Uses slog.Default() — the egents already configure slog via slog.Default()
// at startup, so this picks up that configuration automatically.
func AccessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		duration := time.Since(start)
		level := slog.LevelInfo
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case rec.status >= 400:
			level = slog.LevelWarn
		}

		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int("bytes", rec.bytes),
			slog.Duration("duration", duration),
		}
		if q := r.URL.RawQuery; q != "" {
			attrs = append(attrs, slog.String("query", q))
		}
		if ua := r.UserAgent(); ua != "" {
			attrs = append(attrs, slog.String("user_agent", ua))
		}
		if rid := RequestIDFromContext(r.Context()); rid != "" {
			attrs = append(attrs, slog.String("request_id", rid))
		}
		if uid := r.Header.Get("x-arch-actor-id"); uid != "" {
			attrs = append(attrs, slog.String("actor_id", uid))
		}
		if uid := r.Header.Get("X-User-Id"); uid != "" && attrs[len(attrs)-1].Key != "actor_id" {
			attrs = append(attrs, slog.String("user_id", uid))
		}

		slog.Default().LogAttrs(r.Context(), level, "http_request", attrs...)
	})
}

// RecoverMiddleware catches panics anywhere down the chain, logs the stack at
// error level, and emits a 500 envelope with the request id attached so the
// client can find the matching log entry. Without this, a panic in a tool
// callback would tear down the connection with no response body, which looks
// identical to a network failure from the client side.
//
// The request id is read from w.Header() (set by RequestIDMiddleware BEFORE
// calling next) rather than r.Context(): the inner middlewares replace `r`
// via r.WithContext(), so the outer scope's `r` does not carry the id. The
// header IS visible to the outer scope and is the canonical source here.
func RecoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				rid := w.Header().Get(RequestIDHeader)
				slog.ErrorContext(r.Context(), "panic recovered",
					slog.Any("recover", rec),
					slog.String("stack", string(debug.Stack())),
					slog.String("request_id", rid),
					slog.String("path", r.URL.Path),
				)
				// Re-emit the envelope, overriding the status code. The
				// WriteError call sets status + body; if headers were already
				// sent (mid-stream panic) the write is a no-op for the body
				// but the log entry above still has the request id.
				WriteError(w, r, CodeInternalError, "internal panic")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Chain composes the standard middleware stack in the correct order and
// returns a handler ready to pass to http.ListenAndServe. This is the
// recommended way to wrap an egent's mux:
//
//	mux := http.NewServeMux()
//	mux.HandleFunc("/v1/chat/completions", chatHandler)
//	srv := httpserver.Chain(mux)
//	go http.ListenAndServe(addr, srv)
func Chain(handler http.Handler) http.Handler {
	return RecoverMiddleware(
		RequestIDMiddleware(
			AccessLogMiddleware(handler),
		),
	)
}
