package toolcall

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeJSONPromotesCmdToCommandForExecCommand(t *testing.T) {
	for _, name := range []string{"exec_command", "run_command", "shell", "local_shell", "Shell"} {
		got := NormalizeJSON(`{"cmd":"curl wttr.in/Changsha"}`, name)
		if !strings.Contains(got, `"command"`) {
			t.Fatalf("%s: expected command key, got %s", name, got)
		}
		if strings.Contains(got, `"cmd"`) {
			t.Fatalf("%s: cmd alias must be removed, got %s", name, got)
		}
		if !CompleteJSON(got, name) {
			t.Fatalf("%s: CompleteJSON false for %s", name, got)
		}
	}
}

func TestProjectShellArgsForceCmdEvenForExoticName(t *testing.T) {
	in := EffectiveJSON(`{"command":"curl wttr.in/Changsha"}`, "exec_command")
	out := ProjectShellArgsForClient(in, "exec_command", "cmd")
	if !strings.Contains(out, `"cmd"`) {
		t.Fatalf("expected cmd projection: %s", out)
	}
	if strings.Contains(out, `"command"`) {
		t.Fatalf("command must not remain: %s", out)
	}
}

func TestIsShellToolCoversCodexNames(t *testing.T) {
	for _, name := range []string{"exec_command", "run_command", "shell_command", "local_shell", "default_api.exec_command", "functions.Shell"} {
		if !IsShellTool(name) {
			t.Fatalf("IsShellTool(%q)=false", name)
		}
	}
}

func TestProjectShellArgsRoundTripCmd(t *testing.T) {
	// Client history may send cmd; internal normalize to command; project back to cmd.
	internal := EffectiveJSON(`{"cmd":"pwd"}`, "shell")
	var obj map[string]any
	if err := json.Unmarshal([]byte(internal), &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["command"]; !ok {
		t.Fatalf("internal must use command: %s", internal)
	}
	client := ProjectShellArgsForClient(internal, "shell", "cmd")
	if !strings.Contains(client, `"cmd":"pwd"`) && !strings.Contains(client, `"cmd": "pwd"`) {
		t.Fatalf("client form: %s", client)
	}
}

// Hermes agent (Nous Research) registers tool "terminal" with required
// parameter "command". Defaulting shell-family tools to Codex "cmd" made
// Hermes see empty args.get("command") and fail/retry the tool call.
func TestHermesTerminalIsShellAndKeepsCommand(t *testing.T) {
	if !IsShellTool("terminal") {
		t.Fatal("IsShellTool(terminal)=false")
	}
	if got := DefaultShellArgKey("terminal"); got != "command" {
		t.Fatalf("DefaultShellArgKey(terminal)=%q", got)
	}
	if got := DefaultShellArgKey("exec_command"); got != "cmd" {
		t.Fatalf("DefaultShellArgKey(exec_command)=%q", got)
	}

	// Schema-driven map (what serveChat/serveResponses build from client tools).
	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "terminal",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command":    map[string]any{"type": "string"},
						"background": map[string]any{"type": "boolean"},
					},
					"required": []any{"command"},
				},
			},
		},
	}
	m := ShellArgKeyMap(tools)
	if m["terminal"] != "command" {
		t.Fatalf("ShellArgKeyMap terminal=%q map=%#v", m["terminal"], m)
	}

	// Upstream Grok holds internal {"command":...}; project for Hermes must stay command.
	out := ProjectShellArgsForClient(`{"command":"ls -la"}`, "terminal", DefaultShellArgKey("terminal"))
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("out=%s err=%v", out, err)
	}
	if parsed["command"] != "ls -la" {
		t.Fatalf("command=%v in %s", parsed["command"], out)
	}
	if _, ok := parsed["cmd"]; ok {
		t.Fatalf("Hermes terminal must not project to cmd: %s", out)
	}

	// Empty preferred key also defaults via DefaultShellArgKey for terminal.
	out2 := ProjectShellArgsForClient(`{"command":"pwd"}`, "terminal", "")
	if strings.Contains(out2, `"cmd"`) && !strings.Contains(out2, `"command"`) {
		t.Fatalf("empty preferred projected to cmd-only: %s", out2)
	}
	var p2 map[string]any
	if err := json.Unmarshal([]byte(out2), &p2); err != nil {
		t.Fatalf("out2=%s err=%v", out2, err)
	}
	if p2["command"] != "pwd" {
		t.Fatalf("empty preferred must keep command for terminal: %s", out2)
	}
}

func TestHermesTerminalRequiredComplete(t *testing.T) {
	got := NormalizeJSON(`{"command":"echo hi"}`, "terminal")
	if !CompleteJSON(got, "terminal") {
		t.Fatalf("CompleteJSON false for terminal: %s", got)
	}
	// cmd alias should promote to command for terminal.
	got2 := NormalizeJSON(`{"cmd":"echo hi"}`, "terminal")
	if !strings.Contains(got2, `"command"`) {
		t.Fatalf("cmd alias not promoted: %s", got2)
	}
	if strings.Contains(got2, `"cmd"`) {
		t.Fatalf("cmd alias not stripped: %s", got2)
	}
}
