package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/hm2899/grokcli-2api/internal/pool"
	"golang.org/x/sync/singleflight"
)

// Short in-process candidate window cache to avoid repeated full-row scans under burst.
// TTL is intentionally tiny so cooldown/disable changes still show up quickly.
var (
	candidateCacheMu   sync.Mutex
	candidateCacheAt   time.Time
	candidateCacheData []pool.Candidate
	candidateFlight    singleflight.Group
)

// Slightly longer than previous 400ms: still fresh for kick/cooldown, fewer PG stampedes.
const candidateCacheTTL = 800 * time.Millisecond

// InvalidatePoolCandidateCache drops the hot-path candidate window so the next
// pick re-reads PG (cooldowns / model blocks from empty-output land immediately).
func InvalidatePoolCandidateCache() {
	candidateCacheMu.Lock()
	candidateCacheAt = time.Time{}
	candidateCacheData = nil
	candidateCacheMu.Unlock()
}

// GetPoolCandidate loads one account as a pick candidate (sticky TTFT / prompt-cache path).
func (c *Connector) GetPoolCandidate(ctx context.Context, accountID string) (*pool.Candidate, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, nil
	}
	row := c.Pool.QueryRow(ctx, `
		SELECT a.id, a.payload, a.email, a.user_id, a.team_id, a.expires_at,
		       COALESCE(ap.enabled, true), COALESCE(ap.disabled_for_quota, false),
		       ap.cooldown_until, COALESCE(ap.blocked_models, '{}'::jsonb),
		       COALESCE(ap.request_count, 0), COALESCE(ap.weight, 1)
		FROM accounts a
		LEFT JOIN account_pool ap ON ap.account_id = a.id
		WHERE a.id = $1
		LIMIT 1`, accountID)
	var candidate pool.Candidate
	var payloadBytes, blockedBytes []byte
	var email, userID, teamID *string
	var expiresAt, cooldownUntil *time.Time
	if err := row.Scan(&candidate.ID, &payloadBytes, &email, &userID, &teamID, &expiresAt, &candidate.Enabled, &candidate.DisabledForQuota, &cooldownUntil, &blockedBytes, &candidate.RequestCount, &candidate.Weight); err != nil {
		return nil, err
	}
	payload := decodeMap(payloadBytes)
	candidate.Token, _ = firstString(payload, "key", "access_token", "token")
	candidate.Email = stringValue(email, stringFromMap(payload, "email"))
	candidate.UserID = stringValue(userID, firstMapString(payload, "user_id", "principal_id"))
	candidate.TeamID = stringValue(teamID, stringFromMap(payload, "team_id"))
	candidate.ExpiresAt = expiresAt
	candidate.CooldownUntil = cooldownUntil
	candidate.BlockedModels = decodeMap(blockedBytes)
	if strings.TrimSpace(candidate.Token) == "" {
		return nil, nil
	}
	return &candidate, nil
}

