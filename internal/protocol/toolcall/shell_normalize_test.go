package toolcall_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hm2899/grokcli-2api/internal/protocol/toolcall"
)

func TestNormalizeJSONShellFlattensAndPromotes(t *testing.T) {
	// Nested empty argv collapses → incomplete (no usable command).
	got := toolcall.NormalizeJSON(`{"command":[[""]]}`, "shell")
	if toolcall.CompleteJSON(got, "shell") {
		t.Fatalf("nested empty must be incomplete, got %s", got)
	}

	// Nested argv flattens to string or flat array.
	got2 := toolcall.NormalizeJSON(`{"command":[["echo","hi"]]}`, "shell")
	if !toolcall.CompleteJSON(got2, "shell") {
		t.Fatalf("nested argv incomplete: %s", got2)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(got2), &p); err != nil {
		t.Fatal(err)
	}
	switch v := p["command"].(type) {
	case string:
		if !strings.Contains(v, "echo") {
			t.Fatalf("command string unexpected: %q", v)
		}
	case []any:
		if len(v) < 1 {
			t.Fatalf("empty argv: %#v", v)
		}
		// Must be flat strings, not nested arrays.
		for _, item := range v {
			if _, ok := item.([]any); ok {
				t.Fatalf("still nested: %#v", v)
			}
		}
	default:
		t.Fatalf("unexpected command type %T %#v", v, v)
	}

	// bash/line aliases promote to command and strip leftovers.
	for _, tc := range []struct {
		in, tool, want string
	}{
		{`{"bash":"pwd"}`, "shell", "pwd"},
		{`{"line":"ls"}`, "exec_command", "ls"},
	} {
		out := toolcall.NormalizeJSON(tc.in, tc.tool)
		if !toolcall.CompleteJSON(out, tc.tool) {
			t.Fatalf("incomplete %s from %s → %s", tc.tool, tc.in, out)
		}
		_ = json.Unmarshal([]byte(out), &p)
		if p["command"] != tc.want {
			t.Fatalf("command=%v want %q in %s", p["command"], tc.want, out)
		}
		for _, bad := range []string{"bash", "line", "cmd"} {
			if _, ok := p[bad]; ok {
				t.Fatalf("leftover %s in %s", bad, out)
			}
		}
	}
}

func TestProjectShellArgsForClientAfterNormalize(t *testing.T) {
	internal := toolcall.EffectiveJSON(`{"command":"curl wttr.in/Changsha"}`, "exec_command")
	out := toolcall.ProjectShellArgsForClient(internal, "exec_command", "cmd")
	if !strings.Contains(out, `"cmd"`) || strings.Contains(out, `"command"`) {
		t.Fatalf("expected cmd-only projection, got %s", out)
	}
	if !strings.Contains(out, "Changsha") {
		t.Fatalf("value lost: %s", out)
	}
}

func TestIsShellToolExecCommand(t *testing.T) {
	for _, name := range []string{"exec_command", "run_command", "shell_command", "local_shell", "Shell", "default_api.exec_command"} {
		if !toolcall.IsShellTool(name) {
			t.Fatalf("IsShellTool(%q) = false", name)
		}
	}
}
