package egentcommon

import (
	"encoding/json"
	"testing"
)

func TestMergeGuidance(t *testing.T) {
	content := `{"tools":[{"name":"GITHUB_STAR_A_REPOSITORY","app":"github"}]}`
	guidance := json.RawMessage(`{"results":[{"useCase":"star a repo","difficulty":"easy"}],"nextStepsGuidance":["done"]}`)

	merged := MergeGuidance(content, guidance)

	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(merged), &out); err != nil {
		t.Fatalf("merged not valid JSON: %v (%s)", err, merged)
	}
	if _, ok := out["tools"]; !ok {
		t.Error("tools dropped from merged output")
	}
	if _, ok := out["guidance"]; !ok {
		t.Fatalf("guidance not merged: %s", merged)
	}
	var g struct {
		Results []struct{ UseCase, Difficulty string }
	}
	if err := json.Unmarshal(out["guidance"], &g); err != nil || len(g.Results) != 1 || g.Results[0].UseCase != "star a repo" {
		t.Errorf("guidance body wrong: %s", out["guidance"])
	}
}

func TestMergeGuidanceFailSafe(t *testing.T) {
	// nil guidance → unchanged
	if got := MergeGuidance(`{"tools":[]}`, nil); got != `{"tools":[]}` {
		t.Errorf("nil guidance should be a no-op, got %s", got)
	}
	// non-JSON content → unchanged (never corrupt)
	if got := MergeGuidance("plain string", json.RawMessage(`{"x":1}`)); got != "plain string" {
		t.Errorf("unparseable content should be returned unchanged, got %s", got)
	}
}

func TestGuidanceCollectorFIFO(t *testing.T) {
	c := NewGuidanceCollector()
	c.Push(json.RawMessage(`{"a":1}`))
	c.Push(json.RawMessage(`{"b":2}`))
	c.Push(nil) // ignored

	if got := string(c.Pop()); got != `{"a":1}` {
		t.Errorf("first pop = %s, want {\"a\":1}", got)
	}
	if got := string(c.Pop()); got != `{"b":2}` {
		t.Errorf("second pop = %s, want {\"b\":2}", got)
	}
	if c.Pop() != nil {
		t.Error("empty collector should pop nil")
	}
	if (*GuidanceCollector)(nil).Pop() != nil {
		t.Error("nil collector should pop nil")
	}
}
