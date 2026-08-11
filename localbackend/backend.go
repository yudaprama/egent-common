// Package localbackend implements a bounded, request-scoped Eino filesystem
// backend. It is intended for the colocated Alist Local driver deployment.
package localbackend

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/yudaprama/egent-common/alistbackend"
)

type Config struct {
	Root          string
	MaxFileBytes  int64
	MaxTotalBytes int64
	TTL           time.Duration
}

type Backend struct {
	root     string
	maxFile  int64
	maxTotal int64
	ttl      time.Duration
	mu       sync.Mutex
	expires  map[string]time.Time
}

func New(cfg Config) (*Backend, error) {
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, errors.New("local backend root is required")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve local backend root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create local backend root: %w", err)
	}
	return &Backend{root: root, maxFile: cfg.MaxFileBytes, maxTotal: cfg.MaxTotalBytes, ttl: cfg.TTL, expires: make(map[string]time.Time)}, nil
}

func (b *Backend) scoped(ctx context.Context, requested string) (string, error) {
	scope, err := alistbackend.ScopeFromContext(ctx)
	if err != nil {
		return "", err
	}
	parts := []string{alistbackend.SafeSegment(scope.TenantID), alistbackend.SafeSegment(scope.RequestID)}
	raw := strings.TrimSpace(requested)
	for _, segment := range strings.Split(strings.TrimPrefix(filepath.ToSlash(raw), "/"), "/") {
		if segment == ".." {
			return "", fmt.Errorf("invalid local backend path %q", requested)
		}
	}
	clean := filepath.Clean("/" + filepath.FromSlash(raw))
	if strings.HasPrefix(clean, "/..") {
		return "", fmt.Errorf("invalid local backend path %q", requested)
	}
	segments := append([]string{b.root}, parts...)
	segments = append(segments, strings.TrimPrefix(clean, string(filepath.Separator)))
	return filepath.Join(segments...), nil
}

func (b *Backend) LsInfo(ctx context.Context, req *filesystem.LsInfoRequest) ([]filesystem.FileInfo, error) {
	p, err := b.scoped(ctx, req.Path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]filesystem.FileInfo, 0, len(entries))
	for _, e := range entries {
		entryPath := filepath.Join(p, e.Name())
		if !e.IsDir() && b.expireIfNeeded(entryPath) {
			continue
		}
		info, _ := e.Info()
		fi := filesystem.FileInfo{Path: e.Name(), IsDir: e.IsDir()}
		if info != nil {
			fi.Size = info.Size()
			fi.ModifiedAt = info.ModTime().UTC().Format(time.RFC3339)
		}
		out = append(out, fi)
	}
	return out, nil
}