func (c *Connector) ListPoolCandidates(ctx context.Context) ([]pool.Candidate, error) {
	candidateCacheMu.Lock()
	if time.Since(candidateCacheAt) < candidateCacheTTL && len(candidateCacheData) > 0 {
		out := make([]pool.Candidate, len(candidateCacheData))
		copy(out, candidateCacheData)
		candidateCacheMu.Unlock()
		return out, nil
	}
	candidateCacheMu.Unlock()

	// singleflight: concurrent TTFT requests share one PG scan on cache miss.
	v, err, _ := candidateFlight.Do("pool-candidates", func() (any, error) {
		// Re-check cache inside flight (another waiter may have filled it).
		candidateCacheMu.Lock()
		if time.Since(candidateCacheAt) < candidateCacheTTL && len(candidateCacheData) > 0 {
			out := make([]pool.Candidate, len(candidateCacheData))
			copy(out, candidateCacheData)
			candidateCacheMu.Unlock()
			return out, nil
		}
		candidateCacheMu.Unlock()

		rows, err := c.Pool.Query(ctx, `
			SELECT a.id, a.payload, a.email, a.user_id, a.team_id, a.expires_at,
			       COALESCE(ap.enabled, true), COALESCE(ap.disabled_for_quota, false),
			       ap.cooldown_until, COALESCE(ap.blocked_models, '{}'::jsonb),
			       COALESCE(ap.request_count, 0), COALESCE(ap.weight, 1)
			FROM accounts a
			LEFT JOIN account_pool ap ON ap.account_id = a.id
			WHERE COALESCE(ap.enabled, true) = true
			  AND COALESCE(ap.disabled_for_quota, false) = false
			  AND (ap.cooldown_until IS NULL OR ap.cooldown_until <= now())
			  AND (a.expires_at IS NULL OR a.expires_at > now())
			  AND (
			        COALESCE(a.payload->>'key', '') <> ''
			     OR COALESCE(a.payload->>'access_token', '') <> ''
			     OR COALESCE(a.payload->>'token', '') <> ''
			  )
			ORDER BY COALESCE(ap.weight, 1) DESC, COALESCE(ap.fail_count, 0) ASC, COALESCE(ap.request_count, 0) ASC, a.id ASC
			LIMIT 48`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []pool.Candidate{}
		for rows.Next() {
			var candidate pool.Candidate
			var payloadBytes, blockedBytes []byte
			var email, userID, teamID *string
			var expiresAt, cooldownUntil *time.Time
			if err := rows.Scan(&candidate.ID, &payloadBytes, &email, &userID, &teamID, &expiresAt, &candidate.Enabled, &candidate.DisabledForQuota, &cooldownUntil, &blockedBytes, &candidate.RequestCount, &candidate.Weight); err != nil {
				return nil, err
			}
			payload := decodeMap(payloadBytes)
			candidate.Token, _ = firstString(payload, "key", "access_token", "token")
			candidate.Email = stringValue(email, stringFromMap(payload, "email"))
			candidate.UserID = stringValue(userID, firstMapString(payload, "user_id", "principal_id"))
			candidate.TeamID = stringValue(teamID, stringFromMap(payload, "team_id"))
			candidate.ExpiresAt = expiresAt
			candidate.CooldownUntil = cooldownUntil
			candidate.BlockedModels = decodeMap(blockedBytes)
			if strings.TrimSpace(candidate.Token) != "" {
				out = append(out, candidate)
			}
		}
		if err := rows.Err(); err != nil {
			return out, err
		}
		candidateCacheMu.Lock()
		candidateCacheData = append([]pool.Candidate(nil), out...)
		candidateCacheAt = time.Now()
		candidateCacheMu.Unlock()
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	list, _ := v.([]pool.Candidate)
	out := make([]pool.Candidate, len(list))
	copy(out, list)
	return out, nil
}

// InvalidateCandidateCache drops the short-lived pick window (after kick/disable).
func (c *Connector) InvalidateCandidateCache() {
	candidateCacheMu.Lock()
	candidateCacheData = nil
	candidateCacheAt = time.Time{}
	candidateCacheMu.Unlock()
}

type PoolFailure struct {
	AccountID            string
	Error                string
	StatusCode           *int
	CooldownUntil        *time.Time
	CooldownReason       string
	CooldownCode         string
	CooldownModel        string
	CooldownTokensActual *int64
	CooldownTokensLimit  *int64
	BlockedModel         string
	BlockedUntil         *time.Time
	Detail               map[string]any
}

func (c *Connector) ReportPoolSuccess(ctx context.Context, accountID string, preserveCooldown bool) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
	}
	if preserveCooldown {
		// Successful chat traffic must not wipe a still-active free-usage / rate-limit
		// cooldown, but MUST heal stale markers after cooldown_until has elapsed:
		// leftover pool_status='cooldown' / cooldown_count>0 / status_stack would
		// otherwise keep the admin UI stuck on "冷却中" forever.
		_, err := c.Pool.Exec(ctx, `
			INSERT INTO account_pool (account_id, request_count, success_count, last_used_at, extra, updated_at)
			VALUES ($1, 1, 1, now(), '{}'::jsonb, now())
			ON CONFLICT (account_id) DO UPDATE SET
				request_count = account_pool.request_count + 1,
				success_count = account_pool.success_count + 1,
				last_used_at = now(),
				extra = CASE
					WHEN COALESCE(account_pool.pool_status, '') = 'cooldown' THEN
						jsonb_set(COALESCE(account_pool.extra, '{}'::jsonb), '{consecutive_fails}', '0'::jsonb, true)
					ELSE
						(COALESCE(account_pool.extra, '{}'::jsonb) - 'status_stack' - 'cooldown_count')
						|| jsonb_build_object('consecutive_fails', 0)
				END,
				cooldown_count = CASE
					WHEN COALESCE(account_pool.pool_status, '') = 'cooldown' THEN account_pool.cooldown_count
					ELSE 0
				END,
				cooldown_reason = CASE
					WHEN COALESCE(account_pool.pool_status, '') = 'cooldown' THEN account_pool.cooldown_reason
					ELSE NULL
				END,
				cooldown_code = CASE
					WHEN COALESCE(account_pool.pool_status, '') = 'cooldown' THEN account_pool.cooldown_code
					ELSE NULL
				END,
				cooldown_model = CASE
					WHEN COALESCE(account_pool.pool_status, '') = 'cooldown' THEN account_pool.cooldown_model
					ELSE NULL
				END,
				cooldown_tokens_actual = CASE
					WHEN COALESCE(account_pool.pool_status, '') = 'cooldown' THEN account_pool.cooldown_tokens_actual
					ELSE NULL
				END,
				cooldown_tokens_limit = CASE
					WHEN COALESCE(account_pool.pool_status, '') = 'cooldown' THEN account_pool.cooldown_tokens_limit
					ELSE NULL
				END,
				last_error = CASE
					WHEN COALESCE(account_pool.pool_status, '') = 'cooldown' THEN account_pool.last_error
					ELSE NULL
				END,
				pool_status = CASE
					WHEN account_pool.enabled = false OR account_pool.disabled_for_quota = true THEN 'disabled'
					WHEN COALESCE(account_pool.pool_status, '') = 'cooldown' THEN 'cooldown'
					WHEN COALESCE(account_pool.blocked_models, '{}'::jsonb) <> '{}'::jsonb THEN 'model_blocked'
					ELSE 'normal'
				END,
				updated_at = now()`, accountID)
		if err == nil {
			c.InvalidatePoolSummaryCache()
		}
		return err
	}
	_, err := c.Pool.Exec(ctx, `
		INSERT INTO account_pool (account_id, request_count, success_count, last_used_at, pool_status, extra, updated_at)
		VALUES ($1, 1, 1, now(), 'normal', '{}'::jsonb, now())
		ON CONFLICT (account_id) DO UPDATE SET
			request_count = account_pool.request_count + 1,
			success_count = account_pool.success_count + 1,
			last_used_at = now(),
			last_error = NULL,
			cooldown_until = NULL,
			cooldown_reason = NULL,
			cooldown_code = NULL,
			cooldown_model = NULL,
			cooldown_tokens_actual = NULL,
			cooldown_tokens_limit = NULL,
			cooldown_count = 0,
			pool_status = CASE
				WHEN account_pool.enabled = false OR account_pool.disabled_for_quota = true THEN 'disabled'
				WHEN COALESCE(account_pool.blocked_models, '{}'::jsonb) <> '{}'::jsonb THEN 'model_blocked'
				ELSE 'normal'
			END,
			extra = (COALESCE(account_pool.extra, '{}'::jsonb) - 'status_stack' - 'cooldown_count')
				|| jsonb_build_object('consecutive_fails', 0),
			updated_at = now()`, accountID)
	if err == nil {
		c.InvalidatePoolSummaryCache()
	}
	return err
}

func (c *Connector) ReportPoolFailure(ctx context.Context, failure PoolFailure) error {
	failure.AccountID = strings.TrimSpace(failure.AccountID)
	if failure.AccountID == "" {
		return nil
	}
	detailBytes, err := json.Marshal(failure.Detail)
	if err != nil {
		return err
	}
	blockedBytes := []byte(`{}`)
	clearBlocks := false
	if model := strings.TrimSpace(failure.BlockedModel); model != "" {
		blocked := map[string]any{model: true}
		if failure.BlockedUntil != nil {
			blocked[model] = failure.BlockedUntil.Unix()
		}
		blockedBytes, err = json.Marshal(blocked)
		if err != nil {
			return err
		}
	} else if failure.CooldownUntil != nil {
		// free-usage cool is account-wide only — drop leftover model soft-blocks
		// so the row is tagged cooldown (not 模型封禁) in DB-backed stats/tags.
		code := strings.ToLower(strings.TrimSpace(failure.CooldownCode))
		reason := strings.ToLower(strings.TrimSpace(failure.CooldownReason) + " " + strings.TrimSpace(failure.Error))
		if strings.Contains(code, "free-usage") ||
			strings.Contains(reason, "free-usage") ||
			strings.Contains(reason, "free usage") ||
			strings.Contains(reason, "额度用完") ||
			strings.Contains(reason, "免费额度") {
			clearBlocks = true
		}
		if fc, _ := failure.Detail["failure_class"].(string); strings.Contains(strings.ToLower(fc), "free-usage") {
			clearBlocks = true
		}
	}
	// $12 = clearBlocks: when true replace blocked_models with {}; when false merge $9.
	_, err = c.Pool.Exec(ctx, `
		INSERT INTO account_pool (
			account_id, request_count, fail_count, last_used_at, last_error,
			cooldown_until, pool_status, cooldown_count, cooldown_reason,
			cooldown_code, cooldown_model, cooldown_tokens_actual, cooldown_tokens_limit,
			blocked_models, extra, updated_at
		) VALUES (
			$1, 1, 1, now(), $2,
			$3,
			CASE
				WHEN $9::jsonb <> '{}'::jsonb THEN 'model_blocked'
				WHEN $3::timestamptz IS NULL THEN 'normal'
				ELSE 'cooldown'
			END,
			CASE WHEN $3::timestamptz IS NULL THEN 0 ELSE 1 END, $4,
			$5, $6, $7, $8,
			$9::jsonb, jsonb_build_object('last_status_code', $10::int, 'cooldown_detail', $11::jsonb, 'consecutive_fails', 1), now()
		)
		ON CONFLICT (account_id) DO UPDATE SET
			request_count = account_pool.request_count + 1,
			fail_count = account_pool.fail_count + 1,
			last_used_at = now(),
			last_error = COALESCE($2, account_pool.last_error),
			cooldown_until = COALESCE($3, account_pool.cooldown_until),
			pool_status = CASE
				WHEN account_pool.enabled = false OR account_pool.disabled_for_quota = true THEN 'disabled'
				WHEN NOT $12::bool AND (COALESCE(account_pool.blocked_models, '{}'::jsonb) || $9::jsonb) <> '{}'::jsonb THEN 'model_blocked'
				WHEN COALESCE($3, account_pool.cooldown_until) IS NOT NULL
					AND COALESCE($3, account_pool.cooldown_until) > now() THEN 'cooldown'
				WHEN COALESCE(account_pool.pool_status, '') = 'cooldown'
					AND (account_pool.cooldown_until IS NOT NULL AND account_pool.cooldown_until > now()) THEN 'cooldown'
				WHEN $3::timestamptz IS NOT NULL THEN 'cooldown'
				WHEN $12::bool THEN 'normal'
				ELSE 'normal'
			END,
			cooldown_count = CASE
				WHEN $3::timestamptz IS NOT NULL THEN account_pool.cooldown_count + 1
				WHEN COALESCE(account_pool.pool_status, '') = 'cooldown' THEN GREATEST(account_pool.cooldown_count, 1)
				ELSE account_pool.cooldown_count
			END,
			cooldown_reason = COALESCE($4, account_pool.cooldown_reason),
			cooldown_code = COALESCE($5, account_pool.cooldown_code),
			cooldown_model = COALESCE($6, account_pool.cooldown_model),
			cooldown_tokens_actual = COALESCE($7, account_pool.cooldown_tokens_actual),
			cooldown_tokens_limit = COALESCE($8, account_pool.cooldown_tokens_limit),
			blocked_models = CASE
				WHEN $12::bool THEN '{}'::jsonb
				WHEN $9::jsonb = '{}'::jsonb THEN COALESCE(account_pool.blocked_models, '{}'::jsonb)
				ELSE COALESCE(account_pool.blocked_models, '{}'::jsonb) || $9::jsonb
			END,
			extra = COALESCE(account_pool.extra, '{}'::jsonb) || jsonb_build_object(
				'last_status_code', $10::int,
				'cooldown_detail', $11::jsonb,
				'consecutive_fails', COALESCE((account_pool.extra->>'consecutive_fails')::int, 0) + 1
			),
			updated_at = now()`, failure.AccountID, nilIfEmpty(failure.Error), failure.CooldownUntil, nilIfEmpty(failure.CooldownReason), nilIfEmpty(failure.CooldownCode), nilIfEmpty(failure.CooldownModel), failure.CooldownTokensActual, failure.CooldownTokensLimit, blockedBytes, failure.StatusCode, detailBytes, clearBlocks)
	if err == nil {
		c.InvalidateCandidateCache()
		c.InvalidatePoolSummaryCache()
	}
	return err
}

func (c *Connector) BlockPoolModel(ctx context.Context, accountID, model string, until *time.Time) error {
	accountID = strings.TrimSpace(accountID)
	model = strings.TrimSpace(model)
	if accountID == "" || model == "" {
		return nil
	}
	// Match Python account_pool.block_model shape so UI/picker/SQL all agree.
	entry := map[string]any{
		"blocked_at": float64(time.Now().Unix()),
		"source":     "go_model_health",
	}
	if until != nil {
		u := float64(until.Unix())
		entry["until"] = u
		entry["ttl_sec"] = u - float64(time.Now().Unix())
		entry["reason"] = "model temporarily blocked"
	} else {
		entry["blocked"] = true
		entry["reason"] = "model permanently blocked"
	}
	blocked, err := json.Marshal(map[string]any{model: entry})
	if err != nil {
		return err
	}
	_, err = c.Pool.Exec(ctx, `
		INSERT INTO account_pool (account_id, blocked_models, pool_status, extra, updated_at)
		VALUES ($1, $2::jsonb, 'model_blocked', '{}'::jsonb, now())
		ON CONFLICT (account_id) DO UPDATE SET
			blocked_models = COALESCE(account_pool.blocked_models, '{}'::jsonb) || $2::jsonb,
			pool_status = CASE
				WHEN account_pool.enabled = false OR account_pool.disabled_for_quota = true THEN 'disabled'
				ELSE 'model_blocked'
			END,
			updated_at = now()`, accountID, blocked)
	if err == nil {
		c.InvalidateCandidateCache()
	}
	return err
}

// UnblockPoolModel removes one model (or all when model empty) from blocked_models.
func (c *Connector) UnblockPoolModel(ctx context.Context, accountID, model string) error {
	accountID = strings.TrimSpace(accountID)
	model = strings.TrimSpace(model)
	if accountID == "" {
		return nil
	}
	if model == "" {
		_, err := c.Pool.Exec(ctx, `
			UPDATE account_pool
			SET blocked_models = '{}'::jsonb,
			    pool_status = CASE
					WHEN enabled = false OR disabled_for_quota = true THEN 'disabled'
					WHEN cooldown_until IS NOT NULL AND cooldown_until > now() THEN 'cooldown'
					ELSE 'normal'
				END,
			    updated_at = now()
			WHERE account_id = $1`, accountID)
		return err
	}
	_, err := c.Pool.Exec(ctx, `
		UPDATE account_pool
		SET blocked_models = COALESCE(blocked_models, '{}'::jsonb) - $2,
		    pool_status = CASE
				WHEN enabled = false OR disabled_for_quota = true THEN 'disabled'
				WHEN cooldown_until IS NOT NULL AND cooldown_until > now() THEN 'cooldown'
				WHEN (COALESCE(blocked_models, '{}'::jsonb) - $2) = '{}'::jsonb THEN 'normal'
				ELSE 'model_blocked'
			END,
		    updated_at = now()
		WHERE account_id = $1`, accountID, model)
	if err == nil {
		c.InvalidateCandidateCache()
	}
	return err
}

func stringValue(ptr *string, fallback string) string {
	if ptr != nil && strings.TrimSpace(*ptr) != "" {
		return *ptr
	}
	return fallback
}

// SetAccountEnabled toggles pool enabled flag. Re-enable clears cooldown/quota/model blocks.
func (c *Connector) SetAccountEnabled(ctx context.Context, accountID string, enabled bool) (map[string]any, error) {
	c.InvalidateCandidateCache()
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, errors.New("account id required")
	}
	if err := c.ensureAccountExists(ctx, accountID); err != nil {
		return nil, err
	}
	if enabled {
		_, err := c.Pool.Exec(ctx, `
			INSERT INTO account_pool (account_id, enabled, pool_status, cooldown_count, extra, updated_at)
			VALUES ($1, true, 'normal', 0, '{}'::jsonb, now())
			ON CONFLICT (account_id) DO UPDATE SET
				enabled = true,
				disabled_for_quota = false,
				disabled_reason = NULL,
				quota_disabled_at = NULL,
				quota_source = NULL,
				blocked_models = '{}'::jsonb,
				cooldown_until = NULL,
				cooldown_reason = NULL,
				cooldown_code = NULL,
				cooldown_model = NULL,
				cooldown_tokens_actual = NULL,
				cooldown_tokens_limit = NULL,
				cooldown_count = 0,
				last_error = NULL,
				pool_status = 'normal',
				extra = (COALESCE(account_pool.extra, '{}'::jsonb) - 'status_stack' - 'cooldown_count')
					|| jsonb_build_object('consecutive_fails', 0),
				updated_at = now()
		`, accountID)
		if err != nil {
			return nil, err
		}
	} else {
		_, err := c.Pool.Exec(ctx, `
			INSERT INTO account_pool (account_id, enabled, pool_status, extra, updated_at)
			VALUES ($1, false, 'disabled', '{}'::jsonb, now())
			ON CONFLICT (account_id) DO UPDATE SET
				enabled = false,
				pool_status = 'disabled',
				updated_at = now()
		`, accountID)
		if err != nil {
			return nil, err
		}
	}
	return c.GetAccountPoolView(ctx, accountID)
}

