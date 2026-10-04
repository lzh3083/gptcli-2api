package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/hm2899/grokcli-2api/internal/protocol/toolcall"
)

// Non-stream Completion must still emit delete-match Edit when path+old arrive
// without new_string (true omit at end of turn). Stream Finish uses Coerce;
// non-stream must too, otherwise Claude Code drops the tool.
func TestCompletionForceFillsMissingNewString(t *testing.T) {
	// Direct path+old (as if collector forgot coerce) — currently Effective only.
	p := Completion("m", "g", "", "", "tool_calls",
		[]ToolCall{{ID: "t", Name: "Update", Arguments: `{"file_path":"/x","old_string":"a"}`}},
		Usage{}, []string{"Edit"})
	// After fix: must emit tool_use with new_string "".
	blocks, _ := p["content"].([]any)
	found := false
	for _, b := range blocks {
		m, _ := b.(map[string]any)
		if m["type"] == "tool_use" {
			found = true
			if m["name"] != "Edit" {
				t.Fatalf("name=%v", m["name"])
			}
			input, _ := m["input"].(map[string]any)
			if input["file_path"] != "/x" || input["old_string"] != "a" {
				t.Fatalf("input=%v", input)
			}
			if v, ok := input["new_string"]; !ok || v != "" {
				t.Fatalf("new_string=%#v", v)
			}
		}
	}
	if !found {
		// Diagnostic: what Effective does
		eff := toolcall.EffectiveJSON(`{"file_path":"/x","old_string":"a"}`, "Update")
		t.Fatalf("expected tool_use for path+old delete-match; stop=%v content=%v eff=%s complete=%v",
			p["stop_reason"], p["content"], eff, toolcall.CompleteJSON(eff, "Update"))
	}
}

func TestCompletionSearchReplaceMaps(t *testing.T) {
	p := Completion("m", "g", "", "", "tool_calls",
		[]ToolCall{{ID: "t", Name: "Update", Arguments: `{"path":"/x","search":"a","replace":"b"}`}},
		Usage{}, []string{"Edit"})
	blocks, _ := p["content"].([]any)
	raw, _ := json.Marshal(blocks)
	if !jsonContains(string(raw), "file_path") || !jsonContains(string(raw), "old_string") {
		t.Fatalf("expected mapped edit args: %s", raw)
	}
	if p["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason=%v", p["stop_reason"])
	}
}

func jsonContains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && (func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()))
}