func (b *Backend) Read(ctx context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	p, err := b.scoped(ctx, req.FilePath)
	if err != nil {
		return nil, err
	}
	if b.expireIfNeeded(p) {
		return nil, os.ErrNotExist
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return &filesystem.FileContent{Content: alistbackend.SelectLines(string(data), req.Offset, req.Limit)}, nil
}

func (b *Backend) Write(ctx context.Context, req *filesystem.WriteRequest) error {
	p, err := b.scoped(ctx, req.FilePath)
	if err != nil {
		return err
	}
	if b.maxFile > 0 && int64(len(req.Content)) > b.maxFile {
		return fmt.Errorf("file exceeds limit: %d > %d bytes", len(req.Content), b.maxFile)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if namespace, namespaceErr := b.scoped(ctx, ""); namespaceErr == nil {
		b.pruneExpired(namespace)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	old := int64(0)
	if info, statErr := os.Stat(p); statErr == nil {
		old = info.Size()
	}
	namespace, namespaceErr := b.scoped(ctx, "")
	if namespaceErr != nil {
		return namespaceErr
	}
	if b.maxTotal > 0 && b.totalBytesLocked(namespace)-old+int64(len(req.Content)) > b.maxTotal {
		return fmt.Errorf("context namespace exceeds total limit")
	}
	if err := os.WriteFile(p, []byte(req.Content), 0o600); err != nil {
		return err
	}
	if b.ttl > 0 {
		b.expires[p] = time.Now().Add(b.ttl)
	}
	return nil
}

func (b *Backend) Edit(ctx context.Context, req *filesystem.EditRequest) error {
	if req.OldString == "" {
		return errors.New("old string must not be empty")
	}
	if req.OldString == req.NewString {
		return errors.New("old and new strings must differ")
	}
	p, err := b.scoped(ctx, req.FilePath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	content := string(data)
	count := strings.Count(content, req.OldString)
	if count == 0 {
		return errors.New("old string not found")
	}
	if !req.ReplaceAll && count != 1 {
		return fmt.Errorf("old string found %d times", count)
	}
	if req.ReplaceAll {
		content = strings.ReplaceAll(content, req.OldString, req.NewString)
	} else {
		content = strings.Replace(content, req.OldString, req.NewString, 1)
	}
	return b.Write(ctx, &filesystem.WriteRequest{FilePath: req.FilePath, Content: content})
}

func (b *Backend) GrepRaw(ctx context.Context, req *filesystem.GrepRequest) ([]filesystem.GrepMatch, error) {
	rx, err := regexp.Compile(req.Pattern)
	if req.CaseInsensitive {
		rx, err = regexp.Compile("(?i)" + req.Pattern)
	}
	if err != nil {
		return nil, err
	}
	root, err := b.scoped(ctx, req.Path)
	if err != nil {
		return nil, err
	}
	var out []filesystem.GrepMatch
	err = filepath.WalkDir(root, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if b.expireIfNeeded(p) {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if req.Glob != "" {
			ok, e := doublestar.Match(filepath.ToSlash(req.Glob), filepath.ToSlash(rel))
			if e != nil {
				return e
			}
			if !ok {
				return nil
			}
		}
		if req.FileType != "" && strings.TrimPrefix(filepath.Ext(p), ".") != req.FileType {
			return nil
		}
		data, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		for i, line := range strings.Split(string(data), "\n") {
			if rx.MatchString(line) {
				out = append(out, filesystem.GrepMatch{Path: filepath.ToSlash(rel), Line: i + 1, Content: line})
			}
		}
		return nil
	})
	return out, err
}

func (b *Backend) GlobInfo(ctx context.Context, req *filesystem.GlobInfoRequest) ([]filesystem.FileInfo, error) {
	root, err := b.scoped(ctx, req.Path)
	if err != nil {
		return nil, err
	}
	var out []filesystem.FileInfo
	err = filepath.WalkDir(root, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && b.expireIfNeeded(p) {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		matched, matchErr := doublestar.Match(filepath.ToSlash(req.Pattern), filepath.ToSlash(rel))
		if matchErr != nil {
			return matchErr
		}
		if matched {
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			out = append(out, filesystem.FileInfo{Path: filepath.ToSlash(rel), IsDir: info.IsDir(), Size: info.Size(), ModifiedAt: info.ModTime().UTC().Format(time.RFC3339)})
		}
		return nil
	})
	return out, err
}

func (b *Backend) totalBytesLocked(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err == nil && e != nil && !e.IsDir() {
			if info, statErr := e.Info(); statErr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// expireIfNeeded enforces TTL from the file's mtime as well as the in-memory
// expiry map. Using mtime makes expiry survive process restarts, where the map
// is necessarily empty.
func (b *Backend) expireIfNeeded(path string) bool {
	if b.ttl <= 0 {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || time.Since(info.ModTime()) < b.ttl {
		return false
	}
	_ = os.Remove(path)
	b.mu.Lock()
	delete(b.expires, path)
	b.mu.Unlock()
	return true
}

func (b *Backend) pruneExpired(root string) {
	if b.ttl <= 0 {
		return
	}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry != nil && !entry.IsDir() {
			b.expireIfNeeded(path)
		}
		return nil
	})
}

// CleanupExpired removes expired context files across all namespaces. A
// periodic cleanup worker can call it for proactive cleanup; normal backend
// operations also enforce TTL lazily.
func (b *Backend) CleanupExpired() error {
	b.pruneExpired(b.root)
	b.mu.Lock()
	defer b.mu.Unlock()
	for p := range b.expires {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			delete(b.expires, p)
		}
	}
	return nil
}
