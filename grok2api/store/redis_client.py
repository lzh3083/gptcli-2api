"""Shared Redis client for multi-worker hot state.

Optional dependency: `redis` (see requirements-store.txt). When REDIS_URL is
unset, all helpers no-op / return None so file-mode code paths keep working.
"""

from __future__ import annotations

import os
import threading
import time
from typing import Any

from grok2api.config import REDIS_KEY_PREFIX

_client = None
_client_lock = threading.Lock()
_import_error: str | None = None
_last_ping_ok: bool | None = None
_last_ping_at = 0.0
_PING_CACHE_SEC = 2.0


def redis_url() -> str:
    # Live-read so tests / late env changes work; also honor both env names.
    try:
        import grok2api.config as _cfg

        u = (getattr(_cfg, "REDIS_URL", None) or "").strip()
        if u:
            return u
    except Exception:
        pass
    return (
        os.getenv("GROK2API_REDIS_URL") or os.getenv("REDIS_URL") or ""
    ).strip()


def redis_enabled() -> bool:
    """True when a URL is configured and the redis package is importable."""
    if not redis_url():
        return False
    try:
        get_client()
        return _client is not None
    except Exception:
        return False


def redis_required() -> bool:
    """True when multi-worker mode demands a working Redis (fail-closed)."""
    try:
        import grok2api.config as _cfg

        return int(getattr(_cfg, "WORKERS", 1) or 1) > 1
    except Exception:
        return False


def ensure_redis_or_raise() -> None:
    """Fail closed for multi-worker: refuse to serve without Redis."""
    if not redis_required():
        return
    if not redis_url():
        raise RuntimeError(
            "GROK2API_WORKERS>1 requires REDIS_URL (fail-closed multi-worker mode)"
        )
    if not ping(force=True):
        raise RuntimeError(
            "REDIS_URL configured but Redis unreachable (fail-closed multi-worker mode)"
        )


def key(*parts: str) -> str:
    """Build a namespaced Redis key: g2a:a:b:c"""
    segs = [REDIS_KEY_PREFIX] + [str(p).strip(":") for p in parts if str(p)]
    return ":".join(segs)


def get_client():
    """Return a shared redis.Redis client, or None if disabled / unavailable."""
    global _client, _import_error
    if not redis_url():
        return None
    if _client is not None:
        return _client
    with _client_lock:
        if _client is not None:
            return _client
        try:
            import redis  # type: ignore
        except ImportError as e:
            _import_error = (
                "redis package not installed; "
                "pip install -r requirements-store.txt"
            )
            raise RuntimeError(_import_error) from e
        # decode_responses so callers get str, not bytes
        _client = redis.Redis.from_url(
            redis_url(),
            decode_responses=True,
            socket_connect_timeout=2.0,
            socket_timeout=2.0,
            health_check_interval=30,
        )
        return _client


def ping(*, force: bool = False) -> bool:
    """Cheap connectivity check (cached briefly)."""
    global _last_ping_ok, _last_ping_at
    now = time.time()
    if (
        not force
        and _last_ping_ok is not None
        and now - _last_ping_at < _PING_CACHE_SEC
    ):
        return bool(_last_ping_ok)
    try:
        c = get_client()
        if c is None:
            _last_ping_ok = False
        else:
            _last_ping_ok = bool(c.ping())
    except Exception:
        _last_ping_ok = False
    _last_ping_at = now
    return bool(_last_ping_ok)


def import_error() -> str | None:
    return _import_error


# ── thin helpers used by affinity / locks / sessions ─────────────────────────


def get_str(k: str) -> str | None:
    c = get_client()
    if c is None:
        return None
    v = c.get(k)
    return str(v) if v is not None else None


def set_ex(k: str, value: str, ttl_sec: float | int) -> bool:
    c = get_client()
    if c is None:
        return False
    ttl = max(1, int(ttl_sec))
    c.set(k, value, ex=ttl)
    return True


