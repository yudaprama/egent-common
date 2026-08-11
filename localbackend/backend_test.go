package localbackend

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/yudaprama/egent-common/alistbackend"
)

func scopedContext() context.Context {
	return alistbackend.WithScope(context.Background(), alistbackend.Scope{
		TenantID: "tenant-a", SessionID: "session-a", RequestID: "request-a",
	})
}

func TestBackendScopesAndBoundsLocalFiles(t *testing.T) {
	b, err := New(Config{Root: t.TempDir(), MaxFileBytes: 32, MaxTotalBytes: 48})
	if err != nil {
		t.Fatal(err)
	}
	ctx := scopedContext()
	if err := b.Write(ctx, &filesystem.WriteRequest{FilePath: "notes/summary.md", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	got, err := b.Read(ctx, &filesystem.ReadRequest{FilePath: "notes/summary.md"})
	if err != nil || got.Content != "hello" {
		t.Fatalf("read = %#v, err=%v", got, err)
	}
	if err := b.Write(ctx, &filesystem.WriteRequest{FilePath: "too-large", Content: strings.Repeat("x", 33)}); err == nil {
		t.Fatal("expected per-file quota error")
	}
	if err := b.Write(ctx, &filesystem.WriteRequest{FilePath: "../escape", Content: "x"}); err == nil {
		t.Fatal("expected traversal error")
	}
	other := alistbackend.WithScope(context.Background(), alistbackend.Scope{TenantID: "tenant-b", SessionID: "session-a", RequestID: "request-a"})
	if _, err := b.Read(other, &filesystem.ReadRequest{FilePath: "notes/summary.md"}); err == nil {
		t.Fatal("expected scope isolation")
	}
}

func TestBackendTTLSurvivesBackendRestart(t *testing.T) {
	root := t.TempDir()
	ctx := scopedContext()
	b, err := New(Config{Root: root, TTL: 15 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Write(ctx, &filesystem.WriteRequest{FilePath: "expired.txt", Content: "temporary"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)

	// A fresh backend has an empty in-memory expiry map. Expiry must still be
	// enforced from file metadata rather than depending on process memory.
	fresh, err := New(Config{Root: root, TTL: 15 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fresh.Read(ctx, &filesystem.ReadRequest{FilePath: "expired.txt"})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read expired file error = %v, want os.ErrNotExist", err)
	}
}
