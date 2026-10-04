"""Redis JSON sessions for device-login and registration progress."""

from __future__ import annotations

import os
from typing import Any

from grok2api.store.redis_client import (
    delete,
    get_client,
    get_json,
    key,
    redis_enabled,
    set_json,
)

DEVICE_TTL = 15 * 60  # 15 minutes
# Bulk registration (thousands of accounts) can run well beyond a few hours.
# Keep durable batch/session metadata long enough that a paused/resumed
# 5k job does not silently lose progress mid-flight.
try:
    REG_TTL = max(
        6 * 3600,
        min(
            14 * 86400,
            int(os.environ.get("GROK2API_REG_TTL_SEC", str(72 * 3600)) or (72 * 3600)),
        ),
    )
except (TypeError, ValueError):
    REG_TTL = 72 * 3600  # 72 hours
ADMIN_TTL = 7 * 86400  # 7 days


def _device_key(session_id: str) -> str:
    return key("device", "sess", session_id)


def _reg_sess_key(session_id: str) -> str:
    return key("reg", "sess", session_id)


def _reg_batch_key(batch_id: str) -> str:
    return key("reg", "batch", batch_id)


def _admin_key(token: str) -> str:
    return key("admin", "sess", token)


# ── device login ─────────────────────────────────────────────────────────────


def device_put(session_id: str, sess: dict[str, Any], *, ttl: int = DEVICE_TTL) -> None:
    if not redis_enabled():
        return
    # Drop non-serializable runtime objects if any
    clean = {k: v for k, v in sess.items() if not str(k).startswith("_")}
    set_json(_device_key(session_id), clean, ttl)


def device_get(session_id: str) -> dict[str, Any] | None:
    if not redis_enabled():
        return None
    data = get_json(_device_key(session_id))
    return data if isinstance(data, dict) else None


def device_delete(session_id: str) -> None:
    if not redis_enabled():
        return
    delete(_device_key(session_id))


def device_list() -> list[tuple[str, dict[str, Any]]]:
    if not redis_enabled():
        return []
    c = get_client()
    if c is None:
        return []
    out: list[tuple[str, dict[str, Any]]] = []
    for k in c.scan_iter(match=key("device", "sess", "*"), count=50):
        data = get_json(str(k))
        if isinstance(data, dict):
            sid = str(k).rsplit(":", 1)[-1]
            out.append((sid, data))
    return out


# ── registration ─────────────────────────────────────────────────────────────


def reg_sess_put(session_id: str, sess: dict[str, Any], *, ttl: int = REG_TTL) -> None:
    if not redis_enabled():
        return
    # Never mirror process-local handles (_receiver / clients) — they break
    # json.dumps and leave other workers unable to observe progress.
    clean: dict[str, Any] = {}
    for k, v in (sess or {}).items():
        if not isinstance(k, str):
            continue
        if k.startswith("_") or k in ("_client", "_oauth_client", "_receiver"):
            continue
        if callable(v):
            continue
        clean[k] = v
    try:
        set_json(_reg_sess_key(session_id), clean, ttl)
    except (TypeError, ValueError):
        # Last resort: drop any remaining non-JSON values.
        safe = {}
        for k, v in clean.items():
            try:
                import json as _json

                _json.dumps(v)
                safe[k] = v
            except Exception:
                continue
        set_json(_reg_sess_key(session_id), safe, ttl)


def reg_sess_touch(session_id: str, *, ttl: int = REG_TTL) -> bool:
    """Refresh Redis TTL only — do not mutate session payload/timestamps."""
    if not redis_enabled():
        return False
    from grok2api.store.redis_client import expire

    return bool(expire(_reg_sess_key(session_id), ttl))


def reg_sess_get(session_id: str) -> dict[str, Any] | None:
    if not redis_enabled():
        return None
    data = get_json(_reg_sess_key(session_id))
    return data if isinstance(data, dict) else None


def reg_sess_delete(session_id: str) -> None:
    if not redis_enabled():
        return
    delete(_reg_sess_key(session_id))


def reg_sess_list() -> list[dict[str, Any]]:
    if not redis_enabled():
        return []
    c = get_client()
    if c is None:
        return []
    out: list[dict[str, Any]] = []
    for k in c.scan_iter(match=key("reg", "sess", "*"), count=50):
        data = get_json(str(k))
        if isinstance(data, dict):
            out.append(data)
    return out


def reg_batch_put(batch_id: str, batch: dict[str, Any], *, ttl: int = REG_TTL) -> None:
    if not redis_enabled():
        return
    set_json(_reg_batch_key(batch_id), batch, ttl)


def reg_batch_touch(batch_id: str, *, ttl: int = REG_TTL) -> bool:
    """Refresh Redis TTL only — do not mutate batch payload/timestamps."""
    if not redis_enabled():
        return False
    from grok2api.store.redis_client import expire

    return bool(expire(_reg_batch_key(batch_id), ttl))


def reg_batch_get(batch_id: str) -> dict[str, Any] | None:
    if not redis_enabled():
        return None
    data = get_json(_reg_batch_key(batch_id))
    return data if isinstance(data, dict) else None


def reg_batch_list() -> list[dict[str, Any]]:
    if not redis_enabled():
        return []
    c = get_client()
    if c is None:
        return []
    out: list[dict[str, Any]] = []
    for k in c.scan_iter(match=key("reg", "batch", "*"), count=50):
        data = get_json(str(k))
        if isinstance(data, dict):
            out.append(data)
    return out


# ── admin UI sessions ────────────────────────────────────────────────────────


def admin_session_put(token: str, *, ttl: int = ADMIN_TTL) -> None:
    if not redis_enabled() or not token:
        return
    set_json(_admin_key(token), {"ts": __import__("time").time()}, ttl)


def admin_session_touch(token: str, *, ttl: int = ADMIN_TTL) -> bool:
    if not redis_enabled() or not token:
        return False
    existing = get_json(_admin_key(token))
    if existing is None:
        return False
    set_json(_admin_key(token), {"ts": __import__("time").time()}, ttl)
    return True


def admin_session_get(token: str) -> bool:
    if not redis_enabled() or not token:
        return False
    return get_json(_admin_key(token)) is not None


def admin_session_delete(token: str) -> None:
    if not redis_enabled() or not token:
        return
    delete(_admin_key(token))
