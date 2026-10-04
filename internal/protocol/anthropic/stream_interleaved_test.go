package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// Claude Code regression: two tools streamed in interleaved fragments.
// Earlier Merge() treated repaired-incomplete buffers as complete and dropped
// the live remainder — Read became {"file_path":"/a"} and Bash {"command":"ec"}.
func TestStreamInterleavedToolFragments(t *testing.T) {
	a := NewStreamAssembler("m", "g", true, 2, []string{"Read", "Bash"})
	frames := a.Feed("", "", []ToolDelta{
		{Index: 0, ID: "r", Name: "Read", Arguments: `{"file_path":"/a`},
		{Index: 1, ID: "b", Name: "Bash", Arguments: `{"command":"ec`},
	})
	// Mid-stream: incomplete, must not emit yet.
	joinedMid := strings.Join(frames, "\n")
	if strings.Contains(joinedMid, "tool_use") {
		// If something emitted, it must not be truncated.
		if strings.Contains(joinedMid, `"/a"`) && !strings.Contains(joinedMid, `/a.go`) {
			t.Fatalf("premature truncated Read emit:\n%s", joinedMid)
		}
	}
	frames = append(frames, a.Feed("", "", []ToolDelta{
		{Index: 1, Arguments: `ho hi"}`},
		{Index: 0, Arguments: `.go"}`},
	})...)
	frames = append(frames, a.Finish("tool_calls", Usage{})...)

	var readPath, bashCmd string
	for _, ev := range ParseEvents(frames) {
		if ev["type"] != "content_block_delta" {
			continue
		}
		delta, _ := ev["delta"].(map[string]any)
		if delta["type"] != "input_json_delta" {
			continue
		}
		pj, _ := delta["partial_json"].(string)
		var obj map[string]any
		if json.Unmarshal([]byte(pj), &obj) != nil {
			continue
		}
		if p, ok := obj["file_path"].(string); ok && p != "" {
			readPath = p
		}
		if c, ok := obj["command"].(string); ok && c != "" {
			bashCmd = c
		}
	}
	if readPath != "/a.go" {
		t.Fatalf("Read path=%q want /a.go; frames=\n%s", readPath, strings.Join(frames, "\n"))
	}
	if bashCmd != "echo hi" {
		t.Fatalf("Bash cmd=%q want echo hi; frames=\n%s", bashCmd, strings.Join(frames, "\n"))
	}
}

// Char-level streaming of a single Edit through the Anthropic assembler.
func TestStreamCharLevelEdit(t *testing.T) {
	full := `{"file_path":"/src/main.go","old_string":"foo()","new_string":"bar()"}`
	a := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	// First chunk with name/id, rest as pure argument deltas.
	var frames []string
	frames = append(frames, a.Feed("", "", []ToolDelta{{Index: 0, ID: "t1", Name: "Update", Arguments: full[:8]}})...)
	for i := 8; i < len(full); i += 3 {
		end := i + 3
		if end > len(full) {
			end = len(full)
		}
		frames = append(frames, a.Feed("", "", []ToolDelta{{Index: 0, Arguments: full[i:end]}})...)
	}
	frames = append(frames, a.Finish("tool_calls", Usage{})...)
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, "tool_use") {
		t.Fatalf("expected tool_use after char-level stream:\n%s", joined)
	}
	if !strings.Contains(joined, "/src/main.go") || !strings.Contains(joined, "bar()") {
		t.Fatalf("content lost in char-level Edit:\n%s", joined)
	}
}
