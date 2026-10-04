package toolcall

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProbeUpdateStreamingMergeFragments(t *testing.T) {
	cases := []struct {
		name              string
		steps             []string
		wantCompleteAtEnd bool
		wantContains      []string
	}{
		{"search replace full", []string{`{"path":"/a.go","search":"foo","replace":"bar"}`}, true, []string{"file_path", "old_string", "new_string", "foo", "bar"}},
		{"fragmented path then search then replace", []string{
			`{"path":"/a.go"`,
			`,"search":"foo"`,
			`,"replace":"bar"}`,
		}, true, []string{"file_path", "old_string", "new_string"}},
		{"path old only then new", []string{
			`{"file_path":"/x","old_string":"a"}`,
			`{"new_string":"b"}`,
		}, true, []string{`"b"`}},
		{"path old only mid", []string{
			`{"file_path":"/x","old_string":"a"}`,
		}, false, nil},
		{"update content as new", []string{`{"path":"/x","old_string":"a","content":"b"}`}, true, []string{"new_string", "b"}},
		{"before after", []string{`{"target":"/x","before":"a","after":"b"}`}, true, []string{"file_path", "old_string", "new_string"}},
		{"old_code new_code", []string{`{"path":"/x","old_code":"aa","new_code":"bb"}`}, true, []string{"old_string", "new_string", "aa", "bb"}},
		{"trailing junk", []string{`{"file_path":"/x","old_string":"a","new_string":"b"} trailing`}, true, []string{"file_path", "new_string"}},
		{"truncated brace", []string{`{"file_path":"/x","old_string":"a","new_string":"b"`}, true, []string{"file_path"}},
		{"flip path", []string{
			`{"path":"/wrong","old_string":"a","new_string":"b"}`,
			`{"file_path":"/right","old_string":"a","new_string":"c"}`,
		}, true, []string{"/right", "c"}},
		{"doubled json", []string{`{"file_path":"/x","old_string":"a","new_string":"b"}{"extra":1}`}, true, []string{"file_path", "new_string"}},
		{"search only no replace mid", []string{`{"path":"/x","search":"only"}`}, false, nil},
		{"empty new_string explicit", []string{`{"file_path":"/x","old_string":"a","new_string":""}`}, true, []string{"new_string"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := ""
			for i, step := range tc.steps {
				cur = Merge(cur, step, "Update")
				if i < len(tc.steps)-1 {
					if CompleteJSON(cur, "Update") {
						eff := EffectiveJSON(cur, "Update")
						var obj map[string]any
						_ = json.Unmarshal([]byte(eff), &obj)
						if _, ok := obj["new_string"]; !ok {
							t.Fatalf("step %d completed without new_string: %s", i, cur)
						}
					}
				}
			}
			liveOK := CompleteJSON(cur, "Update")
			coerced := CoerceCompleteJSON(cur, "Update")
			coerceOK := CompleteJSON(coerced, "Update")
			if tc.wantCompleteAtEnd {
				if !liveOK && !coerceOK {
					t.Fatalf("neither live nor coerce complete: cur=%s coerced=%s", cur, coerced)
				}
				use := cur
				if !liveOK {
					use = coerced
				} else {
					use = EffectiveJSON(cur, "Update")
				}
				for _, w := range tc.wantContains {
					if !strings.Contains(use, w) {
						t.Fatalf("missing %q in %s (liveOK=%v)", w, use, liveOK)
					}
				}
			} else {
				if liveOK {
					t.Fatalf("expected incomplete mid-stream, got complete: %s / %s", cur, EffectiveJSON(cur, "Update"))
				}
				if !coerceOK {
					t.Fatalf("coerce should complete path+old: %s", coerced)
				}
			}
		})
	}
}

func TestProbeCanonicalUpdateToEdit(t *testing.T) {
	got := CanonicalName("Update", []string{"Bash", "Read", "Edit"})
	if got != "Edit" {
		t.Fatalf("CanonicalName Update->Edit got %q", got)
	}
	got = CanonicalName("StrReplace", []string{"Edit"})
	if got != "Edit" {
		t.Fatalf("got %q", got)
	}
}

func TestProbeNonStreamForceFinish(t *testing.T) {
	raw := `{"path":"/x","search":"delete me"}`
	got := CoerceCompleteJSON(raw, "Update")
	if !CompleteJSON(got, "Update") {
		t.Fatalf("not complete: %s", got)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["file_path"] != "/x" {
		t.Fatalf("path: %v", obj)
	}
	if obj["old_string"] != "delete me" {
		t.Fatalf("old: %v", obj)
	}
	if v, ok := obj["new_string"]; !ok || v != "" {
		t.Fatalf("new_string want empty got %#v", v)
	}
}
