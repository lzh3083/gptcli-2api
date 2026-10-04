package redis

import (
	"context"
	"sync"
	"time"
)

// affinityLocalCache avoids repeated Redis GETs for the same sticky fingerprint
// under multi-turn Codex bursts (same prompt_cache_key / response chain).
type affinityCacheEntry struct {
	accountID string
	expires   time.Time
}

type responseCacheEntry struct {
	accountID      string
	promptCacheKey string
	expires        time.Time
}

var (
	affinityCacheMu sync.Mutex
	affinityCache   = map[string]affinityCacheEntry{}
	responseCache   = map[string]responseCacheEntry{}

	// Coalesce background Redis writes for the same sticky key so multi-turn
	// bursts (Claude Code / Codex) do not enqueue one goroutine+SET per token hit.
	affinityPendingMu sync.Mutex
	affinityPending   = map[string]affinityPendingWrite{} // fingerprint -> latest
	affinityFlushing  = map[string]bool{}                 // in-flight flush per key

	responsePendingMu sync.Mutex
	responsePending   = map[string]responsePendingWrite{}
	responseFlushing  = map[string]bool{}
)

type affinityPendingWrite struct {
	accountID string
	ttl       time.Duration
	// optional metadata for BindAffinity
	sessionFP      string
	promptCacheKey string
	writer         func(ctx context.Context, fingerprint, accountID string, ttl time.Duration, sessionFP, promptCacheKey string) error
}

type responsePendingWrite struct {
	accountID      string
	promptCacheKey string
	ttl            time.Duration
	writer         func(ctx context.Context, responseID, accountID, promptCacheKey string, ttl time.Duration) error
}

// Long enough to cover multi-turn Codex sessions in-process without thrashing Redis.
// Still short enough that kick/disable eventually re-resolves via Redis/PG eligibility.
const affinityLocalTTL = 30 * time.Minute
const responseLocalTTL = 30 * time.Minute

func affinityCacheGet(fingerprint string) (string, bool) {
	fingerprint = stringsTrim(fingerprint)
	if fingerprint == "" {
		return "", false
	}
	now := time.Now()
	affinityCacheMu.Lock()
	defer affinityCacheMu.Unlock()
	entry, ok := affinityCache[fingerprint]
	if !ok || now.After(entry.expires) {
		if ok {
			delete(affinityCache, fingerprint)
		}
		return "", false
	}
	return entry.accountID, true
}

func affinityCacheSet(fingerprint, accountID string) {
	fingerprint = stringsTrim(fingerprint)
	accountID = stringsTrim(accountID)
	if fingerprint == "" || accountID == "" {
		return
	}
	affinityCacheMu.Lock()
	affinityCache[fingerprint] = affinityCacheEntry{accountID: accountID, expires: time.Now().Add(affinityLocalTTL)}
	if len(affinityCache) > 8192 {
		now := time.Now()
		for k, v := range affinityCache {
			if now.After(v.expires) {
				delete(affinityCache, k)
			}
		}
	}
	affinityCacheMu.Unlock()
}

func responseCacheGet(responseID string) (accountID, promptCacheKey string, ok bool) {
	responseID = stringsTrim(responseID)
	if responseID == "" {
		return "", "", false
	}
	now := time.Now()
	affinityCacheMu.Lock()
	defer affinityCacheMu.Unlock()
	entry, found := responseCache[responseID]
	if !found || now.After(entry.expires) {
		if found {
			delete(responseCache, responseID)
		}
		return "", "", false
	}
	return entry.accountID, entry.promptCacheKey, true
}

func responseCacheSet(responseID, accountID, promptCacheKey string) {
	responseID = stringsTrim(responseID)
	accountID = stringsTrim(accountID)
	if responseID == "" || accountID == "" {
		return
	}
	affinityCacheMu.Lock()
	responseCache[responseID] = responseCacheEntry{
		accountID:      accountID,
		promptCacheKey: stringsTrim(promptCacheKey),
		expires:        time.Now().Add(responseLocalTTL),
	}
	if len(responseCache) > 8192 {
		now := time.Now()
		for k, v := range responseCache {
			if now.After(v.expires) {
				delete(responseCache, k)
			}
		}
	}
	affinityCacheMu.Unlock()
}

func stringsTrim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		last := s[len(s)-1]
		if last == ' ' || last == '\t' || last == '\n' || last == '\r' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

// affinityScheduleWrite merges Redis sticky writes per fingerprint.
// Local cache is assumed already updated by caller. Only one flush goroutine
// runs per key; later binds overwrite the pending payload.
func affinityScheduleWrite(
	fingerprint, accountID string,
	ttl time.Duration,
	sessionFP, promptCacheKey string,
	writer func(ctx context.Context, fingerprint, accountID string, ttl time.Duration, sessionFP, promptCacheKey string) error,
) {
	fingerprint = stringsTrim(fingerprint)
	accountID = stringsTrim(accountID)
	if fingerprint == "" || accountID == "" || writer == nil {
		return
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	affinityPendingMu.Lock()
	affinityPending[fingerprint] = affinityPendingWrite{
		accountID: accountID, ttl: ttl,
		sessionFP: stringsTrim(sessionFP), promptCacheKey: stringsTrim(promptCacheKey),
		writer: writer,
	}
	if affinityFlushing[fingerprint] {
		affinityPendingMu.Unlock()
		return
	}
	affinityFlushing[fingerprint] = true
	affinityPendingMu.Unlock()

	go func(fp string) {
		for {
			affinityPendingMu.Lock()
			job, ok := affinityPending[fp]
			if !ok {
				affinityFlushing[fp] = false
				affinityPendingMu.Unlock()
				return
			}
			delete(affinityPending, fp)
			affinityPendingMu.Unlock()

			bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = job.writer(bg, fp, job.accountID, job.ttl, job.sessionFP, job.promptCacheKey)
			cancel()
		}
	}(fingerprint)
}

func responseScheduleWrite(
	responseID, accountID, promptCacheKey string,
	ttl time.Duration,
	writer func(ctx context.Context, responseID, accountID, promptCacheKey string, ttl time.Duration) error,
) {
	responseID = stringsTrim(responseID)
	accountID = stringsTrim(accountID)
	if responseID == "" || accountID == "" || writer == nil {
		return
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	responsePendingMu.Lock()
	responsePending[responseID] = responsePendingWrite{
		accountID: accountID, promptCacheKey: stringsTrim(promptCacheKey),
		ttl: ttl, writer: writer,
	}
	if responseFlushing[responseID] {
		responsePendingMu.Unlock()
		return
	}
	responseFlushing[responseID] = true
	responsePendingMu.Unlock()

	go func(rid string) {
		for {
			responsePendingMu.Lock()
			job, ok := responsePending[rid]
			if !ok {
				responseFlushing[rid] = false
				responsePendingMu.Unlock()
				return
			}
			delete(responsePending, rid)
			responsePendingMu.Unlock()

			bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = job.writer(bg, rid, job.accountID, job.promptCacheKey, job.ttl)
			cancel()
		}
	}(responseID)
}

func affinityCacheDelete(fingerprint string) {
	fingerprint = stringsTrim(fingerprint)
	if fingerprint == "" {
		return
	}
	affinityCacheMu.Lock()
	delete(affinityCache, fingerprint)
	affinityCacheMu.Unlock()
	// Drop any coalesced pending Redis write so a stale rebind cannot resurrect
	// a just-cleared pin after sticky failover.
	affinityPendingMu.Lock()
	delete(affinityPending, fingerprint)
	affinityPendingMu.Unlock()
}