// ClearAccountCooldown clears durable cooldown so the account re-enters rotation.
// Also resets count/stack markers so admin UI stops showing "冷却中" after recover.
func (c *Connector) ClearAccountCooldown(ctx context.Context, accountID string) (map[string]any, error) {
	c.InvalidateCandidateCache()
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, errors.New("account id required")
	}
	if err := c.ensureAccountExists(ctx, accountID); err != nil {
		return nil, err
	}
	_, err := c.Pool.Exec(ctx, `
		INSERT INTO account_pool (account_id, pool_status, cooldown_count, extra, updated_at)
		VALUES ($1, 'normal', 0, '{}'::jsonb, now())
		ON CONFLICT (account_id) DO UPDATE SET
			cooldown_until = NULL,
			cooldown_reason = NULL,
			cooldown_code = NULL,
			cooldown_model = NULL,
			cooldown_tokens_actual = NULL,
			cooldown_tokens_limit = NULL,
			cooldown_count = 0,
			last_error = NULL,
			pool_status = CASE
				WHEN account_pool.enabled = false OR account_pool.disabled_for_quota = true THEN 'disabled'
				WHEN COALESCE(account_pool.blocked_models, '{}'::jsonb) <> '{}'::jsonb THEN 'model_blocked'
				ELSE 'normal'
			END,
			extra = (COALESCE(account_pool.extra, '{}'::jsonb) - 'status_stack' - 'cooldown_count')
				|| jsonb_build_object('consecutive_fails', 0),
			updated_at = now()
	`, accountID)
	if err != nil {
		return nil, err
	}
	c.InvalidatePoolSummaryCache()
	return c.GetAccountPoolView(ctx, accountID)
}

