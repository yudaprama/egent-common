package alistbackend

import "testing"

func TestSafeSegment(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"ws_abc", "ws_abc"},
		{"chatcmpl-123", "chatcmpl-123"},
		{"tenant.id", "tenant.id"},
		{"with space", "with_space"},
		{"café", "caf_"},
		{"..", ""},   // sentinel: must not be ".." — handled below
		{".", ""},    // sentinel: must not be "." or empty
		{"", ""},     // sentinel: must fall back
		{"../etc", ""}, // traversal attempt — must not contain ".."
	}
	for _, c := range cases {
		got := SafeSegment(c.in)
		switch c.in {
		case "..", ".", "", "../etc":
			// Pathological inputs: must be non-empty, not ".", not "..", and
			// must not let traversal leak (no ".." substring).
			if got == "" || got == "." || got == ".." {
				t.Fatalf("SafeSegment(%q) = %q — must not be empty/dot/dotdot", c.in, got)
			}
			if containsDotDot(got) {
				t.Fatalf("SafeSegment(%q) = %q — traversal leaked", c.in, got)
			}
		default:
			if got != c.want {
				t.Fatalf("SafeSegment(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	}
}

func containsDotDot(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '.' && s[i+1] == '.' {
			return true
		}
	}
	return false
}
