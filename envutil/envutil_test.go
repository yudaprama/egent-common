package envutil

import (
	"os"
	"testing"
)

func TestSplitCSV_Empty(t *testing.T) {
	if got := SplitCSV(""); got != nil {
		t.Errorf("SplitCSV(\"\") = %v, want nil", got)
	}
}

func TestSplitCSV_Single(t *testing.T) {
	got := SplitCSV("key1")
	want := []string{"key1"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("SplitCSV(\"key1\") = %v, want %v", got, want)
	}
}

func TestSplitCSV_Multiple(t *testing.T) {
	got := SplitCSV("a,b,c")
	want := []string{"a", "b", "c"}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, v := range got {
		if v != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, v, want[i])
		}
	}
}

func TestSplitCSV_TrimSpaces(t *testing.T) {
	got := SplitCSV(" a , b , c ")
	want := []string{"a", "b", "c"}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, v := range got {
		if v != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, v, want[i])
		}
	}
}

func TestSplitCSV_FilterEmpty(t *testing.T) {
	got := SplitCSV("a,,b,,,c,")
	want := []string{"a", "b", "c"}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, v := range got {
		if v != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, v, want[i])
		}
	}
}

func TestPickRandomKey_Empty(t *testing.T) {
	os.Unsetenv("TEST_EMPTY_KEYS")
	if got := PickRandomKey("TEST_EMPTY_KEYS"); got != "" {
		t.Errorf("PickRandomKey(unset) = %q, want \"\"", got)
	}
}

func TestPickRandomKey_Single(t *testing.T) {
	os.Setenv("TEST_SINGLE_KEY", "mykey")
	defer os.Unsetenv("TEST_SINGLE_KEY")
	if got := PickRandomKey("TEST_SINGLE_KEY"); got != "mykey" {
		t.Errorf("PickRandomKey(single) = %q, want \"mykey\"", got)
	}
}

func TestPickRandomKey_Multiple(t *testing.T) {
	keys := []string{"k1", "k2", "k3"}
	os.Setenv("TEST_MULTI_KEYS", "k1,k2,k3")
	defer os.Unsetenv("TEST_MULTI_KEYS")

	seen := make(map[string]int)
	n := 1000
	for i := 0; i < n; i++ {
		got := PickRandomKey("TEST_MULTI_KEYS")
		seen[got]++
	}

	for _, k := range keys {
		if seen[k] == 0 {
			t.Errorf("key %q was never selected in %d picks", k, n)
		}
	}
	if len(seen) != 3 {
		t.Errorf("selected %d unique keys, want 3", len(seen))
	}
}