def delete(*keys: str) -> int:
    c = get_client()
    if c is None or not keys:
        return 0
    return int(c.delete(*keys))


def sadd(k: str, *members: str, ttl_sec: float | int | None = None) -> int:
    """Add members to a Redis set. Optionally refresh TTL."""
    c = get_client()
    if c is None or not members:
        return 0
    n = int(c.sadd(k, *members))
    if ttl_sec is not None:
        try:
            c.expire(k, max(1, int(ttl_sec)))
        except Exception:
            pass
    return n


def smembers(k: str) -> set[str]:
    c = get_client()
    if c is None:
        return set()
    raw = c.smembers(k) or set()
    return {str(x) for x in raw}


def scard(k: str) -> int:
    c = get_client()
    if c is None:
        return 0
    try:
        return int(c.scard(k) or 0)
    except Exception:
        return 0


def expire(k: str, ttl_sec: float | int) -> bool:
    c = get_client()
    if c is None:
        return False
    try:
        return bool(c.expire(k, max(1, int(ttl_sec))))
    except Exception:
        return False


def incr(k: str) -> int | None:
    c = get_client()
    if c is None:
        return None
    return int(c.incr(k))


def hgetall(k: str) -> dict[str, str]:
    c = get_client()
    if c is None:
        return {}
    raw = c.hgetall(k) or {}
    return {str(a): str(b) for a, b in raw.items()}


def hset_map(k: str, mapping: dict[str, Any], *, ttl_sec: float | int | None = None) -> bool:
    c = get_client()
    if c is None:
        return False
    payload = {str(a): str(b) for a, b in mapping.items() if b is not None}
    if not payload:
        return True
    c.hset(k, mapping=payload)
    if ttl_sec is not None:
        c.expire(k, max(1, int(ttl_sec)))
    return True


def hincrby(k: str, field: str, amount: int = 1) -> int | None:
    c = get_client()
    if c is None:
        return None
    return int(c.hincrby(k, field, amount))


def set_nx_ex(k: str, value: str, ttl_sec: float | int) -> bool:
    """SET key value NX EX ttl — True if acquired."""
    c = get_client()
    if c is None:
        return False
    return bool(c.set(k, value, nx=True, ex=max(1, int(ttl_sec))))


def compare_and_delete(k: str, expected: str) -> bool:
    """Delete key only if value matches (Lua)."""
    c = get_client()
    if c is None:
        return False
    script = """
    if redis.call('get', KEYS[1]) == ARGV[1] then
        return redis.call('del', KEYS[1])
    else
        return 0
    end
    """
    try:
        return bool(c.eval(script, 1, k, expected))
    except Exception:
        # Fallback: best-effort
        cur = c.get(k)
        if cur == expected:
            c.delete(k)
            return True
        return False


def renew_if_owner(k: str, expected: str, ttl_sec: float | int) -> bool:
    """Refresh TTL only if we still own the key."""
    c = get_client()
    if c is None:
        return False
    script = """
    if redis.call('get', KEYS[1]) == ARGV[1] then
        return redis.call('expire', KEYS[1], ARGV[2])
    else
        return 0
    end
    """
    try:
        return bool(c.eval(script, 1, k, expected, str(max(1, int(ttl_sec)))))
    except Exception:
        cur = c.get(k)
        if cur == expected:
            c.expire(k, max(1, int(ttl_sec)))
            return True
        return False


def get_json(k: str) -> Any | None:
    import json

    raw = get_str(k)
    if raw is None:
        return None
    try:
        return json.loads(raw)
    except (TypeError, ValueError, json.JSONDecodeError):
        return None


def set_json(k: str, value: Any, ttl_sec: float | int) -> bool:
    import json

    return set_ex(k, json.dumps(value, ensure_ascii=False, separators=(",", ":")), ttl_sec)


def worker_id() -> str:
    """Stable-enough identity for this OS process (leader election)."""
    return f"{os.getpid()}@{os.uname().nodename if hasattr(os, 'uname') else 'host'}"
