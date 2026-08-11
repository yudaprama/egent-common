package alistbackend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
)

// Scope identifies the maximum namespace an agent request may access.
type Scope struct {
	TenantID  string
	SessionID string
	RequestID string
}

type scopeKey struct{}

func WithScope(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

func scopeFromContext(ctx context.Context) (Scope, error) {
	scope, ok := ctx.Value(scopeKey{}).(Scope)
	if !ok || scope.TenantID == "" || scope.RequestID == "" {
		return Scope{}, fmt.Errorf("alist backend scope is missing tenant or request identity")
	}
	return scope, nil
}

// ScopeFromContext returns the request scope installed by WithScope. It is
// exported so local and remote backend implementations can share the same
// scope propagation contract.
func ScopeFromContext(ctx context.Context) (Scope, error) {
	return scopeFromContext(ctx)
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:32]
}

// SafeSegment normalizes an identity string (tenant id, request id) into a
// filesystem-safe, readable path segment. It whitelists [a-zA-Z0-9._-],
// strips leading/trailing dots (blocking ".", "..", and hidden files), and
// falls back to a digest only when nothing safe remains — so paths stay
// debuggable (e.g. "ws_abc", "chatcmpl-123") while never letting "../" or
// hostile input reach disk.
func SafeSegment(value string) string {
	s := strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" || out == "." || out == ".." {
		return "x" + digest(s)
	}
	return out
}

func namespace(ctx context.Context, root string) (string, error) {
	scope, err := scopeFromContext(ctx)
	if err != nil {
		return "", err
	}
	return path.Join(root, SafeSegment(scope.TenantID), SafeSegment(scope.RequestID)), nil
}

func scopedPath(ctx context.Context, root, requested string) (string, error) {
	ns, err := namespace(ctx, root)
	if err != nil {
		return "", err
	}
	raw := strings.TrimSpace(requested)
	for _, segment := range strings.Split(strings.TrimPrefix(raw, "/"), "/") {
		if segment == ".." {
			return "", fmt.Errorf("invalid alist path %q", requested)
		}
	}
	clean := path.Clean("/" + raw)
	if clean == "/" || strings.HasPrefix(clean, "/../") || clean == "/.." {
		return "", fmt.Errorf("invalid alist path %q", requested)
	}
	return path.Join(ns, clean), nil
}
