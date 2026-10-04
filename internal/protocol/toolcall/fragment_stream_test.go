package toolcall

import (
	"strings"
	"testing"
)

// Claude Code regression: upstream frequently chunks tool arguments at
// arbitrary byte offsets. Merge must accumulate raw fragments byte-for-byte
// and must NOT treat an unterminated buffer as complete (EffectiveJSON would
// "repair" it, the old Merge then kept it and discarded the live remainder).
func TestMergeCharLevelFragmentStream(t *testing.T) {
	full := `{"file_path":"/src/main.go","old_string":"foo()","new_string":"bar()"}`
	// Split at every position; each split must merge back to the full payload.
	for cut := 1; cut < len(full); cut++ {
		cur := Merge("", full[:cut], "Update")
		cur = Merge(cur, full[cut:], "Update")
		if !CompleteJSON(cur, "Update") {
			t.Fatalf("cut=%d merged=%q not complete", cut, cur)
		}
		if !strings.Contains(cur, "/src/main.go") || !strings.Contains(cur, "bar()") {
			t.Fatalf("cut=%d lost content: %q", cut, cur)
		}
	}
}

// Streaming one character at a time (worst-case upstream chunking).
func TestMergeByteAtATime(t *testing.T) {
	full := `{"command":"echo hello && pwd"}`
	cur := ""
	for i := 0; i < len(full); i++ {
		cur = Merge(cur, full[i:i+1], "Bash")
	}
	if !CompleteJSON(cur, "Bash") {
		t.Fatalf("byte-at-a-time merge incomplete: %q", cur)
	}
	// json.Marshal escapes '&' as &; accept either form.
	if !strings.Contains(cur, "echo hello") || !(strings.Contains(cur, "pwd") && (strings.Contains(cur, "&&") || strings.Contains(cur, `&&`))) {
		t.Fatalf("content lost: %q", cur)
	}
}

// Mid-stream buffer must not be considered complete by merge decisions even
// though EffectiveJSON can repair it into a valid object.
func TestMergeDoesNotKeepTruncatedBufferAsComplete(t *testing.T) {
	cur := Merge("", `{"file_path":"/a.g`, "Read")
	cur = Merge(cur, `o"}`, "Read")
	if cur != `{"file_path":"/a.go"}` {
		t.Fatalf("truncated buffer swallowed live remainder: %q", cur)
	}
}

// A genuinely complete later object still wins over an incomplete early one.
func TestMergeCompleteRewriteStillWinsOverFragment(t *testing.T) {
	cur := Merge("", `{"file_path":`, "Update")
	cur = Merge(cur, `{"file_path":"/right","old_string":"a","new_string":"b"}`, "Update")
	var contains = strings.Contains(cur, "/right") && strings.Contains(cur, "old_string")
	if !contains || !CompleteJSON(cur, "Update") {
		t.Fatalf("complete rewrite lost: %q", cur)
	}
}
