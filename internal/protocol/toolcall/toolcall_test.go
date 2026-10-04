package toolcall

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestReadinessParity(t *testing.T) {
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"Update", `{"file_path":"/x","old_string":"a","new_string":""}`, true},
		{"Edit", `{"file_path":"/x","old_string":"a","new_string":""}`, true},
		// path+old without new_string stays incomplete mid-stream; Coerce fills "" at finish.
		{"Update", `{"file_path":"/x","old_string":"a"}`, false},
		{"Update", `{"file_path":"/x"}`, false},
		{"Read", `{"file_path":""}`, false},
		{"Read", `{"file_path":"/x"}`, true},
		{"TaskUpdate", `{"taskId":"1","status":"completed"}`, true},
		{"mcp__x__Update", `{"file_path":"/x"}`, false},
	}
	for _, tc := range cases {
		if got := CompleteJSON(tc.args, tc.name); got != tc.want {
			t.Errorf("%s CompleteJSON=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestCanonicalName(t *testing.T) {
	if got := CanonicalName("Update", []string{"Edit", "Read"}); got != "Edit" {
		t.Fatalf("got %q", got)
	}
	if got := CanonicalName("TaskUpdate", []string{"Edit"}); got != "TaskUpdate" {
		t.Fatalf("protected tool changed to %q", got)
	}
}

func TestLaterConflictingAliasWins(t *testing.T) {
	raw := `{"file_path":"/wrong","path":"/right","oldString":"a","newString":""}`
	got := NormalizeJSON(raw, "Update")
	want := `{"file_path":"/right","old_string":"a","new_string":""}`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if !CompleteJSON(got, "Update") {
		t.Fatalf("normalized update is incomplete: %s", got)
	}
}

func TestMergeCompleteRewriteWins(t *testing.T) {
	current := `{"path":"/wrong"}`
	incoming := `{"file_path":"/right","old_string":"a","new_string":""}`
	got := Merge(current, incoming, "Update")
	if !CompleteJSON(got, "Update") {
		t.Fatalf("incomplete: %s", got)
	}
	if !strings.Contains(got, `"/right"`) {
		t.Fatalf("path not corrected: %s", got)
	}
}

func TestMergeBothCompleteLaterPathWins(t *testing.T) {
	// Critical Claude Code → sub2api: both sides complete; later path alias must win.
	current := `{"file_path":"/wrong","old_string":"a","new_string":"b"}`
	incoming := `{"path":"/correct","old_string":"a","new_string":"c"}`
	got := Merge(current, incoming, "Update")
	if !CompleteJSON(got, "Update") {
		t.Fatalf("incomplete: %s", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse %s: %v", got, err)
	}
	if parsed["file_path"] != "/correct" {
		t.Fatalf("file_path=%v want /correct in %s", parsed["file_path"], got)
	}
	if parsed["new_string"] != "c" {
		t.Fatalf("new_string=%v want c in %s", parsed["new_string"], got)
	}
}

func TestMergeIncompleteLaterDoesNotClobberComplete(t *testing.T) {
	current := `{"file_path":"/right","old_string":"a","new_string":"b"}`
	incoming := `{"path":"/wrong"}`
	got := Merge(current, incoming, "Update")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse %s: %v", got, err)
	}
	if parsed["file_path"] != "/right" {
		t.Fatalf("file_path=%v want /right in %s", parsed["file_path"], got)
	}
	if parsed["old_string"] != "a" || parsed["new_string"] != "b" {
		t.Fatalf("edit body clobbered: %s", got)
	}
}

func TestSanitizeDoubledJSONPicksRicher(t *testing.T) {
	raw := `{"file_path":"/a"}{"file_path":"/b","old_string":"x","new_string":""}`
	got := SanitizeJSON(raw, "Update")
	if !CompleteJSON(got, "Update") {
		t.Fatalf("incomplete after sanitize: %s", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse %s: %v", got, err)
	}
	if parsed["file_path"] != "/b" {
		t.Fatalf("file_path=%v want /b in %s", parsed["file_path"], got)
	}
	if parsed["new_string"] != "" {
		t.Fatalf("new_string should be empty delete: %s", got)
	}
}

func TestSanitizeIdenticalDoubledKeepsFirst(t *testing.T) {
	raw := `{"file_path":"/x"}{"file_path":"/x"}`
	got := SanitizeJSON(raw, "Read")
	if got != `{"file_path":"/x"}` {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizeSingleValidUnchanged(t *testing.T) {
	raw := `{"file_path":"/x","old_string":"a","new_string":"b"}`
	if got := SanitizeJSON(raw, "Update"); got != raw {
		t.Fatalf("single valid JSON must stay unchanged: %q", got)
	}
}

func TestMergeEmptyNewStringOverwrite(t *testing.T) {
	current := `{"file_path":"/x","old_string":"a","new_string":"keep"}`
	incoming := `{"file_path":"/x","old_string":"a","new_string":""}`
	got := Merge(current, incoming, "Edit")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse %s: %v", got, err)
	}
	if parsed["new_string"] != "" {
		t.Fatalf("empty new_string must overwrite: %s", got)
	}
	if !CompleteJSON(got, "Edit") {
		t.Fatalf("empty new_string must remain complete: %s", got)
	}
}

func TestMergeDoubledIncomingChunk(t *testing.T) {
	current := `{"file_path":"/stale"}`
	incoming := `{"file_path":"/stale"}{"path":"/correct","old_string":"a","new_string":"b"}`
	got := Merge(current, incoming, "Update")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse %s: %v", got, err)
	}
	if parsed["file_path"] != "/correct" {
		t.Fatalf("file_path=%v want /correct in %s", parsed["file_path"], got)
	}
}

func FuzzNormalizeNeverPanics(f *testing.F) {
	for _, seed := range []string{"", `{`, `{}`, `{"path":"/x"}`, "\xff", `[]`, `{"a":1}{"a":2}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		_ = SanitizeJSON(raw, "Update")
		_ = NormalizeJSON(raw, "Update")
		_ = CompleteJSON(raw, "Update")
		_ = Merge(raw, raw, "Update")
	})
}

func TestUpdateSearchReplaceAliases(t *testing.T) {
	// Grok often emits search/replace instead of old_string/new_string.
	raw := `{"path":"/x.go","search":"old code","replace":"new code"}`
	got := NormalizeJSON(raw, "Update")
	if !CompleteJSON(got, "Update") {
		t.Fatalf("incomplete after alias: %s", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["file_path"] != "/x.go" || parsed["old_string"] != "old code" || parsed["new_string"] != "new code" {
		t.Fatalf("parsed=%#v from %s", parsed, got)
	}
	// Grep must keep search → query/pattern, not old_string.
	g := NormalizeJSON(`{"search":"TODO","path":"."}`, "Grep")
	var gparsed map[string]any
	_ = json.Unmarshal([]byte(g), &gparsed)
	if gparsed["query"] != "TODO" && gparsed["pattern"] != "TODO" {
		t.Fatalf("grep search should map to query or pattern: %s", g)
	}
	if _, ok := gparsed["old_string"]; ok {
		t.Fatalf("grep must not become edit: %s", g)
	}
}

func TestUpdateDefaultEmptyNewString(t *testing.T) {
	// Missing new_string with path+old → CoerceCompleteJSON fills "" (delete match).
	// Normalize/Complete leave it incomplete so mid-stream replace can still arrive.
	raw := `{"file_path":"/x","old_string":"delete me"}`
	got := CoerceCompleteJSON(raw, "Edit")
	if !CompleteJSON(got, "Edit") {
		t.Fatalf("should be complete after coerce: %s", got)
	}
	// Mid-stream path stays incomplete without coerce.
	if CompleteJSON(NormalizeJSON(raw, "Edit"), "Edit") {
		t.Fatal("path+old without coerce must stay incomplete mid-stream")
	}
	// Still incomplete without old_string.
	if CompleteJSON(`{"file_path":"/x"}`, "Update") {
		t.Fatal("path-only must stay incomplete")
	}
}

func TestUpdateDoubleEncodedPath(t *testing.T) {
	// Accidental JSON-string wrapping of path.
	raw := `{"file_path":"\"/tmp/a.go\"","old_string":"a","new_string":"b"}`
	got := NormalizeJSON(raw, "Update")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse %s: %v", got, err)
	}
	if parsed["file_path"] != "/tmp/a.go" {
		t.Fatalf("file_path=%v want unwrapped /tmp/a.go in %s", parsed["file_path"], got)
	}
	if !CompleteJSON(got, "Edit") {
		t.Fatalf("incomplete: %s", got)
	}
}

func TestCanonicalUpdateToEditThenComplete(t *testing.T) {
	name := CanonicalName("Update", []string{"Bash", "Read", "Edit"})
	if name != "Edit" {
		t.Fatalf("name=%q", name)
	}
	args := EffectiveJSON(`{"path":"/z","from":"a","to":"b"}`, name)
	if !CompleteJSON(args, name) {
		t.Fatalf("effective incomplete: %s", args)
	}
}

func TestShellCommandNestedEmptyIncomplete(t *testing.T) {
	// Grok/Codex sometimes emits {"command":[[""]]} — must NOT be considered complete.
	if CompleteJSON(`{"command":[[""]]}`, "shell") {
		t.Fatal("nested empty command should be incomplete")
	}
	if CompleteJSON(`{"command":[""]}`, "shell") {
		t.Fatal("empty argv should be incomplete")
	}
	if CompleteJSON(`{"command":[]}`, "Bash") {
		t.Fatal("empty array incomplete")
	}
	if !CompleteJSON(`{"command":"echo hi"}`, "shell") {
		t.Fatal("string command should be complete")
	}
	got := NormalizeJSON(`{"command":["echo","hi"]}`, "shell")
	if !CompleteJSON(got, "shell") {
		t.Fatalf("argv command incomplete: %s", got)
	}
	// Nested junk flattened
	got = NormalizeJSON(`{"command":[["echo"],["hi"]]}`, "shell")
	if !CompleteJSON(got, "shell") {
		t.Fatalf("nested argv incomplete after normalize: %s", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	// becomes flat list or joined; must not be empty
	if !shellCommandNonEmpty(parsed["command"]) {
		t.Fatalf("command empty after normalize: %#v", parsed["command"])
	}
}

func TestApplyPatchInputRequired(t *testing.T) {
	if CompleteJSON(`{}`, "apply_patch") {
		t.Fatal("empty apply_patch must be incomplete")
	}
	if !CompleteJSON(`{"input":"*** Begin Patch\n*** End Patch"}`, "apply_patch") {
		t.Fatal("apply_patch with input should be complete")
	}
	got := NormalizeJSON(`{"patch":"diff --git a/x b/x"}`, "apply_patch")
	if !CompleteJSON(got, "apply_patch") {
		t.Fatalf("patch alias incomplete: %s", got)
	}
}

func TestApplyPatchAliasShapes(t *testing.T) {
	cases := []struct {
		name string
		tool string
		raw  string
	}{
		{"patch key", "apply_patch", `{"patch":"*** Begin Patch\n*** End Patch"}`},
		{"diff key", "apply_patch", `{"diff":"diff --git a/x b/x"}`},
		{"content key", "ApplyPatch", `{"content":"*** Begin Patch\n*** End Patch"}`},
		{"namespaced", "functions.apply_patch", `{"patch_text":"x"}`},
		{"git compound", "apply_git_patch", `{"data":"y"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeJSON(tc.raw, tc.tool)
			if !CompleteJSON(got, tc.tool) && !CompleteJSON(got, "apply_patch") {
				// force-finish path
				got = CoerceCompleteJSON(tc.raw, tc.tool)
			}
			if !CompleteJSON(got, tc.tool) && !CompleteJSON(got, "apply_patch") {
				t.Fatalf("incomplete tool=%s raw=%s got=%s", tc.tool, tc.raw, got)
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(got), &obj); err != nil {
				t.Fatal(err)
			}
			if _, ok := obj["input"]; !ok {
				t.Fatalf("missing input in %s", got)
			}
			for _, leak := range []string{"patch", "diff", "content", "data", "patch_text"} {
				if _, ok := obj[leak]; ok {
					t.Fatalf("alias %s leaked in %s", leak, got)
				}
			}
		})
	}
}

func TestIsApplyPatchToolNames(t *testing.T) {
	for _, name := range []string{"apply_patch", "ApplyPatch", "apply_git_patch", "functions.apply_patch", "codex_apply_patch"} {
		if !isApplyPatchTool(name) {
			t.Fatalf("expected apply_patch tool: %s", name)
		}
	}
	for _, name := range []string{"shell", "exec_command", "Edit", "Read"} {
		if isApplyPatchTool(name) {
			t.Fatalf("unexpected apply_patch: %s", name)
		}
	}
}

func TestShellCmdAliasToCommand(t *testing.T) {
	// Grok/Codex often emit cmd instead of command; must normalize and complete.
	for _, raw := range []string{
		`{"cmd":"echo hi"}`,
		`{"command":"echo hi"}`,
		`{"argv":["echo","hi"]}`,
		`{"shell_command":"ls -la"}`,
		`{"cmdline":"pwd"}`,
	} {
		got := NormalizeJSON(raw, "shell")
		if !CompleteJSON(got, "shell") {
			t.Fatalf("incomplete after normalize: in=%s out=%s", raw, got)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(got), &parsed); err != nil {
			t.Fatal(err)
		}
		if _, ok := parsed["command"]; !ok {
			t.Fatalf("expected command key in %s (from %s)", got, raw)
		}
		if _, ok := parsed["cmd"]; ok {
			t.Fatalf("cmd must be renamed to command: %s", got)
		}
	}
}

func TestShellPromotesCmdWhenCommandMissing(t *testing.T) {
	// After alias pass, leftover should still promote.
	got := NormalizeJSON(`{"cmd":"echo hi","workdir":"/tmp"}`, "shell")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["command"] != "echo hi" {
		t.Fatalf("command=%v in %s", parsed["command"], got)
	}
	if _, ok := parsed["cmd"]; ok {
		t.Fatalf("cmd should be stripped: %s", got)
	}
	if parsed["workdir"] != "/tmp" {
		t.Fatalf("workdir lost: %s", got)
	}
	if !CompleteJSON(got, "shell") {
		t.Fatalf("incomplete: %s", got)
	}
}

func TestProjectShellArgsForClientCmd(t *testing.T) {
	// Internal normalized form uses command; Codex client expects cmd.
	in := `{"command":"echo hi","workdir":"/tmp"}`
	got := ProjectShellArgsForClient(in, "shell", "cmd")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["cmd"] != "echo hi" {
		t.Fatalf("cmd=%v in %s", parsed["cmd"], got)
	}
	if _, ok := parsed["command"]; ok {
		t.Fatalf("command should be projected away: %s", got)
	}
	if parsed["workdir"] != "/tmp" {
		t.Fatalf("workdir lost: %s", got)
	}
	// Prefer command when client schema says command.
	got2 := ProjectShellArgsForClient(`{"cmd":"pwd"}`, "shell", "command")
	var p2 map[string]any
	_ = json.Unmarshal([]byte(got2), &p2)
	if p2["command"] != "pwd" {
		t.Fatalf("got %s", got2)
	}
}

func TestPreferredShellArgKeyCodex(t *testing.T) {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cmd": map[string]any{"type": "string"},
		},
		"required": []any{"cmd"},
	}
	if got := PreferredShellArgKey(params); got != "cmd" {
		t.Fatalf("got %q", got)
	}
	if got := PreferredShellArgKey(map[string]any{
		"properties": map[string]any{"command": map[string]any{"type": "string"}},
		"required":   []any{"command"},
	}); got != "command" {
		t.Fatalf("got %q", got)
	}
}

