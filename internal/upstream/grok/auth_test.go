package grok

import "testing"

func TestAccountFromCredentials(t *testing.T) {
	acct := AccountFromCredentials(Credentials{Token: " tok ", UserID: "user", Email: "e@example.com"})
	if acct.ID != "user" || acct.Token != "tok" {
		t.Fatalf("unexpected account %#v", acct)
	}
}

func TestHeadersForCredentials(t *testing.T) {
	headers := HeadersForCredentials(Credentials{Token: "tok"}, "grok-4.5", Client{})
	if headers["Authorization"] != "Bearer tok" {
		t.Fatalf("unexpected authorization %q", headers["Authorization"])
	}
	if headers["X-XAI-Token-Auth"] != "xai-grok-cli" {
		t.Fatalf("missing token auth %#v", headers)
	}
	// CPA cache path does not force model override by default.
	if _, ok := headers["x-grok-model-override"]; ok {
		t.Fatalf("model override should be optional %#v", headers)
	}
	if headers["x-grok-client-identifier"] != "grok-shell" {
		t.Fatalf("unexpected identifier %#v", headers)
	}
}
