package redis

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// WorkerID matches Python redis_client.worker_id closely enough for leadership.
func WorkerID() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "host"
	}
	return fmt.Sprintf("%d@%s", os.Getpid(), host)
}

func (c *Client) LeaderLockKey() string {
	return c.key("lock", "maintainer_leader")
}

// MaintenanceLockKey is the legacy shared slot. Prefer MaintenanceLockKeyFor
// so model_health and token_maintainer do not block each other.
func (c *Client) MaintenanceLockKey() string {
	return c.key("lock", "maintenance")
}

// MaintenanceLockKeyFor returns a per-owner maintenance lock key.
// owner is sanitized to a short slug (e.g. "model_health", "token_maintainer").
func (c *Client) MaintenanceLockKeyFor(owner string) string {
	owner = strings.TrimSpace(strings.ToLower(owner))
	if owner == "" {
		owner = "default"
	}
	// keep key short / redis-safe
	var b strings.Builder
	for _, r := range owner {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	slug := b.String()
	if slug == "" {
		slug = "default"
	}
	return c.key("lock", "maintenance", slug)
}

// TryAcquireLock SET NX EX.
func (c *Client) TryAcquireLock(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	if !c.Enabled() {
		return false, nil
	}
	sec := int(ttl.Seconds())
	if sec < 1 {
		sec = 1
	}
	return c.SetNXEX(ctx, key, token, sec)
}

func (c *Client) RenewLock(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	if !c.Enabled() {
		return false, nil
	}
	sec := int(ttl.Seconds())
	if sec < 1 {
		sec = 1
	}
	return c.RenewIfOwner(ctx, key, token, sec)
}

func (c *Client) ReleaseLock(ctx context.Context, key, token string) (bool, error) {
	if !c.Enabled() {
		return false, nil
	}
	return c.CompareAndDelete(ctx, key, token)
}

// AcquireMaintenanceLock acquires a per-owner maintenance lock with renew loop.
// owner is part of the Redis key so concurrent jobs (model_health vs token_maintainer)
// do not serialize each other — that used to make "全部模型探测" return 0/0 when the
// token maintainer held the shared lock (deferred_busy on first wave → job abort).
func (c *Client) AcquireMaintenanceLock(ctx context.Context, owner string, timeout time.Duration, blocking bool) (acquired bool, release func(), err error) {
	release = func() {}
	if !c.Enabled() {
		return false, release, nil
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	token := fmt.Sprintf("%s|%s|%d", strings.TrimSpace(owner), WorkerID(), time.Now().Unix())
	lockKey := c.MaintenanceLockKeyFor(owner)
	deadline := time.Now()
	if blocking {
		deadline = time.Now().Add(timeout)
	}
	for {
		ok, aerr := c.TryAcquireLock(ctx, lockKey, token, timeout)
		if aerr != nil {
			return false, release, aerr
		}
		if ok {
			acquired = true
			break
		}
		if !blocking || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return false, release, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !acquired {
		return false, release, nil
	}
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(timeout / 3)
		if timeout/3 < time.Second {
			ticker.Reset(time.Second)
		}
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_, _ = c.RenewLock(context.Background(), lockKey, token, timeout)
			}
		}
	}()
	release = func() {
		close(stop)
		_, _ = c.ReleaseLock(context.Background(), lockKey, token)
	}
	return true, release, nil
}

func (c *Client) MaintenanceLockStatus(ctx context.Context) map[string]any {
	if !c.Enabled() {
		return map[string]any{"backend": "none"}
	}
	// Report both legacy shared key and known owner keys for admin diagnostics.
	keys := []string{
		c.MaintenanceLockKey(),
		c.MaintenanceLockKeyFor("model_health"),
		c.MaintenanceLockKeyFor("token_maintainer"),
	}
	holders := map[string]any{}
	busy := false
	for _, k := range keys {
		cur, err := c.Get(ctx, k)
		if err != nil || strings.TrimSpace(cur) == "" {
			continue
		}
		busy = true
		holder := cur
		if i := strings.Index(cur, "|"); i >= 0 {
			holder = cur[:i]
		}
		holders[k] = map[string]any{"holder": holder, "token": cur}
	}
	return map[string]any{"backend": "redis", "busy": busy, "locks": holders}
}
