package egentcommon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/yudaprama/egent-common/alistbackend"
	"github.com/yudaprama/egent-common/localbackend"
)

func TestStartContextCleanup_NilAndInMemory(t *testing.T) {
	// Nil backend and in-memory backend must be no-ops, never panic.
	t.Parallel()
	StartContextCleanup(context.Background(), nil, 10*time.Millisecond)
	StartContextCleanup(context.Background(), filesystem.NewInMemoryBackend(), 10*time.Millisecond)
	StartContextCleanup(context.Background(), filesystem.NewInMemoryBackend(), 0)
	// Give the goroutine (if any were started) a moment to prove it doesn't run.
	time.Sleep(30 * time.Millisecond)
}

func TestStartContextCleanup_LocalBackendReapsExpired(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	backend, err := localbackend.New(localbackend.Config{
		Root:          root,
		MaxFileBytes:  1024,
		MaxTotalBytes: 4096,
		TTL:           80 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new localbackend: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scopedCtx := alistbackend.WithScope(ctx, alistbackend.Scope{
		TenantID: "t", SessionID: "s", RequestID: "r",
	})
	if err := backend.Write(scopedCtx, &filesystem.WriteRequest{
		FilePath: "big_tool_output.json",
		Content:  "{\"data\":\"x\"}",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Resolve the on-disk path the same way the backend does so the test can
	// assert removal without depending on the digest internals.
	rel, err := filepath.Rel(root, findWrittenFile(t, root))
	if err != nil || rel == "" {
		t.Fatalf("locate written file under %s: %v", root, err)
	}
	full := filepath.Join(root, rel)
	if _, err := os.Stat(full); err != nil {
		t.Fatalf("file should exist before TTL: %v", err)
	}

	// interval shorter than TTL so the reaper runs while/after the file expires.
	StartContextCleanup(ctx, backend, 30*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(full); os.IsNotExist(err) {
			return // reaped — pass
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expired context file was not reaped within timeout: %s still exists", full)
}

func TestStartContextCleanup_StopsOnCancel(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	backend, err := localbackend.New(localbackend.Config{
		Root: root, MaxFileBytes: 1024, MaxTotalBytes: 4096, TTL: 80 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new localbackend: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	StartContextCleanup(ctx, backend, 10*time.Millisecond)
	cancel()

	// After cancel the goroutine must exit promptly and never panic, even if we
	// keep writing. This is a smoke test: the deferred cancel above already
	// selected <-ctx.Done().
	time.Sleep(50 * time.Millisecond)
}

// findWrittenFile returns the first regular file under root, failing the test
// if none exists (the Write above must have produced one).
func findWrittenFile(t *testing.T, root string) string {
	t.Helper()
	var found string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if found == "" {
			found = p
		}
		return nil
	})
	if err != nil || found == "" {
		t.Fatalf("no file found under %s: %v", root, err)
	}
	return found
}