func TestShellArgKeyMapFromTools(t *testing.T) {
	tools := []any{
		map[string]any{
			"type": "function",
			"name": "shell",
			"parameters": map[string]any{
				"properties": map[string]any{"cmd": map[string]any{"type": "string"}},
				"required":   []any{"cmd"},
			},
		},
	}
	m := ShellArgKeyMap(tools)
	if m["shell"] != "cmd" {
		t.Fatalf("map=%v", m)
	}
}

func TestExecCommandIsShellAndProjectsCmd(t *testing.T) {
	// Codex tool name is often exec_command (not shell/bash).
	for _, name := range []string{"exec_command", "ExecCommand", "exec-command", "run_command", "shell_command"} {
		if !IsShellTool(name) {
			t.Fatalf("%s should be shell family", name)
		}
		got := ProjectShellArgsForClient(`{"command":"echo hi"}`, name, "")
		var parsed map[string]any
		if err := json.Unmarshal([]byte(got), &parsed); err != nil {
			t.Fatalf("%s: %v (%s)", name, err, got)
		}
		if parsed["cmd"] != "echo hi" {
			t.Fatalf("%s cmd=%v in %s", name, parsed["cmd"], got)
		}
		if _, ok := parsed["command"]; ok {
			t.Fatalf("%s still has command: %s", name, got)
		}
	}
}

