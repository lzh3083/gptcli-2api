package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hm2899/grokcli-2api/internal/pool"
	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

func TestWriteAnthropicProxyErrorUnwrapsUpstreamQuota(t *testing.T) {
	rec := httptest.NewRecorder()
	err := &grok.UpstreamError{
		Status: http.StatusTooManyRequests,
		Body:   `{"code":"subscription:free-usage-exhausted","error":"You've used all free usage for model grok-4.5"}`,
	}
	writeAnthropicProxyError(rec, err)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429 body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &payload) != nil {
		t.Fatalf("invalid json: %s", rec.Body.String())
	}
	if payload["type"] != "error" {
		t.Fatalf("payload=%#v", payload)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj["type"] != "rate_limit_error" {
		t.Fatalf("error type=%#v", errObj)
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "free usage") {
		t.Fatalf("message should unwrap body, got %q", msg)
	}
	if strings.Contains(msg, "upstream status") {
		t.Fatalf("message still wrapped: %q", msg)
	}
}

func TestWriteAnthropicProxyErrorNoEligibleAccounts(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAnthropicProxyError(rec, pool.ErrNoEligibleAccounts)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "No eligible accounts") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestAnthropicErrorFromCauseEmptyModel(t *testing.T) {
	msg, typ := anthropicErrorFromCause(errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)"))
	if typ != "api_error" {
		t.Fatalf("type=%q", typ)
	}
	if !strings.Contains(msg, "empty model output") {
		t.Fatalf("msg=%q", msg)
	}
}

func TestUnwrapAnthropicUpstreamMessage(t *testing.T) {
	status, body, ok := unwrapAnthropicUpstreamMessage(`upstream status 429: {"error":"quota used up"}`)
	if !ok || status != 429 || body != "quota used up" {
		t.Fatalf("status=%d body=%q ok=%v", status, body, ok)
	}
	status, body, ok = unwrapAnthropicUpstreamMessage(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)
	if !ok || body != "slow down" {
		t.Fatalf("status=%d body=%q ok=%v", status, body, ok)
	}
}