// KickFromPool temporarily cools down or hard-disables an account.
// cooldownSec > 0: temporary cooldown; otherwise enabled=false.
func (c *Connector) KickFromPool(ctx context.Context, accountID, reason string, cooldownSec *float64) (map[string]any, error) {
	c.InvalidateCandidateCache()
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, errors.New("account id required")
	}
	if err := c.ensureAccountExists(ctx, accountID); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "手动移出轮询"
	}
	if len(reason) > 300 {
		reason = reason[:300]
	}
	if cooldownSec != nil && *cooldownSec > 0 {
		// Track stack depth for UI, but DO NOT multiply duration by count.
		// Old formula until = now + baseSec*count turned 2h free-usage cools into multi-day
		// bans after a few probe/request failures (count 30+ → days of cooldown).
		var prevCount int64
		var prevUntil *time.Time
		_ = c.Pool.QueryRow(ctx,
			`SELECT COALESCE(cooldown_count, 0), cooldown_until FROM account_pool WHERE account_id = $1`,
			accountID,
		).Scan(&prevCount, &prevUntil)
		newCount := prevCount + 1
		if newCount < 1 {
			newCount = 1
		}
		baseSec := maxFloat(*cooldownSec, 60)
		// Hard cap single cool window (free-usage rolling window fraction already ≤6h).
		if baseSec > 6*3600 {
			baseSec = 6 * 3600
		}
		now := time.Now()
		base := now
		// If already cooling, extend from the later of now / previous until (no stack multiply).
		if prevUntil != nil && prevUntil.After(now) {
			base = *prevUntil
		}
		until := base.Add(time.Duration(baseSec) * time.Second)
		// Absolute remaining cap from now so stacks cannot run away.
		maxUntil := now.Add(6 * time.Hour)
		if until.After(maxUntil) {
			until = maxUntil
		}
		// free-usage / 额度用完 kicks are account-wide cool only — clear model soft-blocks
		// so admin tags + PoolSummary count this row as 冷却, not 模型封禁.
		lowReason := strings.ToLower(reason)
		clearBlocks := strings.Contains(lowReason, "free-usage") ||
			strings.Contains(lowReason, "free usage") ||
			strings.Contains(lowReason, "额度用完") ||
			strings.Contains(lowReason, "免费额度") ||
			strings.Contains(lowReason, "included free usage") ||
			strings.Contains(lowReason, "subscription:free-usage") ||
			strings.Contains(lowReason, "model temporarily blocked") ||
			strings.Contains(lowReason, "tokens") && (strings.Contains(lowReason, "actual") || strings.Contains(lowReason, "limit"))
		_, err := c.Pool.Exec(ctx, `
			INSERT INTO account_pool (account_id, enabled, pool_status, cooldown_until, cooldown_reason, cooldown_count, last_error, blocked_models, extra, updated_at)
			VALUES ($1, true, 'cooldown', $2, $3, $4::int, $3, '{}'::jsonb, jsonb_build_object('cooldown_count', $4::int), now())
			ON CONFLICT (account_id) DO UPDATE SET
				pool_status = 'cooldown',
				cooldown_until = EXCLUDED.cooldown_until,
				cooldown_reason = EXCLUDED.cooldown_reason,
				cooldown_count = EXCLUDED.cooldown_count,
				last_error = EXCLUDED.last_error,
				blocked_models = CASE WHEN $5::bool THEN '{}'::jsonb ELSE COALESCE(account_pool.blocked_models, '{}'::jsonb) END,
				extra = COALESCE(account_pool.extra, '{}'::jsonb) || jsonb_build_object('cooldown_count', EXCLUDED.cooldown_count),
				updated_at = now()
		`, accountID, until, reason, int(newCount), clearBlocks)
		if err != nil {
			return nil, err
		}
		return c.GetAccountPoolView(ctx, accountID)
	}
	return c.SetAccountEnabled(ctx, accountID, false)
}

