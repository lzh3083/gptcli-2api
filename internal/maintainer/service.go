package maintainer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/hm2899/grokcli-2api/internal/accounts"
	"github.com/hm2899/grokcli-2api/internal/store/postgres"
	"github.com/hm2899/grokcli-2api/internal/store/redis"
	"github.com/hm2899/grokcli-2api/internal/upstream/oidc"
)

type Service struct {
	Store    *postgres.Connector
	Redis    *redis.Client
	OIDC     *oidc.Client
	Interval time.Duration
	Batch    int
	Workers  int
	Skew     time.Duration
	Enabled  func() bool
	IsLeader func() bool

	flight singleflight.Group

	mu        sync.Mutex
	started   bool
	stop      chan struct{}
	runSoon   chan struct{}
	last      map[string]any
	forceNext bool
}

func New(store *postgres.Connector, redisClient *redis.Client, oidcClient *oidc.Client) *Service {
	return &Service{
		Store:    store,
		Redis:    redisClient,
		OIDC:     oidcClient,
		Interval: envDurationSec("GROK2API_TOKEN_MAINTAIN_INTERVAL", 60*time.Second, 5*time.Second, 30*time.Minute),
		Batch:    envInt("GROK2API_TOKEN_REFRESH_BATCH", 80, 1, 500),
		Workers:  envInt("GROK2API_TOKEN_REFRESH_WORKERS", 8, 1, 32),
		Skew:     envDurationSec("GROK2API_TOKEN_REFRESH_SKEW", 180*time.Second, 30*time.Second, 2*time.Hour),
		Enabled:  func() bool { return true },
		IsLeader: func() bool { return true },
		stop:     make(chan struct{}),
		runSoon:  make(chan struct{}, 1),
		last:     map[string]any{"ok": true, "started": false},
	}
}

func (s *Service) Start() {
	slog.Info("maintainer disabled: pure registration mode active (CPA sovereign maintenance)")
	return
}

func (s *Service) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	close(s.stop)
	s.stop = make(chan struct{})
	s.mu.Unlock()
}

func (s *Service) RequestRunSoon(force bool) {
	return
}

func (s *Service) Status() map[string]any {
	if s == nil {
		return map[string]any{"enabled": false, "implementation": "go", "started": false, "running": false}
	}
	s.mu.Lock()
	lastCopy := map[string]any{}
	for k, v := range s.last {
		lastCopy[k] = v
	}
	started := s.started
	interval := s.Interval
	batch := s.Batch
	workers := s.Workers
	skew := s.Skew
	s.mu.Unlock()

	enabled := s.Enabled == nil || s.Enabled()
	isLeader := s.IsLeader == nil || s.IsLeader()
	running := started && enabled && isLeader
	out := map[string]any{
		"enabled":             enabled,
		"started":             started,
		"running":             running,
		"local_running":       running,
		"cluster_running":     running,
		"leader_running":      running,
		"implementation":      "go",
		"interval_sec":        interval.Seconds(),
		"next_wait_sec":       interval.Seconds(),
		"batch":               batch,
		"refresh_batch":       batch,
		"adaptive_batch":      batch,
		"workers":             workers,
		"refresh_workers":     workers,
		"refresh_skew_sec":    skew.Seconds(),
		"background_skew_sec": skew.Seconds(),
		"is_leader":           isLeader,
		"last":                lastCopy,
	}
	if rem, ok := s.computeMinRemainingSec(context.Background()); ok {
		out["min_remaining_sec"] = rem
		lastCopy["min_remaining_sec"] = rem
		out["last"] = lastCopy
	}
	return out
}

func (s *Service) enrichStatusMinRemaining(out map[string]any) {
	rem, ok := s.computeMinRemainingSec(context.Background())
	if !ok {
		return
	}
	out["min_remaining_sec"] = rem
	if last, ok := out["last"].(map[string]any); ok {
		last["min_remaining_sec"] = rem
		out["last"] = last
	}
}

func (s *Service) loop() {
	// short startup delay like Python
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-timer.C:
			s.maybeRun(false)
			timer.Reset(s.Interval)
		case <-s.runSoon:
			force := false
			s.mu.Lock()
			force = s.forceNext
			s.forceNext = false
			s.mu.Unlock()
			s.maybeRun(force)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.Interval)
		}
	}
}

func (s *Service) maybeRun(force bool) {
	if s.Enabled != nil && !s.Enabled() {
		return
	}
	if s.IsLeader != nil && !s.IsLeader() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result := s.RunOnce(ctx, force)
	s.mu.Lock()
	s.last = result
	s.mu.Unlock()
}

// RunOnce performs one normalize-ish + refresh cycle against PostgreSQL.
// Refresh is concurrent (workers) and every outcome is written to DB immediately:
// last_renew_* / renew_fail_count / pool_status(expired|normal).
func (s *Service) RunOnce(ctx context.Context, force bool) map[string]any {
	return map[string]any{
		"ok":             true,
		"force":          force,
		"implementation": "go",
		"at":             time.Now().Unix(),
		"refreshed":      0,
		"attempted":      0,
		"failed":         0,
		"message":        "纯注册机模式：已禁用所有账号探测与刷新逻辑，凭证由 CPA 独占维护",
	}
}

