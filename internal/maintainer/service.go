package maintainer

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

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
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	go s.loop()
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
	s.mu.Lock()
	s.forceNext = s.forceNext || force
	s.mu.Unlock()
	select {
	case s.runSoon <- struct{}{}:
	default:
	}
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
	startedAt := time.Now()
	result := map[string]any{
		"ok":             true,
		"force":          force,
		"implementation": "go",
		"at":             startedAt.Unix(),
	}
	if s.Store == nil {
		result["ok"] = false
		result["error"] = "store unavailable"
		return result
	}
	// maintenance lock best-effort
	if s.Redis != nil && s.Redis.Enabled() {
		ok, release, err := s.Redis.AcquireMaintenanceLock(ctx, "token_maintainer", 180*time.Second, true)
		if err == nil && ok {
			defer release()
		} else if err == nil && !ok {
			result["deferred_busy"] = true
			result["error"] = "maintenance slot busy — deferred"
			return result
		}
	}
	if n, err := s.Store.ExpireDueCooldowns(ctx, 500); err == nil {
		result["expired_cooldowns"] = n
	}
	batch := s.Batch
	if batch <= 0 {
		batch = 80
	}
	// Over-fetch candidates so filtering still fills the batch under force=false.
	fetchN := batch * 3
	if fetchN < 100 {
		fetchN = 100
	}
	if fetchN > 1000 {
		fetchN = 1000
	}
	rows, err := s.Store.ListRefreshableAccounts(ctx, fetchN)
	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
		return result
	}
	now := time.Now()
	skew := s.Skew
	if skew <= 0 {
		skew = 2 * time.Minute
	}
	candidates := make([]postgres.AccountRefreshRow, 0, batch)
	for _, row := range rows {
		rt := stringFrom(row.Payload, "refresh_token")
		if rt == "" {
			continue
		}
		if truthy(row.Payload["refresh_invalid"]) {
			continue
		}
		if !force {
			exp := accounts.ParseExpiresAt(row.Payload["expires_at"], stringFrom(row.Payload, "key"))
			if exp != nil && float64(now.Unix())+skew.Seconds() < *exp {
				continue
			}
		}
		candidates = append(candidates, row)
		if len(candidates) >= batch {
			break
		}
	}

	workers := s.Workers
	if workers <= 0 {
		workers = 8
	}
	if workers > len(candidates) && len(candidates) > 0 {
		workers = len(candidates)
	}
	if workers < 1 {
		workers = 1
	}

	type outcome struct {
		id              string
		ok              bool
		deleted         bool
		permanent       bool
		errText         string
		expiresAt       any
		hasRefreshToken bool
	}
	outCh := make(chan outcome, len(candidates))
	jobs := make(chan postgres.AccountRefreshRow, workers*2)
	var wg sync.WaitGroup
	oidcClient := s.OIDC
	if oidcClient == nil {
		oidcClient = &oidc.Client{}
	}

	workerFn := func() {
		defer wg.Done()
		for row := range jobs {
			if ctx.Err() != nil {
				outCh <- outcome{id: row.ID, ok: false, errText: "refresh cancelled"}
				continue
			}
			tokenData, err := oidcClient.RefreshAccessToken(ctx, row.Payload)
			if err != nil {
				permanent := false
				errText := err.Error()
				var re *oidc.RefreshError
				if asRefresh(err, &re) {
					permanent = re.Permanent
					errText = re.Error()
				}
				// Real-time DB sync: renew fail status (+ expired when permanent).
				status := "fail"
				if permanent {
					status = "invalid"
					_ = s.Store.MarkRefreshInvalid(ctx, row.ID, errText)
				}
				_ = s.Store.SaveRenewStatus(ctx, row.ID, false, status, errText, "token_maintainer")
				deleted := false
				if permanent && accounts.GetSSOValue(row.Payload) == "" {
					if ok, _ := s.Store.DeleteAccount(ctx, row.ID); ok {
						deleted = true
					}
				}
				outCh <- outcome{id: row.ID, ok: false, deleted: deleted, permanent: permanent, errText: errText}
				continue
			}
			newID, entry, err := oidc.EntryFromTokenResponse(tokenData, row.Payload)
			if err != nil {
				_ = s.Store.SaveRenewStatus(ctx, row.ID, false, "parse_fail", err.Error(), "token_maintainer")
				outCh <- outcome{id: row.ID, ok: false, errText: err.Error()}
				continue
			}
			if newID == "" {
				newID = row.ID
			}
			if newID != row.ID {
				_ = s.Store.UpsertAccount(ctx, newID, entry)
				_, _ = s.Store.DeleteAccount(ctx, row.ID)
			} else {
				_ = s.Store.UpsertAccount(ctx, row.ID, entry)
			}
			// Success path: clear cooldown + stamp last_renew_status=ok in DB.
			_, _ = s.Store.ClearAccountCooldown(ctx, newID)
			_ = s.Store.SaveRenewStatus(ctx, newID, true, "ok", "", "token_maintainer")
			outCh <- outcome{
				id: newID, ok: true,
				expiresAt:       entry["expires_at"],
				hasRefreshToken: stringFrom(entry, "refresh_token") != "",
			}
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go workerFn()
	}
	for _, row := range candidates {
		select {
		case <-ctx.Done():
			// remaining candidates not attempted
		case jobs <- row:
		}
	}
	close(jobs)
	wg.Wait()
	close(outCh)

	refreshed, failed, deleted := 0, 0, 0
	failedSample := []map[string]any{}
	results := make([]map[string]any, 0, len(candidates))
	for o := range outCh {
		row := map[string]any{"id": o.id, "account_id": o.id, "ok": o.ok}
		if o.expiresAt != nil {
			row["expires_at"] = o.expiresAt
		}
		if o.hasRefreshToken {
			row["has_refresh_token"] = true
		}
		if o.ok {
			refreshed++
			results = append(results, row)
			continue
		}
		failed++
		row["error"] = o.errText
		row["permanent"] = o.permanent
		row["deleted"] = o.deleted
		if o.deleted {
			deleted++
		}
		results = append(results, row)
		if len(failedSample) < 5 {
			failedSample = append(failedSample, map[string]any{
				"id": o.id, "error": o.errText, "permanent": o.permanent,
			})
		}
	}
	if rem, ok := s.computeMinRemainingSec(ctx); ok {
		result["min_remaining_sec"] = rem
	}
	result["next_wait_sec"] = s.Interval.Seconds()
	result["elapsed_ms"] = time.Since(startedAt).Milliseconds()
	result["adaptive"] = map[string]any{"batch": batch, "skew_sec": skew.Seconds(), "workers": workers}
	result["refresh"] = map[string]any{
		"attempted":     len(candidates),
		"refreshed":     refreshed,
		"failed":        failed,
		"skipped":       0,
		"deleted":       deleted,
		"failed_sample": failedSample,
		"workers":       workers,
		"batch":         batch,
	}
	// Per-row results for admin UI (selected renew + overview toast).
	result["results"] = results
	result["refreshed"] = refreshed
	result["attempted"] = len(candidates)
	result["failed"] = failed
	result["skipped"] = 0
	result["accounts_total"] = len(rows)
	slog.Info("token maintainer cycle",
		"attempted", len(candidates), "refreshed", refreshed, "failed", failed,
		"deleted", deleted, "workers", workers, "elapsed_ms", result["elapsed_ms"],
	)

	// Persist renew outcome to task_logs (admin「任务日志」).
	// progress_done=refreshed, progress_total=attempted (Python token_maintainer parity).
	if s.Store != nil {
		ref := 0
		att := 0
		fail := 0
		if m, ok := result["refresh"].(map[string]any); ok {
			if v, ok := m["refreshed"].(int); ok {
				ref = v
			}
			if v, ok := m["attempted"].(int); ok {
				att = v
			}
			if v, ok := m["failed"].(int); ok {
				fail = v
			}
		}
		// fallbacks from top-level if present
		if att == 0 {
			if v, ok := result["accounts_total"].(int); ok {
				att = v
			}
		}
		status := "done"
		okVal := true
		if result["ok"] == false {
			status = "error"
			okVal = false
		} else if fail > 0 && ref > 0 {
			status = "partial"
		} else if fail > 0 && ref == 0 {
			status = "error"
			okVal = false
		}
		if result["deferred_busy"] == true {
			// skip no-op busy slots
		} else if att > 0 || ref > 0 || fail > 0 || result["ok"] == false {
			summary := "Token 续期：成功 " + itoaMaint(ref) + "/" + itoaMaint(att)
			if fail > 0 {
				summary += " · 失败 " + itoaMaint(fail)
			}
			detail := map[string]any{}
			for _, k := range []string{"refresh", "force", "elapsed_ms", "adaptive", "min_remaining_sec", "expired_cooldowns", "accounts_total", "implementation"} {
				if v, ok := result[k]; ok {
					detail[k] = v
				}
			}
			taskID := "renew:" + time.Now().Format("2006-01-02")
			if force {
				taskID = "renew:force:" + time.Now().UTC().Format("20060102T150405")
			}
			okPtr := okVal
			_, _ = s.Store.WriteTask(ctx, "renew", status, summary, taskID, &okPtr, detail, ref, att, true)
		}
	}
	return result
}