// SetAccountPoolStatus manually sets the admin pool tag (writes account_pool).
// Supported status values (aliases accepted):
//
//	live|normal  — re-enter rotation (clears cool/quota/model-block/disabled)
//	cooldown     — durable cool; optional cooldown_sec (default 7200)
//	model_blocked— soft-block model (optional model, default "manual"); optional cooldown_sec as until
//	quota_disabled — out of rotation for quota
//	disabled     — manual disable
//	expired      — mark expired (out of rotation)
//
// Reason is stored in last_error / cooldown_reason / disabled_reason as appropriate.
func (c *Connector) SetAccountPoolStatus(ctx context.Context, accountID, status, reason, model string, cooldownSec *float64) (map[string]any, error) {
	c.InvalidateCandidateCache()
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, errors.New("account id required")
	}
	if err := c.ensureAccountExists(ctx, accountID); err != nil {
		return nil, err
	}
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "live", "ok", "active", "polling", "normal", "":
		status = "live"
	case "cool", "cooling", "cooldown":
		status = "cooldown"
	case "model_blocked", "model-blocked", "blocked", "model_block":
		status = "model_blocked"
	case "quota", "quota_disabled", "quota-disabled":
		status = "quota_disabled"
	case "disabled", "disable", "off":
		status = "disabled"
	case "expired", "expire":
		status = "expired"
	default:
		return nil, errors.New("unsupported status: " + status + " (use live|cooldown|model_blocked|quota_disabled|disabled|expired)")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "手动设置: " + status
	}
	if len(reason) > 300 {
		reason = reason[:300]
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = "grok-4.5"
	}
	sec := 7200.0
	if cooldownSec != nil && *cooldownSec > 0 {
		sec = *cooldownSec
	}
	if sec < 60 {
		sec = 60
	}
	until := time.Now().Add(time.Duration(sec) * time.Second)

	var err error
	switch status {
	case "live":
		_, err = c.Pool.Exec(ctx, `
			INSERT INTO account_pool (account_id, enabled, pool_status, cooldown_count, extra, updated_at)
			VALUES ($1, true, 'normal', 0, '{}'::jsonb, now())
			ON CONFLICT (account_id) DO UPDATE SET
				enabled = true,
				disabled_for_quota = false,
				disabled_reason = NULL,
				quota_disabled_at = NULL,
				quota_source = NULL,
				blocked_models = '{}'::jsonb,
				cooldown_until = NULL,
				cooldown_reason = NULL,
				cooldown_code = NULL,
				cooldown_model = NULL,
				cooldown_tokens_actual = NULL,
				cooldown_tokens_limit = NULL,
				cooldown_count = 0,
				last_error = NULL,
				pool_status = 'normal',
				extra = (COALESCE(account_pool.extra, '{}'::jsonb) - 'status_stack' - 'cooldown_count' - 'token_expired_at' - 'token_expired_reason')
					|| jsonb_build_object('consecutive_fails', 0, 'manual_status', 'live', 'manual_status_reason', $2::text),
				updated_at = now()
		`, accountID, reason)
	case "cooldown":
		_, err = c.Pool.Exec(ctx, `
			INSERT INTO account_pool (
				account_id, enabled, pool_status, cooldown_until, cooldown_reason, cooldown_code,
				cooldown_count, last_error, blocked_models, extra, updated_at
			) VALUES (
				$1, true, 'cooldown', $2, $3, 'manual',
				1, $3, '{}'::jsonb,
				jsonb_build_object('cooldown_count', 1, 'manual_status', 'cooldown', 'manual_status_reason', $3::text),
				now()
			)
			ON CONFLICT (account_id) DO UPDATE SET
				enabled = true,
				disabled_for_quota = false,
				disabled_reason = NULL,
				quota_disabled_at = NULL,
				quota_source = NULL,
				blocked_models = '{}'::jsonb,
				pool_status = 'cooldown',
				cooldown_until = EXCLUDED.cooldown_until,
				cooldown_reason = EXCLUDED.cooldown_reason,
				cooldown_code = 'manual',
				cooldown_model = NULL,
				cooldown_tokens_actual = NULL,
				cooldown_tokens_limit = NULL,
				cooldown_count = COALESCE(account_pool.cooldown_count, 0) + 1,
				last_error = EXCLUDED.last_error,
				extra = COALESCE(account_pool.extra, '{}'::jsonb)
					|| jsonb_build_object('manual_status', 'cooldown', 'manual_status_reason', $3::text),
				updated_at = now()
		`, accountID, until, reason)
	case "model_blocked":
		entry := map[string]any{
			"blocked_at": float64(time.Now().Unix()),
			"source":     "manual",
			"reason":     reason,
			"until":      float64(until.Unix()),
			"ttl_sec":    sec,
			"blocked":    true,
		}
		blocked, mErr := json.Marshal(map[string]any{model: entry})
		if mErr != nil {
			return nil, mErr
		}
		_, err = c.Pool.Exec(ctx, `
			INSERT INTO account_pool (
				account_id, enabled, pool_status, blocked_models, last_error, extra, updated_at
			) VALUES (
				$1, true, 'model_blocked', $2::jsonb, $3,
				jsonb_build_object('manual_status', 'model_blocked', 'manual_status_reason', $3::text),
				now()
			)
			ON CONFLICT (account_id) DO UPDATE SET
				enabled = true,
				disabled_for_quota = false,
				disabled_reason = NULL,
				cooldown_until = NULL,
				cooldown_reason = NULL,
				cooldown_code = NULL,
				pool_status = 'model_blocked',
				blocked_models = COALESCE(account_pool.blocked_models, '{}'::jsonb) || $2::jsonb,
				last_error = COALESCE($3, account_pool.last_error),
				extra = COALESCE(account_pool.extra, '{}'::jsonb)
					|| jsonb_build_object('manual_status', 'model_blocked', 'manual_status_reason', $3::text),
				updated_at = now()
		`, accountID, blocked, reason)
	case "quota_disabled":
		_, err = c.Pool.Exec(ctx, `
			INSERT INTO account_pool (
				account_id, enabled, disabled_for_quota, disabled_reason, quota_disabled_at, quota_source,
				pool_status, blocked_models, cooldown_until, last_error, extra, updated_at
			) VALUES (
				$1, false, true, $2, now(), 'manual',
				'quota_disabled', '{}'::jsonb, NULL, $2,
				jsonb_build_object('manual_status', 'quota_disabled', 'manual_status_reason', $2::text),
				now()
			)
			ON CONFLICT (account_id) DO UPDATE SET
				enabled = false,
				disabled_for_quota = true,
				disabled_reason = $2,
				quota_disabled_at = now(),
				quota_source = 'manual',
				blocked_models = '{}'::jsonb,
				cooldown_until = NULL,
				cooldown_reason = NULL,
				cooldown_code = NULL,
				pool_status = 'quota_disabled',
				last_error = $2,
				extra = COALESCE(account_pool.extra, '{}'::jsonb)
					|| jsonb_build_object('manual_status', 'quota_disabled', 'manual_status_reason', $2::text),
				updated_at = now()
		`, accountID, reason)
	case "disabled":
		_, err = c.Pool.Exec(ctx, `
			INSERT INTO account_pool (
				account_id, enabled, disabled_for_quota, disabled_reason, pool_status,
				blocked_models, cooldown_until, last_error, extra, updated_at
			) VALUES (
				$1, false, false, $2, 'disabled',
				'{}'::jsonb, NULL, $2,
				jsonb_build_object('manual_status', 'disabled', 'manual_status_reason', $2::text),
				now()
			)
			ON CONFLICT (account_id) DO UPDATE SET
				enabled = false,
				disabled_for_quota = false,
				disabled_reason = $2,
				quota_disabled_at = NULL,
				quota_source = NULL,
				blocked_models = '{}'::jsonb,
				cooldown_until = NULL,
				cooldown_reason = NULL,
				cooldown_code = NULL,
				pool_status = 'disabled',
				last_error = $2,
				extra = COALESCE(account_pool.extra, '{}'::jsonb)
					|| jsonb_build_object('manual_status', 'disabled', 'manual_status_reason', $2::text),
				updated_at = now()
		`, accountID, reason)
	case "expired":
		_, err = c.Pool.Exec(ctx, `
			INSERT INTO account_pool (
				account_id, enabled, pool_status, blocked_models, cooldown_until, last_error, extra, updated_at
			) VALUES (
				$1, true, 'expired', '{}'::jsonb, NULL, $2,
				jsonb_build_object(
					'manual_status', 'expired',
					'manual_status_reason', $2::text,
					'token_expired_at', extract(epoch from now()),
					'token_expired_reason', $2::text
				),
				now()
			)
			ON CONFLICT (account_id) DO UPDATE SET
				enabled = true,
				disabled_for_quota = false,
				blocked_models = '{}'::jsonb,
				cooldown_until = NULL,
				cooldown_reason = NULL,
				cooldown_code = NULL,
				pool_status = 'expired',
				last_error = $2,
				extra = COALESCE(account_pool.extra, '{}'::jsonb)
					|| jsonb_build_object(
						'manual_status', 'expired',
						'manual_status_reason', $2::text,
						'token_expired_at', extract(epoch from now()),
						'token_expired_reason', $2::text
					),
				updated_at = now()
		`, accountID, reason)
		if err == nil {
			// Stamp accounts.expires_at so list/status filters (DB) agree with the tag.
			_, err = c.Pool.Exec(ctx, `
				UPDATE accounts
				SET expires_at = LEAST(COALESCE(expires_at, now()), now() - interval '1 second'),
				    updated_at = now()
				WHERE id = $1
			`, accountID)
		}
	}
	if err != nil {
		return nil, err
	}
	// live: if this was only a manual expired tag, clear past expires_at so DB filters re-admit.
	if status == "live" {
		_, _ = c.Pool.Exec(ctx, `
			UPDATE accounts
			SET expires_at = CASE
				WHEN expires_at IS NOT NULL AND expires_at <= now() THEN NULL
				ELSE expires_at
			END,
			    updated_at = now()
			WHERE id = $1
		`, accountID)
	}
	view, gerr := c.GetAccountPoolView(ctx, accountID)
	if gerr != nil {
		return nil, gerr
	}
	// Attach accounts.expires_at / expired for UI so list patch matches DB.
	var exp *time.Time
	_ = c.Pool.QueryRow(ctx, `SELECT expires_at FROM accounts WHERE id = $1`, accountID).Scan(&exp)
	if exp != nil {
		view["expires_at"] = exp.Unix()
		view["expired"] = exp.Before(time.Now())
	} else {
		view["expires_at"] = nil
		view["expired"] = status == "expired" || view["pool_status"] == "expired"
	}
	view["manual_status"] = status
	return view, nil
}

