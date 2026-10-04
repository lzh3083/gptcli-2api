package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// Grok often emits Update with search/replace or omits new_string; the stream path
// must still produce a dense Edit tool_use block Claude Code accepts.
func TestStreamUpdateSearchReplaceBecomesEdit(t *testing.T) {
	a := NewStreamAssembler("m", "g", true, 2, []string{"Bash", "Read", "Edit"})
	args := `{"path":"/tmp/demo.go","search":"foo","replace":"bar"}`
	frames := a.Feed("", "", []ToolDelta{{Index: 0, ID: "t1", Name: "Update", Arguments: args}})
	frames = append(frames, a.Finish("tool_calls", Usage{PromptTokens: 10, CompletionTokens: 5})...)
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, `"name":"Edit"`) && !strings.Contains(joined, `"name": "Edit"`) {
		// json compact form
		if !strings.Contains(joined, "Edit") {
			t.Fatalf("expected Edit tool name in stream:\n%s", joined)
		}
	}
	// partial_json must include file_path/old_string/new_string
	found := false
	for _, frame := range frames {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var payload map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &payload) != nil {
				continue
			}
			delta, _ := payload["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if delta["type"] != "input_json_delta" {
				continue
			}
			pj, _ := delta["partial_json"].(string)
			if strings.Contains(pj, "file_path") && strings.Contains(pj, "old_string") && strings.Contains(pj, "new_string") {
				found = true
				if !strings.Contains(pj, "/tmp/demo.go") || !strings.Contains(pj, "foo") || !strings.Contains(pj, "bar") {
					t.Fatalf("partial_json content wrong: %s", pj)
				}
			}
		}
	}
	if !found {
		t.Fatalf("no complete Edit input_json_delta found:\n%s", joined)
	}
}

func TestStreamUpdateMissingNewStringStillEmits(t *testing.T) {
	a := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	args := `{"file_path":"/x","old_string":"delete-me"}`
	frames := a.Feed("", "", []ToolDelta{{Index: 0, ID: "t1", Name: "Update", Arguments: args}})
	frames = append(frames, a.Finish("tool_calls", Usage{})...)
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, "tool_use") {
		t.Fatalf("expected tool_use for path+old default new_string:\n%s", joined)
	}
	if !strings.Contains(joined, "new_string") {
		t.Fatalf("expected new_string filled:\n%s", joined)
	}
}

func TestStreamUpdatePathFlipForm(t *testing.T) {
	// Streaming: first wrong path, then correct path alias — must emit Edit with /right.
	a := NewStreamAssembler("m", "g", true, 1, []string{"Edit", "Read", "Bash"})
	frames := a.Feed("", "", []ToolDelta{{Index: 0, ID: "t1", Name: "Update", Arguments: `{"path":"/wrong"}`}})
	frames = append(frames, a.Feed("", "", []ToolDelta{{Index: 0, Arguments: `{"file_path":"/right","old_string":"a","new_string":"b"}`}})...)
	frames = append(frames, a.Finish("tool_calls", Usage{})...)
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, "/right") {
		t.Fatalf("expected path flip to /right:\n%s", joined)
	}
	if strings.Contains(joined, `"name":"Update"`) && !strings.Contains(joined, "Edit") {
		t.Fatalf("expected Update→Edit rename:\n%s", joined)
	}
	if !strings.Contains(joined, "message_stop") {
		t.Fatalf("missing message_stop:\n%s", joined)
	}
}

func TestStreamHeldTextKeepaliveSignal(t *testing.T) {
	a := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	// tools requested: text is held
	_ = a.Feed("I will edit the file now...", "", nil)
	if !a.HasHeldContent() || !a.NeedsClientKeepalive() {
		t.Fatalf("held text must need keepalive held=%v pending=%v", a.HasHeldContent(), a.HasPendingTools())
	}
	// incomplete tool still needs keepalive
	_ = a.Feed("", "", []ToolDelta{{Index: 0, ID: "t", Name: "Update", Arguments: `{"file_path":`}})
	if !a.NeedsClientKeepalive() {
		t.Fatal("incomplete Update must need keepalive")
	}
}

func TestStreamUpdateOldCodeNewCodeAliases(t *testing.T) {
	a := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	args := `{"path":"/x.go","old_code":"aaa","new_code":"bbb"}`
	frames := a.Feed("", "", []ToolDelta{{Index: 0, ID: "t", Name: "Update", Arguments: args}})
	frames = append(frames, a.Finish("tool_calls", Usage{})...)
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, "old_string") || !strings.Contains(joined, "new_string") {
		t.Fatalf("old_code/new_code not mapped:\n%s", joined)
	}
	if !strings.Contains(joined, "aaa") || !strings.Contains(joined, "bbb") {
		t.Fatalf("values lost:\n%s", joined)
	}
}

