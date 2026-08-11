package egentcommon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	usage "github.com/yudaprama/plano-usage"
)

// TestConnectorExecuteAcceptsObjectAndStringArgs guards against the regression
// where a model emitting "arguments" as a real JSON object (instead of the
// documented JSON-object-as-a-string) crashed the turn with
// "cannot unmarshal object into Go struct Field .arguments of type string".
func TestConnectorExecuteAcceptsObjectAndStringArgs(t *testing.T) {
	var (
		mu      sync.Mutex
		gotArgs map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Tool string         `json:"tool"`
			Args map[string]any `json:"args"`
		}
		_ = json.Unmarshal(b, &req)
		mu.Lock()
		gotArgs = req.Args
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := &connectorClient{baseURL: srv.URL, http: &http.Client{Timeout: 5 * time.Second}}
	exec := &connectorExecuteTool{c: c}
	ctx := usage.WithActorID(context.Background(), "u_test")

	cases := []struct {
		name string
		json string
	}{
		{"object_form", `{"tool":"GITHUB_X","arguments":{"owner":"a","repo":"b"}}`},
		{"string_form", `{"tool":"GITHUB_X","arguments":"{\"owner\":\"a\",\"repo\":\"b\"}"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.InvokableRun(ctx, tc.json); err != nil {
				t.Fatalf("InvokableRun: unexpected error: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if gotArgs["owner"] != "a" || gotArgs["repo"] != "b" {
				t.Fatalf("sidecar args: got %v, want {owner:a repo:b}", gotArgs)
			}
		})
	}

	t.Run("no_arguments", func(t *testing.T) {
		if _, err := exec.InvokableRun(ctx, `{"tool":"GITHUB_X"}`); err != nil {
			t.Fatalf("InvokableRun: unexpected error: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(gotArgs) != 0 {
			t.Fatalf("sidecar args: got %v, want empty object", gotArgs)
		}
	})
}