func (c *Connector) ensureAccountExists(ctx context.Context, accountID string) error {
	var exists bool
	if err := c.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1)`, accountID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errAccountNotFound
	}
	return nil
}

var errAccountNotFound = errors.New("account not found")

func IsAccountNotFound(err error) bool {
	return err != nil && (err == errAccountNotFound || strings.Contains(err.Error(), "account not found"))
}

func (c *Connector) GetAccountPoolView(ctx context.Context, accountID string) (map[string]any, error) {
	accountID = strings.TrimSpace(accountID)
	row := c.Pool.QueryRow(ctx, `
		SELECT a.id, a.email, a.user_id, a.team_id, a.expires_at, a.updated_at,
		       COALESCE(ap.enabled, true), COALESCE(ap.weight, 1), COALESCE(ap.request_count, 0),
		       COALESCE(ap.success_count, 0), COALESCE(ap.fail_count, 0), ap.last_used_at, ap.last_error,
		       ap.cooldown_until, COALESCE(ap.disabled_for_quota, false), ap.disabled_reason,
		       ap.quota_disabled_at, ap.quota_source, COALESCE(ap.pool_status, 'normal'), COALESCE(ap.cooldown_count, 0),
		       ap.cooldown_reason, ap.cooldown_code, ap.cooldown_model,
		       ap.cooldown_tokens_actual, ap.cooldown_tokens_limit,
		       COALESCE(ap.blocked_models, '{}'::jsonb),
		       ap.last_quota, ap.last_probe, ap.last_probe_status,
		       COALESCE(ap.extra, '{}'::jsonb)
		FROM accounts a
		LEFT JOIN account_pool ap ON ap.account_id = a.id
		WHERE a.id = $1
	`, accountID)
	var id string
	var email, userID, teamID, lastError, disabledReason, quotaSource, poolStatus *string
	var cooldownReason, cooldownCode, cooldownModel, lastProbeStatus *string
	var expiresAt, updatedAt, lastUsedAt, cooldownUntil, quotaDisabledAt *time.Time
	var enabled, disabledForQuota bool
	var weight, requestCount, successCount, failCount, cooldownCount int64
	var cooldownTokensActual, cooldownTokensLimit *int64
	var blockedBytes, lastQuotaBytes, lastProbeBytes, extraBytes []byte
	if err := row.Scan(
		&id, &email, &userID, &teamID, &expiresAt, &updatedAt,
		&enabled, &weight, &requestCount, &successCount, &failCount, &lastUsedAt, &lastError,
		&cooldownUntil, &disabledForQuota, &disabledReason,
		&quotaDisabledAt, &quotaSource, &poolStatus, &cooldownCount,
		&cooldownReason, &cooldownCode, &cooldownModel,
		&cooldownTokensActual, &cooldownTokensLimit,
		&blockedBytes, &lastQuotaBytes, &lastProbeBytes, &lastProbeStatus, &extraBytes,
	); err != nil {
		return nil, err
	}
	now := time.Now()
	// Wall-clock only. cooldown_count is a historical stack depth, not "still cooling".
	inCooldown := cooldownUntil != nil && cooldownUntil.After(now)
	rawStatus := "normal"
	if poolStatus != nil && strings.TrimSpace(*poolStatus) != "" {
		rawStatus = strings.TrimSpace(*poolStatus)
	}
	// Heal stale pool_status when until has elapsed (UI/API consistency).
	if rawStatus == "cooldown" && !inCooldown {
		rawStatus = "normal"
	}
	blocked := activeBlockedModels(decodeMap(blockedBytes), now)
	extra := decodeMap(extraBytes)
	status := derivePoolStatus(map[string]any{
		"pool_status":        rawStatus,
		"enabled":            enabled,
		"disabled_for_quota": disabledForQuota,
		"in_cooldown":        inCooldown,
		"blocked_model_ids":  mapKeys(blocked),
		"expired":            expiresAt != nil && now.After(*expiresAt),
		"last_renew_status":  stringFromMap(extra, "last_renew_status"),
		"token_expired_at":   extra["token_expired_at"],
	})
	out := map[string]any{
		"id":                     id,
		"email":                  stringPtr(email),
		"user_id":                stringPtr(userID),
		"team_id":                stringPtr(teamID),
		"enabled":                enabled,
		"weight":                 weight,
		"request_count":          requestCount,
		"success_count":          successCount,
		"fail_count":             failCount,
		"last_used_at":           unixOrNil(lastUsedAt),
		"last_error":             stringPtr(lastError),
		"cooldown_until":         unixOrNil(cooldownUntil),
		"cooldown_remaining_sec": cooldownRemaining(now, cooldownUntil),
		"in_cooldown":            inCooldown,
		"disabled_for_quota":     disabledForQuota,
		"disabled_reason":        stringPtr(disabledReason),
		"quota_disabled_at":      unixOrNil(quotaDisabledAt),
		"quota_source":           stringPtr(quotaSource),
		"pool_status":            status,
		"cooldown_count":         cooldownCount,
		"cooldown_reason":        stringPtr(cooldownReason),
		"cooldown_code":          stringPtr(cooldownCode),
		"cooldown_model":         stringPtr(cooldownModel),
		"cooldown_tokens_actual": int64PtrOrNil(cooldownTokensActual),
		"cooldown_tokens_limit":  int64PtrOrNil(cooldownTokensLimit),
		"blocked_models":         blocked,
		"blocked_model_ids":      mapKeys(blocked),
		"last_quota":             decodeMap(lastQuotaBytes),
		"last_probe":             decodeMap(lastProbeBytes),
		"last_probe_status":      stringPtr(lastProbeStatus),
		"status_stack":           statusStackFromExtra(extra),
		"consecutive_fails":      intFromMap(extra, "consecutive_fails"),
		"probe_fail_streak":      intFromMap(extra, "probe_fail_streak"),
		"token_expired_at":       extra["token_expired_at"],
		"token_expired_reason":   stringFromMap(extra, "token_expired_reason"),
		"expires_at":             unixOrNil(expiresAt),
		"updated_at":             unixOrNil(updatedAt),
	}
	return out, nil
}

func maxFloat(v, min float64) float64 {
	if v < min {
		return min
	}
	return v
}

func (c *Connector) CountEnabledAccounts(ctx context.Context) (int64, error) {
	var n int64
	err := c.Pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM accounts a
		LEFT JOIN account_pool ap ON ap.account_id = a.id
		WHERE COALESCE(ap.enabled, true) = true
		  AND COALESCE(ap.disabled_for_quota, false) = false
	`).Scan(&n)
	return n, err
}
