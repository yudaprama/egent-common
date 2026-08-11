package alistbackend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk/filesystem"
)

func TestBackendScopesRequestsAndReadsWrites(t *testing.T) {
	var mu sync.Mutex
	files := map[string]string{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/fs/mkdir":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": nil})
		case "/api/fs/form":
			path, err := url.PathUnescape(r.Header.Get("File-Path"))
			if err != nil {
				t.Errorf("decode path: %v", err)
				return
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse upload: %v", err)
				return
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Errorf("form file: %v", err)
				return
			}
			defer file.Close()
			body, _ := io.ReadAll(file)
			mu.Lock()
			files[path] = string(body)
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": nil})
		case "/api/fs/get":
			var req struct {
				Path string `json:"path"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			content, ok := files[req.Path]
			mu.Unlock()
			if !ok {
				json.NewEncoder(w).Encode(map[string]any{"code": 404, "message": "not found"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{
				"name": "notes.txt", "size": len(content), "is_dir": false,
				"raw_url": server.URL + "/raw?path=" + url.QueryEscape(req.Path),
			}})
		case "/raw":
			mu.Lock()
			content := files[r.URL.Query().Get("path")]
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, content)
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{"content": []any{}}})
		}
	}))
	defer server.Close()

	b, err := New(Config{BaseURL: server.URL, Token: "test-token", MaxFileBytes: 100, MaxTotalBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithScope(context.Background(), Scope{TenantID: "tenant-a", SessionID: "session-a", RequestID: "request-a"})
	if err := b.Write(ctx, &filesystem.WriteRequest{FilePath: "notes.txt", Content: "hello\nworld"}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	var storedPath string
	for p := range files {
		storedPath = p
	}
	mu.Unlock()
	if !strings.Contains(storedPath, "/agent-context/") {
		t.Fatalf("stored path not under root: %q", storedPath)
	}
	if !strings.Contains(storedPath, "tenant-a") || !strings.Contains(storedPath, "request-a") {
		t.Fatalf("stored path should carry readable tenant+request ids: %q", storedPath)
	}
	if strings.Contains(storedPath, "session-a") {
		t.Fatalf("stored path should not leak the session id: %q", storedPath)
	}

	got, err := b.Read(ctx, &filesystem.ReadRequest{FilePath: "notes.txt", Offset: 2, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "world" {
		t.Fatalf("read content = %q, want world", got.Content)
	}
	if err := b.Edit(ctx, &filesystem.EditRequest{FilePath: "notes.txt", OldString: "l", NewString: "L"}); err == nil {
		t.Fatal("expected ambiguous edit to fail")
	}

	if err := b.Write(ctx, &filesystem.WriteRequest{FilePath: "too-large.txt", Content: strings.Repeat("x", 101)}); err == nil {
		t.Fatal("expected per-file quota error")
	}
}

func TestBackendRequiresScopeAndRejectsTraversal(t *testing.T) {
	b, err := New(Config{BaseURL: "http://127.0.0.1:1", Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Write(context.Background(), &filesystem.WriteRequest{FilePath: "x", Content: "x"}); err == nil {
		t.Fatal("expected missing scope error")
	}
	ctx := WithScope(context.Background(), Scope{TenantID: "t", SessionID: "s", RequestID: "r"})
	if _, err := scopedPath(ctx, defaultRoot, "../../escape"); err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestCleanupAllExpired_WorksWithoutScope(t *testing.T) {
	var mu sync.Mutex
	files := map[string][]byte{}
	removed := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/fs/mkdir":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": nil})
		case "/api/fs/form":
			path, _ := url.PathUnescape(r.Header.Get("File-Path"))
			r.ParseMultipartForm(1 << 20)
			f, _, _ := r.FormFile("file")
			defer f.Close()
			body, _ := io.ReadAll(f)
			mu.Lock()
			files[path] = body
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": nil})
		case "/api/fs/remove":
			var req struct {
				Dir   string   `json:"dir"`
				Names []string `json:"names"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			for _, n := range req.Names {
				full := path.Join(req.Dir, n)
				removed[full] = true
				delete(files, full)
			}
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": nil})
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{"content": []any{}}})
		}
	}))
	defer server.Close()

	b, err := New(Config{BaseURL: server.URL, Token: "test-token", MaxFileBytes: 4096, MaxTotalBytes: 1 << 20, TTL: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	// Write one file under scope A and another under scope B.
	ctxA := WithScope(context.Background(), Scope{TenantID: "tenant-a", SessionID: "s", RequestID: "r"})
	ctxB := WithScope(context.Background(), Scope{TenantID: "tenant-b", SessionID: "s", RequestID: "r"})
	if err := b.Write(ctxA, &filesystem.WriteRequest{FilePath: "a.txt", Content: "aaa"}); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if err := b.Write(ctxB, &filesystem.WriteRequest{FilePath: "b.txt", Content: "bbb"}); err != nil {
		t.Fatalf("write B: %v", err)
	}

	// The worker calls CleanupAllExpired on the app-lifetime ctx, which has NO
	// scope. The old CleanupExpired(ctx) would error here (scopeFromContext
	// fails); CleanupAllExpired must NOT require a scope.
	if err := b.CleanupAllExpired(context.Background()); err != nil {
		t.Fatalf("CleanupAllExpired before TTL should be no-op, got: %v", err)
	}
	mu.Lock()
	removedSoFar := len(removed)
	mu.Unlock()
	if removedSoFar != 0 {
		t.Fatalf("nothing should be removed before TTL, got %d", removedSoFar)
	}

	time.Sleep(80 * time.Millisecond)

	if err := b.CleanupAllExpired(context.Background()); err != nil {
		t.Fatalf("CleanupAllExpired after TTL: %v", err)
	}

	mu.Lock()
	gotRemoved := len(removed)
	gotFiles := len(files)
	mu.Unlock()
	if gotRemoved != 2 {
		t.Fatalf("expected 2 files removed across both namespaces, got %d (removed=%v)", gotRemoved, removed)
	}
	if gotFiles != 0 {
		t.Fatalf("expected 0 files remaining, got %d", gotFiles)
	}
}