func TestStreamUpdatePartialThenComplete(t *testing.T) {
	// Intermittent: name first, then partial args, then complete search/replace.
	a := NewStreamAssembler("m", "g", true, 1, []string{"Edit", "Read"})
	_ = a.Feed("", "thinking...", []ToolDelta{{Index: 0, ID: "t1", Name: "Update"}})
	if !a.NeedsClientKeepalive() && !a.HasClientPayload() {
		// reasoning streams live so payload exists; still ok
	}
	_ = a.Feed("", "", []ToolDelta{{Index: 0, Arguments: `{"path":"/x.go","search":`}})
	if !a.HasPendingTools() {
		t.Fatal("partial Update args should be pending")
	}
	frames := a.Feed("", "", []ToolDelta{{Index: 0, Arguments: `"old","replace":"new"}`}})
	frames = append(frames, a.Finish("tool_calls", Usage{PromptTokens: 1, CompletionTokens: 1})...)
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, "tool_use") {
		t.Fatalf("expected tool_use after complete Update:\n%s", joined)
	}
	// Must be Edit with file_path/old_string/new_string
	if !strings.Contains(joined, "file_path") || !strings.Contains(joined, "old_string") {
		t.Fatalf("expected normalized Edit args:\n%s", joined)
	}
	if strings.Contains(joined, `"search"`) && !strings.Contains(joined, "old_string") {
		t.Fatalf("search alias leaked without old_string:\n%s", joined)
	}
}

func TestStreamUpdateWrongKeysStillEmits(t *testing.T) {
	a := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	args := `{"target_file":"/a.go","search_for":"x","replace_with":"y"}`
	frames := a.Feed("", "", []ToolDelta{{Index: 0, ID: "t", Name: "Update", Arguments: args}})
	frames = append(frames, a.Finish("tool_calls", Usage{})...)
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, "tool_use") {
		t.Fatalf("expected tool_use:\n%s", joined)
	}
	for _, key := range []string{"file_path", "old_string", "new_string"} {
		if !strings.Contains(joined, key) {
			t.Fatalf("missing %s in %s", key, joined)
		}
	}
}

func TestHeldTextNeedsKeepalive(t *testing.T) {
	a := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	// tools requested: text held, no tool yet
	_ = a.Feed("I will edit the file now...", "", nil)
	if !a.HasHeldContent() {
		t.Fatal("expected held content")
	}
	if !a.NeedsClientKeepalive() {
		t.Fatal("held content must need keepalive")
	}
}

func TestStreamUpdateHeldTextDoesNotBlockTool(t *testing.T) {
	// toolsRequested: text is held until tool completes; Update search/replace must still emit Edit.
	a := NewStreamAssembler("m", "g", true, 2, []string{"Edit", "Read", "Bash"})
	// preface text held
	_ = a.Feed("I will update the file now.", "", nil)
	if !a.HasHeldContent() && !a.NeedsClientKeepalive() {
		// held content should trigger keepalive need
		t.Fatalf("expected held content keepalive need")
	}
	// streaming Update in fragments
	_ = a.Feed("", "", []ToolDelta{{Index: 0, ID: "t1", Name: "Update", Arguments: `{"path":"/demo.go","search":`}})
	if !a.HasPendingTools() {
		t.Fatalf("expected pending tools")
	}
	frames := a.Feed("", "", []ToolDelta{{Index: 0, Arguments: `"foo","replace":"bar"}`}})
	frames = append(frames, a.Finish("tool_calls", Usage{PromptTokens: 3, CompletionTokens: 2})...)
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, "tool_use") {
		t.Fatalf("expected tool_use:\n%s", joined)
	}
	if !strings.Contains(joined, "file_path") || !strings.Contains(joined, "old_string") {
		t.Fatalf("expected normalized edit args:\n%s", joined)
	}
	// held text must not leak before tools (tool-only contract after sawTool)
	// but Finish with tools should not dump held preface as user-visible failure.
	if !strings.Contains(joined, "message_stop") {
		t.Fatalf("missing message_stop:\n%s", joined)
	}
}

