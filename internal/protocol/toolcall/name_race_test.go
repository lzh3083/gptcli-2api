package toolcall

import (
	"encoding/json"
	"strings"
	"testing"
)

// Grok often streams tool args before the tool name chunk. If name is empty,
// global alias search→query would poison Update args (search should be old_string).
func TestMergeArgsBeforeNameStillMapsSearch(t *testing.T) {
	// Simulate: arguments first with empty name, then name becomes Update.
	cur := Merge("", `{"path":"/x","search":"foo","replace":"bar"}`, "")
	// After name known, re-normalize under Update (stream path does Merge with name).
	// Real stream: first delta may have name+partial args, or name-only then args.
	// Critical: when name is Update from the start of args merge:
	cur2 := Merge("", `{"path":"/x","search":"foo","replace":"bar"}`, "Update")
	if !CompleteJSON(cur2, "Update") {
		t.Fatalf("with Update name: incomplete %s", cur2)
	}
	if strings.Contains(cur2, `"query"`) && !strings.Contains(cur2, "old_string") {
		t.Fatalf("search poisoned to query: %s", cur2)
	}

	// Name arrives late: first merge without name, then with Update name.
	// Current Merge when piece is complete and cur incomplete returns piece —
	// but piece was normalized under empty name.
	late := Merge(cur, `{}`, "Update") // no new args, only name context change won't re-run
	// Actually stream always passes name once known on subsequent Merges.
	// If first merge was name-less and produced complete-looking object with query,
	// late name won't fix it unless we re-normalize.
	t.Logf("nameless merge=%s late=%s", cur, late)

	// Realistic streamer path: name set first (even empty args), then args with name.
	cur3 := ""
	// name-only doesn't go through Merge; stream sets name then merges args with name.
	cur3 = Merge(cur3, `{"path":"/x","search":"foo"`, "Update")
	cur3 = Merge(cur3, `,"replace":"bar"}`, "Update")
	if !CompleteJSON(cur3, "Update") {
		// try coerce
		c := CoerceCompleteJSON(cur3, "Update")
		if !CompleteJSON(c, "Update") {
			t.Fatalf("fragment merge failed: %s / %s", cur3, c)
		}
		cur3 = c
	}
	var obj map[string]any
	_ = json.Unmarshal([]byte(EffectiveJSON(cur3, "Update")), &obj)
	if obj["old_string"] != "foo" || obj["new_string"] != "bar" {
		t.Fatalf("obj=%v from %s", obj, cur3)
	}
}

func TestNamelessSearchNotPoisonedWhenNameLater(t *testing.T) {
	// Worst case: args complete before name is known.
	nameless := Merge("", `{"path":"/x","search":"a","replace":"b"}`, "")
	// Streamer then sets name=Update and may Merge again with empty delta,
	// or emitReady uses EffectiveJSON(state.arguments, state.name).
	// state.arguments may still be the nameless-normalized form.
	got := EffectiveJSON(nameless, "Update")
	t.Logf("nameless=%s under Update Effective=%s Complete=%v", nameless, got, CompleteJSON(got, "Update"))
	// Coerce path
	coerced := CoerceCompleteJSON(nameless, "Update")
	t.Logf("coerce=%s complete=%v", coerced, CompleteJSON(coerced, "Update"))
	var obj map[string]any
	_ = json.Unmarshal([]byte(coerced), &obj)
	// Ideal: old_string/new_string recovered even if intermediate had query.
	if obj["file_path"] != "/x" {
		// path might also be wrong
		t.Logf("obj=%#v", obj)
	}
	if _, hasOld := obj["old_string"]; !hasOld {
		// This is the bug if search became query permanently
		if q, ok := obj["query"]; ok {
			t.Fatalf("search stuck as query=%v; old_string missing in %s", q, coerced)
		}
		t.Fatalf("old_string missing: %s", coerced)
	}
}
