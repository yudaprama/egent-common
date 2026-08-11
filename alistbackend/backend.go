// Package alistbackend implements Eino's filesystem.Backend over the Alist
// HTTP API. The adapter is intentionally shared by all egents.
package alistbackend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/cloudwego/eino/adk/filesystem"
)

const defaultRoot = "/agent-context"

type Config struct {
	BaseURL string
	Token   string
	Client  *http.Client
	Root    string

	MaxFileBytes  int64
	MaxTotalBytes int64
	TTL           time.Duration
}

type Backend struct {
	baseURL  string
	token    string
	client   *http.Client
	root     string
	maxFile  int64
	maxTotal int64
	ttl      time.Duration

	mu    sync.Mutex
	files map[string]fileRecord
}

type fileRecord struct {
	size      int64
	expiresAt time.Time
}

type envelope[T any] struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

type alistFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	IsDir    bool   `json:"is_dir"`
	Modified string `json:"modified"`
}

type listResponse struct {
	Content []alistFile `json:"content"`
	Total   int64       `json:"total"`
}

type fileDetail struct {
	alistFile
	RawURL string `json:"raw_url"`
}

func New(cfg Config) (*Backend, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("alist base URL is required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("alist token is required")
	}
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid alist base URL %q", cfg.BaseURL)
	}
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	root := cfg.Root
	if root == "" {
		root = defaultRoot
	}
	root = path.Clean("/" + root)
	return &Backend{
		baseURL:  strings.TrimRight(cfg.BaseURL, "/"),
		token:    cfg.Token,
		client:   client,
		root:     root,
		maxFile:  cfg.MaxFileBytes,
		maxTotal: cfg.MaxTotalBytes,
		ttl:      cfg.TTL,
		files:    make(map[string]fileRecord),
	}, nil
}

func (b *Backend) endpoint(p string) string { return b.baseURL + p }

func (b *Backend) doJSON(ctx context.Context, method, endpoint string, in any, out any) error {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.endpoint(endpoint), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", b.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	data, err := b.doRequest(ctx, req, fmt.Sprintf("%s %s", method, endpoint))
	if err != nil {
		return err
	}
	if out != nil && len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, out); err != nil {
			return err
		}
	}
	return nil
}

// doRequest performs the request and validates both the HTTP status and the
// alist envelope code, returning the envelope data on success.
func (b *Backend) doRequest(ctx context.Context, req *http.Request, action string) (json.RawMessage, error) {
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("alist %s: HTTP %d", action, resp.StatusCode)
	}
	var env envelope[json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	if env.Code < 200 || env.Code >= 300 {
		return nil, fmt.Errorf("alist %s: %d %s", action, env.Code, env.Message)
	}
	return env.Data, nil
}

func (b *Backend) list(ctx context.Context, scoped string) ([]alistFile, error) {
	var data listResponse
	err := b.doJSON(ctx, http.MethodPost, "/api/fs/list", map[string]any{
		"path": scoped, "page": 1, "per_page": -1,
	}, &data)
	return data.Content, err
}

func (b *Backend) LsInfo(ctx context.Context, req *filesystem.LsInfoRequest) ([]filesystem.FileInfo, error) {
	scoped, err := scopedPath(ctx, b.root, req.Path)
	if err != nil {
		return nil, err
	}
	files, err := b.list(ctx, scoped)
	if err != nil {
		return nil, err
	}
	out := make([]filesystem.FileInfo, 0, len(files))
	for _, f := range files {
		out = append(out, filesystem.FileInfo{Path: f.Name, IsDir: f.IsDir, Size: f.Size, ModifiedAt: f.Modified})
	}
	return out, nil
}