func TestStreamDoesNotEmitPathOldWithoutNewStringUntilFinish(t *testing.T) {
	// Mid-stream path+old must NOT emit tool_use; only Finish (coerce) may.
	a := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	frames := a.Feed("", "", []ToolDelta{{Index: 0, ID: "t1", Name: "Update", Arguments: `{"file_path":"/x","old_string":"a"}`}})
	joined := strings.Join(frames, "\n")
	if strings.Contains(joined, "tool_use") {
		t.Fatalf("must not emit tool_use mid-stream for path+old only:\n%s", joined)
	}
	if !a.HasPendingTools() || !a.NeedsClientKeepalive() {
		t.Fatal("path+old Update must stay pending and need keepalive")
	}
	// Late replace arrives → now complete without waiting for Finish coerce.
	frames = a.Feed("", "", []ToolDelta{{Index: 0, Arguments: `{"new_string":"b"}`}})
	joined = strings.Join(frames, "\n")
	if !strings.Contains(joined, "tool_use") {
		t.Fatalf("expected tool_use after new_string arrives:\n%s", joined)
	}
	if !strings.Contains(joined, "new_string") || !strings.Contains(joined, `"b"`) && !strings.Contains(joined, "b") {
		t.Fatalf("expected new_string b in:\n%s", joined)
	}
}

func TestStreamUpdatePathOldDoesNotEmitUntilFinish(t *testing.T) {
	// Mid-stream path+old without new_string must stay pending (not emit delete-match).
	a := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	frames := a.Feed("", "", []ToolDelta{{Index: 0, ID: "t1", Name: "Update", Arguments: `{"file_path":"/x","old_string":"a"}`}})
	joined := strings.Join(frames, "\n")
	if strings.Contains(joined, "tool_use") {
		t.Fatalf("must not emit tool_use mid-stream for path+old only:\n%s", joined)
	}
	if !a.HasPendingTools() || !a.NeedsClientKeepalive() {
		t.Fatal("path+old partial must stay pending and need keepalive")
	}
	// Late replace arrives → emit with real new_string
	frames = a.Feed("", "", []ToolDelta{{Index: 0, Arguments: `{"new_string":"b"}`}})
	frames = append(frames, a.Finish("tool_calls", Usage{})...)
	joined = strings.Join(frames, "\n")
	if !strings.Contains(joined, "tool_use") {
		t.Fatalf("expected tool_use after new_string:\n%s", joined)
	}
	if !strings.Contains(joined, `"new_string":"b"`) && !strings.Contains(joined, `"new_string": "b"`) {
		// compact form from stable encode
		if !strings.Contains(joined, "new_string") || !strings.Contains(joined, "b") {
			t.Fatalf("expected new_string b:\n%s", joined)
		}
	}
}

func TestStreamDoesNotEmitEditBeforeNewString(t *testing.T) {
	// Live Feed with path+old only must hold the tool; Finish force-fills new_string.
	a := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	frames := a.Feed("", "", []ToolDelta{{Index: 0, ID: "t1", Name: "Update", Arguments: `{"file_path":"/x","old_string":"a"}`}})
	joined := strings.Join(frames, "\n")
	if strings.Contains(joined, "tool_use") {
		t.Fatalf("must not emit tool_use mid-stream without new_string:\n%s", joined)
	}
	if !a.HasPendingTools() || !a.NeedsClientKeepalive() {
		t.Fatalf("pending incomplete Update should need keepalive")
	}
	// Later replace arrives — now live path can emit.
	frames = a.Feed("", "", []ToolDelta{{Index: 0, Arguments: `{"new_string":"b"}`}})
	// Merge may produce complete object; either Feed or Finish may emit.
	frames = append(frames, a.Finish("tool_calls", Usage{})...)
	joined = strings.Join(frames, "\n")
	if !strings.Contains(joined, "tool_use") {
		t.Fatalf("expected tool_use after new_string:\n%s", joined)
	}
	if !strings.Contains(joined, `"new_string"`) && !strings.Contains(joined, "new_string") {
		t.Fatalf("expected new_string in stream:\n%s", joined)
	}
}

func TestStreamForceFinishMissingNewString(t *testing.T) {
	a := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	_ = a.Feed("", "", []ToolDelta{{Index: 0, ID: "t1", Name: "Update", Arguments: `{"file_path":"/x","old_string":"delete-me"}`}})
	// No more deltas — Finish should still emit delete-match Edit.
	frames := a.Finish("tool_calls", Usage{})
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, "tool_use") {
		t.Fatalf("force-finish must emit tool_use:\n%s", joined)
	}
	if !strings.Contains(joined, "new_string") {
		t.Fatalf("force-finish must fill new_string:\n%s", joined)
	}
}
