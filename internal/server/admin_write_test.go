package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hm2899/grokcli-2api/internal/config"
)

func TestAdminWriteRoutesGated(t *testing.T) {
	for _, path := range []string{"/admin/api/login", "/admin/api/setup", "/admin/api/keys", "/admin/api/accounts/x/kick"} {
		rec := httptest.NewRecorder()
		NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s disabled = %d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestAdminLoginEnvBootstrapWithoutStoreHash(t *testing.T) {
	// Without store, login should still fail closed on store unavailable.
	rec := httptest.NewRecorder()
	NewMux(Options{
		Ready:             func() bool { return true },
		AdminReadEnabled:  true,
		AdminWriteEnabled: true,
		Config:            config.Config{LegacyAdminPassword: "bootstrap-pass"},
	}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{"password":"bootstrap-pass"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("login without store = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminSessionUnauthorized(t *testing.T) {
	rec := httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }, AdminReadEnabled: true, AdminSessions: fakeAdminSessions{ok: false}}).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/session", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("session unauthorized = %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["authenticated"] != false {
		t.Fatalf("body %#v", body)
	}
}

func TestAdminSettingsWriteGated(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/settings", strings.NewReader(`{"outbound_max_tools":1}`))
	NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("settings patch disabled = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRegistrationFacadeGated(t *testing.T) {
	rec := httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/register-email/availability", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("registration facade disabled = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/accounts/register-email/availability", nil)
	req.Header.Set("X-Admin-Token", "token")
	NewMux(Options{Ready: func() bool { return true }, AdminReadEnabled: true, AdminSessions: fakeAdminSessions{ok: true}}).
		ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("registration without service url = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAccountImportExportDeleteGated(t *testing.T) {
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/admin/api/accounts/import"},
		{http.MethodGet, "/admin/api/accounts/export"},
		{http.MethodPost, "/admin/api/accounts/delete-batch"},
		{http.MethodDelete, "/admin/api/accounts/acc-1"},
		{http.MethodPost, "/admin/api/accounts/import-sso"},
		{http.MethodGet, "/admin/api/accounts/register-email/export-sso"},
		{http.MethodPost, "/admin/api/accounts/register-email/export-sso"},
		{http.MethodGet, "/admin/api/accounts/export-sso"},
		{http.MethodPost, "/admin/api/accounts/export-sso"},
	} {
		rec := httptest.NewRecorder()
		NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s disabled = %d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestRegisterEmailExportSSORouteExists(t *testing.T) {
	// Regression: Go migration dropped /register-email/export-sso → admin 404.
	// With admin session but no store/reg service, must NOT be a mux-level 404.
	mux := NewMux(Options{
		Ready:             func() bool { return true },
		AdminReadEnabled:  true,
		AdminWriteEnabled: true,
		AdminSessions:     fakeAdminSessions{ok: true},
	})
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/admin/api/accounts/register-email/export-sso?download=0", ""},
		{http.MethodPost, "/admin/api/accounts/register-email/export-sso", `{"format":"sso","download":false}`},
	} {
		rec := httptest.NewRecorder()
		var req *http.Request
		if tc.body != "" {
			req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(tc.method, tc.path, nil)
		}
		req.Header.Set("X-Admin-Token", "token")
		mux.ServeHTTP(rec, req)
		// Missing routes produce plain 404; our handler returns JSON 200 empty-export.
		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s %s hit missing route (404): body=%s", tc.method, tc.path, rec.Body.String())
		}
		if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s unexpected status=%d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if rec.Code == http.StatusOK {
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s %s body not JSON: %q", tc.method, tc.path, rec.Body.String())
			}
			// Empty store/reg: ok=false, count=0, detail explains no SSO matched.
			if body["ok"] != false {
				t.Fatalf("%s %s empty export want ok=false got %v body=%s", tc.method, tc.path, body["ok"], rec.Body.String())
			}
			detail, _ := body["detail"].(string)
			if !strings.Contains(detail, "SSO") && !strings.Contains(detail, "sso") {
				t.Fatalf("%s %s unexpected empty detail=%q", tc.method, tc.path, detail)
			}
		}
	}
}

func TestBuildSSOExportFormats(t *testing.T) {
	authMap := map[string]any{
		"auth": map[string]any{
			"a1": map[string]any{"email": "u@x.com", "sso": "cookie1", "password": "p1"},
			"a2": map[string]any{"email": "v@x.com", "sso_cookie": "cookie2"},
			"a3": map[string]any{"email": "nope@x.com"}, // no sso
		},
	}
	out := buildSSOExport(authMap, false)
	if out["count"] != 2 {
		t.Fatalf("count=%v want 2", out["count"])
	}
	content, _ := out["content"].(string)
	if !strings.Contains(content, "cookie1") || !strings.Contains(content, "cookie2") {
		t.Fatalf("content missing sso: %q", content)
	}
	if strings.Contains(content, "p1") {
		t.Fatalf("password leaked when includePassword=false: %q", content)
	}
	outPW := buildSSOExport(authMap, true)
	contentPW, _ := outPW["content"].(string)
	if !strings.Contains(contentPW, "p1") {
		t.Fatalf("password missing when includePassword=true: %q", contentPW)
	}
}
