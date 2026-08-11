package egentcommon

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/filesystem"
	filesystemmw "github.com/cloudwego/eino/adk/middlewares/filesystem"
	"github.com/cloudwego/eino/adk/middlewares/reduction"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/yudaprama/egent-common/alistbackend"
	"github.com/yudaprama/egent-common/localbackend"
)

// ContextBackendFromEnv builds the local context backend from AGENT_CONTEXT_PATH.
// The local backend is the only supported context backend — there is no Alist
// or in-memory fallback. If AGENT_CONTEXT_PATH is unset or the backend cannot be
// initialized, the egent cannot manage its context window and must fail fast:
// this panics.
func ContextBackendFromEnv() filesystem.Backend {
	contextRoot := os.Getenv("AGENT_CONTEXT_PATH")
	if contextRoot == "" {
		panic("AGENT_CONTEXT_PATH is required: the context backend has no fallback")
	}
	backend, err := localbackend.New(localbackend.Config{
		Root:          contextRoot,
		MaxFileBytes:  10 * 1024 * 1024,
		MaxTotalBytes: 50 * 1024 * 1024,
		TTL:           30 * time.Minute,
	})
	if err != nil {
		panic(fmt.Sprintf("build local context backend at %s: %v", contextRoot, err))
	}
	return backend
}

// StartContextCleanup runs a periodic cleanup goroutine that reaps expired
// context files across the whole root of the local backend (mtime-based, no
// request scope needed, so the app-lifetime context works). It is a no-op for
// any backend that is not the local backend. The goroutine stops when ctx is
// cancelled — pass the app-lifetime context threaded into NewAgent.
func StartContextCleanup(ctx context.Context, backend filesystem.Backend, interval time.Duration) {
	if backend == nil || interval <= 0 {
		return
	}
	lb, ok := backend.(*localbackend.Backend)
	if !ok {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = lb.CleanupExpired()
			}
		}
	}()
}

// WithContextScope makes the request identity available to the Alist backend.
func WithContextScope(ctx context.Context, tenantID, sessionID, requestID string) context.Context {
	if tenantID == "" {
		tenantID = "unscoped-tenant"
	}
	if sessionID == "" {
		sessionID = "unscoped-session"
	}
	if requestID == "" {
		requestID = GenerateID()
	}
	return alistbackend.WithScope(ctx, alistbackend.Scope{
		TenantID: tenantID, SessionID: sessionID, RequestID: requestID,
	})
}

// ContextMiddlewareConfig configures context reduction and summarization for
// a ChatModelAgent. Backend is shared by the filesystem and reduction
// middleware, so offloaded tool results can be read back by read_file.
type ContextMiddlewareConfig struct {
	Backend                   filesystem.Backend
	RootDir                   string
	MaxOutputLength           int
	MaxContextTokens          int64
	ClearRetentionSuffixLimit int
	TruncExcludeTools         []string
	ClearExcludeTools         []string
	SummaryModel              model.BaseModel[*schema.Message]
	SummaryTrigger            *summarization.TriggerCondition
}

// BuildContextMiddlewares builds the native Eino middleware chain. Reduction
// includes the filesystem tool set because reduction notices reference
// read_file; the tool and reduction must use the same backend.
func BuildContextMiddlewares(ctx context.Context, cfg ContextMiddlewareConfig) ([]adk.ChatModelAgentMiddleware, error) {
	var out []adk.ChatModelAgentMiddleware

	if cfg.Backend != nil {
		truncExcludeTools := append([]string{"read_file"}, cfg.TruncExcludeTools...)
		clearExcludeTools := append([]string{"read_file"}, cfg.ClearExcludeTools...)
		fs, err := filesystemmw.New(ctx, &filesystemmw.MiddlewareConfig{Backend: cfg.Backend})
		if err != nil {
			return nil, fmt.Errorf("build context filesystem middleware: %w", err)
		}
		out = append(out, fs)

		rm, err := reduction.New(ctx, &reduction.Config{
			Backend:                   cfg.Backend,
			RootDir:                   cfg.RootDir,
			MaxLengthForTrunc:         cfg.MaxOutputLength,
			MaxTokensForClear:         cfg.MaxContextTokens,
			ClearRetentionSuffixLimit: cfg.ClearRetentionSuffixLimit,
			TruncExcludeTools:         truncExcludeTools,
			ClearExcludeTools:         clearExcludeTools,
		})
		if err != nil {
			return nil, fmt.Errorf("build context reduction middleware: %w", err)
		}
		out = append(out, rm)
	}

	if cfg.SummaryModel != nil {
		sm, err := summarization.New(ctx, &summarization.Config{
			Model:   cfg.SummaryModel,
			Trigger: cfg.SummaryTrigger,
		})
		if err != nil {
			return nil, fmt.Errorf("build context summarization middleware: %w", err)
		}
		out = append(out, sm)
	}

	return out, nil
}
