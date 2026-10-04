package postgres

import (
	"context"
	"strings"
	"sync"
	"time"
)

type KeyStats struct {
	Total         int64 `json:"total"`
	Enabled       int64 `json:"enabled"`
	Disabled      int64 `json:"disabled"`
	TotalRequests int64 `json:"total_requests"`
}

type PoolSummary struct {
	Mode       string `json:"mode,omitempty"`
	Total      int64  `json:"total"`
	Live       int64  `json:"live"`
	Rotatable  int64  `json:"rotatable"`
	Enabled    int64  `json:"enabled"`
	InCooldown int64  `json:"in_cooldown"`
	// CooldownStacks is SUM of per-account kick depth (cooldown_count) for rows
	// currently in the cooldown bucket. Distinct from InCooldown (account count):
	// one account with 叠加×3 contributes 1 to InCooldown and 3 to CooldownStacks.
	CooldownStacks int64  `json:"cooldown_stacks"`
	QuotaDisabled  int64  `json:"quota_disabled"`
	ModelBlocked   int64  `json:"model_blocked"`
	Expired        int64  `json:"expired"`
	Disabled       int64  `json:"disabled"`
	Source         string `json:"source"`
}

func (c *Connector) CountAccounts(ctx context.Context) (int64, error) {
	return countQuery(ctx, c, "SELECT COUNT(*) FROM accounts")
}

func (c *Connector) CountModels(ctx context.Context, includeHidden bool) (int64, error) {
	if includeHidden {
		return countQuery(ctx, c, "SELECT COUNT(*) FROM models")
	}
	return countQuery(ctx, c, "SELECT COUNT(*) FROM models WHERE hidden = false")
}