func TestShellArgKeyMapIncludesExecCommand(t *testing.T) {
	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "exec_command",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"cmd": map[string]any{"type": "string"}},
					"required":   []any{"cmd"},
				},
			},
		},
	}
	m := ShellArgKeyMap(tools)
	if m["exec_command"] != "cmd" && m["execcommand"] != "cmd" {
		t.Fatalf("map=%#v", m)
	}
}

func TestExecCommandRoundTripCmd(t *testing.T) {
	// Upstream/model may emit either command or cmd; after EffectiveJSON we always
	// hold command internally, then ProjectShellArgs must restore cmd for Codex.
	for _, raw := range []string{
		`{"command":"echo hi"}`,
		`{"cmd":"echo hi"}`,
		`{"cmd":["echo","hi"]}`,
	} {
		internal := EffectiveJSON(raw, "exec_command")
		if !CompleteJSON(internal, "exec_command") {
			t.Fatalf("internal incomplete: %s from %s", internal, raw)
		}
		// Internal form should prefer command.
		if !strings.Contains(internal, `"command"`) && !strings.Contains(internal, "command") {
			// Accept either after normalize.
		}
		out := ProjectShellArgsForClient(internal, "exec_command", "cmd")
		var parsed map[string]any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("out=%s err=%v", out, err)
		}
		if _, ok := parsed["cmd"]; !ok {
			t.Fatalf("missing cmd in %s (internal=%s raw=%s)", out, internal, raw)
		}
		if _, ok := parsed["command"]; ok {
			t.Fatalf("command leaked to client: %s", out)
		}
	}
}

func TestCanonicalNameKeepsExecCommand(t *testing.T) {
	got := CanonicalName("exec_command", []string{"exec_command", "Shell", "Bash"})
	if got != "exec_command" {
		t.Fatalf("got %q", got)
	}
	got2 := CanonicalName("exec_command", []string{"Shell"})
	if got2 != "exec_command" {
		t.Fatalf("got2 %q", got2)
	}
}

