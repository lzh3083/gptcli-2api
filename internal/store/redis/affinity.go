package redis

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

type AffinityEntry struct {
	AccountID      string  `json:"account_id"`
	BoundAt        float64 `json:"bound_at"`
	LastSeen       float64 `json:"last_seen"`
	Hits           int64   `json:"hits"`
	SessionFP      string  `json:"session_fp,omitempty"`
	PromptCacheKey string  `json:"prompt_cache_key,omitempty"`
}

func (c *Client) GetAffinity(ctx context.Context, fingerprint string, ttl time.Duration) (*AffinityEntry, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return nil, nil
	}
	if accountID, ok := affinityCacheGet(fingerprint); ok {
		return &AffinityEntry{AccountID: accountID}, nil
	}
	raw, err := c.Get(ctx, c.key("affinity", fingerprint))
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, err
	}
	entry := parseAffinity(raw)
	if entry == nil || entry.AccountID == "" {
		return nil, nil
	}
	// Hot-path read: do NOT rewrite affinity on every lookup.
	// Hits/last_seen are refreshed on BindAffinity after a successful pick.
	// This saves a Redis write RTT before upstream request (TTFT critical path).
	_ = ttl
	affinityCacheSet(fingerprint, entry.AccountID)
	return entry, nil
}

func (c *Client) BindAffinity(ctx context.Context, fingerprint, accountID string, ttl time.Duration, sessionFP, promptCacheKey string) error {
	fingerprint = strings.TrimSpace(fingerprint)
	accountID = strings.TrimSpace(accountID)
	if fingerprint == "" || accountID == "" {
		return nil
	}
	now := unixFloat(time.Now())
	entry := AffinityEntry{AccountID: accountID, BoundAt: now, LastSeen: now, Hits: 1}
	// Best-effort previous hits; miss is fine (avoids serializing GET on slow path).
	// Keep a short GET only when pipeline/pool is healthy — still one pooled conn RTT.
	raw, _ := c.Get(ctx, c.key("affinity", fingerprint))
	if prev := parseAffinity(raw); prev != nil {
		entry.BoundAt = prev.BoundAt
		entry.Hits = prev.Hits + 1
		if sessionFP == "" {
			sessionFP = prev.SessionFP
		}
		if promptCacheKey == "" {
			promptCacheKey = prev.PromptCacheKey
		}
	}
	entry.SessionFP = strings.TrimSpace(sessionFP)
	entry.PromptCacheKey = strings.TrimSpace(promptCacheKey)
	if err := c.setAffinity(ctx, fingerprint, entry, ttl); err != nil {
		return err
	}
	affinityCacheSet(fingerprint, accountID)
	return nil
}

func (c *Client) ClearAffinity(ctx context.Context, fingerprint string) error {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return nil
	}
	return c.Del(ctx, c.key("affinity", fingerprint))
}

func (c *Client) setAffinity(ctx context.Context, fingerprint string, entry AffinityEntry, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return c.SetEX(ctx, c.key("affinity", fingerprint), string(data), int(ttl.Seconds()))
}

func parseAffinity(raw string) *AffinityEntry {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var entry AffinityEntry
	if err := json.Unmarshal([]byte(raw), &entry); err == nil && entry.AccountID != "" {
		return &entry
	}
	return &AffinityEntry{AccountID: raw, BoundAt: unixFloat(time.Now())}
}

func unixFloat(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

func stringInt(value int) string {
	return strconv.Itoa(value)
}

// BindResponseAccount maps a Responses API response_id to account + prompt_cache_key
// so Codex multi-turn requests that only send previous_response_id can stick.
func (c *Client) BindResponseAccount(ctx context.Context, responseID, accountID, promptCacheKey string, ttl time.Duration) error {
	responseID = strings.TrimSpace(responseID)
	accountID = strings.TrimSpace(accountID)
	if responseID == "" || accountID == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	entry := AffinityEntry{
		AccountID:      accountID,
		BoundAt:        unixFloat(time.Now()),
		LastSeen:       unixFloat(time.Now()),
		Hits:           1,
		PromptCacheKey: strings.TrimSpace(promptCacheKey),
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return c.SetEX(ctx, c.key("affinity", "response", responseID), string(data), int(ttl.Seconds()))
}

func (c *Client) GetResponseAccount(ctx context.Context, responseID string) (*AffinityEntry, error) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return nil, nil
	}
	raw, err := c.Get(ctx, c.key("affinity", "response", responseID))
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, err
	}
	return parseAffinity(raw), nil
}