// RunForIDs refreshes selected account IDs (admin selected renew / 续期选中).
// Always returns results[] so the frontend can clear busy rows and patch pool
// status immediately. Token upsert / renew status still write to PostgreSQL.
func (s *Service) RunForIDs(ctx context.Context, ids []string, force bool) map[string]any {
	startedAt := time.Now()
	result := map[string]any{
		"ok": true, "force": force, "implementation": "go",
		"at": startedAt.Unix(), "selected": true,
	}
	if s == nil || s.Store == nil {
		result["ok"] = false
		result["error"] = "store unavailable"
		result["results"] = []any{}
		return result
	}
	seen := map[string]struct{}{}
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		clean = append(clean, id)
		if len(clean) >= 500 {
			break
		}
	}
	if len(clean) == 0 {
		result["results"] = []any{}
		result["refreshed"] = 0
		result["attempted"] = 0
		result["failed"] = 0
		result["skipped"] = 0
		result["refresh"] = map[string]any{"attempted": 0, "refreshed": 0, "failed": 0, "skipped": 0}
		return result
	}

	type rowOut struct {
		id              string
		ok              bool
		skipped         bool
		deleted         bool
		permanent       bool
		errText         string
		expiresAt       any
		hasRefreshToken bool
	}
	workers := s.Workers
	if workers <= 0 {
		workers = 8
	}
	if workers > len(clean) {
		workers = len(clean)
	}
	if workers < 1 {
		workers = 1
	}
	skew := s.Skew
	if skew <= 0 {
		skew = 2 * time.Minute
	}
	outCh := make(chan rowOut, len(clean))
	jobs := make(chan string, workers*2)
	var wg sync.WaitGroup
	oidcClient := s.OIDC
	if oidcClient == nil {
		oidcClient = &oidc.Client{}
	}
	workerFn := func() {
		defer wg.Done()
		for id := range jobs {
			row, err := s.Store.GetAccountRefreshRow(ctx, id)
			if err != nil || row == nil {
				outCh <- rowOut{id: id, ok: false, errText: "account not found"}
				continue
			}
			payload := row.Payload
			if payload == nil {
				outCh <- rowOut{id: id, ok: false, errText: "account payload unavailable"}
				continue
			}
			rt := stringFrom(payload, "refresh_token")
			if rt == "" {
				outCh <- rowOut{id: id, ok: true, skipped: true, errText: "no refresh_token"}
				continue
			}
			if truthy(payload["refresh_invalid"]) {
				outCh <- rowOut{id: id, ok: false, permanent: true, errText: "refresh_token marked invalid"}
				continue
			}
			if !force {
				exp := accounts.ParseExpiresAt(payload["expires_at"], stringFrom(payload, "key"))
				if exp != nil && float64(time.Now().Unix())+skew.Seconds() < *exp {
					outCh <- rowOut{
						id: id, ok: true, skipped: true,
						expiresAt: payload["expires_at"], hasRefreshToken: true,
						errText: "not near expiry",
					}
					continue
				}
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
					_ = s.Store.MarkRefreshInvalid(ctx, id, errText)
				}
				_ = s.Store.SaveRenewStatus(ctx, id, false, status, errText, "manual_renew")
				deleted := false
				if permanent && accounts.GetSSOValue(payload) == "" {
					if ok, _ := s.Store.DeleteAccount(ctx, id); ok {
						deleted = true
					}
				}
				outCh <- rowOut{id: id, ok: false, deleted: deleted, permanent: permanent, errText: errText}
				continue
			}
			newID, entry, err := oidc.EntryFromTokenResponse(tokenData, payload)
			if err != nil {
				_ = s.Store.SaveRenewStatus(ctx, id, false, "parse_fail", err.Error(), "manual_renew")
				outCh <- rowOut{id: id, ok: false, errText: err.Error()}
				continue
			}
			if newID == "" {
				newID = id
			}
			if newID != id {
				_ = s.Store.UpsertAccount(ctx, newID, entry)
				_, _ = s.Store.DeleteAccount(ctx, id)
			} else {
				_ = s.Store.UpsertAccount(ctx, id, entry)
			}
			_, _ = s.Store.ClearAccountCooldown(ctx, newID)
			_ = s.Store.SaveRenewStatus(ctx, newID, true, "ok", "", "manual_renew")
			outCh <- rowOut{
				id: newID, ok: true,
				expiresAt:       entry["expires_at"],
				hasRefreshToken: stringFrom(entry, "refresh_token") != "",
			}
		}
	}
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go workerFn()
	}
	for _, id := range clean {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	close(outCh)

	refreshed, failed, skipped, deleted := 0, 0, 0, 0
	results := make([]map[string]any, 0, len(clean))
	for o := range outCh {
		row := map[string]any{"id": o.id, "account_id": o.id, "ok": o.ok, "skipped": o.skipped}
		if o.expiresAt != nil {
			row["expires_at"] = o.expiresAt
		}
		if o.hasRefreshToken {
			row["has_refresh_token"] = true
		}
		if o.ok && !o.skipped {
			refreshed++
		} else if o.ok && o.skipped {
			skipped++
			if o.errText != "" {
				row["message"] = o.errText
			}
		} else {
			failed++
			row["error"] = o.errText
		}
		if o.deleted {
			deleted++
			row["deleted"] = true
		}
		if o.permanent {
			row["permanent"] = true
		}
		results = append(results, row)
	}
	result["results"] = results
	result["refreshed"] = refreshed
	result["attempted"] = len(clean)
	result["failed"] = failed
	result["skipped"] = skipped
	result["refresh"] = map[string]any{
		"attempted": len(clean), "refreshed": refreshed, "failed": failed,
		"skipped": skipped, "deleted": deleted, "workers": workers,
	}
	result["elapsed_ms"] = time.Since(startedAt).Milliseconds()
	if s.Store != nil {
		status := "done"
		okVal := true
		if failed > 0 && refreshed > 0 {
			status = "partial"
		} else if failed > 0 && refreshed == 0 {
			status = "error"
			okVal = false
		}
		summary := "选中续期：成功 " + itoaMaint(refreshed) + "/" + itoaMaint(len(clean))
		if failed > 0 {
			summary += " · 失败 " + itoaMaint(failed)
		}
		if skipped > 0 {
			summary += " · 跳过 " + itoaMaint(skipped)
		}
		detail := map[string]any{"refresh": result["refresh"], "force": force, "selected": len(clean)}
		taskID := "renew:selected:" + time.Now().UTC().Format("20060102T150405")
		okPtr := okVal
		_, _ = s.Store.WriteTask(ctx, "renew", status, summary, taskID, &okPtr, detail, refreshed, len(clean), true)
	}
	return result
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