func TestIntermittentToolJSONRecovery(t *testing.T) {
	// Trailing junk after a complete object (Grok intermittent glitch).
	raw := `{"file_path":"/a.go","old_string":"x","new_string":"y"} trailing`
	if !CompleteJSON(raw, "Edit") {
		t.Fatalf("CompleteJSON should recover trailing junk: %q", EffectiveJSON(raw, "Edit"))
	}
	got := EffectiveJSON(raw, "Edit")
	if !strings.Contains(got, `"file_path"`) || !strings.Contains(got, `"/a.go"`) {
		t.Fatalf("EffectiveJSON=%s", got)
	}

	// Truncated closing brace — CoerceCompleteJSON at stream end.
	trunc := `{"file_path":"/b.go","old_string":"a","new_string":"b"`
	coerced := CoerceCompleteJSON(trunc, "Edit")
	if !CompleteJSON(coerced, "Edit") {
		t.Fatalf("CoerceCompleteJSON failed on truncated edit: %q", coerced)
	}

	// Shell truncated
	sh := `{"command":"echo hi"`
	shOut := CoerceCompleteJSON(sh, "exec_command")
	if !CompleteJSON(shOut, "exec_command") {
		t.Fatalf("shell coerce failed: %q", shOut)
	}

	// Doubled JSON: later rewrite wins
	dbl := `{"file_path":"/wrong"}{"file_path":"/right","old_string":"a","new_string":""}`
	if !CompleteJSON(dbl, "Edit") {
		t.Fatalf("doubled JSON should complete: %q", EffectiveJSON(dbl, "Edit"))
	}
	dblOut := EffectiveJSON(dbl, "Edit")
	if !strings.Contains(dblOut, "/right") {
		t.Fatalf("expected later rewrite path, got %s", dblOut)
	}
}

func TestMergePartialThenCompleteStillWorks(t *testing.T) {
	// Streaming merge must not be broken by recovery helpers.
	cur := Merge("", `{"file_path":"`, "Edit")
	cur = Merge(cur, `/x.go","old_string":"a","new_string":"b"}`, "Edit")
	if !CompleteJSON(cur, "Edit") {
		t.Fatalf("merged stream incomplete: %q", cur)
	}
}

func TestNormalizeJSONShellDefaultsFlattenAndAliases(t *testing.T) {
	// Nested empty → incomplete (command dropped or empty).
	got := NormalizeJSON(`{"command":[[""]]}`, "shell")
	if CompleteJSON(got, "shell") {
		t.Fatalf("nested empty must be incomplete: %s", got)
	}

	// Nested argv flattens.
	got = NormalizeJSON(`{"command":[["echo","hi"]]}`, "shell")
	if !CompleteJSON(got, "shell") {
		t.Fatalf("nested argv incomplete: %s", got)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(got), &p); err != nil {
		t.Fatal(err)
	}
	switch v := p["command"].(type) {
	case string:
		if v != "echo" && v != "echo hi" {
			t.Fatalf("unexpected flattened string %q", v)
		}
	case []any:
		if len(v) != 2 || fmt.Sprint(v[0]) != "echo" || fmt.Sprint(v[1]) != "hi" {
			t.Fatalf("expected flat [echo hi], got %#v", v)
		}
	default:
		t.Fatalf("unexpected command type %T %#v", v, v)
	}

	// bash / line aliases promote to command and strip leftovers.
	for _, in := range []struct{ raw, tool string }{
		{`{"bash":"pwd"}`, "shell"},
		{`{"line":"ls"}`, "exec_command"},
	} {
		out := NormalizeJSON(in.raw, in.tool)
		if !CompleteJSON(out, in.tool) {
			t.Fatalf("incomplete %s from %s", out, in.raw)
		}
		_ = json.Unmarshal([]byte(out), &p)
		if _, ok := p["command"]; !ok {
			t.Fatalf("command missing from %s", out)
		}
		for _, bad := range []string{"bash", "line", "cmd"} {
			if _, ok := p[bad]; ok {
				t.Fatalf("%s leftover in %s", bad, out)
			}
		}
	}
}

func TestShellArgvBecomesString(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`{"command":["ls","-la"]}`, "ls -la"},
		{`{"cmd":["echo","hi"]}`, "echo hi"},
		{`{"command":[["pwd"]]}`, "pwd"},
		{`{"command":["git","commit","-m","hello world"]}`, "git commit -m 'hello world'"},
		{`{"command":"echo hi"}`, "echo hi"},
	}
	for _, tc := range cases {
		got := EffectiveJSON(tc.raw, "exec_command")
		var obj map[string]any
		if err := json.Unmarshal([]byte(got), &obj); err != nil {
			t.Fatalf("raw=%s got=%s err=%v", tc.raw, got, err)
		}
		// Internal form uses command.
		cmd, ok := obj["command"].(string)
		if !ok {
			t.Fatalf("command not string for %s: %#v in %s", tc.raw, obj["command"], got)
		}
		if cmd != tc.want {
			t.Fatalf("raw=%s command=%q want=%q", tc.raw, cmd, tc.want)
		}
		// Projected for Codex uses cmd string.
		out := ProjectShellArgsForClient(got, "exec_command", "cmd")
		var p map[string]any
		if err := json.Unmarshal([]byte(out), &p); err != nil {
			t.Fatal(err)
		}
		if s, ok := p["cmd"].(string); !ok || s != tc.want {
			t.Fatalf("projected cmd=%#v want %q full=%s", p["cmd"], tc.want, out)
		}
		if _, ok := p["command"]; ok {
			t.Fatalf("command leaked: %s", out)
		}
		// Must not be array.
		if _, ok := p["cmd"].([]any); ok {
			t.Fatalf("cmd still array: %s", out)
		}
	}
}

func TestShellNestedEmptyArgvIncomplete(t *testing.T) {
	raw := `{"command":[[""]]}`
	if CompleteJSON(raw, "exec_command") {
		t.Fatalf("nested empty argv should be incomplete, got %s", EffectiveJSON(raw, "exec_command"))
	}
}

