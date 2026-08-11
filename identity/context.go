// Package identity carries request-scoped identity (tenant, user, session,
// project) through the context. It is the shared kernel consumed by tools,
// embedding router, ingest workers, and policy — independent of any single
// component implementation.
package identity

import "context"

// context key types. Unexported so they can never collide with keys defined
// elsewhere; only the With*/From* accessors in this package read or write them.
type (
	tenantIDKey  struct{}
	userIDKey    struct{}
	sessionIDKey struct{}
	projectIDKey struct{}
)

// WithTenantID returns a copy of ctx carrying the active workspace (tenant) ID.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantIDKey{}, tenantID)
}

// TenantIDFromContext returns the active workspace ID, or "" when unset.
func TenantIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tenantIDKey{}).(string); ok {
		return v
	}
	return ""
}

// WithUserID returns a copy of ctx carrying the authenticated user ID.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey{}, userID)
}

// UserIDFromContext returns the user ID, or "" when unset.
func UserIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(userIDKey{}).(string); ok {
		return v
	}
	return ""
}

// WithSessionID returns a copy of ctx carrying the session/conversation ID.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// SessionIDFromContext returns the session ID, or "" when unset.
func SessionIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sessionIDKey{}).(string); ok {
		return v
	}
	return ""
}

// WithProjectID returns a copy of ctx carrying the active project ID.
func WithProjectID(ctx context.Context, projectID string) context.Context {
	return context.WithValue(ctx, projectIDKey{}, projectID)
}

// ProjectIDFromContext returns the project ID, or "" when unset.
func ProjectIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(projectIDKey{}).(string); ok {
		return v
	}
	return ""
}
