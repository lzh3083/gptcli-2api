package toolcall

import (
	"encoding/json"
	"testing"
)

// Mid-stream path+old without new_string must stay incomplete so Claude Code
// does not accept a premature delete-match Edit before Grok streams replace.
// Force-finish (CoerceCompleteJSON) fills new_string="" for true omit cases.
func TestMidStreamVsForceFinishSemantics(t *testing.T) {
	pathOld := `{"file_path":"/x","old_string":"a"}`
	if CompleteJSON(pathOld, "Update") {
		t.Fatalf("mid-stream path+old without new_string must be incomplete, got complete Effective=%s", EffectiveJSON(pathOld, "Update"))
	}
	if CompleteJSON(pathOld, "Edit") {
		t.Fatalf("mid-stream Edit path+old without new_string must be incomplete")
	}
	coerced := CoerceCompleteJSON(pathOld, "Update")
	if !CompleteJSON(coerced, "Update") {
		t.Fatalf("coerce should complete: %s", coerced)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(coerced), &obj); err != nil {
		t.Fatal(err)
	}
	if v, ok := obj["new_string"]; !ok || v != "" {
		t.Fatalf("new_string=%#v want empty string in %s", v, coerced)
	}
	if !CompleteJSON(`{"file_path":"/x","old_string":"a","new_string":""}`, "Edit") {
		t.Fatal("explicit empty new_string must be complete")
	}
	if !CompleteJSON(`{"path":"/x","search":"a","replace":"b"}`, "Update") {
		t.Fatal("search/replace should complete mid-stream")
	}
}