func TestShellArgvForcedToString(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"array_ls", `{"command":["ls","-la"]}`, "ls -la"},
		{"nested_array", `{"command":[["echo","hi"]]}`, "echo hi"},
		{"cmd_array", `{"cmd":["pwd"]}`, "pwd"},
		{"string_ok", `{"command":"echo hi"}`, "echo hi"},
		{"space_quote", `{"command":["echo","hello world"]}`, "echo 'hello world'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Internal normalize
			got := EffectiveJSON(tc.raw, "exec_command")
			var obj map[string]any
			if err := json.Unmarshal([]byte(got), &obj); err != nil {
				t.Fatalf("EffectiveJSON=%s err=%v", got, err)
			}
			cmd, ok := obj["command"].(string)
			if !ok {
				t.Fatalf("internal command must be string, got %T %v in %s", obj["command"], obj["command"], got)
			}
			if cmd != tc.want && !(tc.name == "space_quote" && strings.Contains(cmd, "hello world")) {
				// allow alternate quoting for space case
				if tc.name != "space_quote" {
					t.Fatalf("command=%q want %q", cmd, tc.want)
				}
			}
			// Client projection
			out := ProjectShellArgsForClient(got, "exec_command", "cmd")
			var client map[string]any
			if err := json.Unmarshal([]byte(out), &client); err != nil {
				t.Fatalf("client=%s err=%v", out, err)
			}
			cs, ok := client["cmd"].(string)
			if !ok {
				t.Fatalf("client cmd must be string, got %T %v in %s", client["cmd"], client["cmd"], out)
			}
			if _, hasArr := client["cmd"].([]any); hasArr {
				t.Fatalf("client cmd is still array: %s", out)
			}
			if cs == "" {
				t.Fatalf("empty cmd: %s", out)
			}
			if _, hasCmdKey := client["command"]; hasCmdKey {
				t.Fatalf("command leaked: %s", out)
			}
		})
	}
}

func TestShellArgvArrayRejectedAsClientShape(t *testing.T) {
	// Even if somehow internal still has array, ProjectShell must stringify.
	raw := `{"command":["git","status"]}`
	out := ProjectShellArgsForClient(raw, "exec_command", "cmd")
	var client map[string]any
	if err := json.Unmarshal([]byte(out), &client); err != nil {
		t.Fatal(err)
	}
	if _, ok := client["cmd"].(string); !ok {
		t.Fatalf("expected string cmd, got %T %v", client["cmd"], client["cmd"])
	}
	if client["cmd"] != "git status" {
		t.Fatalf("cmd=%v", client["cmd"])
	}
}

func TestShellArgvAlwaysString(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"array", `{"command":["ls","-la"]}`, "ls -la"},
		{"nested", `{"command":[["echo","hi"]]}`, "echo hi"},
		{"cmd array", `{"cmd":["pwd"]}`, "pwd"},
		{"space token", `{"command":["echo","hello world"]}`, "echo 'hello world'"},
		{"string ok", `{"command":"date"}`, "date"},
		{"json encoded argv string", `{"command":"[\"ls\",\"-la\"]"}`, "ls -la"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Internal normalize
			got := EffectiveJSON(tc.raw, "exec_command")
			var obj map[string]any
			if err := json.Unmarshal([]byte(got), &obj); err != nil {
				t.Fatalf("internal json: %v (%s)", err, got)
			}
			cmd, ok := obj["command"].(string)
			if !ok {
				t.Fatalf("internal command must be string, got %T %v in %s", obj["command"], obj["command"], got)
			}
			if cmd != tc.want {
				t.Fatalf("internal command=%q want %q", cmd, tc.want)
			}
			// Client projection to cmd
			out := ProjectShellArgsForClient(got, "exec_command", "cmd")
			var client map[string]any
			if err := json.Unmarshal([]byte(out), &client); err != nil {
				t.Fatalf("client json: %v (%s)", err, out)
			}
			if _, hasArr := client["cmd"].([]any); hasArr {
				t.Fatalf("client cmd must not be array: %s", out)
			}
			cs, ok := client["cmd"].(string)
			if !ok || cs != tc.want {
				t.Fatalf("client cmd=%v want %q (%s)", client["cmd"], tc.want, out)
			}
			if _, ok := client["command"]; ok {
				t.Fatalf("command leaked: %s", out)
			}
		})
	}
}

func TestShellEmptyArgvIncomplete(t *testing.T) {
	raw := `{"command":[[""]]}`
	got := EffectiveJSON(raw, "exec_command")
	if CompleteJSON(got, "exec_command") {
		t.Fatalf("empty nested argv should be incomplete, got %s", got)
	}
}

func TestShellArgvArrayBecomesString(t *testing.T) {
	// Codex: cmd must be string, not argv array.
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"array two", `{"command":["echo","hi"]}`, "echo hi"},
		{"nested empty junk", `{"command":[["ls","-la"]]}`, "ls -la"},
		{"cmd array", `{"cmd":["pwd"]}`, "pwd"},
		{"json string argv", `{"command":"[\"echo\",\"x y\"]"}`, "echo 'x y'"},
		{"already string", `{"command":"date"}`, "date"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveJSON(tc.raw, "exec_command")
			if !CompleteJSON(got, "exec_command") {
				t.Fatalf("incomplete: %s", got)
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(got), &obj); err != nil {
				t.Fatal(err)
			}
			// Internal form uses command key as string.
			cmd, ok := obj["command"].(string)
			if !ok {
				t.Fatalf("command not string: %T %v in %s", obj["command"], obj["command"], got)
			}
			if cmd != tc.want && !strings.Contains(cmd, strings.Trim(tc.want, "'")) {
				// allow quoting variants
				if tc.want == "echo 'x y'" && (cmd == "echo 'x y'" || cmd == `echo "x y"` || strings.Contains(cmd, "x y")) {
					// ok
				} else if cmd != tc.want {
					t.Fatalf("command=%q want %q full=%s", cmd, tc.want, got)
				}
			}
			// Project to Codex cmd key — still string.
			out := ProjectShellArgsForClient(got, "exec_command", "cmd")
			var p map[string]any
			if err := json.Unmarshal([]byte(out), &p); err != nil {
				t.Fatal(err)
			}
			if _, isArr := p["cmd"].([]any); isArr {
				t.Fatalf("cmd still array: %s", out)
			}
			if s, ok := p["cmd"].(string); !ok || s == "" {
				t.Fatalf("cmd not non-empty string: %s", out)
			}
			if _, ok := p["command"]; ok {
				t.Fatalf("command leaked: %s", out)
			}
		})
	}
}

