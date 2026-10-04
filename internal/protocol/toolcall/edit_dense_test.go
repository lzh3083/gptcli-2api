package toolcall

import (
	"encoding/json"
	"testing"
)

func TestEditDensifyStripsUnknownKeys(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]any
	}{
		{
			name: "strip explanation",
			raw:  `{"path":"/x","search":"a","replace":"b","explanation":"why"}`,
			want: map[string]any{"file_path": "/x", "old_string": "a", "new_string": "b"},
		},
		{
			name: "prefer new_string over content alias",
			raw:  `{"file_path":"/x","old_string":"a","new_string":"b","content":"leak","query":"q"}`,
			want: map[string]any{"file_path": "/x", "old_string": "a", "new_string": "b"},
		},
		{
			name: "strip mode keep mapped before/after",
			raw:  `{"target_file":"/x","before":"a","after":"b","mode":"strict"}`,
			want: map[string]any{"file_path": "/x", "old_string": "a", "new_string": "b"},
		},
		{
			name: "keep replace_all",
			raw:  `{"file_path":"/x","old_string":"a","new_string":"b","replace_all":true}`,
			want: map[string]any{"file_path": "/x", "old_string": "a", "new_string": "b", "replace_all": true},
		},
		{
			name: "stringify numbers",
			raw:  `{"file_path":"/x","old_string":1,"new_string":2}`,
			want: map[string]any{"file_path": "/x", "old_string": "1", "new_string": "2"},
		},
		{
			name: "unwrap nested path object",
			raw:  `{"file_path":{"path":"/x"},"old_string":"a","new_string":"b"}`,
			want: map[string]any{"file_path": "/x", "old_string": "a", "new_string": "b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CoerceCompleteJSON(tc.raw, "Update")
			if !CompleteJSON(got, "Edit") {
				t.Fatalf("incomplete: %s", got)
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(got), &obj); err != nil {
				t.Fatal(err)
			}
			if len(obj) != len(tc.want) {
				t.Fatalf("keys mismatch got=%v want=%v raw=%s", obj, tc.want, got)
			}
			for k, wantV := range tc.want {
				gv, ok := obj[k]
				if !ok {
					t.Fatalf("missing key %s in %s", k, got)
				}
				switch w := wantV.(type) {
				case bool:
					if gb, ok := gv.(bool); !ok || gb != w {
						t.Fatalf("%s=%v want %v in %s", k, gv, w, got)
					}
				case string:
					if gs, ok := gv.(string); !ok || gs != w {
						t.Fatalf("%s=%v want %v in %s", k, gv, w, got)
					}
				default:
					t.Fatalf("unexpected want type for %s", k)
				}
			}
		})
	}
}

func TestEditDensifyMidStreamStillIncompleteWithoutNew(t *testing.T) {
	// densify must not invent new_string mid-stream.
	got := EffectiveJSON(`{"path":"/x","search":"a","explanation":"x"}`, "Update")
	if CompleteJSON(got, "Update") {
		t.Fatalf("mid-stream search without replace must be incomplete: %s", got)
	}
	var obj map[string]any
	_ = json.Unmarshal([]byte(got), &obj)
	if _, ok := obj["explanation"]; ok {
		t.Fatalf("explanation leaked: %s", got)
	}
	if obj["old_string"] != "a" {
		t.Fatalf("old_string not mapped: %s", got)
	}
}
