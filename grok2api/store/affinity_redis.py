"""Redis-backed conversation affinity (TTL keys)."""

from __future__ import annotations

import json
import time
from typing import Any

from grok2api.store.redis_client import delete, get_str, key, redis_enabled, set_ex


def _k(fp: str) -> str:
    return key("affinity", fp)


def _parse_entry(raw: str | None) -> dict[str, Any] | None:
    if not raw:
        return None
    account_id: str | None = None
    hits = 0
    bound_at = time.time()
    session_fp: str | None = None
    prompt_cache_key: str | None = None
    try:
        data = json.loads(raw)
        if isinstance(data, dict):
            account_id = str(data.get("account_id") or "") or None
            hits = int(data.get("hits") or 0)
            bound_at = float(data.get("bound_at") or bound_at)
            sfp = data.get("session_fp")
            if sfp:
                session_fp = str(sfp)
            pck = data.get("prompt_cache_key")
            if pck:
                prompt_cache_key = str(pck)
        else:
            account_id = str(raw)
    except (TypeError, ValueError, json.JSONDecodeError):
        account_id = str(raw)
    if not account_id:
        return None
    out: dict[str, Any] = {
        "account_id": account_id,
        "hits": hits,
        "bound_at": bound_at,
    }
    if session_fp:
        out["session_fp"] = session_fp
    if prompt_cache_key:
        out["prompt_cache_key"] = prompt_cache_key
    return out


def get_entry(fingerprint: str, *, ttl_sec: float) -> dict[str, Any] | None:
    """Return full affinity entry and refresh TTL/hits."""
    if not redis_enabled() or not fingerprint:
        return None
    raw = get_str(_k(fingerprint))
    entry = _parse_entry(raw)
    if not entry:
        return None
    account_id = entry["account_id"]
    hits = int(entry.get("hits") or 0)
    bound_at = float(entry.get("bound_at") or time.time())
    payload: dict[str, Any] = {
        "account_id": account_id,
        "bound_at": bound_at,
        "last_seen": time.time(),
        "hits": hits + 1,
    }
    sfp = entry.get("session_fp")
    if sfp:
        payload["session_fp"] = sfp
    pck = entry.get("prompt_cache_key")
    if pck:
        payload["prompt_cache_key"] = pck
    set_ex(_k(fingerprint), json.dumps(payload, separators=(",", ":")), ttl_sec)
    return payload


def get(fingerprint: str, *, ttl_sec: float) -> str | None:
    entry = get_entry(fingerprint, ttl_sec=ttl_sec)
    if not entry:
        return None
    return str(entry.get("account_id") or "") or None


def bind(
    fingerprint: str,
    account_id: str,
    *,
    ttl_sec: float,
    session_fp: str | None = None,
    prompt_cache_key: str | None = None,
) -> None:
    if not redis_enabled() or not fingerprint or not account_id:
        return
    now = time.time()
    prev_hits = 0
    prev_session: str | None = None
    prev_pck: str | None = None
    prev_bound = now
    raw = get_str(_k(fingerprint))
    if raw:
        prev = _parse_entry(raw)
        if prev:
            prev_hits = int(prev.get("hits") or 0)
            prev_session = prev.get("session_fp")
            prev_pck = prev.get("prompt_cache_key")
            if prev.get("account_id") == account_id:
                prev_bound = float(prev.get("bound_at") or now)
    sfp = (session_fp or "").strip() or prev_session
    pck = (prompt_cache_key or "").strip() or prev_pck
    payload: dict[str, Any] = {
        "account_id": account_id,
        "bound_at": prev_bound if (prev_hits and raw) else now,
        "last_seen": now,
        "hits": (prev_hits + 1) if prev_hits else 1,
    }
    if raw:
        prev = _parse_entry(raw)
        if prev and prev.get("account_id") == account_id:
            payload["bound_at"] = float(prev.get("bound_at") or now)
            payload["hits"] = int(prev.get("hits") or 0) + 1
    if sfp:
        payload["session_fp"] = sfp
    if pck:
        payload["prompt_cache_key"] = pck
    set_ex(_k(fingerprint), json.dumps(payload, separators=(",", ":")), ttl_sec)


def clear(fingerprint: str) -> None:
    if not redis_enabled() or not fingerprint:
        return
    delete(_k(fingerprint))


def clear_for_account(account_id: str, *, scan_count: int = 200) -> int:
    """Delete every affinity entry bound to ``account_id``.

    No secondary index — SCAN ``g2a:affinity:*``. Intended for rare paths
    (account disable / quota kill), not the hot request path.
    Mirrors CLIProxyAPI SessionAffinitySelector.InvalidateAuth.
    """
    if not redis_enabled() or not account_id:
        return 0
    aid = str(account_id)
    try:
        from grok2api.store.redis_client import get_client

        c = get_client()
        if c is None:
            return 0
        pattern = key("affinity", "*")
        removed = 0
        for k in c.scan_iter(match=pattern, count=scan_count):
            raw = c.get(k)
            if not raw:
                continue
            try:
                data = json.loads(raw)
            except Exception:
                data = None
            bound = None
            if isinstance(data, dict):
                bound = str(data.get("account_id") or "") or None
            else:
                bound = str(raw)
            if bound == aid:
                try:
                    c.delete(k)
                    removed += 1
                except Exception:
                    pass
        return removed
    except Exception:
        return 0


def status_sample(*, max_n: int = 8) -> dict[str, Any]:
    """Best-effort sample (SCAN). Costly on huge keyspaces — keep small."""
    if not redis_enabled():
        return {"active": 0, "sample": []}
    try:
        from grok2api.store.redis_client import get_client

        c = get_client()
        if c is None:
            return {"active": 0, "sample": []}
        pattern = key("affinity", "*")
        sample: list[dict[str, Any]] = []
        count = 0
        for k in c.scan_iter(match=pattern, count=50):
            count += 1
            if len(sample) >= max_n:
                continue
            raw = c.get(k)
            if not raw:
                continue
            try:
                data = json.loads(raw)
            except Exception:
                data = {"account_id": str(raw)}
            fp = str(k).split(":")[-1]
            sample.append(
                {
                    "fp": fp[:12] + "…",
                    "account_id": str(data.get("account_id") or "")[:48],
                    "hits": data.get("hits"),
                    "session_fp": (
                        (str(data.get("session_fp") or "")[:16] + "…")
                        if data.get("session_fp")
                        else None
                    ),
                    "age_sec": int(
                        time.time() - float(data.get("bound_at") or time.time())
                    ),
                }
            )
        return {"active": count, "sample": sample}
    except Exception as e:  # noqa: BLE001
        return {"active": 0, "sample": [], "error": str(e)}
