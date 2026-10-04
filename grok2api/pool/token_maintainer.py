"""Background maintenance for multi-account auth on long-running servers.

- Normalize auth.json keys (CLI client_id → per-user multi-account)
- Proactively refresh access tokens via refresh_token before expiry
- Adaptive interval / batch / skew: keep large pools warm without stampede
- Batched / concurrency-capped cycles so large pools (700+) don't freeze WSL
"""

from __future__ import annotations

import os
import threading
import time
from typing import Any

from grok2api.admin.maintenance_gate import maintenance_slot

_stop = threading.Event()
_thread: threading.Thread | None = None
_last_run: dict[str, Any] = {}
_wakeup = threading.Event()  # force an early cycle from admin UI
_force_next = False
_force_lock = threading.Lock()
# Cache both min remaining and near-expiry pressure counts (one pool scan).
_remaining_stats_cache: dict[str, Any] = {"at": 0.0, "stats": None}
_MIN_REMAINING_CACHE_TTL = 15.0


def _interval() -> float:
    try:
        from grok2api.admin.settings_store import get_token_maintain_interval_sec

        # Allow 30s+ so operators can run faster recovery batches (e.g. 90s).
        return max(30.0, float(get_token_maintain_interval_sec()))
    except Exception:
        pass
    try:
        from grok2api.config import TOKEN_MAINTAIN_INTERVAL

        return max(30.0, float(TOKEN_MAINTAIN_INTERVAL or 90.0))
    except Exception:
        pass
    try:
        return max(30.0, float(os.getenv("GROK2API_TOKEN_MAINTAIN_INTERVAL", "90")))
    except ValueError:
        return 90.0


def _skew() -> float:
    try:
        from grok2api.admin.settings_store import get_token_refresh_skew_sec

        return max(30.0, float(get_token_refresh_skew_sec()))
    except Exception:
        pass
    try:
        from grok2api.config import TOKEN_REFRESH_SKEW

        return max(30.0, float(TOKEN_REFRESH_SKEW or 120.0))
    except Exception:
        pass
    try:
        return float(os.getenv("GROK2API_TOKEN_REFRESH_SKEW", "120"))
    except ValueError:
        return 120.0


def _startup_delay() -> float:
    try:
        from grok2api.config import TOKEN_MAINTAIN_STARTUP_DELAY

        return max(5.0, float(TOKEN_MAINTAIN_STARTUP_DELAY))
    except Exception:
        return 45.0


def _remaining_stats(*, force: bool = False) -> dict[str, Any]:
    """One-pass pool scan: min remaining + urgency buckets for adaptive scheduling."""
    now = time.time()
    if (
        not force
        and _remaining_stats_cache.get("at")
        and now - float(_remaining_stats_cache["at"]) < _MIN_REMAINING_CACHE_TTL
        and isinstance(_remaining_stats_cache.get("stats"), dict)
    ):
        return dict(_remaining_stats_cache["stats"])  # type: ignore[arg-type]

    stats: dict[str, Any] = {
        "min_remaining_sec": None,
        "live": 0,
        "expired": 0,
        "le_15m": 0,
        "le_30m": 0,
        "le_60m": 0,
        "need_refresh": 0,
    }
    try:
        from grok2api.pool.auth import list_live_credentials

        remains: list[float] = []
        for c in list_live_credentials(include_expired=True, auto_refresh=False):
            stats["live"] = int(stats["live"]) + 1
            if c.expires_at is None:
                continue
            rem = float(c.expires_at) - now
            remains.append(rem)
            if rem <= 0:
                stats["expired"] = int(stats["expired"]) + 1
            if rem <= 15 * 60:
                stats["le_15m"] = int(stats["le_15m"]) + 1
            if rem <= 30 * 60:
                stats["le_30m"] = int(stats["le_30m"]) + 1
            if rem <= 60 * 60:
                stats["le_60m"] = int(stats["le_60m"]) + 1
        if remains:
            stats["min_remaining_sec"] = min(remains)
        # "Need refresh soon" ≈ already expired + within 30m. Used for batch sizing.
        stats["need_refresh"] = int(stats["expired"]) + max(
            0, int(stats["le_30m"]) - int(stats["expired"])
        )
    except Exception:
        pass

    _remaining_stats_cache["at"] = now
    _remaining_stats_cache["stats"] = dict(stats)
    return stats


