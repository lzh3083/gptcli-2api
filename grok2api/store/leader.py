"""Maintainer leader election (Redis) so only one process runs background jobs.

File / single-worker mode: this process always leads.
Multi-worker + Redis: SET NX lock with periodic renew + re-election.

Critical: after a container recreate the old leader lock may still be held for
up to TTL seconds, so every worker can miss leadership at startup. We keep a
watcher that retries election and starts/stops maintainers when leadership
changes.
"""

from __future__ import annotations

import threading
import time
from typing import Any

from grok2api.config import MAINTAINER_LEADER, MAINTAINER_LEADER_RENEW, MAINTAINER_LEADER_TTL, WORKERS

_lock = threading.Lock()
_is_leader = False
_leader_id: str | None = None
_watch_thread: threading.Thread | None = None
_stop = threading.Event()
_started = False
_maintainers_started = False


def _want_force_leader() -> bool | None:
    """None = auto, True = force lead, False = never lead."""
    v = (MAINTAINER_LEADER or "auto").lower()
    if v in ("1", "true", "yes", "on", "always"):
        return True
    if v in ("0", "false", "no", "off", "never"):
        return False
    return None  # auto


def is_leader() -> bool:
    with _lock:
        return _is_leader


def status() -> dict[str, Any]:
    lid = None
    is_lead = False
    with _lock:
        lid = _leader_id
        is_lead = _is_leader
    # Always surface the redis lock owner so non-leader workers can show cluster state.
    try:
        from grok2api.store.redis_client import get_str, key, redis_enabled

        if redis_enabled():
            remote = get_str(key("lock", "maintainer_leader"))
            if remote:
                lid = remote
    except Exception:
        pass
    return {
        "is_leader": is_lead,
        "leader_id": lid,
        "mode": MAINTAINER_LEADER or "auto",
        "workers": WORKERS,
        "ttl_sec": MAINTAINER_LEADER_TTL,
        "renew_sec": MAINTAINER_LEADER_RENEW,
        "maintainers_started": _maintainers_started,
    }


def _start_maintainers_if_needed() -> None:
    """Idempotently start token + model maintainers on the elected leader."""
    global _maintainers_started
    with _lock:
        if _maintainers_started:
            return
        _maintainers_started = True
    try:
        import grok2api.pool.token_maintainer as token_maintainer

        token_maintainer.start_background()
        print("  [leader] token maintainer started", flush=True)
    except Exception as e:  # noqa: BLE001
        print(f"  [leader] token maintainer start failed: {e}", flush=True)
    try:
        import grok2api.pool.model_health as model_health

        model_health.start_background()
        print("  [leader] model health started", flush=True)
    except Exception as e:  # noqa: BLE001
        print(f"  [leader] model health start failed: {e}", flush=True)


def _stop_maintainers_if_needed() -> None:
    """Stop maintainers when leadership is lost (best-effort)."""
    global _maintainers_started
    with _lock:
        if not _maintainers_started:
            return
        _maintainers_started = False
    try:
        import grok2api.pool.token_maintainer as token_maintainer

        token_maintainer.stop_background()
    except Exception:
        pass
    try:
        import grok2api.pool.model_health as model_health

        model_health.stop_background()
    except Exception:
        pass
    print("  [leader] maintainers stopped (lost leadership)", flush=True)


def _set_leader_state(is_lead: bool, lid: str | None) -> bool:
    """Update local leader flags. Returns True if leadership was newly gained."""
    global _is_leader, _leader_id
    gained = False
    with _lock:
        was = _is_leader
        _is_leader = bool(is_lead)
        _leader_id = lid if is_lead else None
        gained = bool(is_lead) and not was
        lost = (not is_lead) and was
    if gained:
        _start_maintainers_if_needed()
    elif lost:
        _stop_maintainers_if_needed()
    return gained