func (c *Connector) KeyStats(ctx context.Context, legacyEnvKey bool, authRequired bool) (map[string]any, error) {
	var stats KeyStats
	err := c.Pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE enabled = true),
		       COUNT(*) FILTER (WHERE enabled = false),
		       COALESCE(SUM(request_count), 0)
		FROM api_keys`,
	).Scan(&stats.Total, &stats.Enabled, &stats.Disabled, &stats.TotalRequests)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"total":          stats.Total,
		"enabled":        stats.Enabled,
		"disabled":       stats.Disabled,
		"total_requests": stats.TotalRequests,
		"auth_required":  authRequired,
		"legacy_env_key": legacyEnvKey,
	}, nil
}

// activeModelBlockSQL is shared by PoolSummary + list filters. A row is model-blocked
// only when blocked_models has at least one currently active entry.
const activeModelBlockSQL = `
EXISTS (
  SELECT 1
  FROM jsonb_each(COALESCE(ap.blocked_models, '{}'::jsonb)) AS e(model, value)
  WHERE
    (jsonb_typeof(e.value) = 'boolean' AND e.value = 'true'::jsonb)
    OR (jsonb_typeof(e.value) = 'number' AND (e.value #>> '{}')::double precision > EXTRACT(EPOCH FROM now()))
    OR (
      jsonb_typeof(e.value) = 'object'
      AND (
        (e.value ? 'until' AND COALESCE((e.value->>'until')::double precision, 0) > EXTRACT(EPOCH FROM now()))
        OR (NOT (e.value ? 'until'))
        OR ((e.value ? 'blocked') AND (e.value->>'blocked') IN ('true','1'))
      )
    )
)
`

// Short TTL cache for admin /status auto-refresh. 7k-account full scan is ~0.5s;
// without caching, Cloudflare/nginx idle limits return HTML 502 on /admin/api/status.
var (
	poolSummaryMu    sync.Mutex
	poolSummaryCache PoolSummary
	poolSummaryAt    time.Time
)

const poolSummaryTTL = 3 * time.Second

// InvalidatePoolSummaryCache drops the status pool snapshot (after kick/disable/import).
func (c *Connector) InvalidatePoolSummaryCache() {
	poolSummaryMu.Lock()
	poolSummaryAt = time.Time{}
	poolSummaryCache = PoolSummary{}
	poolSummaryMu.Unlock()
}

// PoolSummary returns mutually exclusive account-pool buckets from PostgreSQL.
// Priority (matches list filters + admin tags):
//
//	expired > quota_disabled > disabled > model_blocked > cooldown > live
//
// model_blocked is above cooldown so empty-model-output soft-blocks surface under
// 「模型封禁」even if a residual cooldown_until is still set.
// live == rotatable; sum(live+cooldown+model_blocked+expired+quota_disabled+disabled) == total.
func (c *Connector) PoolSummary(ctx context.Context) (PoolSummary, error) {
	poolSummaryMu.Lock()
	if time.Since(poolSummaryAt) < poolSummaryTTL && poolSummaryCache.Total > 0 {
		out := poolSummaryCache
		poolSummaryMu.Unlock()
		return out, nil
	}
	poolSummaryMu.Unlock()

	var summary PoolSummary
	err := c.Pool.QueryRow(ctx, `
		WITH classified AS (
		  SELECT
		    CASE
		      WHEN (a.expires_at IS NOT NULL AND a.expires_at <= now())
		        OR COALESCE(ap.pool_status, '') = 'expired'
		        THEN 'expired'
		      WHEN COALESCE(ap.disabled_for_quota, false) = true
		        OR COALESCE(ap.pool_status, '') = 'quota_disabled'
		        THEN 'quota_disabled'
		      WHEN COALESCE(ap.enabled, true) = false
		        OR COALESCE(ap.pool_status, '') = 'disabled'
		        THEN 'disabled'
		      WHEN `+activeModelBlockSQL+`
		        THEN 'model_blocked'
		      WHEN COALESCE(ap.pool_status, '') = 'cooldown'
		        OR (ap.cooldown_until IS NOT NULL AND ap.cooldown_until > now())
		        THEN 'cooldown'
		      ELSE 'live'
		    END AS bucket,
		    COALESCE(ap.enabled, true) AS is_enabled,
		    -- Stack depth for cool rows only (叠加×N). Floor at 1 while cooling.
		    CASE
		      WHEN COALESCE(ap.pool_status, '') = 'cooldown'
		        OR (ap.cooldown_until IS NOT NULL AND ap.cooldown_until > now())
		      THEN GREATEST(COALESCE(ap.cooldown_count, 0), 1)
		      ELSE 0
		    END AS cool_stack
		  FROM accounts a
		  LEFT JOIN account_pool ap ON ap.account_id = a.id
		)
		SELECT
		  COUNT(*)::bigint AS total,
		  COUNT(*) FILTER (WHERE is_enabled)::bigint AS enabled,
		  COUNT(*) FILTER (WHERE bucket = 'live')::bigint AS live,
		  COUNT(*) FILTER (WHERE bucket = 'live')::bigint AS rotatable,
		  COUNT(*) FILTER (WHERE bucket = 'cooldown')::bigint AS in_cooldown,
		  COALESCE(SUM(cool_stack) FILTER (WHERE bucket = 'cooldown'), 0)::bigint AS cooldown_stacks,
		  COUNT(*) FILTER (WHERE bucket = 'quota_disabled')::bigint AS quota_disabled,
		  COUNT(*) FILTER (WHERE bucket = 'model_blocked')::bigint AS model_blocked,
		  COUNT(*) FILTER (WHERE bucket = 'expired')::bigint AS expired,
		  COUNT(*) FILTER (WHERE bucket = 'disabled')::bigint AS disabled
		FROM classified`,
	).Scan(
		&summary.Total,
		&summary.Enabled,
		&summary.Live,
		&summary.Rotatable,
		&summary.InCooldown,
		&summary.CooldownStacks,
		&summary.QuotaDisabled,
		&summary.ModelBlocked,
		&summary.Expired,
		&summary.Disabled,
	)
	if err != nil {
		return summary, err
	}
	// Prefer configured account mode when present.
	if modeVal, err := c.GetSetting(ctx, "account_mode"); err == nil {
		switch v := modeVal.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				summary.Mode = strings.TrimSpace(v)
			}
		}
	}
	if summary.Mode == "" {
		summary.Mode = "round_robin"
	}
	summary.Source = "postgres"
	poolSummaryMu.Lock()
	poolSummaryCache = summary
	poolSummaryAt = time.Now()
	poolSummaryMu.Unlock()
	return summary, nil
}

// freeUsageSignalSQL is true when last_error / last_probe / cooldown fields indicate
// free-usage exhaustion (额度用完). Used by repair + summary consistency.
const freeUsageSignalSQL = `
(
  COALESCE(ap.cooldown_code, '') ILIKE '%free-usage%'
  OR COALESCE(ap.cooldown_reason, '') ILIKE '%free-usage%'
  OR COALESCE(ap.cooldown_reason, '') ILIKE '%free usage%'
  OR COALESCE(ap.cooldown_reason, '') ILIKE '%额度用完%'
  OR COALESCE(ap.cooldown_reason, '') ILIKE '%免费额度%'
  OR COALESCE(ap.last_error, '') ILIKE '%free-usage%'
  OR COALESCE(ap.last_error, '') ILIKE '%free usage%'
  OR COALESCE(ap.last_error, '') ILIKE '%included free usage%'
  OR COALESCE(ap.last_error, '') ILIKE '%额度用完%'
  OR COALESCE(ap.last_error, '') ILIKE '%免费额度%'
  OR COALESCE(ap.last_probe::text, '') ILIKE '%free-usage%'
  OR COALESCE(ap.last_probe::text, '') ILIKE '%free usage%'
  OR COALESCE(ap.last_probe::text, '') ILIKE '%额度用完%'
  OR COALESCE(ap.last_probe::text, '') ILIKE '%included free usage%'
  OR COALESCE(ap.extra #>> '{cooldown_detail,failure_class}', '') ILIKE '%free-usage%'
  OR COALESCE(ap.extra #>> '{cooldown_detail,code}', '') ILIKE '%free-usage%'
)
`

// RepairFreeUsageModelBlocks moves free-usage mis-tagged "模型封禁" rows back into
// the cooldown pool, AND re-applies cooldown for recent free-usage probe/request
// failures that only saved last_probe/last_error without cooldown_until.
// Returns total account_pool rows updated across both repairs.
func (c *Connector) RepairFreeUsageModelBlocks(ctx context.Context) (int64, error) {
	if c == nil || c.Pool == nil {
		return 0, nil
	}
	var total int64

	// 1) Mis-tagged model_blocked free-usage → durable cooldown only.
	tag, err := c.Pool.Exec(ctx, `
		UPDATE account_pool ap
		SET
		  blocked_models = '{}'::jsonb,
		  cooldown_until = CASE
		    WHEN ap.cooldown_until IS NOT NULL AND ap.cooldown_until > now() THEN ap.cooldown_until
		    ELSE now() + interval '2 hours'
		  END,
		  cooldown_reason = COALESCE(
		    NULLIF(btrim(ap.cooldown_reason), ''),
		    NULLIF(btrim(ap.last_error), ''),
		    'free usage exhausted'
		  ),
		  cooldown_code = COALESCE(
		    NULLIF(btrim(ap.cooldown_code), ''),
		    'subscription:free-usage-exhausted'
		  ),
		  cooldown_count = GREATEST(COALESCE(ap.cooldown_count, 0), 1),
		  pool_status = CASE
		    WHEN COALESCE(ap.enabled, true) = false OR COALESCE(ap.disabled_for_quota, false) = true THEN 'disabled'
		    ELSE 'cooldown'
		  END,
		  updated_at = now()
		WHERE
		  `+freeUsageSignalSQL+`
		  AND (
		    COALESCE(ap.blocked_models, '{}'::jsonb) <> '{}'::jsonb
		    OR COALESCE(ap.pool_status, '') = 'model_blocked'
		  )
	`)
	if err != nil {
		return 0, err
	}
	total += tag.RowsAffected()

	// 2) Recent free-usage FAIL probes/errors that never got cooldown_until
	//    (probe path wrote last_probe but KickFromPool failed / autoDisable off / race).
	//    Only re-enter cool when not currently cooling, still enabled, and signal is
	//    from last_probe fail or last_error free-usage. Window: 2h cool from now.
	tag2, err := c.Pool.Exec(ctx, `
		UPDATE account_pool ap
		SET
		  blocked_models = '{}'::jsonb,
		  cooldown_until = now() + interval '2 hours',
		  cooldown_reason = COALESCE(
		    NULLIF(btrim(ap.cooldown_reason), ''),
		    NULLIF(btrim(ap.last_error), ''),
		    NULLIF(ap.last_probe->>'error', ''),
		    'free usage exhausted'
		  ),
		  cooldown_code = COALESCE(
		    NULLIF(btrim(ap.cooldown_code), ''),
		    'subscription:free-usage-exhausted'
		  ),
		  cooldown_count = GREATEST(COALESCE(ap.cooldown_count, 0), 1),
		  pool_status = 'cooldown',
		  updated_at = now()
		WHERE COALESCE(ap.enabled, true) = true
		  AND COALESCE(ap.disabled_for_quota, false) = false
		  AND COALESCE(ap.pool_status, '') NOT IN ('expired', 'disabled', 'quota_disabled')
		  AND (ap.cooldown_until IS NULL OR ap.cooldown_until <= now())
		  AND `+freeUsageSignalSQL+`
		  AND (
		    COALESCE(ap.last_probe_status, '') = 'fail'
		    OR COALESCE(ap.last_probe->>'ok', '') IN ('false', '0')
		    OR COALESCE(ap.last_probe->>'available', '') IN ('false', '0')
		  )
		  -- Prefer recent probes (last 36h). Older historical fails stay normal until re-probed.
		  AND (
		    COALESCE((ap.last_probe->>'probed_at')::double precision, 0)
		      > extract(epoch from now()) - 36 * 3600
		    OR ap.updated_at > now() - interval '36 hours'
		  )
	`)
	if err != nil {
		return total, err
	}
	total += tag2.RowsAffected()

	// 3) Bare 429 rate-limit fails (not free-usage) with no cool — short 10m cool
	//    so health probe storms still take accounts out of rotation briefly.
	tag3, err := c.Pool.Exec(ctx, `
		UPDATE account_pool ap
		SET
		  cooldown_until = now() + interval '10 minutes',
		  cooldown_reason = COALESCE(
		    NULLIF(btrim(ap.last_error), ''),
		    NULLIF(ap.last_probe->>'error', ''),
		    'rate limit'
		  ),
		  cooldown_code = COALESCE(NULLIF(btrim(ap.cooldown_code), ''), 'rate_limit'),
		  cooldown_count = GREATEST(COALESCE(ap.cooldown_count, 0), 1),
		  pool_status = 'cooldown',
		  updated_at = now()
		WHERE COALESCE(ap.enabled, true) = true
		  AND COALESCE(ap.disabled_for_quota, false) = false
		  AND COALESCE(ap.pool_status, '') NOT IN ('expired', 'disabled', 'quota_disabled', 'cooldown')
		  AND (ap.cooldown_until IS NULL OR ap.cooldown_until <= now())
		  AND COALESCE(ap.last_probe_status, '') = 'fail'
		  AND (
		    COALESCE(ap.last_probe::text, '') ILIKE '%"status_code": 429%'
		    OR COALESCE(ap.last_probe::text, '') ILIKE '%"status_code":429%'
		    OR COALESCE(ap.last_probe->>'status_code', '') = '429'
		    OR COALESCE(ap.last_error, '') ILIKE '% 429%'
		    OR COALESCE(ap.last_error, '') ILIKE '%rate limit%'
		    OR COALESCE(ap.last_error, '') ILIKE '%too many requests%'
		  )
		  AND NOT (`+freeUsageSignalSQL+`)
		  AND (
		    COALESCE((ap.last_probe->>'probed_at')::double precision, 0)
		      > extract(epoch from now()) - 6 * 3600
		    OR ap.updated_at > now() - interval '6 hours'
		  )
	`)
	if err != nil {
		return total, err
	}
	total += tag3.RowsAffected()

	// 4) Heal stale pool_status=model_blocked with no active blocked_models entry.
	_, _ = c.Pool.Exec(ctx, `
		UPDATE account_pool ap
		SET
		  blocked_models = '{}'::jsonb,
		  pool_status = CASE
		    WHEN COALESCE(ap.enabled, true) = false OR COALESCE(ap.disabled_for_quota, false) = true THEN 'disabled'
		    WHEN ap.cooldown_until IS NOT NULL AND ap.cooldown_until > now() THEN 'cooldown'
		    ELSE 'normal'
		  END,
		  updated_at = now()
		WHERE COALESCE(ap.pool_status, '') = 'model_blocked'
		  AND NOT (`+activeModelBlockSQL+`)
		  AND (ap.cooldown_until IS NULL OR ap.cooldown_until <= now())
	`)
	// 5) Intentionally NOT auto-clearing pool_status='cooldown' when until expires.
	// Sticky cool exits only via ClearAccountCooldown / successful probe or model call.

	if total > 0 {
		c.InvalidateCandidateCache()
	}
	return total, nil
}

func countQuery(ctx context.Context, c *Connector, sql string) (int64, error) {
	var count int64
	if err := c.Pool.QueryRow(ctx, sql).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}
