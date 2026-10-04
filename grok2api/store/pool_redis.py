"""Redis hot fields for account pool: cooldown, stats counters, RR index."""

from __future__ import annotations

import time
from typing import Any

from grok2api.store.redis_client import (
    get_str,
    hgetall,
    hincrby,
    hset_map,
    incr,
    key,
    redis_enabled,
    set_ex,
    delete,
)


def rr_next() -> int | None:
    """Atomically advance global round-robin cursor. None if Redis off."""
    if not redis_enabled():
        return None
    return incr(key("rr", "index"))


# ── in-flight / soft-pick load spread ──────────────────────────────────────
# Concurrent workers used to all pick the same least_used / RR head account
# because last_used only updated on success. Soft-mark at pick time so the
# next request sees this account as "hot" without waiting for upstream.

_INFLIGHT_TTL_SEC = 90  # auto-expire if process dies mid-request
_SOFT_USED_TTL_SEC = 45  # soft last_used hint for least_used scoring


def inflight_key(account_id: str) -> str:
    return key("inflight", account_id)


def soft_used_key(account_id: str) -> str:
    return key("soft_used", account_id)


def mark_inflight(account_id: str, *, ttl_sec: int = _INFLIGHT_TTL_SEC) -> int:
    """Increment in-flight count for an account. Returns new count (0 if Redis off)."""
    if not redis_enabled() or not account_id:
        return 0
    k = inflight_key(account_id)
    n = incr(k) or 0
    try:
        from grok2api.store.redis_client import get_client

        c = get_client()
        if c is not None:
            c.expire(k, max(5, int(ttl_sec)))
    except Exception:
        pass
    return int(n)


def release_inflight(account_id: str) -> None:
    """Decrement in-flight count (floor at 0)."""
    if not redis_enabled() or not account_id:
        return
    k = inflight_key(account_id)
    try:
        from grok2api.store.redis_client import get_client

        c = get_client()
        if c is None:
            return
        n = c.decr(k)
        if n is not None and int(n) <= 0:
            c.delete(k)
        else:
            # refresh TTL so long streams keep the key alive
            c.expire(k, _INFLIGHT_TTL_SEC)
    except Exception:
        pass


def get_inflight(account_id: str) -> int:
    if not redis_enabled() or not account_id:
        return 0
    raw = get_str(inflight_key(account_id))
    if raw is None:
        return 0
    try:
        return max(0, int(raw))
    except (TypeError, ValueError):
        return 0


def get_inflight_many(account_ids: list[str]) -> dict[str, int]:
    """Batch read in-flight counts. Missing keys → 0."""
    out: dict[str, int] = {}
    if not redis_enabled() or not account_ids:
        return out
    try:
        from grok2api.store.redis_client import get_client

        c = get_client()
        if c is None:
            return out
        keys = [inflight_key(a) for a in account_ids if a]
        if not keys:
            return out
        vals = c.mget(keys)
        for aid, v in zip([a for a in account_ids if a], vals or []):
            try:
                out[aid] = max(0, int(v)) if v is not None else 0
            except (TypeError, ValueError):
                out[aid] = 0
    except Exception:
        pass
    return out


def mark_soft_used(account_id: str, *, ttl_sec: int = _SOFT_USED_TTL_SEC) -> float:
    """Stamp a soft last_used_at so least_used spreads concurrent picks."""
    now = time.time()
    if not redis_enabled() or not account_id:
        return now
    set_ex(soft_used_key(account_id), str(now), max(5, int(ttl_sec)))
    return now


def get_soft_used_many(account_ids: list[str]) -> dict[str, float]:
    out: dict[str, float] = {}
    if not redis_enabled() or not account_ids:
        return out
    try:
        from grok2api.store.redis_client import get_client

        c = get_client()
        if c is None:
            return out
        aids = [a for a in account_ids if a]
        keys = [soft_used_key(a) for a in aids]
        if not keys:
            return out
        vals = c.mget(keys)
        for aid, v in zip(aids, vals or []):
            if v is None:
                continue
            try:
                out[aid] = float(v)
            except (TypeError, ValueError):
                pass
    except Exception:
        pass
    return out


def note_pick(account_id: str) -> None:
    """Pick-time soft mark: +1 inflight + soft last_used (non-blocking best-effort)."""
    if not account_id:
        return
    try:
        mark_inflight(account_id)
    except Exception:
        pass
    try:
        mark_soft_used(account_id)
    except Exception:
        pass


def set_cooldown(account_id: str, until_ts: float) -> None:
    if not redis_enabled() or not account_id:
        return
    now = time.time()
    remaining = max(1, int(float(until_ts) - now))
    set_ex(key("cooldown", account_id), str(float(until_ts)), remaining)


