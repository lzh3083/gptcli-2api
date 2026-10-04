"""Serialize heavy background maintenance across workers.

Token refresh and model probes both open outbound HTTP and rewrite shared
state. On a large multi-account pool they must not run at the same time or the
Uvicorn worker + disk become unresponsive.

When REDIS_URL is configured, the slot is a Redis lock shared by all
processes; otherwise a process-local threading.Lock is used (single-worker).
"""

from __future__ import annotations

import threading
import time
from contextlib import contextmanager
from typing import Iterator

from grok2api.config import MAINTENANCE_LOCK_TIMEOUT

_lock = threading.Lock()
_holder: str | None = None
_held_since = 0.0


def _redis_mode() -> bool:
    try:
        from grok2api.store.redis_client import redis_enabled

        return redis_enabled()
    except Exception:
        return False


@contextmanager
def maintenance_slot(
    owner: str,
    *,
    timeout: float | None = None,
    blocking: bool = True,
) -> Iterator[bool]:
    """
    Acquire the global maintenance slot.

    Yields True when the slot was acquired. When blocking=False and the slot is
    busy, yields False immediately so the caller can defer work.
    """
    global _holder, _held_since
    wait = MAINTENANCE_LOCK_TIMEOUT if timeout is None else max(0.0, float(timeout))

    if _redis_mode():
        try:
            from grok2api.store.locks_redis import redis_maintenance_slot

            with redis_maintenance_slot(owner, timeout=wait, blocking=blocking) as ok:
                if ok:
                    _holder = owner
                    _held_since = time.time()
                try:
                    yield ok
                finally:
                    if ok:
                        _holder = None
                        _held_since = 0.0
            return
        except Exception:
            # Fall through to local lock if Redis path fails mid-flight.
            pass

    acquired = _lock.acquire(blocking=blocking, timeout=wait if blocking else -1)
    if not acquired:
        yield False
        return
    _holder = owner
    _held_since = time.time()
    try:
        yield True
    finally:
        _holder = None
        _held_since = 0.0
        _lock.release()


def status() -> dict[str, float | str | bool | None]:
    backend = "redis" if _redis_mode() else "local"
    extra: dict = {}
    if backend == "redis":
        try:
            from grok2api.store.locks_redis import redis_lock_status

            extra = redis_lock_status()
        except Exception as e:  # noqa: BLE001
            extra = {"error": str(e)}
    held = _lock.locked() if backend == "local" else bool(extra.get("busy"))
    holder = _holder if held else (extra.get("holder") if backend == "redis" else None)
    return {
        "busy": held or bool(extra.get("busy")),
        "holder": holder if (held or extra.get("busy")) else None,
        "held_for_sec": (time.time() - _held_since) if held and _held_since else 0.0,
        "timeout_sec": MAINTENANCE_LOCK_TIMEOUT,
        "backend": backend,
    }
