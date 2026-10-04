package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// upstreamStatusCache keeps the last probe result so the models-page poller and
// /admin/api/status can share a fresh snapshot without hammering cli-chat-proxy.
var (
	upstreamStatusMu    sync.Mutex
	upstreamStatusCache map[string]any
	upstreamStatusAt    time.Time
	upstreamStatusInFly bool
)

const (
	upstreamStatusCacheTTL   = 5 * time.Second
	upstreamStatusStaleTTL   = 30 * time.Second
	upstreamProbeHTTPTimeout = 8 * time.Second
	upstreamProbeDialTimeout = 3 * time.Second
)

// serveUpstreamStatus returns a live (or short-cached) probe of the configured
// UpstreamBase. Used by the models admin page real-time monitor.
//
// GET /admin/api/upstream-status?force=1  — bypass cache
func serveUpstreamStatus(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	result := probeUpstreamStatus(r.Context(), options, force)
	writeJSON(w, http.StatusOK, result)
}

// cachedUpstreamStatus returns the last probe if still within stale TTL, else nil.
// Safe for embedding into /admin/api/status (never blocks on a live probe).
func cachedUpstreamStatus() map[string]any {
	upstreamStatusMu.Lock()
	defer upstreamStatusMu.Unlock()
	if upstreamStatusCache == nil {
		return nil
	}
	if time.Since(upstreamStatusAt) > upstreamStatusStaleTTL {
		return nil
	}
	out := cloneStringAnyMap(upstreamStatusCache)
	out["cached"] = true
	out["cache_age_ms"] = time.Since(upstreamStatusAt).Milliseconds()
	return out
}

func probeUpstreamStatus(ctx context.Context, options Options, force bool) map[string]any {
	// Fast path: fresh cache.
	upstreamStatusMu.Lock()
	if !force && upstreamStatusCache != nil && time.Since(upstreamStatusAt) < upstreamStatusCacheTTL {
		out := cloneStringAnyMap(upstreamStatusCache)
		out["cached"] = true
		out["cache_age_ms"] = time.Since(upstreamStatusAt).Milliseconds()
		upstreamStatusMu.Unlock()
		return out
	}
	// Avoid stampedes: if a probe is already running, return stale cache or a "probing" stub.
	if upstreamStatusInFly {
		if upstreamStatusCache != nil {
			out := cloneStringAnyMap(upstreamStatusCache)
			out["cached"] = true
			out["probing"] = true
			out["cache_age_ms"] = time.Since(upstreamStatusAt).Milliseconds()
			upstreamStatusMu.Unlock()
			return out
		}
		upstreamStatusMu.Unlock()
		return map[string]any{
			"ok": false, "reachable": false, "probing": true,
			"base_url":   strings.TrimSpace(options.Config.UpstreamBase),
			"checked_at": time.Now().Unix(),
			"error":      "probe in flight",
		}
	}
	upstreamStatusInFly = true
	upstreamStatusMu.Unlock()

	// Always clear in-flight, even if probe panics or the request is canceled.
	// A stuck true here made /upstream-status return "probe in flight" forever
	// and the models-page monitor look unusable.
	defer func() {
		upstreamStatusMu.Lock()
		upstreamStatusInFly = false
		upstreamStatusMu.Unlock()
	}()

	result := doUpstreamProbe(ctx, options)

	upstreamStatusMu.Lock()
	upstreamStatusCache = cloneStringAnyMap(result)
	upstreamStatusAt = time.Now()
	upstreamStatusMu.Unlock()

	result["cached"] = false
	result["cache_age_ms"] = int64(0)
	return result
}