def get_cooldown(account_id: str) -> float | None:
    if not redis_enabled() or not account_id:
        return None
    raw = get_str(key("cooldown", account_id))
    if raw is None:
        return None
    try:
        return float(raw)
    except (TypeError, ValueError):
        return None


def clear_cooldown(account_id: str) -> None:
    if not redis_enabled() or not account_id:
        return
    delete(key("cooldown", account_id))


def touch_stats(
    account_id: str,
    *,
    success: bool = True,
    error: str = "",
    cooldown_until: float | None = None,
    clear_cooldown_flag: bool = False,
    consecutive_fails: int | None = None,
    last_status_code: int | None = None,
    cooldown_sec: float | None = None,
) -> dict[str, Any] | None:
    """Increment request counters in Redis. Returns snapshot or None if disabled.

    Note: live-success paths should pass clear_cooldown_flag=False and leave
    cooldown_until=None so Redis does not wipe an active durable cooldown.
    """
    if not redis_enabled() or not account_id:
        return None
    k = key("stats", account_id)
    hincrby(k, "request_count", 1)
    if success:
        hincrby(k, "success_count", 1)
    else:
        hincrby(k, "fail_count", 1)
    mapping: dict[str, Any] = {"last_used_at": time.time()}
    # Only overwrite last_error when caller provided one (failure) or explicitly
    # cleared via non-empty handling. Empty error + success leaves existing.
    if error:
        mapping["last_error"] = error[:500]
    if success:
        mapping["consecutive_fails"] = 0
    elif consecutive_fails is not None:
        mapping["consecutive_fails"] = int(consecutive_fails)
    if last_status_code is not None:
        mapping["last_status_code"] = int(last_status_code)
    if cooldown_sec is not None:
        mapping["cooldown_sec"] = float(cooldown_sec)
    hset_map(k, mapping)
    if cooldown_until is not None:
        set_cooldown(account_id, float(cooldown_until))
    if clear_cooldown_flag:
        clear_cooldown(account_id)
        # reset streak fields when clearing
        hset_map(k, {"consecutive_fails": 0, "cooldown_sec": 0})
    return get_stats(account_id)


def get_stats(account_id: str) -> dict[str, Any]:
    if not redis_enabled() or not account_id:
        return {}
    raw = hgetall(key("stats", account_id))
    out: dict[str, Any] = {}
    for field in ("request_count", "success_count", "fail_count", "consecutive_fails", "last_status_code"):
        if field in raw:
            try:
                out[field] = int(float(raw[field]))
            except ValueError:
                out[field] = 0
    if "cooldown_sec" in raw:
        try:
            out["cooldown_sec"] = float(raw["cooldown_sec"])
        except ValueError:
            pass
    if "last_used_at" in raw:
        try:
            out["last_used_at"] = float(raw["last_used_at"])
        except ValueError:
            pass
    if raw.get("last_error"):
        out["last_error"] = raw["last_error"]
    cd = get_cooldown(account_id)
    if cd is not None:
        out["cooldown_until"] = cd
    return out


def merge_pool_meta(account_id: str, base: dict[str, Any]) -> dict[str, Any]:
    """Overlay Redis hot fields onto durable pool meta dict.

    Durable PostgreSQL/file cooldown_until is the source of truth. Redis may
    only extend / fill a missing until — never wipe an active durable cooldown
    just because the Redis TTL key expired early.
    """
    if not redis_enabled() or not account_id:
        return base
    hot = get_stats(account_id)
    durable_cd = base.get("cooldown_until") if isinstance(base, dict) else None
    try:
        durable_active = durable_cd is not None and float(durable_cd) > time.time()
    except (TypeError, ValueError):
        durable_active = False

    if not hot:
        # still check cooldown key alone
        cd = get_cooldown(account_id)
        if cd is not None:
            base = dict(base)
            # Prefer the later of durable / redis so neither side under-reports.
            try:
                if durable_active and float(durable_cd) >= float(cd):
                    base["cooldown_until"] = float(durable_cd)
                else:
                    base["cooldown_until"] = float(cd)
            except (TypeError, ValueError):
                base["cooldown_until"] = cd
        return base

    merged = dict(base)
    for k, v in hot.items():
        if v is None or v == "":
            continue
        if k == "cooldown_until":
            # Never let a missing/short redis cooldown erase durable active CD.
            try:
                hot_cd = float(v)
            except (TypeError, ValueError):
                continue
            if durable_active:
                try:
                    merged["cooldown_until"] = max(float(durable_cd), hot_cd)
                except (TypeError, ValueError):
                    merged["cooldown_until"] = hot_cd
            else:
                merged["cooldown_until"] = hot_cd
            continue
        merged[k] = v

    # If redis has no cooldown key but durable still active, keep durable.
    if durable_active and not merged.get("cooldown_until"):
        merged["cooldown_until"] = float(durable_cd)  # type: ignore[arg-type]
    return merged
