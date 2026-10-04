package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hm2899/grokcli-2api/internal/store/postgres"
	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

type Service struct {
	Store      *postgres.Connector
	Upstream   string
	Workers    int
	httpClient *http.Client
}

func New(store *postgres.Connector, upstream string) *Service {
	return &Service{
		Store:      store,
		Upstream:   strings.TrimRight(upstream, "/"),
		Workers:    envInt("GROK2API_QUOTA_WORKERS", 3, 1, 8),
		httpClient: newQuotaHTTPClient(),
	}
}

func newQuotaHTTPClient() *http.Client {
	// Keep connection fan-out low: each account probe may hit billing + chat + user.
	// High MaxConnsPerHost previously stampeded privoxy / cli-chat-proxy and caused
	// "too many connections" / provider retry storms during auto quota refresh.
	proxy := proxyFromEnv()
	proxyFn := http.ProxyFromEnvironment
	if proxy != nil {
		proxyFn = http.ProxyURL(proxy)
	}
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:                 proxyFn,
			MaxIdleConns:          16,
			MaxIdleConnsPerHost:   4,
			MaxConnsPerHost:       6,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   8 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			ForceAttemptHTTP2:     true,
			// Prefer fewer sockets under proxy: HTTP/1.1 keep-alive is fine; avoid
			// opening dozens of concurrent TLS sessions through privoxy.
			DialContext: (&net.Dialer{
				Timeout:   6 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
}

func (s *Service) client() *http.Client {
	// Always reuse the shared client so MaxConnsPerHost is actually enforced.
	// Cloning Transport per request previously bypassed the pool limit.
	if s != nil && s.httpClient != nil {
		return s.httpClient
	}
	return newQuotaHTTPClient()
}

func (s *Service) FetchCached(ctx context.Context) (map[string]any, error) {
	if s.Store == nil {
		return map[string]any{"ok": false, "error": "store unavailable"}, nil
	}
	return s.Store.ListCachedQuotas(ctx)
}

func (s *Service) FetchOne(ctx context.Context, accountID string) (map[string]any, error) {
	if s.Store == nil {
		return map[string]any{"ok": false, "error": "store unavailable"}, nil
	}
	auth, err := s.Store.GetAccountAuth(ctx, accountID)
	if err != nil || auth == nil {
		return map[string]any{"ok": false, "account_id": accountID, "error": "account not found or has no token"}, nil
	}
	item := s.fetchOne(ctx, *auth)
	if item == nil {
		item = map[string]any{"ok": false, "account_id": auth.ID, "error": "empty quota result"}
	}
	// Durable write BEFORE response: admin hard-refresh must see type/usage in DB.
	// SaveQuotaSnapshot merges with previous snap so a failed probe cannot wipe data.
	// Detach from request cancel but still wait (bounded) so we don't race the UI.
	if saved := s.persistQuotaSnapshotSync(auth.ID, item); len(saved) > 0 {
		// Prefer durable merged snap for response fields (type/usage stable).
		for _, k := range []string{
			"account_type", "plan", "plan_label", "plan_source",
			"tokens_limit", "tokens_used", "tokens_remaining", "tokens_actual", "tokens_usage_percent",
			"monthly_limit", "used", "remaining", "usage_percent",
			"weekly_limit", "weekly_used", "weekly_remaining", "weekly_usage_percent",
			"on_demand_cap", "on_demand_used", "free_tokens", "unlimited_or_free",
			"summary", "display", "source", "exhausted", "auto_disabled", "ok",
			"fetched_at", "error", "exhaust_reason",
		} {
			if v, ok := saved[k]; ok && v != nil {
				item[k] = v
			}
		}
		item["cached_merged"] = true
	}
	if item["exhausted"] == true {
		// Enter cooldown pool (not permanent disable).
		item["auto_disabled"] = true
		item["pool_disabled"] = false
		item["in_cooldown"] = true
	}
	// Synthesize a lightweight pool view. Do NOT embed item inside pool (JSON cycle).
	item["pool"] = syntheticPoolFromQuota(auth.ID, item)
	if lq := stripQuotaForPool(item); len(lq) > 0 {
		if pool, ok := item["pool"].(map[string]any); ok && pool != nil {
			pool["last_quota"] = lq
		}
	}
	return item, nil
}

// stripQuotaForPool copies durable quota fields for embedding as last_quota (no nested pool).
func stripQuotaForPool(item map[string]any) map[string]any {
	if item == nil {
		return nil
	}
	out := make(map[string]any, 24)
	for _, k := range []string{
		"ok", "fetched_at", "account_id", "email", "user_id",
		"account_type", "plan", "plan_label", "plan_source",
		"monthly_limit", "used", "remaining", "usage_percent",
		"weekly_limit", "weekly_used", "weekly_remaining", "weekly_usage_percent",
		"on_demand_cap", "on_demand_used", "prepaid_balance",
		"free_tokens", "unlimited_or_free",
		"tokens_limit", "tokens_remaining", "tokens_used", "tokens_actual", "tokens_usage_percent",
		"requests_limit", "requests_remaining",
		"exhausted", "exhaust_reason", "auto_disabled", "summary", "display",
		"billing_period_end", "error", "status_code", "source",
	} {
		if v, ok := item[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

func (s *Service) FetchAll(ctx context.Context) (map[string]any, error) {
	if s.Store == nil {
		return map[string]any{"ok": false, "error": "store unavailable"}, nil
	}
	// Include disabled accounts so recovery can re-enable them after billing heals.
	auths, err := s.Store.ListAccountAuths(ctx, 2000, false)
	if err != nil {
		return nil, err
	}
	workers := s.Workers
	if workers <= 0 {
		workers = 3
	}
	if workers > 2 {
		workers = 2 // hard cap: avoid proxy connection stampede
	}
	if workers > len(auths) && len(auths) > 0 {
		workers = len(auths)
	}
	type result struct{ item map[string]any }
	ch := make(chan result, len(auths))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	// Collect snapshots for a single async bulk persist after the response is built.
	// Live billing is the slow part; PG writes must not serialize the admin button.
	snaps := make([]quotaSnap, 0, len(auths))
	var snapsMu sync.Mutex
	for _, auth := range auths {
		wg.Add(1)
		go func(a postgres.AccountAuth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			item := s.fetchOne(ctx, a)
			if item == nil {
				item = map[string]any{"ok": false, "account_id": a.ID, "error": "empty quota result"}
			}
			if item["exhausted"] == true {
				item["auto_disabled"] = true
				item["pool_disabled"] = false
				item["in_cooldown"] = true
			}
			item["pool"] = syntheticPoolFromQuota(a.ID, item)
			snapsMu.Lock()
			snaps = append(snaps, quotaSnap{id: a.ID, item: item})
			snapsMu.Unlock()
			ch <- result{item: item}
		}(auth)
	}
	wg.Wait()
	close(ch)
	// Fire bulk persist AFTER live results are ready so the HTTP handler can return.
	s.persistQuotaSnapshots(snaps)
	results := make([]map[string]any, 0, len(auths))
	for r := range ch {
		results = append(results, r.item)
	}
	okCount, exhausted, autoDisabled, poolDisabled := 0, 0, 0, 0
	var totalUsed, totalLimit, totalRemaining float64
	activeOK := 0
	for _, r := range results {
		if r["ok"] == true {
			okCount++
		}
		if r["exhausted"] == true {
			exhausted++
		}
		if r["auto_disabled"] == true {
			autoDisabled++
		}
		if r["pool_disabled"] == true {
			poolDisabled++
		}
		if r["ok"] == true && r["pool_disabled"] != true && r["exhausted"] != true {
			activeOK++
			totalUsed += floatOf(r["used"])
			totalLimit += floatOf(r["monthly_limit"])
			totalRemaining += floatOf(r["remaining"])
		}
	}
	return map[string]any{
		"ok":                  true,
		"fetched_at":          time.Now().Unix(),
		"count":               len(results),
		"ok_count":            okCount,
		"exhausted_count":     exhausted,
		"auto_disabled_count": autoDisabled,
		"pool_disabled_count": poolDisabled,
		"active_ok_count":     activeOK,
		"total_used":          totalUsed,
		"total_monthly_limit": totalLimit,
		"total_remaining":     totalRemaining,
		"workers":             workers,
		// Both keys: frontend accepts results || accounts; keep both for clients.
		"accounts": results,
		"results":  results,
	}, nil
}

// persistQuotaSnapshotAsync writes last_quota + pool status without blocking the
// request that already has live billing data for the UI.

// FetchByIDs live-probes a subset of accounts (visible page / missing quota).
// Much cheaper than FetchAll on multi-thousand pools.
func (s *Service) FetchByIDs(ctx context.Context, accountIDs []string) (map[string]any, error) {
	if s.Store == nil {
		return map[string]any{"ok": false, "error": "store unavailable"}, nil
	}
	// Dedupe + cap to protect upstream.
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(accountIDs))
	for _, raw := range accountIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		if len(ids) >= 25 {
			break
		}
	}
	if len(ids) == 0 {
		return map[string]any{
			"ok": true, "count": 0, "ok_count": 0, "exhausted_count": 0,
			"accounts": []map[string]any{}, "results": []map[string]any{},
			"fetched_at": time.Now().Unix(), "scoped": true,
		}, nil
	}

	workers := s.Workers
	if workers <= 0 {
		workers = 3
	}
	if workers > 4 {
		workers = 4
	}
	if workers > len(ids) {
		workers = len(ids)
	}
	type result struct{ item map[string]any }
	ch := make(chan result, len(ids))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	snaps := make([]quotaSnap, 0, len(ids))
	var snapsMu sync.Mutex

	for _, id := range ids {
		wg.Add(1)
		go func(accountID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			auth, err := s.Store.GetAccountAuth(ctx, accountID)
			var item map[string]any
			if err != nil || auth == nil {
				item = map[string]any{
					"ok": false, "account_id": accountID,
					"error": "account not found or has no token",
				}
			} else {
				item = s.fetchOne(ctx, *auth)
				if item == nil {
					item = map[string]any{"ok": false, "account_id": auth.ID, "error": "empty quota result"}
				}
			}
			if item["exhausted"] == true {
				item["auto_disabled"] = true
				item["pool_disabled"] = false
				item["in_cooldown"] = true
			}
			item["pool"] = syntheticPoolFromQuota(stringFromAny(item["account_id"]), item)
			if item["account_id"] == nil || item["account_id"] == "" {
				item["account_id"] = accountID
			}
			snapsMu.Lock()
			snaps = append(snaps, quotaSnap{id: accountID, item: item})
			snapsMu.Unlock()
			ch <- result{item: item}
		}(id)
	}
	wg.Wait()
	close(ch)
	s.persistQuotaSnapshots(snaps)

	results := make([]map[string]any, 0, len(ids))
	for r := range ch {
		results = append(results, r.item)
	}
	okCount, exhausted := 0, 0
	for _, r := range results {
		if r["ok"] == true {
			okCount++
		}
		if r["exhausted"] == true {
			exhausted++
		}
	}
	return map[string]any{
		"ok":              true,
		"fetched_at":      time.Now().Unix(),
		"count":           len(results),
		"ok_count":        okCount,
		"exhausted_count": exhausted,
		"workers":         workers,
		"scoped":          true,
		"accounts":        results,
		"results":         results,
	}, nil
}

// persistQuotaSnapshotSync writes last_quota (merged) and returns the durable snap.
// Uses a background-derived context so client disconnect cannot cancel the write,
// but still waits so FetchOne responses match what is in PostgreSQL.
func (s *Service) persistQuotaSnapshotSync(accountID string, item map[string]any) map[string]any {
	if s == nil || s.Store == nil || strings.TrimSpace(accountID) == "" || item == nil {
		return nil
	}
	copyItem := make(map[string]any, len(item))
	for k, v := range item {
		if k == "pool" {
			continue // never persist nested pool view
		}
		copyItem[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()
	saved, err := s.Store.SaveQuotaSnapshot(ctx, accountID, copyItem)
	if err != nil {
		slog.Warn("quota snapshot save failed", "account_id", accountID, "error", err)
		return nil
	}
	return saved
}

// persistQuotaSnapshotAsync is a fire-and-forget wrapper for bulk paths that
// already return live results; prefer persistQuotaSnapshotSync for single-account.
func (s *Service) persistQuotaSnapshotAsync(accountID string, item map[string]any) {
	if s == nil || s.Store == nil || strings.TrimSpace(accountID) == "" || item == nil {
		return
	}
	copyItem := make(map[string]any, len(item))
	for k, v := range item {
		if k == "pool" {
			continue
		}
		copyItem[k] = v
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
		defer cancel()
		if _, err := s.Store.SaveQuotaSnapshot(ctx, accountID, copyItem); err != nil {
			slog.Warn("quota snapshot async save failed", "account_id", accountID, "error", err)
		}
	}()
}

type quotaSnap struct {
	id   string
	item map[string]any
}

func (s *Service) persistQuotaSnapshots(snaps []quotaSnap) {
	if s == nil || s.Store == nil || len(snaps) == 0 {
		return
	}
	// Cap concurrent PG writers so a 7k-account pool does not stampede the DB.
	workers := s.Workers
	if workers <= 0 {
		workers = 8
	}
	if workers > 16 {
		workers = 16
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, sn := range snaps {
		sn := sn
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			copyItem := make(map[string]any, len(sn.item))
			for k, v := range sn.item {
				if k == "pool" {
					continue
				}
				copyItem[k] = v
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := s.Store.SaveQuotaSnapshot(ctx, sn.id, copyItem); err != nil {
				slog.Warn("quota snapshot bulk save failed", "account_id", sn.id, "error", err)
			}
		}()
	}
	wg.Wait()
}

// syntheticPoolFromQuota builds a client-side pool view so the admin UI can paint
// cool/re-enter status immediately, before async SaveQuotaSnapshot.
//
// Exhausted free/paid quota → cooldown (not permanent 额度禁用). Matches live-traffic
// free-usage handling and SaveQuotaSnapshot.
//
// IMPORTANT: do NOT set pool["last_quota"] = item when the caller also attaches
// item["pool"] = pool. That creates a cycle and encoding/json fails with
// "unsupported value: encountered a cycle", producing an empty HTTP body.
// Frontend already treats the quota response itself as last_quota.
func syntheticPoolFromQuota(accountID string, item map[string]any) map[string]any {
	if item == nil {
		return map[string]any{
			"id":         accountID,
			"account_id": accountID,
		}
	}
	exhausted := item["exhausted"] == true || item["auto_disabled"] == true
	ok := item["ok"] == true && !exhausted
	pool := map[string]any{
		"id":         accountID,
		"account_id": accountID,
	}
	if exhausted {
		// Cool into rotation-out pool; keep enabled=true so recovery paths stay simple.
		pool["disabled_for_quota"] = false
		pool["enabled"] = true
		pool["pool_status"] = "cooldown"
		pool["in_cooldown"] = true
		pool["pool_disabled"] = false
		reason := ""
		if r, ok := item["exhaust_reason"].(string); ok && strings.TrimSpace(r) != "" {
			reason = r
		} else if d, ok := item["display"].(map[string]any); ok {
			if s, ok := d["summary"].(string); ok {
				reason = s
			}
		}
		if reason == "" {
			reason = "额度已耗尽"
		}
		pool["cooldown_reason"] = reason
		src := firstNonEmpty(stringFromAny(item["source"]), "billing")
		if src == "free_tokens" || src == "free" || truthyMap(item, "free_tokens") || stringFromAny(item["account_type"]) == "free" {
			pool["cooldown_code"] = "subscription:free-usage-exhausted"
		} else {
			pool["cooldown_code"] = "billing_quota"
		}
		// ~2h free / ~6h paid UI remaining (actual DB until set by SaveQuotaSnapshot).
		coolSec := 2 * 3600
		if pool["cooldown_code"] == "billing_quota" {
			coolSec = 6 * 3600
		}
		pool["cooldown_until"] = time.Now().Unix() + int64(coolSec)
		if v, ok := item["tokens_used"]; ok {
			pool["cooldown_tokens_actual"] = v
		} else if v, ok := item["tokens_actual"]; ok {
			pool["cooldown_tokens_actual"] = v
		}
		if v, ok := item["tokens_limit"]; ok {
			pool["cooldown_tokens_limit"] = v
		}
	} else if ok {
		pool["disabled_for_quota"] = false
		pool["enabled"] = true
		pool["pool_status"] = "normal"
		pool["in_cooldown"] = false
		pool["disabled_reason"] = nil
		pool["quota_source"] = nil
	}
	return pool
}

func stringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func (s *Service) fetchOne(ctx context.Context, auth postgres.AccountAuth) map[string]any {
	out := map[string]any{
		"ok":         false,
		"account_id": auth.ID,
		"email":      auth.Email,
		"fetched_at": time.Now().Unix(),
		"source":     "billing",
	}
	gc := &grok.Client{BaseURL: s.Upstream}
	headers := gc.Headers(auth.Token, "grok-4.5")
	headers["Accept"] = "application/json"

	// 1) Billing dollars (paid / promo). Free build accounts usually return $0/$0.
	billOK := false
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Upstream+"/billing", nil)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		out["error"] = err.Error()
		// Still try free-token probe below.
	} else {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			out["error"] = fmt.Sprintf("billing HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
			out["status_code"] = resp.StatusCode
		} else {
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				out["error"] = "parse billing: " + err.Error()
			} else {
				norm := normalizeBilling(raw)
				for k, v := range norm {
					out[k] = v
				}
				billOK = norm["ok"] != false && norm["error"] == nil
				out["ok"] = billOK
			}
		}
	}

	// 2) Token-volume probe (chat rate-limit headers). Free/build accounts have
	// $0 billing — real capacity is x-ratelimit-*-tokens. SuperGrok still gets a
	// probe so request/token windows are visible alongside monthly/weekly $.
	needTokenProbe := !billOK || truthyMap(out, "unlimited_or_free") || isZeroMoney(out["monthly_limit"]) ||
		floatOf(out["monthly_limit"]) > 0 // SuperGrok: also refresh live windows
	if needTokenProbe {
		if tok := s.probeFreeTokenQuota(ctx, auth, headers); tok != nil {
			paid := floatOf(out["monthly_limit"]) > 0 || floatOf(out["on_demand_cap"]) > 0 || floatOf(out["weekly_limit"]) > 0
			for k, v := range tok {
				if v == nil {
					continue
				}
				// Paid SuperGrok: keep dollar fields authoritative; still attach token window.
				if paid && (k == "unlimited_or_free" || k == "free_tokens") {
					continue
				}
				if paid && k == "exhausted" && !truthyMap(out, "exhausted") {
					// Don't let free-style token exhaust override healthy paid billing
					// unless remaining tokens are truly 0.
					if rem := int64Of(tok["tokens_remaining"]); rem > 0 {
						continue
					}
				}
				out[k] = v
			}
			if !paid && truthyMap(out, "free_tokens") {
				out["source"] = "free_tokens"
				out["ok"] = true
				delete(out, "error")
			} else if paid {
				out["source"] = "billing+tokens"
				out["ok"] = true
				out["free_tokens"] = false
				out["unlimited_or_free"] = false
			}
			// Auto-calc used when only limit+remaining present.
			normalizeTokenUsageFields(out)
		}
	}
	// Dollar-side usage percent / weekly math.
	normalizeBillingUsageFields(out)

	// 3) Profile for plan classification (free vs SuperGrok / team).
	// Skip /user when free token probe already classified the account — saves one
	// upstream connection per account during auto refresh storms.
	needUser := true
	if truthyMap(out, "free_tokens") && int64Of(out["tokens_limit"]) > 0 && isZeroMoney(out["monthly_limit"]) {
		needUser = false
	}
	if needUser {
		if user := s.fetchUserProfile(ctx, headers); user != nil {
			out["user"] = user
			for k, v := range classifyAccountPlan(out, user) {
				out[k] = v
			}
		} else {
			for k, v := range classifyAccountPlan(out, nil) {
				if out[k] == nil {
					out[k] = v
				}
			}
		}
	} else {
		for k, v := range classifyAccountPlan(out, nil) {
			if out[k] == nil {
				out[k] = v
			}
		}
	}

	// Normalize display summary once all fields are known.
	out["display"] = buildQuotaDisplay(out)
	if s, ok := out["display"].(map[string]any); ok {
		if sum, _ := s["summary"].(string); sum != "" {
			out["summary"] = sum
		}
	}
	return out
}

// fetchUserProfile GET /user — used to distinguish free vs SuperGrok/team.
func (s *Service) fetchUserProfile(ctx context.Context, headers map[string]string) map[string]any {
	if s == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Upstream+"/user", nil)
	if err != nil {
		return nil
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return nil
	}
	return normalizeUser(raw)
}

func normalizeUser(raw map[string]any) map[string]any {
	if raw == nil {
		return nil
	}
	return map[string]any{
		"user_id":              firstNonEmpty(stringFromAny(raw["userId"]), stringFromAny(raw["principalId"]), stringFromAny(raw["user_id"])),
		"email":                stringFromAny(raw["email"]),
		"has_grok_code_access": raw["hasGrokCodeAccess"],
		"team_id":              firstNonEmpty(stringFromAny(raw["teamId"]), stringFromAny(raw["team_id"])),
		"team_name":            stringFromAny(raw["teamName"]),
		"team_role":            stringFromAny(raw["teamRole"]),
		"organization_id":      firstNonEmpty(stringFromAny(raw["organizationId"]), stringFromAny(raw["organization_id"])),
		"organization_name":    stringFromAny(raw["organizationName"]),
		"organization_type":    stringFromAny(raw["organizationType"]),
		"principal_type":       stringFromAny(raw["principalType"]),
		"user_blocked_reason":  raw["userBlockedReason"],
		"team_blocked_reasons": raw["teamBlockedReasons"],
	}
}

// classifyAccountPlan returns account_type / plan_label / plan_source.
//
//	free       — $0 billing + free token window (build-free / free-usage)
//	supergrok  — paid monthlyLimit > 0 or SuperGrok-style org/team entitlements
//	team       — team/org principal without clear personal SuperGrok billing
//	unknown    — insufficient signal
func classifyAccountPlan(quota map[string]any, user map[string]any) map[string]any {
	out := map[string]any{}
	monthly := floatOf(quota["monthly_limit"])
	onDemand := floatOf(quota["on_demand_cap"])
	freeTokens := truthyMap(quota, "free_tokens") || truthyMap(quota, "unlimited_or_free")
	tokLimit := int64Of(quota["tokens_limit"])
	probeModel := strings.ToLower(stringFromAny(quota["probe_model"]))

	teamID := ""
	orgType := ""
	hasCode := false
	if user != nil {
		teamID = stringFromAny(user["team_id"])
		orgType = strings.ToLower(stringFromAny(user["organization_type"]))
		switch v := user["has_grok_code_access"].(type) {
		case bool:
			hasCode = v
		case string:
			hasCode = strings.EqualFold(strings.TrimSpace(v), "true")
		}
	}

	plan := "unknown"
	label := "未知"
	source := "heuristic"

	// Paid SuperGrok / subscription: non-zero monthly or on-demand dollar budget.
	if monthly > 0 || onDemand > 0 || floatOf(quota["weekly_limit"]) > 0 {
		plan = "supergrok"
		label = "SuperGrok"
		source = "billing"
	} else if strings.Contains(probeModel, "build-free") || strings.Contains(probeModel, "free") {
		plan = "free"
		label = "Free"
		source = "probe_model"
	} else if freeTokens && tokLimit > 0 && monthly == 0 && onDemand == 0 {
		// Free window with explicit token limit, no dollar budget.
		plan = "free"
		label = "Free"
		source = "free_tokens"
	} else if teamID != "" || orgType != "" {
		plan = "team"
		label = "Team"
		source = "user_profile"
	} else if freeTokens || (monthly == 0 && onDemand == 0) {
		plan = "free"
		label = "Free"
		source = "billing_zero"
	}

	// Soft signal: hasGrokCodeAccess alone is NOT SuperGrok (free also true).
	_ = hasCode

	out["account_type"] = plan
	out["plan"] = plan
	out["plan_label"] = label
	out["plan_source"] = source
	return out
}

// probeFreeTokenQuota issues a minimal chat completion and reads rate-limit
// headers. Grok free/build accounts advertise remaining token volume there
// (x-ratelimit-remaining-tokens / x-ratelimit-limit-tokens).
func (s *Service) probeFreeTokenQuota(ctx context.Context, auth postgres.AccountAuth, headers map[string]string) map[string]any {
	if s == nil || strings.TrimSpace(auth.Token) == "" {
		return nil
	}
	// Prefer chat/completions — headers are consistent and body is small.
	body := map[string]any{
		"model":      "grok-4.5",
		"stream":     false,
		"max_tokens": 1,
		"messages": []map[string]any{
			{"role": "user", "content": "ping"},
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Upstream+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return nil
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Bound free-token probe separately so a hung chat does not stall the whole quota button.
	pctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	req = req.WithContext(pctx)

	resp, err := s.client().Do(req)
	if err != nil {
		return map[string]any{
			"free_tokens":       false,
			"token_probe_error": err.Error(),
		}
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	out := map[string]any{
		"free_tokens":        true,
		"token_probe_status": resp.StatusCode,
	}

	// Headers are the primary signal for free token volume.
	limitTok := headerInt64(resp.Header, "x-ratelimit-limit-tokens", "X-RateLimit-Limit-Tokens")
	remainTok := headerInt64(resp.Header, "x-ratelimit-remaining-tokens", "X-RateLimit-Remaining-Tokens")
	limitReq := headerInt64(resp.Header, "x-ratelimit-limit-requests", "X-RateLimit-Limit-Requests")
	remainReq := headerInt64(resp.Header, "x-ratelimit-remaining-requests", "X-RateLimit-Remaining-Requests")
	if limitTok != nil {
		out["tokens_limit"] = *limitTok
	}
	if remainTok != nil {
		out["tokens_remaining"] = *remainTok
	}
	if limitTok != nil && remainTok != nil {
		used := *limitTok - *remainTok
		if used < 0 {
			used = 0
		}
		out["tokens_used"] = used
		if *limitTok > 0 {
			out["tokens_usage_percent"] = round2(100.0 * float64(used) / float64(*limitTok))
		}
	}
	if limitReq != nil {
		out["requests_limit"] = *limitReq
	}
	if remainReq != nil {
		out["requests_remaining"] = *remainReq
	}

	// Free / promo billing is typically $0 — mark unlimited_or_free for UI pill.
	out["unlimited_or_free"] = true

	// Exhaustion from free-usage error body (tokens actual/limit) or zero remaining.
	bodyText := string(rawBody)
	if resp.StatusCode >= 400 {
		out["token_probe_error"] = truncate(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, bodyText), 240)
		// Parse free-usage token pair from body: "tokens (actual/limit): 2368681/2000000"
		if actual, limit, ok := parseTokenPair(bodyText); ok {
			out["tokens_actual"] = actual
			out["tokens_limit"] = limit
			if limit > 0 {
				remain := limit - actual
				if remain < 0 {
					remain = 0
				}
				out["tokens_used"] = actual
				out["tokens_remaining"] = remain
				out["tokens_usage_percent"] = round2(100.0 * float64(actual) / float64(limit))
			}
			if actual >= limit && limit > 0 {
				out["exhausted"] = true
				out["exhaust_reason"] = "免费额度 token 已用尽"
			}
		}
		low := strings.ToLower(bodyText)
		if strings.Contains(low, "free-usage") || strings.Contains(low, "free usage") ||
			strings.Contains(low, "额度用完") || strings.Contains(low, "included free usage") {
			out["exhausted"] = true
			if out["exhaust_reason"] == nil {
				out["exhaust_reason"] = "free-usage exhausted"
			}
		}
	} else if remainTok != nil && *remainTok <= 0 && limitTok != nil && *limitTok > 0 {
		// Only treat as exhausted when we have a real limit and remaining is zero.
		// Bare remaining=0 without limit is an ambiguous header and must not cool
		// freshly registered accounts after a successful model 测活.
		out["exhausted"] = true
		out["exhaust_reason"] = "剩余 token 为 0"
	}

	// Also surface usage from successful completion body when present.
	if resp.StatusCode < 400 {
		var payload map[string]any
		if json.Unmarshal(rawBody, &payload) == nil {
			if usage, _ := payload["usage"].(map[string]any); usage != nil {
				out["last_usage"] = usage
			}
			if model, _ := payload["model"].(string); model != "" {
				out["probe_model"] = model
			}
		}
	}

	// Only treat as free-token signal if we got at least one token counter.
	if out["tokens_limit"] == nil && out["tokens_remaining"] == nil && out["tokens_actual"] == nil {
		// Headers missing — not a useful free probe (maybe paid account).
		if resp.StatusCode < 400 {
			return map[string]any{"free_tokens": false}
		}
	}
	return out
}

func headerInt64(h http.Header, keys ...string) *int64 {
	if h == nil {
		return nil
	}
	for _, k := range keys {
		raw := strings.TrimSpace(h.Get(k))
		if raw == "" {
			continue
		}
		// Some proxies send "1000000, 1000000"
		if i := strings.IndexByte(raw, ','); i >= 0 {
			raw = strings.TrimSpace(raw[:i])
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}
		return &n
	}
	return nil
}

func parseTokenPair(text string) (actual, limit int64, ok bool) {
	// tokens (actual/limit): 2368681/2000000  OR  tokens: 1/2
	re := regexp.MustCompile(`(?i)tokens?\s*(?:\(actual\s*/\s*limit\))?\s*[:=]?\s*(\d+)\s*/\s*(\d+)`)
	m := re.FindStringSubmatch(text)
	if len(m) != 3 {
		return 0, 0, false
	}
	a, err1 := strconv.ParseInt(m[1], 10, 64)
	b, err2 := strconv.ParseInt(m[2], 10, 64)
	if err1 != nil || err2 != nil || b <= 0 {
		return 0, 0, false
	}
	return a, b, true
}

func isZeroMoney(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case float64:
		return t == 0
	case int:
		return t == 0
	case int64:
		return t == 0
	case *float64:
		return t == nil || *t == 0
	default:
		return false
	}
}

func truthyMap(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "1" || s == "true" || s == "yes"
	default:
		return false
	}
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}

func buildQuotaDisplay(out map[string]any) map[string]any {
	planLabel := stringFromAny(out["plan_label"])
	// Free token volume takes priority for free/build accounts.
	if truthyMap(out, "free_tokens") || out["tokens_limit"] != nil || out["tokens_remaining"] != nil || stringFromAny(out["account_type"]) == "free" {
		limit := int64Of(out["tokens_limit"])
		remain := int64Of(out["tokens_remaining"])
		used := int64Of(out["tokens_used"])
		if used == 0 && limit > 0 && remain >= 0 && remain <= limit {
			used = limit - remain
		}
		parts := []string{}
		if limit > 0 {
			parts = append(parts, fmt.Sprintf("token %s / %s", fmtInt(used), fmtInt(limit)))
			if remain >= 0 {
				parts = append(parts, fmt.Sprintf("剩 %s", fmtInt(remain)))
			}
			if pct := floatOf(out["tokens_usage_percent"]); pct > 0 {
				parts = append(parts, fmt.Sprintf("%.0f%%", pct))
			}
		} else if remain > 0 {
			parts = append(parts, fmt.Sprintf("剩 token %s", fmtInt(remain)))
		}
		reqL := int64Of(out["requests_limit"])
		reqR := int64Of(out["requests_remaining"])
		if reqL > 0 {
			parts = append(parts, fmt.Sprintf("请求 %d/%d", reqL-reqR, reqL))
		}
		sum := strings.Join(parts, " · ")
		if sum == "" {
			if truthyMap(out, "exhausted") {
				sum = "免费 token 已用尽"
			} else {
				sum = "免费 token（未读到限额）"
			}
		}
		if truthyMap(out, "exhausted") && !strings.Contains(sum, "尽") {
			sum = "已耗尽 · " + sum
		}
		if planLabel != "" && !strings.Contains(sum, planLabel) {
			sum = planLabel + " · " + sum
		}
		return map[string]any{"summary": sum}
	}
	// SuperGrok / paid: monthly + weekly dollar budgets (and optional token window).
	if floatOf(out["monthly_limit"]) > 0 || floatOf(out["weekly_limit"]) > 0 || floatOf(out["on_demand_cap"]) > 0 {
		sum := formatSuperGrokSummary(out)
		// Append live token window when probe returned one (secondary).
		if lim := int64Of(out["tokens_limit"]); lim > 0 {
			used := int64Of(out["tokens_used"])
			rem := int64Of(out["tokens_remaining"])
			if rem == 0 && used == 0 {
				// leave
			} else {
				sum += fmt.Sprintf(" · token %s/%s", fmtInt(used), fmtInt(lim))
			}
		}
		if planLabel == "" {
			planLabel = "SuperGrok"
		}
		if sum == "" {
			sum = planLabel
		} else if !strings.Contains(sum, planLabel) {
			sum = planLabel + " · " + sum
		}
		return map[string]any{"summary": sum}
	}
	if d, ok := out["display"].(map[string]any); ok {
		return d
	}
	return map[string]any{"summary": "—"}
}

func int64Of(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case *int64:
		if t == nil {
			return 0
		}
		return *t
	default:
		return 0
	}
}

func fmtInt(n int64) string {
	if n < 0 {
		n = 0
	}
	s := strconv.FormatInt(n, 10)
	// thousands separator
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre == 0 {
		pre = 3
	}
	b.WriteString(s[:pre])
	for i := pre; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func envInt(name string, fallback, min, max int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

func proxyFromEnv() *url.URL {
	proxyURL := firstNonEmpty(
		os.Getenv("GROK2API_XAI_PROXY"),
		os.Getenv("GROK2API_PROXY"),
		os.Getenv("HTTPS_PROXY"),
		os.Getenv("HTTP_PROXY"),
		os.Getenv("https_proxy"),
		os.Getenv("http_proxy"),
	)
	if proxyURL == "" {
		return nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	return u
}

func normalizeBilling(raw map[string]any) map[string]any {
	if raw == nil {
		return map[string]any{"ok": false, "error": "empty billing response"}
	}
	cfg := raw
	if nested, ok := raw["config"].(map[string]any); ok {
		cfg = nested
	}
	// Monthly dollar budget (SuperGrok included allowance).
	monthly := firstMoney(cfg, "monthlyLimit", "monthly_limit", "monthLimit", "month_limit")
	used := firstMoney(cfg, "used", "monthlyUsed", "monthly_used", "includedUsed", "included_used")
	// Weekly dollar / credit window (SuperGrok short cycle when present).
	weekly := firstMoney(cfg, "weeklyLimit", "weekly_limit", "weekLimit", "week_limit", "weeklyCap", "weekly_cap")
	weeklyUsed := firstMoney(cfg, "weeklyUsed", "weekly_used", "weekUsed", "week_used")
	onDemand := firstMoney(cfg, "onDemandCap", "on_demand_cap", "onDemandLimit", "on_demand_limit")
	onDemandUsed := firstMoney(cfg, "onDemandUsed", "on_demand_used")
	prepaid := firstMoney(cfg, "prepaidBalance", "prepaid_balance", "prepaid")

	var remaining *float64
	if monthly != nil && used != nil {
		v := *monthly - *used
		if v < 0 {
			v = 0
		}
		remaining = &v
	}
	var weeklyRemaining *float64
	if weekly != nil && weeklyUsed != nil {
		v := *weekly - *weeklyUsed
		if v < 0 {
			v = 0
		}
		weeklyRemaining = &v
	}

	// Exhausted when monthly included is gone AND on-demand (if any) is also gone.
	exhausted := false
	exhaustReason := ""
	if monthly != nil && used != nil && *monthly > 0 && *used >= *monthly {
		if onDemand != nil && *onDemand > 0 {
			odUsed := 0.0
			if onDemandUsed != nil {
				odUsed = *onDemandUsed
			}
			if odUsed >= *onDemand {
				exhausted = true
				exhaustReason = "月额度与按需额度均已用尽"
			}
		} else {
			exhausted = true
			exhaustReason = "月额度已用尽"
		}
	}
	if !exhausted && weekly != nil && weeklyUsed != nil && *weekly > 0 && *weeklyUsed >= *weekly {
		// Weekly hard cap alone cools SuperGrok (rolling week window).
		exhausted = true
		exhaustReason = "周额度已用尽"
	}

	unlimited := (monthly == nil || *monthly == 0) &&
		(onDemand == nil || *onDemand == 0) &&
		(weekly == nil || *weekly == 0)

	var monthlyPct *float64
	if monthly != nil && *monthly > 0 && used != nil {
		p := round2(100.0 * (*used) / (*monthly))
		monthlyPct = &p
	}
	var weeklyPct *float64
	if weekly != nil && *weekly > 0 && weeklyUsed != nil {
		p := round2(100.0 * (*weeklyUsed) / (*weekly))
		weeklyPct = &p
	}

	out := map[string]any{
		"ok":                   true,
		"monthly_limit":        monthly,
		"used":                 used,
		"remaining":            remaining,
		"usage_percent":        monthlyPct,
		"weekly_limit":         weekly,
		"weekly_used":          weeklyUsed,
		"weekly_remaining":     weeklyRemaining,
		"weekly_usage_percent": weeklyPct,
		"on_demand_cap":        onDemand,
		"on_demand_used":       onDemandUsed,
		"prepaid_balance":      prepaid,
		"exhausted":            exhausted,
		"unlimited_or_free":    unlimited,
		"billing_period_start": firstString(cfg, "billingPeriodStart", "billing_period_start"),
		"billing_period_end":   firstString(cfg, "billingPeriodEnd", "billing_period_end"),
		"raw":                  raw,
	}
	if exhaustReason != "" {
		out["exhaust_reason"] = exhaustReason
	}
	// History (last months) for SuperGrok trend — keep compact.
	if hist := billingHistory(cfg["history"]); len(hist) > 0 {
		out["history"] = hist
	}
	if !unlimited {
		out["display"] = map[string]any{"summary": formatSuperGrokSummary(out)}
	} else {
		out["display"] = map[string]any{"summary": "免费/促销（查 token 量）"}
	}
	return out
}

func firstMoney(cfg map[string]any, keys ...string) *float64 {
	for _, k := range keys {
		if v := money(cfg[k]); v != nil {
			return v
		}
	}
	return nil
}

func firstString(cfg map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := stringFromAny(cfg[k]); s != "" {
			return s
		}
	}
	return ""
}

func billingHistory(raw any) []map[string]any {
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		cycle, _ := m["billingCycle"].(map[string]any)
		if cycle == nil {
			cycle, _ = m["billing_cycle"].(map[string]any)
		}
		row := map[string]any{
			"included_used":  money(firstNonNil(m["includedUsed"], m["included_used"])),
			"on_demand_used": money(firstNonNil(m["onDemandUsed"], m["on_demand_used"])),
			"total_used":     money(firstNonNil(m["totalUsed"], m["total_used"])),
		}
		if cycle != nil {
			row["year"] = cycle["year"]
			row["month"] = cycle["month"]
		}
		out = append(out, row)
		if len(out) >= 6 {
			break
		}
	}
	return out
}

func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func formatSuperGrokSummary(out map[string]any) string {
	parts := []string{}
	// Monthly
	ml := floatPtr(out["monthly_limit"])
	mu := floatPtr(out["used"])
	if ml != nil && mu != nil {
		line := fmt.Sprintf("月 %s / %s", fmtUSD(mu), fmtUSD(ml))
		if p := floatPtr(out["usage_percent"]); p != nil {
			line += fmt.Sprintf(" · %.0f%%", *p)
		}
		if r := floatPtr(out["remaining"]); r != nil {
			line += fmt.Sprintf(" · 剩 %s", fmtUSD(r))
		}
		parts = append(parts, line)
	}
	// Weekly
	wl := floatPtr(out["weekly_limit"])
	wu := floatPtr(out["weekly_used"])
	if wl != nil && *wl > 0 {
		line := "周 "
		if wu != nil {
			line += fmt.Sprintf("%s / %s", fmtUSD(wu), fmtUSD(wl))
		} else {
			line += fmt.Sprintf("限额 %s", fmtUSD(wl))
		}
		if p := floatPtr(out["weekly_usage_percent"]); p != nil {
			line += fmt.Sprintf(" · %.0f%%", *p)
		}
		if r := floatPtr(out["weekly_remaining"]); r != nil {
			line += fmt.Sprintf(" · 剩 %s", fmtUSD(r))
		}
		parts = append(parts, line)
	}
	// On-demand
	odc := floatPtr(out["on_demand_cap"])
	odu := floatPtr(out["on_demand_used"])
	if odc != nil && *odc > 0 {
		if odu != nil {
			parts = append(parts, fmt.Sprintf("按需 %s / %s", fmtUSD(odu), fmtUSD(odc)))
		} else {
			parts = append(parts, fmt.Sprintf("按需限额 %s", fmtUSD(odc)))
		}
	}
	if len(parts) == 0 {
		return "SuperGrok"
	}
	return strings.Join(parts, " · ")
}

func floatPtr(v any) *float64 {
	switch t := v.(type) {
	case *float64:
		return t
	case float64:
		return &t
	case int:
		f := float64(t)
		return &f
	case int64:
		f := float64(t)
		return &f
	default:
		return nil
	}
}

// normalizeTokenUsageFields fills tokens_used / remaining / percent from each other.
func normalizeTokenUsageFields(out map[string]any) {
	if out == nil {
		return
	}
	limit := int64Of(out["tokens_limit"])
	remain := out["tokens_remaining"]
	used := out["tokens_used"]
	var remainN, usedN *int64
	if remain != nil {
		v := int64Of(remain)
		remainN = &v
	}
	if used != nil {
		v := int64Of(used)
		usedN = &v
	}
	if limit > 0 {
		if usedN == nil && remainN != nil {
			u := limit - *remainN
			if u < 0 {
				u = 0
			}
			out["tokens_used"] = u
			usedN = &u
		}
		if remainN == nil && usedN != nil {
			r := limit - *usedN
			if r < 0 {
				r = 0
			}
			out["tokens_remaining"] = r
			remainN = &r
		}
		if usedN != nil {
			out["tokens_usage_percent"] = round2(100.0 * float64(*usedN) / float64(limit))
		}
		// Full → exhausted
		if remainN != nil && *remainN <= 0 {
			out["exhausted"] = true
			if out["exhaust_reason"] == nil || out["exhaust_reason"] == "" {
				out["exhaust_reason"] = "token 用量已满"
			}
		} else if usedN != nil && *usedN >= limit {
			out["exhausted"] = true
			if out["exhaust_reason"] == nil || out["exhaust_reason"] == "" {
				out["exhaust_reason"] = "token 用量已满"
			}
		}
	}
}

// normalizeBillingUsageFields recomputes dollar percents after merges.
func normalizeBillingUsageFields(out map[string]any) {
	if out == nil {
		return
	}
	ml := floatPtr(out["monthly_limit"])
	mu := floatPtr(out["used"])
	if ml != nil && *ml > 0 && mu != nil {
		out["usage_percent"] = round2(100.0 * (*mu) / (*ml))
		rem := *ml - *mu
		if rem < 0 {
			rem = 0
		}
		out["remaining"] = rem
	}
	wl := floatPtr(out["weekly_limit"])
	wu := floatPtr(out["weekly_used"])
	if wl != nil && *wl > 0 && wu != nil {
		out["weekly_usage_percent"] = round2(100.0 * (*wu) / (*wl))
		rem := *wl - *wu
		if rem < 0 {
			rem = 0
		}
		out["weekly_remaining"] = rem
	}
}

func money(v any) *float64 {
	switch t := v.(type) {
	case float64:
		return &t
	case int:
		f := float64(t)
		return &f
	case int64:
		f := float64(t)
		return &f
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return &f
		}
	case map[string]any:
		if val, ok := t["val"]; ok {
			return money(val)
		}
	}
	return nil
}

func fmtUSD(v *float64) string {
	if v == nil {
		return "$0.00"
	}
	return fmt.Sprintf("$%.2f", *v)
}

func floatOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case *float64:
		if t == nil {
			return 0
		}
		return *t
	default:
		return 0
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func quotaTransport() http.RoundTripper {
	// Prefer explicit env proxy; DefaultTransport already honors HTTP(S)_PROXY.
	proxyURL := firstNonEmpty(
		os.Getenv("GROK2API_XAI_PROXY"),
		os.Getenv("GROK2API_PROXY"),
		os.Getenv("HTTPS_PROXY"),
		os.Getenv("HTTP_PROXY"),
		os.Getenv("https_proxy"),
		os.Getenv("http_proxy"),
	)
	base, _ := http.DefaultTransport.(*http.Transport)
	if base == nil {
		return http.DefaultTransport
	}
	tr := base.Clone()
	if proxyURL == "" {
		return tr
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return tr
	}
	tr.Proxy = http.ProxyURL(u)
	return tr
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