func doUpstreamProbe(ctx context.Context, options Options) map[string]any {
	base := strings.TrimRight(strings.TrimSpace(options.Config.UpstreamBase), "/")
	now := time.Now()
	out := map[string]any{
		"ok":             false,
		"reachable":      false,
		"base_url":       base,
		"checked_at":     now.Unix(),
		"checked_at_ms":  now.UnixMilli(),
		"implementation": "go",
	}
	if base == "" {
		out["error"] = "upstream base not configured"
		return out
	}
	origin := base + "/models"
	out["origin"] = origin

	// 1) TCP/TLS reachability (host:port from URL).
	if hostPort, scheme, err := upstreamHostPort(base); err == nil {
		out["host"] = hostPort
		out["scheme"] = scheme
		dialStart := time.Now()
		conn, derr := net.DialTimeout("tcp", hostPort, upstreamProbeDialTimeout)
		dialMS := time.Since(dialStart).Milliseconds()
		out["dial_ms"] = dialMS
		if derr != nil {
			out["error"] = "dial: " + derr.Error()
			out["latency_ms"] = dialMS
			return out
		}
		_ = conn.Close()
		out["reachable"] = true
	} else {
		out["error"] = "parse base: " + err.Error()
		return out
	}

	// 2) HTTP GET /models — with live account token when available.
	viaEmail := ""
	token := ""
	if options.Store != nil {
		if auths, err := options.Store.ListAccountAuths(ctx, 5, true); err == nil && len(auths) > 0 {
			// Prefer a non-empty token.
			for _, a := range auths {
				if strings.TrimSpace(a.Token) != "" {
					token = a.Token
					viaEmail = a.Email
					break
				}
			}
		}
	}
	if viaEmail != "" {
		out["via"] = viaEmail
		out["auth"] = "account"
	} else {
		out["via"] = nil
		out["auth"] = "anonymous"
	}

	reqCtx, cancel := context.WithTimeout(ctx, upstreamProbeHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, origin, nil)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	if token != "" {
		gc := upstreamClient(options)
		model := "grok-4.5"
		if options.Config.DefaultModel != "" {
			model = options.Config.DefaultModel
		}
		for k, v := range gc.Headers(token, model) {
			req.Header.Set(k, v)
		}
	} else {
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "grokcli-2api-upstream-probe/1")
	}

	client := &http.Client{
		Timeout: upstreamProbeHTTPTimeout,
		Transport: &http.Transport{
			// Fail-fast; do not reuse the shared pool for monitoring probes.
			// Honor HTTP(S)_PROXY / NO_PROXY (compose routes Grok via privoxy).
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   upstreamProbeDialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 6 * time.Second,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}

	httpStart := time.Now()
	resp, err := client.Do(req)
	latencyMS := time.Since(httpStart).Milliseconds()
	out["latency_ms"] = latencyMS
	if err != nil {
		// Dial already succeeded; classify as degraded reachability.
		out["error"] = err.Error()
		out["reachable"] = true // TCP worked; HTTP path failed
		return out
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	out["status_code"] = resp.StatusCode

	// Any HTTP response means the edge is up. Auth errors still count as "up".
	if resp.StatusCode >= 200 && resp.StatusCode < 500 {
		out["reachable"] = true
	}
	// Healthy catalog: 2xx with parseable models list.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var payload any
		if jerr := json.Unmarshal(body, &payload); jerr == nil {
			items := parseUpstreamModels(payload)
			out["models_count"] = len(items)
			out["ok"] = true
			out["error"] = nil
			return out
		}
		// 2xx but not JSON models — still treat as reachable / ok-ish.
		out["ok"] = true
		out["error"] = "response not parseable as models list"
		out["body_preview"] = trimPreview(string(body), 180)
		return out
	}

	// 401/403 without token: upstream is up, just needs auth.
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && token == "" {
		out["ok"] = true // edge alive; no pool account for full catalog check
		out["error"] = fmt.Sprintf("HTTP %d (no live account token for full check)", resp.StatusCode)
		out["body_preview"] = trimPreview(string(body), 180)
		return out
	}

	out["ok"] = false
	out["error"] = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, trimPreview(string(body), 200))
	return out
}

func upstreamHostPort(base string) (hostPort, scheme string, err error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", "", err
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("missing host in %q", base)
	}
	scheme = u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	return net.JoinHostPort(host, port), scheme, nil
}

func trimPreview(s string, n int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