def try_become_leader() -> bool:
    """Attempt to acquire leadership. Idempotent; safe to call repeatedly."""
    force = _want_force_leader()
    if force is False:
        _set_leader_state(False, None)
        return False
    if force is True or WORKERS <= 1:
        _set_leader_state(True, "local" if WORKERS <= 1 else "forced")
        return True

    # auto + multi-worker → need Redis
    try:
        from grok2api.store.redis_client import (
            get_str,
            key,
            redis_enabled,
            renew_if_owner,
            set_nx_ex,
            worker_id,
        )
    except Exception:
        # No redis module path — fall back to local lead only if single worker
        lead = WORKERS <= 1
        _set_leader_state(lead, "local-fallback" if lead else None)
        return lead

    if not redis_enabled():
        # Multi-worker without redis should have been rejected at startup;
        # be conservative: do not start maintainers.
        _set_leader_state(False, None)
        return False

    wid = worker_id()
    lock_key = key("lock", "maintainer_leader")
    acquired = set_nx_ex(lock_key, wid, MAINTAINER_LEADER_TTL)
    if not acquired:
        # Maybe we already own it (restart race / renew path)
        cur = get_str(lock_key)
        if cur == wid:
            acquired = renew_if_owner(lock_key, wid, MAINTAINER_LEADER_TTL)

    if acquired:
        _set_leader_state(True, wid)
        return True

    # Someone else holds the lock (or lock empty and race lost).
    _set_leader_state(False, None)
    return False


def _watch_loop() -> None:
    """Keep trying to elect / renew leadership for the process lifetime."""
    force = _want_force_leader()
    if force is False:
        return
    if force is True or WORKERS <= 1:
        # No redis election needed; maintainers already started via should_start.
        return

    try:
        from grok2api.store.redis_client import (
            get_str,
            key,
            redis_enabled,
            renew_if_owner,
            worker_id,
        )
    except Exception:
        return

    # Poll a bit faster than renew so a dead leader's TTL expiry is picked up soon.
    interval = max(2.0, min(float(MAINTAINER_LEADER_RENEW), float(MAINTAINER_LEADER_TTL) / 2.0))
    while not _stop.wait(interval):
        if not redis_enabled():
            _set_leader_state(False, None)
            continue
        wid = worker_id()
        lock_key = key("lock", "maintainer_leader")
        try:
            cur = get_str(lock_key)
            if cur == wid:
                # Renew ownership
                ok = renew_if_owner(lock_key, wid, MAINTAINER_LEADER_TTL)
                if ok:
                    _set_leader_state(True, wid)
                else:
                    # Lost ownership mid-flight
                    _set_leader_state(False, None)
                continue
            if cur:
                # Another live owner (or stale until TTL)
                _set_leader_state(False, None)
                continue
            # Lock free — attempt acquire
            if try_become_leader():
                print(f"  [leader] elected {wid}", flush=True)
        except Exception as e:  # noqa: BLE001
            print(f"  [leader] watch error: {e}", flush=True)


def _ensure_watch_thread() -> None:
    global _watch_thread, _started
    force = _want_force_leader()
    if force is False:
        return
    if force is True or WORKERS <= 1:
        return
    with _lock:
        if _started and _watch_thread and _watch_thread.is_alive():
            return
        _started = True
        _stop.clear()
        _watch_thread = threading.Thread(
            target=_watch_loop, name="g2a-leader-watch", daemon=True
        )
        _watch_thread.start()


def release_leader() -> None:
    """Best-effort release (shutdown)."""
    global _is_leader, _started, _maintainers_started
    _stop.set()
    force = _want_force_leader()
    if force is True or WORKERS <= 1:
        with _lock:
            _is_leader = False
            _maintainers_started = False
        return
    try:
        from grok2api.store.redis_client import compare_and_delete, key, redis_enabled, worker_id

        if redis_enabled():
            compare_and_delete(key("lock", "maintainer_leader"), worker_id())
    except Exception:
        pass
    with _lock:
        _is_leader = False
        _started = False
        _maintainers_started = False


def should_start_maintainers() -> bool:
    """Call once at process startup.

    Returns True only when this process is leader *right now* so the first
    maintainer start can happen immediately. Always arms the re-election
    watcher so a later free lock still starts maintainers.
    """
    force = _want_force_leader()
    if force is False:
        _set_leader_state(False, None)
        return False
    if force is True:
        _set_leader_state(True, "forced")
        return True
    if WORKERS <= 1:
        _set_leader_state(True, "local")
        return True

    # Multi-worker auto: try once now, then keep watching.
    acquired = try_become_leader()
    _ensure_watch_thread()
    return bool(acquired)
