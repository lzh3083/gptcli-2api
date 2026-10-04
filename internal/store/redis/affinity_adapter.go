package redis

import (
	"context"
	"strings"
	"time"
)

type ChatAffinity struct {
	Client *Client
	TTL    time.Duration
}

func NewChatAffinity(client *Client, ttl time.Duration) ChatAffinity {
	if ttl <= 0 {
		// Sticky maps must outlive multi-hour Codex sessions / overnight tools.
		ttl = 24 * time.Hour
	}
	return ChatAffinity{Client: client, TTL: ttl}
}

func (a ChatAffinity) GetAffinity(ctx context.Context, fingerprint string) (string, error) {
	if a.Client == nil {
		return "", nil
	}
	entry, err := a.Client.GetAffinity(ctx, fingerprint, a.TTL)
	if err != nil || entry == nil {
		return "", err
	}
	return entry.AccountID, nil
}

func (a ChatAffinity) BindAffinity(ctx context.Context, fingerprint, accountID string) error {
	if a.Client == nil {
		return nil
	}
	// Optimistic local cache first so same-process next turn always sticks,
	// even if Redis write is still in flight.
	affinityCacheSet(fingerprint, accountID)
	// Coalesce Redis SETs per fingerprint: multi-turn bursts refresh local cache
	// every hit but only flush the latest account/TTL to Redis once.
	client := a.Client
	ttl := a.TTL
	affinityScheduleWrite(fingerprint, accountID, ttl, "", "", func(bg context.Context, fp, acc string, d time.Duration, sessionFP, pck string) error {
		return client.BindAffinity(bg, fp, acc, d, sessionFP, pck)
	})
	return nil
}

func (a ChatAffinity) BindResponseAccount(ctx context.Context, responseID, accountID, promptCacheKey string) error {
	if a.Client == nil {
		return nil
	}
	// Local first: next Codex turn with previous_response_id must recover pck+account.
	responseCacheSet(responseID, accountID, promptCacheKey)
	if pck := stringsTrim(promptCacheKey); pck != "" {
		// Also pin model-less pck fingerprint immediately.
		affinityCacheSet("chat:prompt_cache_key:"+pck, accountID)
		// Schedule pck sticky write (coalesced) so model-less recovery stays durable.
		client := a.Client
		ttl := a.TTL
		affinityScheduleWrite("chat:prompt_cache_key:"+pck, accountID, ttl, "", pck, func(bg context.Context, fp, acc string, d time.Duration, sessionFP, pck2 string) error {
			return client.BindAffinity(bg, fp, acc, d, sessionFP, pck2)
		})
	}
	client := a.Client
	ttl := a.TTL
	responseScheduleWrite(responseID, accountID, promptCacheKey, ttl, func(bg context.Context, rid, acc, pck string, d time.Duration) error {
		return client.BindResponseAccount(bg, rid, acc, pck, d)
	})
	return nil
}

func (a ChatAffinity) GetResponseAccount(ctx context.Context, responseID string) (accountID, promptCacheKey string, err error) {
	if a.Client == nil {
		return "", "", nil
	}
	if acc, pck, ok := responseCacheGet(responseID); ok {
		return acc, pck, nil
	}
	entry, err := a.Client.GetResponseAccount(ctx, responseID)
	if err != nil || entry == nil {
		return "", "", err
	}
	responseCacheSet(responseID, entry.AccountID, entry.PromptCacheKey)
	if entry.PromptCacheKey != "" && entry.AccountID != "" {
		affinityCacheSet("chat:prompt_cache_key:"+entry.PromptCacheKey, entry.AccountID)
	}
	return entry.AccountID, entry.PromptCacheKey, nil
}

func (a ChatAffinity) ClearAffinity(ctx context.Context, fingerprint string) error {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return nil
	}
	affinityCacheDelete(fingerprint)
	if a.Client == nil {
		return nil
	}
	return a.Client.ClearAffinity(ctx, fingerprint)
}
