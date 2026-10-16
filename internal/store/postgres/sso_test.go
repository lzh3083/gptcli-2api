package postgres

import (
	"testing"

	"github.com/hm2899/grokcli-2api/internal/accounts"
)

func TestHasSSONestedAndCookieHeader(t *testing.T) {
	cases := []map[string]any{
		{"sso": "abc123"},
		{"sso_cookie": "sso=xyz"},
		{"session_cookies": map[string]any{"sso": "nested"}},
		{"cookies": map[string]any{"sso-rw": "rwval"}},
		{"cookie": "a=1; sso=fromheader; b=2"},
		{"set_cookie": "sso=setcookieval; Path=/"},
	}
	for i, c := range cases {
		if !hasSSO(c) {
			t.Fatalf("case %d should have sso: %#v get=%q", i, c, accounts.GetSSOValue(c))
		}
	}
	if hasSSO(map[string]any{"email": "a@b.c"}) {
		t.Fatal("no sso should be false")
	}
}

func TestBuildAccountListWhereHasSSOUsesNestedPaths(t *testing.T) {
	trueVal := true
	where, _ := buildAccountListWhere("", "", &trueVal)
	if where == "" || !containsFold(where, "session_cookies") || !containsFold(where, "sso=%") {
		t.Fatalf("where missing nested sso paths: %s", where)
	}
}

func TestCanonicalizeSSOFieldsFromNested(t *testing.T) {
	payload := map[string]any{
		"email":           "a@x.ai",
		"session_cookies": map[string]any{"sso": "nested-cookie-val"},
	}
	canonicalizeSSOFields(payload)
	if payload["sso"] != "nested-cookie-val" {
		t.Fatalf("sso not lifted: %#v", payload["sso"])
	}
	if payload["sso_cookie"] != "nested-cookie-val" {
		t.Fatalf("sso_cookie missing: %#v", payload["sso_cookie"])
	}
	sc, _ := payload["session_cookies"].(map[string]any)
	if sc["sso"] != "nested-cookie-val" || sc["sso-rw"] != "nested-cookie-val" {
		t.Fatalf("session_cookies incomplete: %#v", sc)
	}
}

func TestMergeDurableLocalCanonicalizesSSO(t *testing.T) {
	// New entry only has nested SSO; merge must write top-level for export-all.
	entry := map[string]any{
		"key":     "tok",
		"cookies": map[string]any{"sso-rw": "from-cookies"},
	}
	out := mergeDurableLocal(entry, nil)
	if accounts.GetSSOValue(out) != "from-cookies" {
		t.Fatalf("get sso=%q", accounts.GetSSOValue(out))
	}
	if out["sso"] != "from-cookies" {
		t.Fatalf("top-level sso missing: %#v", out)
	}
	// Old nested SSO preserved when new has none.
	newEntry := map[string]any{"key": "tok2", "email": "b@x.ai"}
	old := map[string]any{"session_cookies": map[string]any{"sso": "old-nested"}}
	out2 := mergeDurableLocal(newEntry, old)
	if out2["sso"] != "old-nested" {
		t.Fatalf("old sso not merged: %#v", out2)
	}
}

func TestNormalizeExportPayloadLiftsSSO(t *testing.T) {
	email := "c@x.ai"
	p := normalizeExportPayload(map[string]any{
		"cookie": "a=1; sso=hdrval; b=2",
	}, "acc-1", &email)
	if p["sso"] != "hdrval" {
		t.Fatalf("export normalize sso=%#v", p["sso"])
	}
	if p["email"] != "c@x.ai" {
		t.Fatalf("email=%#v", p["email"])
	}
}