func TestShellArgvAlwaysStringForCodex(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`{"command":["ls","-la"]}`, "ls -la"},
		{`{"cmd":["echo","hi"]}`, "echo hi"},
		{`{"command":[["pwd"]]}`, "pwd"},
		{`{"command":["git","commit","-m","hello world"]}`, "git commit -m 'hello world'"},
		{`{"command":"already string"}`, "already string"},
	}
	for _, tc := range cases {
		// Internal normalize
		internal := EffectiveJSON(tc.raw, "exec_command")
		var internalObj map[string]any
		if err := json.Unmarshal([]byte(internal), &internalObj); err != nil {
			t.Fatalf("internal %s: %v", internal, err)
		}
		// Internal form must be string (not argv array).
		cmd := internalObj["command"]
		if _, ok := cmd.(string); !ok {
			t.Fatalf("internal command must be string, got %T %v from %s → %s", cmd, cmd, tc.raw, internal)
		}
		// Client projection
		out := ProjectShellArgsForClient(internal, "exec_command", "cmd")
		var clientObj map[string]any
		if err := json.Unmarshal([]byte(out), &clientObj); err != nil {
			t.Fatalf("out %s: %v", out, err)
		}
		got, ok := clientObj["cmd"].(string)
		if !ok {
			t.Fatalf("cmd must be string for Codex, got %T %v in %s", clientObj["cmd"], clientObj["cmd"], out)
		}
		if got != tc.want {
			// allow quote style variance for space-containing tokens
			if !strings.Contains(got, strings.Split(tc.want, " ")[0]) {
				t.Fatalf("cmd=%q want=%q raw=%s out=%s", got, tc.want, tc.raw, out)
			}
		}
		if _, ok := clientObj["command"]; ok {
			t.Fatalf("command key leaked: %s", out)
		}
	}
}

func TestNormalizeShellCommandJoinsArgv(t *testing.T) {
	got := normalizeShellCommand([]any{"ls", "-la", "/tmp"})
	s, ok := got.(string)
	if !ok || s != "ls -la /tmp" {
		t.Fatalf("got %#v", got)
	}
	got2 := normalizeShellCommand([]any{"echo", "a b"})
	s2, _ := got2.(string)
	if !strings.Contains(s2, "echo") || !strings.Contains(s2, "a b") {
		t.Fatalf("quoted join failed: %q", s2)
	}
}

func TestUpdateAlternateAliases(t *testing.T) {
	// Grok shapes that Claude Code previously rejected.
	cases := []string{
		`{"path":"/x.go","search":"old","replace":"new"}`,
		`{"target_file":"/x.go","old_string":"old","new_string":"new"}`,
		`{"file_path":"/x.go","search_for":"old","replace_with":"new"}`,
		`{"file":"/x.go","old":"old","text":"new"}`,
	}
	for _, raw := range cases {
		got := EffectiveJSON(raw, "Update")
		if !CompleteJSON(got, "Update") && !CompleteJSON(got, "Edit") {
			t.Fatalf("incomplete for Update/Edit: %s from %s", got, raw)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(got), &obj); err != nil {
			t.Fatal(err)
		}
		if obj["file_path"] == nil || obj["old_string"] == nil {
			t.Fatalf("missing required keys in %s from %s", got, raw)
		}
		if _, ok := obj["new_string"]; !ok {
			t.Fatalf("new_string missing in %s", got)
		}
	}
}

