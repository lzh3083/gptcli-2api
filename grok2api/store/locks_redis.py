"""Redis distributed lock for maintenance_gate."""

from __future__ import annotations

import time
from contextlib import contextmanager
from typing import Iterator

from grok2api.store.redis_client import (
    compare_and_delete,
    get_str,
    key,
    redis_enabled,
    renew_if_owner,
    set_nx_ex,
    worker_id,
)


def maintenance_lock_key() -> str:
    return key("lock", "maintenance")


@contextmanager
def redis_maintenance_slot(
    owner: str,
    *,
    timeout: float,
    blocking: bool,
) -> Iterator[bool]:
    """
    Acquire g2a:lock:maintenance with token=owner@worker.

    Yields True if acquired. When blocking=False and busy, yields False.
    """
    if not redis_enabled():
        yield False
        return

    token = f"{owner}|{worker_id()}|{time.time():.0f}"
    lock_key = maintenance_lock_key()
    ttl = max(5, int(timeout) if timeout else 60)
    deadline = time.time() + (timeout if blocking else 0.0)
    acquired = False

    while True:
        if set_nx_ex(lock_key, token, ttl):
            acquired = True
            break
        if not blocking:
            break
        if time.time() >= deadline:
            break
        time.sleep(0.05)

    if not acquired:
        yield False
        return

    # While held, optionally keep renewing if work runs long — caller usually
    # finishes within timeout; renew halfway through wait loop is enough for
    # long probe batches when we spawn a tiny renew thread.
    stop = False

    def _renew() -> None:
        while not stop:
            time.sleep(max(1.0, ttl / 3))
            if stop:
                break
            renew_if_owner(lock_key, token, ttl)

    import threading

    t = threading.Thread(target=_renew, name="g2a-maint-lock-renew", daemon=True)
    t.start()
    try:
        yield True
    finally:
        stop = True
        compare_and_delete(lock_key, token)


def redis_lock_status() -> dict:
    if not redis_enabled():
        return {"backend": "none"}
    cur = get_str(maintenance_lock_key())
    return {
        "backend": "redis",
        "busy": bool(cur),
        "holder": cur.split("|", 1)[0] if cur else None,
        "token": cur,
    }
