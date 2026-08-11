package egentcommon

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// sampleToolsPayload mirrors the sidecar GET /tools response: tools carry a full
// JSON Schema under "inputSchema" (the bloat we strip for the LLM).
const sampleToolsPayload = `{"tools":[
 {"name":"GITHUB_CREATE_AN_ISSUE","description":"Create an issue.","app":"github",
  "inputSchema":{"type":"object","required":["owner","repo","title"],
   "properties":{
     "owner":{"type":"string","description":"Repo owner","examples":["octocat"]},
     "repo":{"type":"string","description":"Repo name"},
     "title":{"type":"string","description":"Issue title"},
     "body":{"type":"string","description":"Issue body"},
     "labels":{"type":"array","items":{"type":"string"}}}}},
 {"name":"GITHUB_GET_THE_AUTHENTICATED_USER","description":"Who am I.","app":"github",
  "inputSchema":{"type":"object","properties":{}}}
]}`

func TestSlimToolList_dropsSchemaKeepsParams(t *testing.T) {
	out := slimToolList([]byte(sampleToolsPayload))

	// The heavy schema detail must be gone.
	for _, banned := range []string{"inputSchema", "properties", "examples", "octocat"} {
		if strings.Contains(out, banned) {
			t.Fatalf("slimmed output still contains %q: %s", banned, out)
		}
	}
	// Must be materially smaller.
	if len(out) >= len(sampleToolsPayload) {
		t.Fatalf("expected smaller output, got %d >= %d", len(out), len(sampleToolsPayload))
	}

	var got struct {
		Tools []struct {
			Name   string `json:"name"`
			App    string `json:"app"`
			Params struct {
				Required []string `json:"required"`
				Optional []string `json:"optional"`
			} `json:"params"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("slimmed output not valid JSON: %v (%s)", err, out)
	}
	if len(got.Tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(got.Tools))
	}
	first := got.Tools[0]
	if first.Name != "GITHUB_CREATE_AN_ISSUE" || first.App != "github" {
		t.Fatalf("name/app not preserved: %+v", first)
	}
	if strings.Join(first.Params.Required, ",") != "owner,repo,title" {
		t.Fatalf("required params wrong: %v", first.Params.Required)
	}
	// body + labels are optional, sorted.
	if strings.Join(first.Params.Optional, ",") != "body,labels" {
		t.Fatalf("optional params wrong: %v", first.Params.Optional)
	}
}

func TestSlimToolList_failSafeOnGarbage(t *testing.T) {
	raw := []byte(`not json at all`)
	if got := slimToolList(raw); got != string(raw) {
		t.Fatalf("expected raw passthrough on parse error, got %q", got)
	}
}

func TestNoConnectorTools(t *testing.T) {
	if !noConnectorTools(`{"tools":[]}`) {
		t.Fatal("empty tools should report true")
	}
	if !noConnectorTools(`{"tools":null}`) {
		t.Fatal("null tools should report true")
	}
	if noConnectorTools(`{"tools":[{"name":"GITHUB_X"}]}`) {
		t.Fatal("non-empty tools should report false")
	}
	if noConnectorTools(`not json`) {
		t.Fatal("garbage must not be treated as empty (avoid overriding a real payload)")
	}
}

func TestCapConnectorResponse(t *testing.T) {
	small := []byte(`{"data":{"ok":true}}`)
	if got := capConnectorResponse(small); got != string(small) {
		t.Fatalf("small payload should pass through unchanged")
	}

	big := make([]byte, connectorExecuteMaxBytes+5000)
	for i := range big {
		big[i] = 'a'
	}
	got := capConnectorResponse(big)
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected truncation marker")
	}
	if len(got) > connectorExecuteMaxBytes+200 {
		t.Fatalf("truncated output not bounded: %d", len(got))
	}
}

// A GitHub-style "list repos" payload: an array of large objects nested under
// data.repositories. The old byte-cut sliced this mid-object into invalid JSON;
// the structural slim must keep it VALID, report the true count, and fit budget.
func TestCapConnectorResponse_slimsArrayKeepsCountAndValidJSON(t *testing.T) {
	const total = 137
	repos := make([]map[string]any, total)
	for i := range repos {
		repos[i] = map[string]any{
			"id":          1000000 + i,
			"name":        fmt.Sprintf("repo-%d", i),
			"full_name":   fmt.Sprintf("yudaprama/repo-%d", i),
			"private":     false,
			"description": strings.Repeat("x", 200), // bloat so the payload overflows
			"owner":       map[string]any{"login": "yudaprama", "id": 42, "type": "User"},
		}
	}
	raw, _ := json.Marshal(map[string]any{"data": map[string]any{"repositories": repos}})
	if len(raw) <= connectorExecuteMaxBytes {
		t.Fatalf("test payload should overflow the cap; got %d bytes", len(raw))
	}

	got := capConnectorResponse(raw)

	if len(got) > connectorExecuteMaxBytes {
		t.Fatalf("slimmed output not under budget: %d > %d", len(got), connectorExecuteMaxBytes)
	}
	var parsed any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("slimmed output must stay valid JSON: %v\n%s", err, got)
	}
	if !strings.Contains(got, fmt.Sprintf("of %d items", total)) {
		t.Fatalf("slimmed output must report the true item count (%d); got:\n%s", total, got)
	}
	if strings.Contains(got, "truncated: full response") {
		t.Fatalf("should have used structural slim, not the byte-cut fallback")
	}
}