// RunForIDs refreshes selected account IDs (admin selected renew / 续期选中).
// Always returns results[] so the frontend can clear busy rows and patch pool
// status immediately. Token upsert / renew status still write to PostgreSQL.
func (s *Service) RunForIDs(ctx context.Context, ids []string, force bool) map[string]any {
	resList := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		resList = append(resList, map[string]any{
			"id":      id,
			"ok":      true,
			"status":  "skipped",
			"message": "纯注册机模式：跳过刷新，凭证由 CPA 独占维护",
		})
	}
	return map[string]any{
		"ok":             true,
		"force":          force,
		"implementation": "go",
		"at":             time.Now().Unix(),
		"selected":       true,
		"results":        resList,
		"refreshed":      0,
		"attempted":      0,
		"failed":         0,
		"skipped":        len(ids),
		"message":        "纯注册机模式：已禁用所有账号探测与刷新逻辑，凭证由 CPA 独占维护",
	}
}

func asRefresh(err error, target **oidc.RefreshError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*oidc.RefreshError); ok {
		*target = e
		return true
	}
	return false
}

func stringFrom(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func truthy(v any) bool {
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

func (s *Service) computeMinRemainingSec(ctx context.Context) (float64, bool) {
	if s == nil || s.Store == nil {
		return 0, false
	}
	rows, err := s.Store.ListRefreshableAccounts(ctx, 200)
	if err != nil || len(rows) == 0 {
		return 0, false
	}
	now := float64(time.Now().Unix())
	minRem := 0.0
	found := false
	for _, row := range rows {
		exp := accounts.ParseExpiresAt(row.Payload["expires_at"], stringFrom(row.Payload, "key"))
		if exp == nil {
			continue
		}
		rem := *exp - now
		if !found || rem < minRem {
			minRem = rem
			found = true
		}
	}
	return minRem, found
}

func envDurationSec(name string, fallback, min, max time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	sec, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	d := time.Duration(sec * float64(time.Second))
	if d < min {
		return min
	}
	if d > max {
		return max
	}
	return d
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

func itoaMaint(n int) string {
	return strconv.Itoa(n)
}

// RefreshOutcome encapsulates the result of an account token refresh attempt.
type RefreshOutcome struct {
	ID              string
	Ok              bool
	Skipped         bool
	Deleted         bool
	Permanent       bool
	ErrText         string
	ExpiresAt       any
	HasRefreshToken bool
}

// refreshSingleAccount performs a safe, deduplicated, distributed-locked token refresh.
func (s *Service) refreshSingleAccount(ctx context.Context, accountID string, cachedPayload map[string]any, force bool, source string) RefreshOutcome {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return RefreshOutcome{Ok: false, ErrText: "empty account id"}
	}

	// 1. SingleFlight within process: coalesce concurrent refreshes for the same account
	key := "refresh:" + accountID
	val, err, _ := s.flight.Do(key, func() (any, error) {
		return s.doLockedRefresh(ctx, accountID, cachedPayload, force, source), nil
	})
	if err != nil {
		return RefreshOutcome{ID: accountID, Ok: false, ErrText: err.Error()}
	}
	if out, ok := val.(RefreshOutcome); ok {
		return out
	}
	return RefreshOutcome{ID: accountID, Ok: false, ErrText: "invalid refresh outcome"}
}

func (s *Service) doLockedRefresh(ctx context.Context, accountID string, cachedPayload map[string]any, force bool, source string) RefreshOutcome {
	// A. Distributed mutex via Redis (if enabled)
	lockToken := fmt.Sprintf("%d-%s", time.Now().UnixNano(), accountID)
	lockKey := ""
	haveRedisLock := false
	if s.Redis != nil && s.Redis.Enabled() {
		lockKey = fmt.Sprintf("lock:refresh:%s", sanitizeLockKey(accountID))
		acquired, err := s.Redis.TryAcquireLock(ctx, lockKey, lockToken, 30*time.Second)
		if err == nil && !acquired {
			// Another cluster worker/instance is refreshing this account right now.
			return RefreshOutcome{ID: accountID, Ok: true, Skipped: true, ErrText: "concurrent refresh in-flight by another worker"}
		}
		if acquired {
			haveRedisLock = true
			defer func() {
				if haveRedisLock {
					_, _ = s.Redis.ReleaseLock(context.Background(), lockKey, lockToken)
				}
			}()
		}
	}

	// B. Double-Check Lock: Always load the latest payload from PostgreSQL after obtaining lock
	var payload map[string]any
	if s.Store != nil {
		row, err := s.Store.GetAccountRefreshRow(ctx, accountID)
		if err != nil || row == nil {
			return RefreshOutcome{ID: accountID, Ok: false, ErrText: "account not found in store"}
		}
		payload = row.Payload
	} else {
		payload = cachedPayload
	}
	if payload == nil {
		return RefreshOutcome{ID: accountID, Ok: false, ErrText: "account payload unavailable"}
	}

	// C. Skip externally managed accounts (e.g. managed by upstream CLIProxyAPI)
	if truthy(payload["disable_auto_refresh"]) || truthy(payload["external_managed"]) {
		return RefreshOutcome{ID: accountID, Ok: true, Skipped: true, ErrText: "account is externally managed (auto-refresh disabled)"}
	}

	// D. Check permanent invalid marker
	if truthy(payload["refresh_invalid"]) {
		return RefreshOutcome{ID: accountID, Ok: false, Permanent: true, ErrText: "refresh_token marked invalid"}
	}

	rt := stringFrom(payload, "refresh_token")
	if rt == "" {
		return RefreshOutcome{ID: accountID, Ok: true, Skipped: true, ErrText: "no refresh_token"}
	}

	// E. Debounce window: if refreshed within last 2 minutes, avoid sending duplicate refresh request
	nowUnix := float64(time.Now().Unix())
	if lastRenew, ok := parseNumericFloat(payload["last_renew_at"]); ok && lastRenew > 0 {
		if nowUnix-lastRenew < 120 {
			return RefreshOutcome{
				ID:              accountID,
				Ok:              true,
				Skipped:         true,
				ExpiresAt:       payload["expires_at"],
				HasRefreshToken: true,
				ErrText:         "recently refreshed within 2 minutes",
			}
		}
	}

	// F. Skew check (for non-force periodic check)
	skew := s.Skew
	if skew <= 0 {
		skew = 2 * time.Minute
	}
	if !force {
		exp := accounts.ParseExpiresAt(payload["expires_at"], stringFrom(payload, "key"))
		if exp != nil && nowUnix+skew.Seconds() < *exp {
			return RefreshOutcome{
				ID:              accountID,
				Ok:              true,
				Skipped:         true,
				ExpiresAt:       payload["expires_at"],
				HasRefreshToken: true,
				ErrText:         "not near expiry",
			}
		}
	}

	// G. Execute OIDC refresh
	oidcClient := s.OIDC
	if oidcClient == nil {
		oidcClient = &oidc.Client{}
	}
	tokenData, err := oidcClient.RefreshAccessToken(ctx, payload)
	if err != nil {
		permanent := false
		errText := err.Error()
		var re *oidc.RefreshError
		if asRefresh(err, &re) {
			permanent = re.Permanent
			errText = re.Error()
		}
		status := "fail"
		if permanent {
			status = "invalid"
			if s.Store != nil {
				_ = s.Store.MarkRefreshInvalid(ctx, accountID, errText)
				_, _ = s.Store.SetAccountEnabled(ctx, accountID, false)
			}
		}
		if s.Store != nil {
			_ = s.Store.SaveRenewStatus(ctx, accountID, false, status, errText, source)
		}
		deleted := false
		if permanent && accounts.GetSSOValue(payload) == "" && s.Store != nil {
			if ok, _ := s.Store.DeleteAccount(ctx, accountID); ok {
				deleted = true
			}
		}
		return RefreshOutcome{ID: accountID, Ok: false, Deleted: deleted, Permanent: permanent, ErrText: errText}
	}

	// H. Parse updated tokens and atomically update PostgreSQL
	newID, entry, err := oidc.EntryFromTokenResponse(tokenData, payload)
	if err != nil {
		if s.Store != nil {
			_ = s.Store.SaveRenewStatus(ctx, accountID, false, "parse_fail", err.Error(), source)
		}
		return RefreshOutcome{ID: accountID, Ok: false, ErrText: err.Error()}
	}
	if newID == "" {
		newID = accountID
	}
	entry["last_renew_at"] = time.Now().Unix()

	if s.Store != nil {
		if newID != accountID {
			_ = s.Store.UpsertAccount(ctx, newID, entry)
			_, _ = s.Store.DeleteAccount(ctx, accountID)
		} else {
			_ = s.Store.UpsertAccount(ctx, accountID, entry)
		}
		_, _ = s.Store.ClearAccountCooldown(ctx, newID)
		_ = s.Store.SaveRenewStatus(ctx, newID, true, "ok", "", source)
	}

	// Sovereign ownership mode: keep disk CPA auth files strictly aligned
	_ = accounts.SyncCredentialToDisk(entry)

	return RefreshOutcome{
		ID:              newID,
		Ok:              true,
		ExpiresAt:       entry["expires_at"],
		HasRefreshToken: stringFrom(entry, "refresh_token") != "",
	}
}

func sanitizeLockKey(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	res := b.String()
	if len(res) > 80 {
		res = res[len(res)-80:]
	}
	return res
}

func parseNumericFloat(v any) (float64, bool) {
	if v == nil {
		return 0, false
	}
	switch val := v.(type) {
	case float64:
		return val, true
	case int64:
		return float64(val), true
	case int:
		return float64(val), true
	case json.Number:
		if f, err := val.Float64(); err == nil {
			return f, true
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}
