package redis

import (
	"context"
	"time"
)

type PickObserver struct {
	Client *Client
}

func NewPickObserver(client *Client) PickObserver {
	return PickObserver{Client: client}
}

func (o PickObserver) LoadPenalty(ctx context.Context, accountID string) int64 {
	if o.Client == nil {
		return 0
	}
	// Inflight dominates (avoid stacking concurrent streams on one token).
	// Soft-used adds a smaller spread so least_used does not pin the same few
	// accounts under burst traffic (reduces WAF/403 heat).
	var penalty int64
	if inflight, err := o.Client.GetInflight(ctx, accountID); err == nil && inflight > 0 {
		// Super-linear: 1→1000, 2→3000, 3→6000 … so multi-inflight accounts fall far back.
		penalty += inflight * (inflight + 1) / 2 * 1000
	}
	if age, err := o.Client.GetSoftUsedAgeSec(ctx, accountID, time.Now()); err == nil && age >= 0 {
		// Recently used within SoftUsedTTL: up to +800, decaying with age.
		// age=0 → 800; age>=30 → 0
		remain := 30.0 - age
		if remain > 0 {
			penalty += int64(remain * (800.0 / 30.0))
		}
	}
	return penalty
}

// LoadPenalties batches inflight + soft_used lookups for a candidate window (hot path).
func (o PickObserver) LoadPenalties(ctx context.Context, accountIDs []string) map[string]int64 {
	out := map[string]int64{}
	if o.Client == nil || len(accountIDs) == 0 {
		return out
	}
	inflight := o.Client.GetInflightMany(ctx, accountIDs)
	soft := o.Client.GetSoftUsedAgeSecMany(ctx, accountIDs, time.Now())
	for _, id := range accountIDs {
		var penalty int64
		if n := inflight[id]; n > 0 {
			penalty += n * (n + 1) / 2 * 1000
		}
		if age, ok := soft[id]; ok && age >= 0 {
			remain := 30.0 - age
			if remain > 0 {
				penalty += int64(remain * (800.0 / 30.0))
			}
		}
		if penalty > 0 {
			out[id] = penalty
		}
	}
	return out
}

func (o PickObserver) MarkPick(ctx context.Context, accountID string) {
	if o.Client == nil {
		return
	}
	// Fire-and-forget: never block TTFT on inflight/soft_used bookkeeping.
	// Use a detached short timeout so request cancel does not drop the mark.
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		// One pooled connection: INCR+EXPIRE pipeline + soft_used SETEX.
		_, _ = o.Client.MarkInflight(bg, accountID, InflightTTLSeconds)
		_, _ = o.Client.MarkSoftUsed(bg, accountID, SoftUsedTTLSeconds, time.Now())
	}()
}

func (o PickObserver) ReleasePick(ctx context.Context, accountID string) {
	if o.Client == nil {
		return
	}
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		_ = o.Client.ReleaseInflight(bg, accountID)
	}()
}