def _min_remaining_seconds(*, force: bool = False) -> float | None:
    """Smallest access-token remaining lifetime across live accounts."""
    rem = _remaining_stats(force=force).get("min_remaining_sec")
    try:
        return float(rem) if rem is not None else None
    except (TypeError, ValueError):
        return None


def _adaptive_batch(
    *,
    force: bool = False,
    rem: float | None = None,
    stats: dict[str, Any] | None = None,
) -> int:
    """Scale per-cycle refresh batch with pool pressure (still hard-capped)."""
    try:
        from grok2api.config import TOKEN_REFRESH_BATCH
    except Exception:
        TOKEN_REFRESH_BATCH = 40
    base = max(1, int(TOKEN_REFRESH_BATCH or 40))
    stats = dict(stats or _remaining_stats(force=True))
    if rem is None:
        rem = stats.get("min_remaining_sec")
        try:
            rem = float(rem) if rem is not None else None
        except (TypeError, ValueError):
            rem = None

    expired = int(stats.get("expired") or 0)
    le15 = int(stats.get("le_15m") or 0)
    le30 = int(stats.get("le_30m") or 0)
    le60 = int(stats.get("le_60m") or 0)
    live = max(0, int(stats.get("live") or 0))

    # Admin force: larger but still capped — never rewrite whole 3k pool at once.
    if force:
        return min(max(base * 3, 60), 160)

    # Expired tokens first: recover quickly.
    if expired > 0 or (rem is not None and rem <= 0):
        target = max(base * 2, expired, 40)
        return min(int(target), 120)

    # Wave of near-expiry tokens (common after bulk register/import).
    # Aim to clear ~15–30m window within a few cycles, not hours.
    pressure = max(le15, le30 // 2, le60 // 6)
    if pressure >= base:
        # e.g. 200 due soon with base=40 → batch ~80–100
        target = max(base, min(pressure, base * 3))
        # Large live pools can tolerate a bit more network concurrency.
        if live >= 1500:
            target = max(target, min(base * 2, 80))
        return min(int(target), 120)

    if le60 >= base * 2:
        return min(max(base, base + base // 2), 80)

    return base


def _adaptive_skew(
    *,
    force: bool = False,
    stats: dict[str, Any] | None = None,
) -> float:
    """Background refresh horizon.

    Request-path skew stays small (TOKEN_REFRESH_SKEW). Background uses a wider
    window so large pools refresh *before* expiry waves hit the request path.
    """
    base_skew = max(120.0, _skew())
    # Historical floor: 15 minutes. Large pools need earlier pre-warm.
    skew = max(900.0, base_skew * 4)
    if force:
        return 365 * 86400.0

    stats = dict(stats or _remaining_stats(force=False))
    live = int(stats.get("live") or 0)
    le30 = int(stats.get("le_30m") or 0)
    le60 = int(stats.get("le_60m") or 0)

    # 3k-class pools: refresh up to ~45m early so batch cadence can keep up.
    if live >= 1500:
        skew = max(skew, 45 * 60)
    elif live >= 500:
        skew = max(skew, 30 * 60)

    # If a large fraction is already inside 1h, widen further to smooth the wave.
    if live > 0 and le60 >= max(40, live // 20):
        skew = max(skew, 45 * 60)
    if live > 0 and le30 >= max(20, live // 30):
        skew = max(skew, 40 * 60)

    # Hard cap: don't burn refresh_token quota hours early.
    return min(float(skew), 60 * 60)


def _next_wait_seconds() -> float:
    """
    Adaptive sleep: if any token expires soon, poll more frequently so
    expires_at gets refreshed automatically without manual clicks.
    """
    base = _interval()
    stats = _remaining_stats()
    rem = stats.get("min_remaining_sec")
    try:
        rem_f = float(rem) if rem is not None else None
    except (TypeError, ValueError):
        rem_f = None
    expired = int(stats.get("expired") or 0)
    le15 = int(stats.get("le_15m") or 0)
    le30 = int(stats.get("le_30m") or 0)

    if rem_f is None and expired <= 0:
        return base

    # Already expired (or past skew) → retry aggressively.
    if expired > 0 or (rem_f is not None and rem_f <= 0):
        # Many expired → almost continuous recovery cycles.
        if expired >= 40:
            return min(base, 20.0)
        return min(base, 30.0)

    # Dense near-expiry wave → shorter cadence so adaptive batch can drain it.
    if le15 >= 40:
        return min(base, 30.0)
    if rem_f is not None and rem_f <= 15 * 60:
        return min(base, 45.0)
    if le30 >= 80:
        return min(base, 45.0)
    if rem_f is not None and rem_f <= 3600:
        return min(base, 90.0 if le30 >= 20 else 120.0)
    return base


def run_once(*, force: bool = False) -> dict[str, Any]:
    """
    Normalize keys + refresh tokens.
    force=True refreshes every account that has refresh_token (updates expires_at),
    still batch-capped so a single cycle never fans out to all 700 accounts.
    """
    result: dict[str, Any] = {
        "ok": True,
        "normalized": None,
        "refresh": None,
        "force": force,
        "accounts": [],
        "deferred_busy": False,
    }
    # Prefer waiting for model probes to finish (tokens are more important),
    # but never hang forever if a probe cycle is stuck on network.
    with maintenance_slot("token_maintainer", blocking=True, timeout=180.0) as got:
        if not got:
            result["ok"] = True
            result["deferred_busy"] = True
            result["error"] = "maintenance slot busy — deferred"
            _last_run.clear()
            _last_run.update(result)
            _last_run["at"] = time.time()
            print("  [token-maintainer] deferred: maintenance slot busy")
            return result
        try:
            from grok2api.upstream.oidc_auth import normalize_auth_file_keys, refresh_all_accounts

            result["normalized"] = normalize_auth_file_keys()
            # Reclaim free-usage cooldowns whose wall-clock TTL elapsed so they
            # re-enter rotation without waiting for a successful model probe.
            try:
                from grok2api.pool import account_pool as _ap

                expired = _ap.expire_due_cooldowns(limit=200)
                result["expired_cooldowns"] = expired
            except Exception as e:  # noqa: BLE001
                result["expired_cooldowns"] = {"ok": False, "error": str(e)[:200]}
            # Opportunistic cleanup of permanently unusable accounts:
            # refresh_invalid marks, no-RT+no-access, no-RT+access-expired.
            # Default hard-delete; soft-disable only when env opts out.
            try:
                from grok2api.upstream.oidc_auth import purge_refresh_invalid_accounts

                purged = purge_refresh_invalid_accounts(dry_run=False)
                result["purged_dead"] = {
                    "deleted": int((purged or {}).get("deleted") or 0),
                    "disabled": int((purged or {}).get("disabled") or 0),
                    "action": (purged or {}).get("action") or "disabled",
                    "by_reason": (purged or {}).get("by_reason") or {},
                }
            except Exception as e:  # noqa: BLE001
                result["purged_dead"] = {
                    "deleted": 0,
                    "disabled": 0,
                    "error": str(e)[:200],
                }
            # Adaptive skew + batch: large pools pre-warm earlier and drain near-expiry
            # waves faster, without one-shot rewriting the entire pool.
            pre_stats = _remaining_stats(force=True)
            rem = pre_stats.get("min_remaining_sec")
            try:
                rem = float(rem) if rem is not None else None
            except (TypeError, ValueError):
                rem = None
            skew = _adaptive_skew(force=force, stats=pre_stats)
            force_batch = _adaptive_batch(force=force, rem=rem, stats=pre_stats)
            refresh = refresh_all_accounts(
                only_near_expiry=not force,
                skew_seconds=skew,
                max_accounts=force_batch,
                # Background / force batch: strict non-repeat sweep so permanent
                # refresh failures cannot monopolize every cycle.
                strict_sweep=True,
            )
            # Keep full result for the direct admin/API caller, but never retain
            # hundreds of per-account rows in the background status cache —
            # that alone made /health ~100KB on a 400+ pool.
            rows = refresh.get("results") if isinstance(refresh, dict) else None
            slim_refresh = {
                k: v
                for k, v in (refresh or {}).items()
                if k != "results"
            }
            if isinstance(rows, list):
                failed = [r for r in rows if not r.get("ok") and not r.get("skipped")]
                slim_refresh["failed_sample"] = failed[:5]
                slim_refresh["failed"] = len(failed)
                slim_refresh["skipped"] = sum(1 for r in rows if r.get("skipped"))
                slim_refresh["invalidated"] = sum(
                    1
                    for r in rows
                    if r.get("permanent")
                    or r.get("deleted")
                    or r.get("reason")
                    in ("refresh_invalid", "refresh_invalid_deleted")
                )
                slim_refresh["deleted"] = int(
                    (refresh or {}).get("deleted")
                    or sum(1 for r in rows if r.get("deleted"))
                    or 0
                )
            result["refresh"] = slim_refresh
            result["accounts"] = []  # never embed full account list in status cache
            result["accounts_total"] = int(pre_stats.get("live") or 0)
            # Refresh changed expiries; invalidate the pre-scan cache so wait/status
            # use a fresh one-pass scan instead of looping on stale near-expiry data.
            if int(slim_refresh.get("attempted") or 0) or int(slim_refresh.get("refreshed") or 0):
                _remaining_stats_cache["at"] = 0.0
                _remaining_stats_cache["stats"] = None
            result["min_remaining_sec"] = _min_remaining_seconds(force=True)
            # Attach full refresh only on the returned object for admin force-run.
            result_full = dict(result)
            result_full["refresh"] = refresh
            result = result_full
            # Operator-visible cycle log (kept short).
            try:
                sw = (refresh or {}).get("sweep") or {}
                print(
                    "  [token-maintainer] cycle: "
                    f"refreshed={slim_refresh.get('refreshed')} "
                    f"attempted={slim_refresh.get('attempted')} "
                    f"failed={slim_refresh.get('failed')} "
                    f"deferred={slim_refresh.get('deferred')} "
                    f"deleted={slim_refresh.get('deleted') or slim_refresh.get('invalidated') or 0} "
                    f"batch={force_batch} skew={int(skew)}s force={force}"
                    + (
                        f" sweep=gen:{sw.get('generation')} "
                        f"covered={sw.get('covered')}/{sw.get('need_refresh')} "
                        f"left={sw.get('remaining')}"
                        if sw
                        else ""
                    )
                )
            except Exception:
                pass
            # Surface adaptive knobs in status / health for operators.
            result["adaptive"] = {
                "batch": force_batch,
                "skew_sec": float(skew),
                "stats": {
                    k: pre_stats.get(k)
                    for k in (
                        "live",
                        "expired",
                        "le_15m",
                        "le_30m",
                        "le_60m",
                        "need_refresh",
                    )
                },
            }
        except Exception as e:  # noqa: BLE001
            result["ok"] = False
            result["error"] = str(e)[:400]
    # Persist a slim snapshot for status()/health, not the full per-account dump.
    slim_last = {
        k: v
        for k, v in result.items()
        if k not in ("accounts",)
    }
    if isinstance(slim_last.get("refresh"), dict) and "results" in slim_last["refresh"]:
        rows = slim_last["refresh"].get("results") or []
        sweep = slim_last["refresh"].get("sweep")
        slim_last["refresh"] = {
            k: v for k, v in slim_last["refresh"].items() if k != "results"
        }
        slim_last["refresh"]["failed"] = sum(
            1 for r in rows if not r.get("ok") and not r.get("skipped")
        )
        slim_last["refresh"]["skipped"] = sum(1 for r in rows if r.get("skipped"))
        slim_last["refresh"]["failed_sample"] = [
            r for r in rows if not r.get("ok") and not r.get("skipped")
        ][:5]
        if sweep:
            slim_last["refresh"]["sweep"] = sweep
    # Stamp completion time AFTER slim copy so Redis/UI always get `at`.
    finished_at = time.time()
    slim_last["at"] = finished_at
    # Prefer the just-computed remaining; fall back if cycle errored early.
    if slim_last.get("min_remaining_sec") is None:
        try:
            slim_last["min_remaining_sec"] = _min_remaining_seconds(force=True)
        except Exception:
            pass
    try:
        slim_last["next_wait_sec"] = _next_wait_seconds()
    except Exception:
        slim_last["next_wait_sec"] = _interval()
    # Durable task log for admin「任务日志」.
    # Skip pure deferred no-ops and empty background sweeps to avoid flood.
    if not slim_last.get("deferred_busy"):
        try:
            import grok2api.admin.task_log as task_log

            rf = slim_last.get("refresh") if isinstance(slim_last.get("refresh"), dict) else {}
            refreshed = int(rf.get("refreshed") or 0)
            attempted = int(rf.get("attempted") or refreshed or 0)
            failed = int(rf.get("failed") or 0)
            deleted = int(rf.get("deleted") or 0)
            disabled = int(rf.get("disabled") or rf.get("invalidated") or 0)
            purged = 0
            purged_disabled = 0
            try:
                pd = (slim_last.get("purged_dead") or {}) or {}
                purged = int(pd.get("deleted") or 0)
                purged_disabled = int(pd.get("disabled") or 0)
            except Exception:
                purged = 0
                purged_disabled = 0
            meaningful = bool(
                force
                or slim_last.get("ok") is False
                or attempted
                or refreshed
                or failed
                or deleted
                or disabled
                or purged
                or purged_disabled
                or slim_last.get("error")
            )
            if meaningful:
                cleanup_n = deleted or purged
                disable_n = disabled or purged_disabled
                extra = ""
                if cleanup_n:
                    extra += f" · 硬删除 {cleanup_n}"
                elif disable_n:
                    extra += f" · 移出轮询 {disable_n}"
                summary = (
                    f"Token 续期{'（强制）' if force else ''}："
                    f"刷新 {refreshed}/{attempted or refreshed}"
                    f" · 失败 {failed}"
                    + extra
                )
                st = "done"
                if failed and refreshed:
                    st = "partial"
                elif failed and not refreshed and attempted:
                    st = "error"
                if slim_last.get("ok") is False:
                    st = "error"
                task_log.record(
                    "token_refresh",
                    summary=summary,
                    status=st,
                    ok=bool(slim_last.get("ok", True))
                    and not (failed and not refreshed and attempted),
                    progress_done=refreshed,
                    progress_total=attempted or refreshed,
                    detail={
                        "force": force,
                        "refresh": rf,
                        "purged_dead": slim_last.get("purged_dead"),
                        "error": slim_last.get("error"),
                    },
                )
        except Exception:
            pass
    _last_run.clear()
    _last_run.update(slim_last)
    # Also mirror last run into Redis so non-leader workers can show real status.
    try:
        from grok2api.store.redis_client import key, redis_enabled, set_ex
        import json as _json

        if redis_enabled() and slim_last:
            set_ex(
                key("token_maintainer", "last_run"),
                _json.dumps(slim_last, ensure_ascii=False, default=str),
                3600,
            )
    except Exception:
        pass
    return result


def request_run_soon(*, force: bool = True) -> None:
    """Wake the background worker for an early cycle."""
    global _force_next
    with _force_lock:
        _force_next = bool(force)
    _wakeup.set()


def _worker() -> None:
    # Stagger startup so normalize + first HTTP requests aren't simultaneous
    # with model-health probe fan-out (large pools freeze WSL otherwise).
    if _stop.wait(_startup_delay()):
        return
    while not _stop.is_set():
        if not is_enabled():
            # paused via admin toggle — idle until re-enabled / stop
            _wakeup.clear()
            _wakeup.wait(timeout=5.0)
            continue
        run_once(force=False)
        wait = _next_wait_seconds()
        # Wait either for interval or an admin-triggered wakeup
        _wakeup.clear()
        triggered = _wakeup.wait(timeout=wait)
        if _stop.is_set():
            break
        if triggered:
            with _force_lock:
                global _force_next
                do_force = _force_next
                _force_next = False
            # admin asked for refresh — do a force pass (still batch-capped)
            run_once(force=do_force)


def is_enabled() -> bool:
    try:
        from grok2api.admin.settings_store import get_token_maintain_enabled
        return bool(get_token_maintain_enabled())
    except Exception:
        return os.getenv("GROK2API_TOKEN_MAINTAIN", "1").lower() not in ("0", "false", "no")


def start_background() -> None:
    global _thread
    if not is_enabled():
        return
    if _thread and _thread.is_alive():
        return
    _stop.clear()
    _thread = threading.Thread(target=_worker, name="g2a-token-maintainer", daemon=True)
    _thread.start()


def stop_background() -> None:
    global _thread
    _stop.set()
    _wakeup.set()
    th = _thread
    if th and th.is_alive():
        th.join(timeout=2.0)
    _thread = None


def status(*, light: bool = False) -> dict[str, Any]:
    local_running = bool(_thread and _thread.is_alive())
    try:
        from grok2api.config import TOKEN_REFRESH_BATCH, TOKEN_REFRESH_WORKERS
    except Exception:
        TOKEN_REFRESH_BATCH = 20
        TOKEN_REFRESH_WORKERS = 2
    # Cluster-aware: only leader process has the thread. Non-leaders would
    # otherwise always report running=false and confuse the admin UI.
    cluster_running = local_running
    leader_id = None
    is_leader = False
    try:
        from grok2api.store.leader import is_leader as _is_leader, status as _leader_status
        is_leader = bool(_is_leader())
        ls = _leader_status()
        leader_id = ls.get("leader_id")
        if local_running:
            cluster_running = True
        elif is_enabled():
            # Only claim cluster_running when a live Redis leader lock exists.
            # A stale last_run snapshot alone must NOT report running=true.
            try:
                from grok2api.store.redis_client import get_str, key, redis_enabled
                if redis_enabled():
                    lid = get_str(key("lock", "maintainer_leader"))
                    if lid:
                        leader_id = lid
                        # Presence of lock key (with TTL) means some worker owns leadership.
                        cluster_running = True
            except Exception:
                pass
    except Exception:
        pass
    # Adaptive values help operators verify large-pool tuning without reading logs.
    try:
        adaptive_skew = _adaptive_skew(force=False)
    except Exception:
        adaptive_skew = max(900.0, _skew() * 4)
    try:
        adaptive_batch = _adaptive_batch(force=False)
    except Exception:
        adaptive_batch = TOKEN_REFRESH_BATCH
    out = {
        "running": bool(cluster_running),
        "local_running": local_running,
        "cluster_running": bool(cluster_running),
        "leader_running": bool(cluster_running and is_enabled()),
        "is_leader": is_leader,
        "leader_id": leader_id,
        "enabled": is_enabled(),
        "interval_sec": _interval(),
        "refresh_skew_sec": _skew(),
        "background_skew_sec": adaptive_skew,
        "startup_delay_sec": _startup_delay(),
        "refresh_workers": TOKEN_REFRESH_WORKERS,
        "refresh_batch": TOKEN_REFRESH_BATCH,
        "adaptive_batch": adaptive_batch,
    }
    last = dict(_last_run) if _last_run else None
    # Non-leader workers: read mirrored last_run from Redis.
    if last is None:
        try:
            from grok2api.store.redis_client import get_str, key, redis_enabled
            import json as _json

            if redis_enabled():
                raw = get_str(key("token_maintainer", "last_run"))
                if raw:
                    last = _json.loads(raw)
        except Exception:
            last = None

    # Always surface fields the admin UI needs — even in light mode.
    # Prefer live local compute on the leader; fall back to last-run snapshot.
    rem = None
    next_wait = None
    if local_running:
        try:
            rem = _min_remaining_seconds(force=not light)
        except Exception:
            rem = None
        try:
            next_wait = _next_wait_seconds()
        except Exception:
            next_wait = _interval()
    if rem is None and isinstance(last, dict):
        try:
            rem = float(last.get("min_remaining_sec")) if last.get("min_remaining_sec") is not None else None
        except (TypeError, ValueError):
            rem = None
    if next_wait is None and isinstance(last, dict) and last.get("next_wait_sec") is not None:
        try:
            next_wait = float(last.get("next_wait_sec"))
        except (TypeError, ValueError):
            next_wait = None
    if next_wait is None:
        next_wait = _interval()

    out["min_remaining_sec"] = rem
    out["next_wait_sec"] = next_wait

    if light:
        # Keep /health tiny: only last outcome summary, no per-account rows.
        if last:
            refresh = last.get("refresh")
            if isinstance(refresh, dict):
                refresh = {
                    k: v
                    for k, v in refresh.items()
                    if k
                    in (
                        "ok",
                        "refreshed",
                        "deferred",
                        "attempted",
                        "workers",
                        "failed",
                        "skipped",
                        "invalidated",
                        "deleted",
                        "sweep",
                    )
                }
            adaptive = last.get("adaptive")
            if isinstance(adaptive, dict):
                adaptive = {
                    k: v
                    for k, v in adaptive.items()
                    if k in ("batch", "skew_sec", "stats")
                }
            out["last"] = {
                "ok": last.get("ok"),
                "at": last.get("at"),
                "force": last.get("force"),
                "deferred_busy": last.get("deferred_busy"),
                "accounts_total": last.get("accounts_total"),
                "min_remaining_sec": last.get("min_remaining_sec")
                if last.get("min_remaining_sec") is not None
                else rem,
                "next_wait_sec": last.get("next_wait_sec")
                if last.get("next_wait_sec") is not None
                else next_wait,
                "refresh": refresh,
                "adaptive": adaptive,
            }
        else:
            out["last"] = None
    else:
        out["last"] = last
    return out