func (b *Backend) Read(ctx context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	scoped, err := scopedPath(ctx, b.root, req.FilePath)
	if err != nil {
		return nil, err
	}
	if err := b.checkRecord(scoped); err != nil {
		return nil, err
	}
	var detail fileDetail
	if err := b.doJSON(ctx, http.MethodPost, "/api/fs/get", map[string]string{"path": scoped}, &detail); err != nil {
		return nil, err
	}
	if detail.IsDir || detail.RawURL == "" {
		return nil, fmt.Errorf("alist path is not a readable file: %s", req.FilePath)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, detail.RawURL, nil)
	if err != nil {
		return nil, err
	}
	if target, err := url.Parse(detail.RawURL); err == nil {
		if base, baseErr := url.Parse(b.baseURL); baseErr == nil && target.Scheme == base.Scheme && target.Host == base.Host {
			r.Header.Set("Authorization", b.token)
		}
	}
	resp, err := b.client.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("alist download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &filesystem.FileContent{Content: SelectLines(string(data), req.Offset, req.Limit)}, nil
}

func SelectLines(content string, offset, limit int) string {
	lines := strings.Split(content, "\n")
	start := offset - 1
	if start < 0 {
		start = 0
	}
	if start >= len(lines) {
		return ""
	}
	end := len(lines)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	return strings.Join(lines[start:end], "\n")
}

func (b *Backend) Write(ctx context.Context, req *filesystem.WriteRequest) error {
	scoped, err := scopedPath(ctx, b.root, req.FilePath)
	if err != nil {
		return err
	}
	size := int64(len(req.Content))
	if b.maxFile > 0 && size > b.maxFile {
		return fmt.Errorf("alist file exceeds limit: %d > %d bytes", size, b.maxFile)
	}
	if err := b.reserve(scoped, size); err != nil {
		return err
	}
	if err := b.ensureParents(ctx, path.Dir(scoped)); err != nil {
		b.release(scoped)
		return err
	}
	if err := b.upload(ctx, scoped, []byte(req.Content)); err != nil {
		b.release(scoped)
		return err
	}
	return nil
}

func (b *Backend) upload(ctx context.Context, scoped string, content []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", path.Base(scoped))
	if err != nil {
		return err
	}
	if _, err := part.Write(content); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, b.endpoint("/api/fs/form"), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", b.token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("File-Path", url.PathEscape(scoped))
	_, err = b.doRequest(ctx, req, "upload")
	return err
}

func (b *Backend) ensureParents(ctx context.Context, dir string) error {
	ns, err := namespace(ctx, b.root)
	if err != nil {
		return err
	}
	rel := strings.TrimPrefix(path.Clean(dir), ns)
	current := ns
	for _, part := range strings.Split(strings.Trim(rel, "/"), "/") {
		if part == "" {
			continue
		}
		current = path.Join(current, part)
		if _, err := b.list(ctx, current); err == nil {
			continue
		}
		if err := b.doJSON(ctx, http.MethodPost, "/api/fs/mkdir", map[string]string{"path": current}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) Edit(ctx context.Context, req *filesystem.EditRequest) error {
	content, err := b.Read(ctx, &filesystem.ReadRequest{FilePath: req.FilePath})
	if err != nil {
		return err
	}
	if req.OldString == "" {
		return errors.New("old string must not be empty")
	}
	if !strings.Contains(content.Content, req.OldString) {
		return errors.New("old string not found")
	}
	count := strings.Count(content.Content, req.OldString)
	if !req.ReplaceAll && count != 1 {
		return fmt.Errorf("old string found %d times", count)
	}
	replaced := strings.Replace(content.Content, req.OldString, req.NewString, 1)
	if req.ReplaceAll {
		replaced = strings.ReplaceAll(content.Content, req.OldString, req.NewString)
	}
	return b.Write(ctx, &filesystem.WriteRequest{FilePath: req.FilePath, Content: replaced})
}

func (b *Backend) GlobInfo(ctx context.Context, req *filesystem.GlobInfoRequest) ([]filesystem.FileInfo, error) {
	base, err := scopedPath(ctx, b.root, req.Path)
	if err != nil {
		return nil, err
	}
	files, err := b.walk(ctx, base)
	if err != nil {
		return nil, err
	}
	pattern := req.Pattern
	if pattern == "" {
		return nil, errors.New("glob pattern is required")
	}
	out := make([]filesystem.FileInfo, 0)
	for _, f := range files {
		rel := strings.TrimPrefix(f.Path, base+"/")
		matched, err := doublestar.Match(pattern, rel)
		if err != nil {
			return nil, err
		}
		if matched {
			requestedBase := path.Clean("/" + req.Path)
			f.Path = path.Join(requestedBase, rel)
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (b *Backend) walk(ctx context.Context, dir string) ([]filesystem.FileInfo, error) {
	entries, err := b.list(ctx, dir)
	if err != nil {
		return nil, err
	}
	var out []filesystem.FileInfo
	for _, entry := range entries {
		p := path.Join(dir, entry.Name)
		info := filesystem.FileInfo{Path: p, IsDir: entry.IsDir, Size: entry.Size, ModifiedAt: entry.Modified}
		out = append(out, info)
		if entry.IsDir {
			children, err := b.walk(ctx, p)
			if err != nil {
				return nil, err
			}
			out = append(out, children...)
		}
	}
	return out, nil
}

func (b *Backend) GrepRaw(ctx context.Context, req *filesystem.GrepRequest) ([]filesystem.GrepMatch, error) {
	if req.Pattern == "" {
		return nil, errors.New("grep pattern is required")
	}
	pattern := req.Pattern
	if req.CaseInsensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	globPattern := "**/*"
	if req.Glob != "" {
		globPattern = req.Glob
	}
	files, err := b.GlobInfo(ctx, &filesystem.GlobInfoRequest{Pattern: globPattern, Path: req.Path})
	if err != nil {
		return nil, err
	}
	var matches []filesystem.GrepMatch
	for _, f := range files {
		if f.IsDir {
			continue
		}
		if req.FileType != "" && strings.TrimPrefix(path.Ext(f.Path), ".") != req.FileType {
			continue
		}
		content, err := b.Read(ctx, &filesystem.ReadRequest{FilePath: f.Path})
		if err != nil {
			return nil, err
		}
		for n, line := range strings.Split(content.Content, "\n") {
			if re.MatchString(line) {
				matches = append(matches, filesystem.GrepMatch{Path: f.Path, Line: n + 1, Content: line})
			}
		}
	}
	return matches, nil
}

func (b *Backend) reserve(path string, size int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	old := b.files[path].size
	var total int64
	for _, f := range b.files {
		total += f.size
	}
	if b.maxTotal > 0 && total-old+size > b.maxTotal {
		return fmt.Errorf("alist context quota exceeded: %d > %d bytes", total-old+size, b.maxTotal)
	}
	expires := time.Time{}
	if b.ttl > 0 {
		expires = time.Now().Add(b.ttl)
	}
	b.files[path] = fileRecord{size: size, expiresAt: expires}
	return nil
}

func (b *Backend) release(path string) { b.mu.Lock(); delete(b.files, path); b.mu.Unlock() }

func (b *Backend) checkRecord(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if f, ok := b.files[path]; ok && !f.expiresAt.IsZero() && time.Now().After(f.expiresAt) {
		return fmt.Errorf("alist context file expired")
	}
	return nil
}

func (b *Backend) expireLocked() {
	now := time.Now()
	for p, f := range b.files {
		if !f.expiresAt.IsZero() && now.After(f.expiresAt) {
			delete(b.files, p)
		}
	}
}

// Remove deletes a file or directory from Alist. It is intentionally an
// extension beyond Eino's Backend interface for TTL cleanup workers.
func (b *Backend) Remove(ctx context.Context, requested string) error {
	scoped, err := scopedPath(ctx, b.root, requested)
	if err != nil {
		return err
	}
	return b.removeScoped(ctx, scoped)
}

func (b *Backend) removeScoped(ctx context.Context, scoped string) error {
	dir, name := path.Dir(scoped), path.Base(scoped)
	if err := b.doJSON(ctx, http.MethodPost, "/api/fs/remove", map[string]any{"dir": dir, "names": []string{name}}, nil); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.files, scoped)
	b.mu.Unlock()
	return nil
}

// CleanupExpired removes files whose TTL has elapsed for the current scope.
// A worker should call this periodically; Alist itself does not expire normal
// files, only signed links/shares.
func (b *Backend) CleanupExpired(ctx context.Context) error {
	ns, err := namespace(ctx, b.root)
	if err != nil {
		return err
	}
	return b.removeExpired(ctx, b.expiredPaths(ns+"/"))
}

// CleanupAllExpired removes expired files across ALL namespaces tracked by
// this backend, without requiring a request scope in ctx. It is the right
// entry point for the periodic cleanup worker, which runs on the app-lifetime
// context and has no tenant/request identity. Files orphaned by a process
// restart are not in the in-memory index and are not reaped here — persist the
// index or scan Alist if restart durability is required.
func (b *Backend) CleanupAllExpired(ctx context.Context) error {
	return b.removeExpired(ctx, b.expiredPaths(""))
}

// expiredPaths returns the paths in the in-memory index whose TTL has elapsed,
// optionally restricted to those under prefix ("" = no restriction).
func (b *Backend) expiredPaths(prefix string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	var expired []string
	for p, f := range b.files {
		if !f.expiresAt.IsZero() && now.After(f.expiresAt) && (prefix == "" || strings.HasPrefix(p, prefix)) {
			expired = append(expired, p)
		}
	}
	return expired
}

func (b *Backend) removeExpired(ctx context.Context, expired []string) error {
	for _, p := range expired {
		if err := b.removeScoped(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

var _ filesystem.Backend = (*Backend)(nil)