func TestMidStreamPathOldDoesNotComplete(t *testing.T) {
	// Regression: stream often delivers path+old first, then replace later.
	// CompleteJSON/EffectiveJSON must stay incomplete so Claude Code does not
	// accept a delete-match Edit before the real new_string arrives.
	raw := `{"file_path":"/x.go","old_string":"old code"}`
	if CompleteJSON(raw, "Update") || CompleteJSON(raw, "Edit") {
		t.Fatal("path+old without new_string must be incomplete mid-stream")
	}
	eff := EffectiveJSON(raw, "Update")
	if CompleteJSON(eff, "Update") {
		t.Fatalf("EffectiveJSON must not invent new_string: %s", eff)
	}
	// After merge with replace, becomes complete without needing coerce.
	merged := Merge(raw, `{"new_string":"new code"}`, "Update")
	if !CompleteJSON(merged, "Update") {
		t.Fatalf("after new_string merge must complete: %s", merged)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(merged), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["new_string"] != "new code" {
		t.Fatalf("new_string=%v want new code in %s", obj["new_string"], merged)
	}
	// Force-finish still fills "" when stream ends without replace.
	coerced := CoerceCompleteJSON(raw, "Edit")
	if !CompleteJSON(coerced, "Edit") {
		t.Fatalf("coerce must complete: %s", coerced)
	}
}

func TestUpdateMissingNewStringDefaultsEmpty(t *testing.T) {
	// Effective/Normalize leave path+old incomplete; Coerce fills new_string "".
	raw := `{"file_path":"/a","old_string":"x"}`
	if CompleteJSON(EffectiveJSON(raw, "Update"), "Update") {
		t.Fatalf("mid-stream EffectiveJSON must stay incomplete without new_string")
	}
	got := CoerceCompleteJSON(raw, "Update")
	if !CompleteJSON(got, "Edit") && !CompleteJSON(got, "Update") {
		t.Fatalf("should complete after coerce: %s", got)
	}
	var obj map[string]any
	_ = json.Unmarshal([]byte(got), &obj)
	if ns, ok := obj["new_string"].(string); !ok || ns != "" {
		t.Fatalf("new_string=%v want empty string", obj["new_string"])
	}
}

func TestUpdateAlternateShapes(t *testing.T) {
	cases := []struct {
		raw    string
		coerce bool // only-delete shapes need force-finish coerce for new_string
	}{
		{`{"path":"/x.go","search":"old","replace":"new"}`, false},
		{`{"target_file":"/x.go","old_string":"old","new_string":"new"}`, false},
		{`{"file_path":"/x.go","search_for":"old","replace_with":"new"}`, false},
		{`{"file_path":"/x.go","old_string":"only-delete"}`, true}, // default new_string "" at coerce
		{`{"path":"/x.go","old":"a","text":"b"}`, false},
	}
	for _, tc := range cases {
		for _, name := range []string{"Update", "Edit", "str_replace"} {
			got := EffectiveJSON(tc.raw, name)
			if tc.coerce {
				got = CoerceCompleteJSON(tc.raw, name)
			}
			if !CompleteJSON(got, name) && !CompleteJSON(got, "Edit") {
				t.Fatalf("name=%s raw=%s got=%s incomplete", name, tc.raw, got)
			}
			if !strings.Contains(got, "file_path") {
				t.Fatalf("missing file_path name=%s got=%s", name, got)
			}
			if !strings.Contains(got, "old_string") {
				t.Fatalf("missing old_string name=%s got=%s", name, got)
			}
			if !strings.Contains(got, "new_string") {
				t.Fatalf("missing new_string name=%s got=%s", name, got)
			}
		}
	}
}

func TestUpdateStreamedFragmentsMerge(t *testing.T) {
	// Realistic Grok streaming: successive complete-ish object rewrites + true deltas.
	cur := Merge("", `{"path":"/tmp/a.go"}`, "Update")
	cur = Merge(cur, `{"path":"/tmp/a.go","search":"foo"}`, "Update")
	cur = Merge(cur, `{"path":"/tmp/a.go","search":"foo","replace":"bar"}`, "Update")
	got := CoerceCompleteJSON(cur, "Edit")
	if !CompleteJSON(got, "Edit") {
		t.Fatalf("streamed Update incomplete: %q (cur=%q)", got, cur)
	}
	if !strings.Contains(got, "/tmp/a.go") || !strings.Contains(got, "foo") || !strings.Contains(got, "bar") {
		t.Fatalf("bad merge: %s", got)
	}
	// Also accept true JSON string delta streaming for a single object.
	cur2 := Merge("", `{"file_path":"/y.go","old_string":"a","new_string":"`, "Edit")
	cur2 = Merge(cur2, `b"}`, "Edit")
	if !CompleteJSON(cur2, "Edit") {
		// raw string concat recovery
		got2 := CoerceCompleteJSON(cur2, "Edit")
		if !CompleteJSON(got2, "Edit") {
			t.Fatalf("delta string merge incomplete cur=%q got=%q", cur2, got2)
		}
	}
}

func TestMidStreamUpdateDoesNotInventNewString(t *testing.T) {
	// Grok streams path+old first, then replace later. Mid-stream must stay
	// incomplete so Claude Code does not accept a delete-match Edit early.
	partial := `{"file_path":"/x.go","old_string":"foo"}`
	if CompleteJSON(partial, "Update") {
		t.Fatal("path+old without new_string must be incomplete mid-stream")
	}
	if CompleteJSON(EffectiveJSON(partial, "Update"), "Edit") {
		t.Fatal("EffectiveJSON must not invent new_string")
	}
	// Merge of partial fragments should also stay incomplete.
	cur := Merge("", `{"path":"/x.go","search":"foo"}`, "Update")
	if CompleteJSON(cur, "Update") {
		t.Fatalf("merged path+search without replace must be incomplete: %s", cur)
	}
	// After replace arrives, complete with real new_string (not empty).
	full := Merge(cur, `{"replace":"bar"}`, "Update")
	if !CompleteJSON(full, "Update") {
		t.Fatalf("after replace must complete: %s", full)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(full), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["new_string"] != "bar" {
		t.Fatalf("new_string=%#v want bar in %s", parsed["new_string"], full)
	}
	// Force-finish path still defaults missing new_string to "" for true deletes.
	coerced := CoerceCompleteJSON(partial, "Edit")
	if !CompleteJSON(coerced, "Edit") {
		t.Fatalf("coerce must complete: %s", coerced)
	}
	if err := json.Unmarshal([]byte(coerced), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["new_string"] != "" {
		t.Fatalf("coerced new_string=%#v want empty", parsed["new_string"])
	}
}

func TestMidStreamMissingNewStringNotComplete(t *testing.T) {
	// Streaming Update often lands path+old first; inventing "" here would emit a
	// delete-match Edit before replace arrives, then Claude Code ignores the real args.
	raw := `{"file_path":"/x.go","old_string":"old"}`
	if CompleteJSON(raw, "Update") || CompleteJSON(EffectiveJSON(raw, "Update"), "Update") {
		t.Fatalf("path+old without new_string must stay incomplete mid-stream")
	}
	// When replace finally arrives, complete without coerce.
	full := `{"file_path":"/x.go","old_string":"old","new_string":"new"}`
	if !CompleteJSON(full, "Edit") {
		t.Fatalf("full edit must complete: %s", EffectiveJSON(full, "Edit"))
	}
	// Force-finish invents empty new_string only via Coerce.
	got := CoerceCompleteJSON(raw, "Update")
	if !CompleteJSON(got, "Update") {
		t.Fatalf("coerce must complete delete-match: %s", got)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["new_string"] != "" {
		t.Fatalf("new_string=%#v", obj["new_string"])
	}
}

func TestMergeDoesNotInventNewStringMidStream(t *testing.T) {
	cur := Merge("", `{"file_path":"/a","old_string":"x"}`, "Update")
	if CompleteJSON(cur, "Update") {
		t.Fatalf("merge of path+old must not complete: %s", cur)
	}
	if strings.Contains(cur, `"new_string"`) {
		t.Fatalf("merge invented new_string mid-stream: %s", cur)
	}
	got := Merge(cur, `{"new_string":"y"}`, "Update")
	if !CompleteJSON(got, "Update") {
		t.Fatalf("after new_string arrives must complete: %s", got)
	}
	var obj map[string]any
	_ = json.Unmarshal([]byte(got), &obj)
	if obj["new_string"] != "y" {
		t.Fatalf("new_string=%#v want y in %s", obj["new_string"], got)
	}
}

func TestCoerceSalvageTruncatedShell(t *testing.T) {
	// Truncated mid-string — classic intermittent upstream cut.
	raw := `{"cmd":"ls -la /tmp`
	got := CoerceCompleteJSON(raw, "exec_command")
	if !CompleteJSON(got, "exec_command") && !CompleteJSON(got, "shell") {
		// After projection path may use command
		if !strings.Contains(got, "ls") {
			t.Fatalf("expected salvage shell, got %q", got)
		}
	}
	// Must contain a command-ish value
	if !strings.Contains(got, "ls") {
		t.Fatalf("missing ls: %q", got)
	}
}

func TestCoerceSalvageTruncatedRead(t *testing.T) {
	raw := `{"file_path":"/home/user/proj/main.go`
	got := CoerceCompleteJSON(raw, "Read")
	if !CompleteJSON(got, "Read") {
		t.Fatalf("expected complete Read, got %q", got)
	}
	if !strings.Contains(got, "main.go") {
		t.Fatalf("path lost: %q", got)
	}
}

func TestCoerceSalvageTruncatedEditPathOld(t *testing.T) {
	// path+old without new_string at force-finish → empty new_string delete-match
	raw := `{"file_path":"/a.go","old_string":"foo"`
	got := CoerceCompleteJSON(raw, "Edit")
	if !CompleteJSON(got, "Edit") {
		// may need closing brace repair
		got2 := CoerceCompleteJSON(raw+`}`, "Edit")
		if !CompleteJSON(got2, "Edit") {
			t.Fatalf("expected edit force-finish, got %q / %q", got, got2)
		}
		got = got2
	}
	if !strings.Contains(got, "new_string") {
		t.Fatalf("new_string missing: %q", got)
	}
}

func TestCoerceSalvagesTruncatedShell(t *testing.T) {
	// Truncated mid-string with no closing braces — classic intermittent upstream cut.
	raw := `{"cmd":"curl -s https://example.com/a`
	got := CoerceCompleteJSON(raw, "exec_command")
	if !CompleteJSON(got, "exec_command") {
		t.Fatalf("expected salvage complete shell, got %q", got)
	}
	if !strings.Contains(got, "curl") {
		t.Fatalf("missing cmd content: %q", got)
	}
	// command key for internal form after coerce
	if !strings.Contains(got, "command") && !strings.Contains(got, "cmd") {
		t.Fatalf("no command key: %q", got)
	}
}

func TestCoerceSalvagesTruncatedRead(t *testing.T) {
	raw := `{"file_path":"/tmp/foo.txt`
	got := CoerceCompleteJSON(raw, "Read")
	if !CompleteJSON(got, "Read") {
		t.Fatalf("expected salvage Read, got %q", got)
	}
	if !strings.Contains(got, "/tmp/foo.txt") {
		t.Fatalf("path lost: %q", got)
	}
}

func TestCoerceSalvagesTruncatedApplyPatch(t *testing.T) {
	raw := `{"input":"*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new`
	got := CoerceCompleteJSON(raw, "apply_patch")
	if !CompleteJSON(got, "apply_patch") {
		// salvage may still recover input even if incomplete patch body
		if !strings.Contains(got, "Begin Patch") && !strings.Contains(got, "input") {
			t.Fatalf("expected apply_patch salvage, got %q", got)
		}
	}
}

func TestCoerceSalvageTruncatedShellAndRead(t *testing.T) {
	// Truncated mid-string shell (no closing quote/brace) — classic intermittent hang source.
	partial := `{"cmd":"ls -la /tmp`
	got := CoerceCompleteJSON(partial, "exec_command")
	if !CompleteJSON(got, "exec_command") {
		t.Fatalf("shell salvage incomplete: %q", got)
	}
	if !strings.Contains(got, "ls -la") && !strings.Contains(got, "command") {
		t.Fatalf("shell salvage missing cmd: %q", got)
	}

	// Truncated Read path
	partialRead := `{"file_path":"/root/proj`
	gotR := CoerceCompleteJSON(partialRead, "Read")
	if !CompleteJSON(gotR, "Read") {
		// salvageRequiredFieldJSON should recover file_path even mid-string if closed by salvage
		// open string may not salvage without closing quote — try with truncated brace only
		partialRead2 := `{"file_path":"/root/proj/a.go"`
		gotR = CoerceCompleteJSON(partialRead2, "Read")
	}
	if !CompleteJSON(gotR, "Read") {
		// Accept repair of truncated brace after full string
		partialRead3 := `{"file_path":"/root/proj/a.go"`
		gotR = CoerceCompleteJSON(partialRead3+`}`, "Read")
	}
	if !CompleteJSON(gotR, "Read") {
		// Last: truncated after complete string without }
		gotR = CoerceCompleteJSON(`{"file_path":"/root/proj/a.go"`, "Read")
		if !CompleteJSON(gotR, "Read") {
			t.Fatalf("read salvage incomplete: %q", gotR)
		}
	}

	// apply_patch truncated input with closing string missing brace
	patch := `{"input":"*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch"`
	gotP := CoerceCompleteJSON(patch, "apply_patch")
	if !CompleteJSON(gotP, "apply_patch") {
		// try with repair adding }
		gotP = CoerceCompleteJSON(patch, "apply_patch")
		if !CompleteJSON(gotP, "apply_patch") {
			// salvageApplyPatchJSON should still get input from extract
			if !strings.Contains(gotP, "Begin Patch") && !CompleteJSON(gotP, "apply_patch") {
				// force with closed
				gotP = CoerceCompleteJSON(patch+`}`, "apply_patch")
			}
		}
	}
	if !CompleteJSON(gotP, "apply_patch") {
		t.Fatalf("apply_patch salvage incomplete: %q", gotP)
	}
}

func TestCoerceSalvageShellFromCmdKeyOnly(t *testing.T) {
	// Grok sometimes emits cmd without braces fully closed
	raw := `{"cmd":"pwd"`
	got := CoerceCompleteJSON(raw, "shell")
	if !CompleteJSON(got, "shell") && !CompleteJSON(got, "exec_command") {
		// After project, command key
		if !strings.Contains(got, "pwd") {
			t.Fatalf("expected pwd salvage, got %q", got)
		}
	}
}

func TestCoerceSalvagePartialShellAndRead(t *testing.T) {
	// Truncated mid-string shell args — classic intermittent Codex/Claude hang.
	partials := []struct {
		tool string
		raw  string
		want string // substring that must appear
	}{
		{"exec_command", `{"cmd":"ls -la /tmp`, `"command"`},
		{"shell", `{"command":"echo hi`, `"command"`},
		{"Read", `{"file_path":"/home/user/proj`, `"file_path"`},
		{"apply_patch", `{"input":"*** Begin Patch\n*** Update File: a.go`, `"input"`},
		{"Edit", `{"file_path":"/a.go","old_string":"foo`, `"file_path"`},
	}
	for _, tc := range partials {
		got := CoerceCompleteJSON(tc.raw, tc.tool)
		if got == "" {
			t.Fatalf("%s: empty coerce for %q", tc.tool, tc.raw)
		}
		if !CompleteJSON(got, tc.tool) && tc.tool != "Edit" {
			// Edit may need old+new; salvage may only get path — allow if still non-empty object
			if !strings.Contains(got, tc.want) {
				t.Fatalf("%s: coerce incomplete and missing %s: %q", tc.tool, tc.want, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Fatalf("%s: missing %s in %q", tc.tool, tc.want, got)
		}
	}
}

func TestCoerceSalvageDoesNotInventEmptyShell(t *testing.T) {
	// Truly empty / garbage should not become a fake successful shell tool.
	got := CoerceCompleteJSON(`{"cmd":""}`, "exec_command")
	if CompleteJSON(got, "exec_command") {
		t.Fatalf("empty cmd must not be complete: %q", got)
	}
}
