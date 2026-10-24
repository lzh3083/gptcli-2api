"""Dual-mode (Random Interval + Low-Watermark Replenish) Auto-Registration Scheduler.

Runs as a background daemon inside the registration sidecar service.
Persists configuration in `registration_config` and runtime state/history in
PostgreSQL `app_settings` under key `auto_register_scheduler_state`.
"""

from __future__ import annotations

import logging
import os
import random
import threading
import time
from typing import Any, Callable

logger = logging.getLogger(__name__)

STATE_SETTING_KEY = "auto_register_scheduler_state"
MAX_HISTORY_ITEMS = 25
try:
    STALE_SESSION_TIMEOUT_SEC = max(
        180.0,
        float(os.environ.get("GROK2API_SCHEDULER_STALE_SESSION_SEC", "900") or 900.0),
    )
except (TypeError, ValueError):
    STALE_SESSION_TIMEOUT_SEC = 900.0

TERMINAL_SESSION_STATUSES = frozenset({
    "success", "completed", "done", "failed", "error", "stopped",
    "abandoned", "expired", "imported", "cancelled", "canceled",
    "timed_out", "timeout",
})


def _record_auto_reg_task(
    *,
    task_id: str,
    status: str,
    summary: str,
    progress_done: int = 0,
    progress_total: int = 0,
    finished: bool = False,
    ok: bool | None = None,
    detail: dict[str, Any] | None = None,
) -> None:
    """Write scheduled registration progress/outcome into PostgreSQL task_logs."""
    tid = str(task_id or "").strip()
    if not tid:
        return
    try:
        import grok2api.admin.task_log as task_log

        task_log.record(
            "auto_register",
            task_id=tid,
            summary=str(summary or f"定时注册 {tid}")[:500],
            status=str(status or "done"),
            ok=ok,
            progress_done=int(progress_done or 0),
            progress_total=int(progress_total or 0),
            finished=bool(finished),
            detail=detail if isinstance(detail, dict) else {},
        )
    except Exception:
        pass


DEFAULT_AUTO_REGISTER_CONFIG: dict[str, Any] = {
    "auto_register_enabled": False,
    "auto_register_mode": "both",  # "both" | "interval" | "watermark"
    "auto_register_min_interval_min": 60,
    "auto_register_max_interval_min": 180,
    "auto_register_batch_size": 2,
    "auto_register_watermark_min": 20,
    "auto_register_watermark_target": 30,
    "auto_register_watermark_cooldown_min": 15,
    "auto_register_max_pool_size": 100,
    "auto_register_max_consecutive_failures": 3,
}


def _as_bool(val: Any, default: bool = False) -> bool:
    if isinstance(val, bool):
        return val
    if val is None:
        return default
    s = str(val).strip().lower()
    if s in ("1", "true", "yes", "on"):
        return True
    if s in ("0", "false", "no", "off", ""):
        return False
    return default


def _as_int(val: Any, default: int, minimum: int = 0, maximum: int = 100000) -> int:
    try:
        n = int(float(str(val).strip()))
    except (TypeError, ValueError):
        n = default
    return max(minimum, min(maximum, n))


def extract_auto_register_config(raw: dict[str, Any] | None) -> dict[str, Any]:
    """Normalize auto-registration fields from registration_config dict."""
    cfg = raw if isinstance(raw, dict) else {}
    enabled = _as_bool(cfg.get("auto_register_enabled"), False)
    mode = str(cfg.get("auto_register_mode") or "both").strip().lower()
    if mode not in ("both", "interval", "watermark"):
        mode = "both"

    min_iv = _as_int(cfg.get("auto_register_min_interval_min"), 60, 1, 10080)
    max_iv = _as_int(cfg.get("auto_register_max_interval_min"), 180, 1, 10080)
    if max_iv < min_iv:
        max_iv = min_iv

    batch_size = _as_int(cfg.get("auto_register_batch_size"), 2, 1, 50)
    wm_min = _as_int(cfg.get("auto_register_watermark_min"), 20, 0, 50000)
    wm_target = _as_int(cfg.get("auto_register_watermark_target"), 30, 1, 50000)
    if wm_target < wm_min:
        wm_target = max(1, wm_min)

    wm_cooldown = _as_int(cfg.get("auto_register_watermark_cooldown_min"), 15, 1, 1440)
    max_pool = _as_int(cfg.get("auto_register_max_pool_size"), 100, 0, 100000)
    max_fails = _as_int(cfg.get("auto_register_max_consecutive_failures"), 3, 1, 20)

    return {
        "auto_register_enabled": enabled,
        "auto_register_mode": mode,
        "auto_register_min_interval_min": min_iv,
        "auto_register_max_interval_min": max_iv,
        "auto_register_batch_size": batch_size,
        "auto_register_watermark_min": wm_min,
        "auto_register_watermark_target": wm_target,
        "auto_register_watermark_cooldown_min": wm_cooldown,
        "auto_register_max_pool_size": max_pool,
        "auto_register_max_consecutive_failures": max_fails,
    }


class AutoRegisterScheduler:
    """Background daemon managing dual-mode random-interval + watermark registration."""

    def __init__(
        self,
        start_job_fn: Callable[[dict[str, Any]], dict[str, Any]],
        get_adapter_fn: Callable[[], Any],
    ) -> None:
        self._start_job_fn = start_job_fn
        self._get_adapter_fn = get_adapter_fn
        self._lock = threading.RLock()
        self._stop_event = threading.Event()
        self._wake_event = threading.Event()
        self._thread: threading.Thread | None = None

        # Runtime state
        self._state: dict[str, Any] = {
            "phase": "disabled",
            "next_run_at": None,
            "next_interval_sec": None,
            "next_watermark_at": None,
            "last_run_at": None,
            "last_finish_at": None,
            "last_trigger_reason": None,
            "last_result": None,
            "active_batch_id": None,
            "active_session_ids": [],
            "active_requested_count": 0,
            "consecutive_failures": 0,
            "circuit_broken": False,
            "circuit_broken_reason": None,
            "total_runs": 0,
            "total_imported": 0,
            "history": [],
        }
        self._manual_trigger_pending = False
        self._manual_trigger_count: int | None = None
        self._last_cfg_signature: tuple[Any, ...] | None = None
        self._state_loaded = False
        self._pending_backfill_history: list[dict[str, Any]] | None = None

    def _ensure_state_loaded(self) -> None:
        if self._state_loaded:
            return
        self._state_loaded = True
        self._load_persisted_state()

    def _load_persisted_state(self) -> None:
        try:
            from grok2api.store import settings_pg
            if settings_pg.enabled():
                saved = settings_pg.get_setting(STATE_SETTING_KEY, None)
                if isinstance(saved, dict):
                    with self._lock:
                        for k in (
                            "next_run_at",
                            "next_interval_sec",
                            "next_watermark_at",
                            "last_run_at",
                            "last_finish_at",
                            "last_trigger_reason",
                            "last_result",
                            "consecutive_failures",
                            "circuit_broken",
                            "circuit_broken_reason",
                            "total_runs",
                            "total_imported",
                            "history",
                        ):
                            if k in saved and saved[k] is not None:
                                self._state[k] = saved[k]
                    # Defer DB write backfill to background tick
                    hist = saved.get("history")
                    if isinstance(hist, list) and hist:
                        self._pending_backfill_history = hist
        except Exception as exc:
            logger.warning("Failed to load persisted scheduler state: %s", exc)

    def _backfill_history_to_task_logs(self, history: list[dict[str, Any]] | None) -> None:
        if not isinstance(history, list) or not history:
            return
        try:
            from grok2api.store.pg import connection, json_dump
            with connection() as conn:
                with conn.cursor() as cur:
                    for item in reversed(history):
                        if not isinstance(item, dict):
                            continue
                        fin_at = item.get("finished_at")
                        if not fin_at:
                            continue
                        tid = item.get("batch_id") or (
                            item.get("session_ids")[0] if isinstance(item.get("session_ids"), list) and item.get("session_ids") else None
                        ) or f"auto_reg_{int(fin_at)}"
                        cur.execute(
                            "SELECT id FROM task_logs WHERE kind = 'auto_register' AND task_id = %s LIMIT 1",
                            (str(tid),),
                        )
                        if cur.fetchone():
                            continue
                        ok = bool(item.get("ok"))
                        st = "done" if ok else ("partial" if int(item.get("imported") or 0) > 0 else "error")
                        reason = item.get("reason") or "定时注册"
                        msg = item.get("message") or ""
                        summary = f"定时注册{'完成' if ok else '失败'}：{reason} · {msg}"
                        cur.execute(
                            """
                            INSERT INTO task_logs (
                              kind, task_id, status, summary, detail, ok,
                              progress_done, progress_total, created_at, updated_at, finished_at
                            ) VALUES (
                              'auto_register', %s, %s, %s, %s::jsonb, %s,
                              %s, %s, to_timestamp(%s), to_timestamp(%s), to_timestamp(%s)
                            )
                            """,
                            (
                                str(tid),
                                st,
                                summary[:500],
                                json_dump(item),
                                ok,
                                int(item.get("imported") or 0),
                                int(item.get("requested") or 0),
                                float(fin_at),
                                float(fin_at),
                                float(fin_at),
                            ),
                        )
                conn.commit()
        except Exception as exc:
            logger.debug("Failed to backfill scheduler history to task_logs: %s", exc)


    def _persist_state(self) -> None:
        try:
            from grok2api.store import settings_pg
            if settings_pg.enabled():
                with self._lock:
                    snapshot = {
                        k: self._state.get(k)
                        for k in (
                            "phase",
                            "next_run_at",
                            "next_interval_sec",
                            "next_watermark_at",
                            "last_run_at",
                            "last_finish_at",
                            "last_trigger_reason",
                            "last_result",
                            "consecutive_failures",
                            "circuit_broken",
                            "circuit_broken_reason",
                            "total_runs",
                            "total_imported",
                            "history",
                        )
                    }
                    snapshot["updated_at"] = time.time()
                settings_pg.set_setting(STATE_SETTING_KEY, snapshot)
        except Exception as exc:
            logger.debug("Failed to persist scheduler state: %s", exc)

    def _read_full_reg_config(self) -> dict[str, Any]:
        try:
            from grok2api.admin.settings_store import get_registration_config
            cfg = get_registration_config()
            if isinstance(cfg, dict):
                return dict(cfg)
        except Exception:
            pass
        try:
            from grok2api.store import settings_pg
            if settings_pg.enabled():
                cfg = settings_pg.get_setting("registration_config", {})
                if isinstance(cfg, dict):
                    return dict(cfg)
        except Exception:
            pass
        return {}

    def _save_reg_config_patch(self, patch: dict[str, Any]) -> dict[str, Any]:
        full = self._read_full_reg_config()
        full.update(patch)
        try:
            from grok2api.admin.settings_store import set_registration_config
            set_registration_config(full)
        except Exception:
            try:
                from grok2api.store import settings_pg
                if settings_pg.enabled():
                    settings_pg.set_setting("registration_config", full)
            except Exception:
                pass
        return extract_auto_register_config(full)

    def _get_pool_counts(self) -> dict[str, int]:
        try:
            from grok2api.store import settings_pg
            if settings_pg.enabled():
                return settings_pg.pool_counts(maintain=False)
        except Exception:
            pass
        return {
            "total": 0,
            "enabled": 0,
            "live": 0,
            "rotatable": 0,
            "in_cooldown": 0,
            "quota_disabled": 0,
            "model_blocked": 0,
            "expired": 0,
            "disabled": 0,
        }

    def start(self) -> None:
        with self._lock:
            if self._thread is not None and self._thread.is_alive():
                return
            self._stop_event.clear()
            self._thread = threading.Thread(
                target=self._run_loop,
                name="auto-register-scheduler",
                daemon=True,
            )
            self._thread.start()
            print("[auto-reg-scheduler] background scheduler started", flush=True)

    def stop(self) -> None:
        self._stop_event.set()
        self._wake_event.set()

    def update_config(self, patch: dict[str, Any]) -> dict[str, Any]:
        """Update auto-register config fields in registration_config and wake scheduler."""
        clean_patch: dict[str, Any] = {}
        for k in DEFAULT_AUTO_REGISTER_CONFIG:
            if k in patch:
                clean_patch[k] = patch[k]
        normalized = self._save_reg_config_patch(clean_patch)
        with self._lock:
            if normalized["auto_register_enabled"]:
                # If turning on or changing interval bounds, reschedule next_run_at
                now = time.time()
                min_s = normalized["auto_register_min_interval_min"] * 60
                max_s = normalized["auto_register_max_interval_min"] * 60
                nxt = self._state.get("next_run_at")
                prev_iv = int(self._state.get("next_interval_sec") or 0)
                is_postponed_short = (
                    self._state.get("phase") == "postponed"
                    or (nxt and (nxt - now) <= 65 and prev_iv > 120)
                )
                if not nxt or nxt <= now or (nxt - now) > max_s or is_postponed_short:
                    chosen = int(random.uniform(min_s, max_s))
                    self._state["next_interval_sec"] = chosen
                    self._state["next_run_at"] = now + chosen
                if self._state.get("phase") in ("disabled", "postponed"):
                    self._state["phase"] = "waiting"
            else:
                self._state["phase"] = "disabled"
                self._state["next_run_at"] = None
                self._state["next_interval_sec"] = None
        self._persist_state()
        self._wake_event.set()
        return self.get_status()

    def trigger_now(self, count: int | None = None) -> dict[str, Any]:
        """Request an immediate registration cycle on the scheduler thread."""
        with self._lock:
            self._circuit_reset_locked()
            self._manual_trigger_pending = True
            self._manual_trigger_count = (
                _as_int(count, 1, 1, 50) if count is not None else None
            )
        self._wake_event.set()
        # Give the background thread a brief moment to pick up the trigger
        time.sleep(0.3)
        return self.get_status()

    def reset_circuit_breaker(self) -> dict[str, Any]:
        with self._lock:
            self._circuit_reset_locked()
            cfg = extract_auto_register_config(self._read_full_reg_config())
            if cfg["auto_register_enabled"]:
                self._schedule_next_interval_locked(cfg)
                self._state["phase"] = "waiting"
            else:
                self._state["phase"] = "disabled"
        self._persist_state()
        self._wake_event.set()
        return self.get_status()

    def _circuit_reset_locked(self) -> None:
        self._state["consecutive_failures"] = 0
        self._state["circuit_broken"] = False
        self._state["circuit_broken_reason"] = None

    def _schedule_next_interval_locked(self, cfg: dict[str, Any]) -> None:
        min_s = max(60, int(cfg["auto_register_min_interval_min"]) * 60)
        max_s = max(min_s, int(cfg["auto_register_max_interval_min"]) * 60)
        chosen = int(random.uniform(min_s, max_s))
        self._state["next_interval_sec"] = chosen
        self._state["next_run_at"] = time.time() + chosen

    def _schedule_next_watermark_locked(self, cfg: dict[str, Any]) -> None:
        cd_min = max(1, int(cfg["auto_register_watermark_cooldown_min"]))
        chosen_cd = int(random.uniform(cd_min * 60, cd_min * 60 * 1.4))
        self._state["next_watermark_at"] = time.time() + chosen_cd

    def _append_history_locked(self, item: dict[str, Any]) -> None:
        hist = list(self._state.get("history") or [])
        hist.insert(0, item)
        self._state["history"] = hist[:MAX_HISTORY_ITEMS]

    def _has_external_active_sessions(self) -> bool:
        """Check if any registration session is currently active in the adapter."""
        try:
            adapter = self._get_adapter_fn()
            if adapter is None:
                return False
            raw = adapter.list_registration_sessions() or []
            sessions = raw.get("sessions", []) if isinstance(raw, dict) else raw
            now = time.time()
            for s in sessions:
                if not isinstance(s, dict):
                    continue
                st = str(s.get("status") or "").strip().lower()
                if st and st not in TERMINAL_SESSION_STATUSES and not s.get("finished"):
                    # Check age: any session older than STALE_SESSION_TIMEOUT_SEC (default 900s) is stale/abandoned
                    stale_sec = STALE_SESSION_TIMEOUT_SEC
                    try:
                        stale_sec = max(
                            180.0,
                            float(os.environ.get("GROK2API_SCHEDULER_STALE_SESSION_SEC", str(STALE_SESSION_TIMEOUT_SEC)) or STALE_SESSION_TIMEOUT_SEC),
                        )
                    except (TypeError, ValueError):
                        stale_sec = STALE_SESSION_TIMEOUT_SEC
                    ua = float(s.get("updated_at") or s.get("created_at") or 0)
                    if ua and (now - ua) > stale_sec:
                        sid = str(s.get("id") or "").strip()
                        if sid:
                            try:
                                adapter.stop_registration_session(sid)
                            except Exception as stop_exc:
                                logger.warning("Failed to stop stale registration session %s: %s", sid, stop_exc)
                        continue
                    return True
        except Exception as exc:
            logger.warning("Failed to check external active sessions: %s", exc)
        return False

    def _check_active_sessions_settled(self) -> tuple[bool, int, int, list[str]]:
        """Return (all_settled, imported_ok_count, failed_count, notes)."""
        with self._lock:
            sids = list(self._state.get("active_session_ids") or [])
            batch_id = self._state.get("active_batch_id")
        if not sids and not batch_id:
            return True, 0, 0, []

        try:
            adapter = self._get_adapter_fn()
            if adapter is None:
                return True, 0, 0, ["adapter unavailable"]

            # If a batch was started, inspect batch master state and refresh session list
            if batch_id:
                b = adapter.get_registration_batch(batch_id)
                if isinstance(b, dict):
                    bst = str(b.get("status") or "").strip().lower()
                    batch_sids = [str(x) for x in (b.get("session_ids") or []) if x]
                    if batch_sids:
                        sids = batch_sids
                        with self._lock:
                            self._state["active_session_ids"] = sids

                    batch_total = int(b.get("count") or b.get("total") or 0)
                    batch_done = int(b.get("finished") or b.get("done") or 0)
                    runner_alive = bool(b.get("runner_alive"))
                    cur_ok = int(b.get("ok_count") or b.get("imported") or 0)
                    cur_fail = int(b.get("fail_count") or b.get("error") or 0)

                    # If the batch runner is still active or not all requested jobs have finished, it is NOT settled
                    if bst not in TERMINAL_SESSION_STATUSES and (
                        runner_alive or (batch_total > 0 and batch_done < batch_total)
                    ):
                        return False, cur_ok, cur_fail, []

            if not sids:
                return False, 0, 0, []

            imported_ok = 0
            failed_n = 0
            notes: list[str] = []
            for sid in sids:
                s = adapter.get_registration_session(sid)
                if not isinstance(s, dict):
                    continue
                st = str(s.get("status") or "").strip().lower()
                if st not in TERMINAL_SESSION_STATUSES and not s.get("finished"):
                    return False, imported_ok, failed_n, notes
                # Session settled; check if it produced healthy imported account(s)
                probe = s.get("probe") if isinstance(s.get("probe"), dict) else {}
                discarded = probe.get("discarded") or []
                imp_ids = s.get("imported_account_ids") or []
                if st in ("imported", "success", "completed", "done") and imp_ids:
                    valid_imp = [aid for aid in imp_ids if aid not in discarded]
                    if valid_imp:
                        imported_ok += len(valid_imp)
                    else:
                        failed_n += 1
                        notes.append(f"{sid}: 测活未通过已丢弃")
                elif st == "imported" and not discarded:
                    imported_ok += 1
                else:
                    # Self-healing: if session failed at importing stage but SSO backup exists, try auto-rescue
                    sso_val = s.get("sso")
                    email_val = s.get("email")
                    rescued = False
                    if sso_val and email_val:
                        try:
                            import scripts.sso_to_auth_json as sso_import
                            import grok2api.pool.accounts as acc_store
                            tok = sso_import.sso_to_token(sso_val, quiet=True)
                            if tok and tok.get("access_token"):
                                _k, ent = sso_import.token_to_auth_entry(tok, email=email_val)
                                imp_res = acc_store.import_auth_payload(
                                    {
                                        "key": ent["key"],
                                        "auth_mode": ent.get("auth_mode", "oidc"),
                                        "email": email_val,
                                        "refresh_token": ent.get("refresh_token", ""),
                                        "expires_at": ent.get("expires_at"),
                                        "oidc_issuer": ent.get("oidc_issuer", "https://auth.x.ai"),
                                        "oidc_client_id": ent.get("oidc_client_id", ""),
                                        "source": "register-email",
                                        "sso": sso_val,
                                        "sso_cookie": sso_val,
                                        "sso_token": sso_val,
                                        "password": s.get("password", ""),
                                    },
                                    merge=True,
                                )
                                if imp_res.get("ok"):
                                    imported_ok += 1
                                    rescued = True
                        except Exception as rescue_exc:
                            logger.warning(
                                "Auto-rescue SSO import failed for session %s (%s): %s",
                                sid,
                                email_val,
                                rescue_exc,
                            )
                            rescued = False

                    if not rescued:
                        failed_n += 1
                        err = str(s.get("error") or s.get("message") or st)[:80]
                        notes.append(f"{sid}: {err}")

            return True, imported_ok, failed_n, notes
        except Exception as exc:
            logger.warning("Error checking active sessions settlement: %s", exc)
            return False, 0, 0, [str(exc)]

    def get_status(self) -> dict[str, Any]:
        self._ensure_state_loaded()
        full_cfg = self._read_full_reg_config()
        cfg = extract_auto_register_config(full_cfg)
        pool = self._get_pool_counts()
        now = time.time()
        with self._lock:
            st = dict(self._state)
            next_run = st.get("next_run_at")
            remaining_sec = (
                max(0, int(next_run - now))
                if isinstance(next_run, (int, float)) and next_run > now
                else 0
            )
            return {
                "ok": True,
                "running": bool(self._thread and self._thread.is_alive()),
                "config": cfg,
                "phase": st.get("phase") or ("waiting" if cfg["auto_register_enabled"] else "disabled"),
                "next_run_at": next_run,
                "next_run_in_sec": remaining_sec if cfg["auto_register_enabled"] else None,
                "next_interval_sec": st.get("next_interval_sec"),
                "next_watermark_at": st.get("next_watermark_at"),
                "last_run_at": st.get("last_run_at"),
                "last_finish_at": st.get("last_finish_at"),
                "last_trigger_reason": st.get("last_trigger_reason"),
                "last_result": st.get("last_result"),
                "active_batch_id": st.get("active_batch_id"),
                "active_session_ids": list(st.get("active_session_ids") or []),
                "consecutive_failures": int(st.get("consecutive_failures") or 0),
                "circuit_broken": bool(st.get("circuit_broken")),
                "circuit_broken_reason": st.get("circuit_broken_reason"),
                "total_runs": int(st.get("total_runs") or 0),
                "total_imported": int(st.get("total_imported") or 0),
                "pool": pool,
                "history": list(st.get("history") or [])[:MAX_HISTORY_ITEMS],
                "server_time": now,
            }

    def _run_loop(self) -> None:
        while not self._stop_event.is_set():
            try:
                self._tick()
            except Exception as exc:
                print(f"[auto-reg-scheduler] tick error: {exc}", flush=True)
            self._wake_event.wait(timeout=5.0)
            self._wake_event.clear()

    def _tick(self) -> None:
        self._ensure_state_loaded()
        if self._pending_backfill_history:
            hist_to_backfill = self._pending_backfill_history
            self._pending_backfill_history = None
            self._backfill_history_to_task_logs(hist_to_backfill)
        full_cfg = self._read_full_reg_config()
        cfg = extract_auto_register_config(full_cfg)
        now = time.time()

        # 1. Check if an active scheduled job is in progress
        with self._lock:
            has_active = bool(
                self._state.get("active_session_ids") or self._state.get("active_batch_id")
            )

        if has_active:
            settled, imported_ok, failed_n, notes = self._check_active_sessions_settled()
            if not settled:
                with self._lock:
                    self._state["phase"] = "running_job"
                    t_id = self._state.get("active_task_id") or self._state.get("active_batch_id")
                    reason_s = self._state.get("last_trigger_reason") or "定时注册"
                    req_cnt = int(self._state.get("active_requested_count") or 1)
                    bid_s = self._state.get("active_batch_id")
                    sids_s = list(self._state.get("active_session_ids") or [])
                    started_at_check = float(self._state.get("last_run_at") or now)

                elapsed_check = now - started_at_check
                max_job_timeout = max(1200, req_cnt * 480)
                if elapsed_check >= max_job_timeout:
                    print(
                        f"[auto-reg-scheduler] job {bid_s or t_id} timed out after {int(elapsed_check)}s "
                        f"(limit={max_job_timeout}s), forcing termination",
                        flush=True,
                    )
                    try:
                        from grok2api.upstream import grok_build_adapter as reg
                        if bid_s:
                            reg.stop_registration_batch(str(bid_s))
                        for sid_item in sids_s:
                            reg.stop_registration_session(str(sid_item))
                    except Exception:
                        pass
                    settled = True
                    failed_n = max(failed_n, max(0, req_cnt - imported_ok))
                    notes.append(f"执行超时({int(elapsed_check)}s > {max_job_timeout}s)，已强制终止")
                else:
                    if t_id:
                        done_now = imported_ok + failed_n
                        _record_auto_reg_task(
                            task_id=str(t_id),
                            status="running",
                            summary=f"定时注册进行中：{reason_s} · 进度 {done_now}/{req_cnt} (成功 {imported_ok} 失败 {failed_n})",
                            progress_done=done_now,
                            progress_total=req_cnt,
                            finished=False,
                            detail={
                                "reason": reason_s,
                                "requested": req_cnt,
                                "imported": imported_ok,
                                "failed": failed_n,
                                "batch_id": bid_s,
                                "session_ids": sids_s,
                                "phase": "running",
                            },
                        )
                    return

            # Job finished! Record outcome
            with self._lock:
                req_n = int(self._state.get("active_requested_count") or 1)
                reason = str(self._state.get("last_trigger_reason") or "scheduled")
                batch_id = self._state.get("active_batch_id")
                sids = list(self._state.get("active_session_ids") or [])
                started_at = float(self._state.get("last_run_at") or now)
                duration_s = max(1, int(now - started_at))
                task_id = self._state.get("active_task_id") or batch_id or f"auto_reg_{int(started_at)}"

                self._state["active_batch_id"] = None
                self._state["active_session_ids"] = []
                self._state["active_requested_count"] = 0
                self._state["active_task_id"] = None
                self._state["last_finish_at"] = now

                if imported_ok > 0:
                    self._state["consecutive_failures"] = 0
                    self._state["total_imported"] = int(self._state.get("total_imported") or 0) + imported_ok
                    msg = f"完成：成功入库 {imported_ok}/{req_n} 个账号（耗时 {duration_s}s）"
                    ok_flag = True
                else:
                    fails = int(self._state.get("consecutive_failures") or 0) + 1
                    self._state["consecutive_failures"] = fails
                    note_str = "; ".join(notes[:2]) if notes else "无可用账号入池"
                    msg = f"失败：入库 0/{req_n} 个（连续失败 {fails} 次 · {note_str}）"
                    ok_flag = False
                    if fails >= int(cfg["auto_register_max_consecutive_failures"]):
                        self._state["circuit_broken"] = True
                        self._state["circuit_broken_reason"] = (
                            f"连续 {fails} 轮定时注册未产出可用账号，已触发安全熔断保护（{note_str}）"
                        )
                        self._state["phase"] = "circuit_broken"
                        print(
                            f"[auto-reg-scheduler] CIRCUIT BROKEN after {fails} consecutive failures",
                            flush=True,
                        )

                result_summary = {
                    "ok": ok_flag,
                    "requested": req_n,
                    "imported": imported_ok,
                    "failed": failed_n,
                    "duration_sec": duration_s,
                    "batch_id": batch_id,
                    "session_ids": sids,
                    "reason": reason,
                    "message": msg,
                    "finished_at": now,
                }
                self._state["last_result"] = result_summary
                self._append_history_locked(result_summary)

                st_code = "done" if ok_flag else ("partial" if imported_ok > 0 else "error")
                _record_auto_reg_task(
                    task_id=str(task_id),
                    status=st_code,
                    summary=f"定时注册{'完成' if ok_flag else '失败'}：{reason} · {msg}",
                    progress_done=imported_ok,
                    progress_total=req_n,
                    finished=True,
                    ok=ok_flag,
                    detail=result_summary,
                )

                if not self._state.get("circuit_broken"):
                    if cfg["auto_register_enabled"]:
                        self._schedule_next_interval_locked(cfg)
                        self._schedule_next_watermark_locked(cfg)
                        self._state["phase"] = "waiting"
                    else:
                        self._state["phase"] = "disabled"
            self._persist_state()
            return

        # 2. Check manual immediate trigger
        manual_req = False
        manual_cnt: int | None = None
        with self._lock:
            if self._manual_trigger_pending:
                manual_req = True
                manual_cnt = self._manual_trigger_count
                self._manual_trigger_pending = False
                self._manual_trigger_count = None

        if manual_req:
            if self._has_external_active_sessions():
                with self._lock:
                    self._append_history_locked({
                        "ok": False,
                        "requested": 0,
                        "imported": 0,
                        "failed": 0,
                        "duration_sec": 0,
                        "reason": "手动立即执行",
                        "message": "跳过：当前已有正在运行的注册任务，请等待其结束后再试",
                        "finished_at": now,
                    })
                self._persist_state()
                return
            count_to_run = manual_cnt or int(cfg["auto_register_batch_size"])
            self._launch_cycle(
                full_cfg=full_cfg,
                cfg=cfg,
                count=count_to_run,
                reason=f"手动立即触发 (×{count_to_run})",
            )
            return

        # 3. Check if enabled & not circuit-broken
        if not cfg["auto_register_enabled"]:
            with self._lock:
                if self._state.get("phase") != "disabled":
                    self._state["phase"] = "disabled"
                    self._state["next_run_at"] = None
                    self._state["next_interval_sec"] = None
                    self._persist_state()
            return

        with self._lock:
            if self._state.get("circuit_broken"):
                last_fin = float(self._state.get("last_finish_at") or self._state.get("last_run_at") or 0.0)
                # 熔断超过 60 分钟自动半开恢复重试，避免永久停滞
                if (now - last_fin) >= 3600.0:
                    print(
                        f"[auto-reg-scheduler] circuit breaker cooled down after {int(now - last_fin)}s, half-open auto recovery triggered",
                        flush=True,
                    )
                    self._state["circuit_broken"] = False
                    self._state["circuit_broken_reason"] = None
                    self._state["consecutive_failures"] = 0
                    self._state["phase"] = "waiting"
                    self._schedule_next_interval_locked(cfg)
                    self._persist_state()
                else:
                    self._state["phase"] = "circuit_broken"
                    return

        # Detect config changes to interval bounds
        sig = (
            cfg["auto_register_enabled"],
            cfg["auto_register_mode"],
            cfg["auto_register_min_interval_min"],
            cfg["auto_register_max_interval_min"],
        )
        with self._lock:
            if self._last_cfg_signature != sig:
                self._last_cfg_signature = sig
                nxt = self._state.get("next_run_at")
                max_s = int(cfg["auto_register_max_interval_min"]) * 60
                if not nxt or nxt <= now or (nxt - now) > max_s:
                    self._schedule_next_interval_locked(cfg)
                self._persist_state()

        pool = self._get_pool_counts()
        live_count = int(pool.get("live") or 0)
        mode = cfg["auto_register_mode"]
        batch_size = int(cfg["auto_register_batch_size"])
        max_pool = int(cfg["auto_register_max_pool_size"])

        # 4. Mode Check A: Low-Watermark Replenish ("watermark" or "both")
        if mode in ("both", "watermark") and int(cfg["auto_register_watermark_min"]) > 0:
            wm_min = int(cfg["auto_register_watermark_min"])
            wm_target = int(cfg["auto_register_watermark_target"])
            if live_count < wm_min:
                with self._lock:
                    next_wm = float(self._state.get("next_watermark_at") or 0.0)
                if now >= next_wm:
                    if self._has_external_active_sessions():
                        return
                    need = max(1, wm_target - live_count)
                    if max_pool > 0:
                        need = min(need, max(0, max_pool - live_count))
                    run_n = min(batch_size, need)
                    if run_n > 0:
                        self._launch_cycle(
                            full_cfg=full_cfg,
                            cfg=cfg,
                            count=run_n,
                            reason=f"低水位自动补齐 (当前可用 {live_count} < 阈值 {wm_min}，目标 {wm_target})",
                        )
                        return

        # 5. Mode Check B: Random Interval ("interval" or "both")
        if mode in ("both", "interval"):
            with self._lock:
                next_run = self._state.get("next_run_at")
                if not next_run:
                    self._schedule_next_interval_locked(cfg)
                    next_run = self._state.get("next_run_at")

            if max_pool > 0 and live_count >= max_pool:
                with self._lock:
                    self._state["phase"] = "max_pool_reached"
                    if next_run and now >= next_run:
                        self._schedule_next_interval_locked(cfg)
                        self._persist_state()
                return

            with self._lock:
                self._state["phase"] = "waiting"

            if next_run and now >= float(next_run):
                if self._has_external_active_sessions():
                    # Postpone by 60 seconds if another manual registration is running
                    with self._lock:
                        self._state["next_run_at"] = now + 60
                        self._state["phase"] = "postponed"
                        self._persist_state()
                    return
                run_n = batch_size
                if max_pool > 0:
                    run_n = min(run_n, max(1, max_pool - live_count))
                iv_min = round(int(self._state.get("next_interval_sec") or 0) / 60.0, 1)
                self._launch_cycle(
                    full_cfg=full_cfg,
                    cfg=cfg,
                    count=run_n,
                    reason=f"随机间隔定时触发 (本轮间隔 {iv_min} 分钟)",
                )
        else:
            # Pure watermark mode and live_count >= wm_min
            with self._lock:
                self._state["phase"] = "waiting"

    def _launch_cycle(
        self,
        *,
        full_cfg: dict[str, Any],
        cfg: dict[str, Any],
        count: int,
        reason: str,
    ) -> None:
        now = time.time()
        job_body = dict(full_cfg)
        job_body["count"] = count
        job_body["concurrency"] = _as_int(full_cfg.get("concurrency"), 1, 1, 16)

        print(
            f"[auto-reg-scheduler] Launching registration cycle: count={count}, reason={reason}",
            flush=True,
        )
        try:
            res = self._start_job_fn(job_body)
            batch_id = res.get("batch_id")
            sids: list[str] = []
            if isinstance(res.get("session_ids"), list):
                sids = [str(x) for x in res["session_ids"] if x]
            elif res.get("id"):
                sids = [str(res["id"])]
            task_id = str(batch_id or (sids[0] if sids else f"auto_reg_{int(now)}"))

            with self._lock:
                self._state["phase"] = "running_job"
                self._state["last_run_at"] = now
                self._state["last_trigger_reason"] = reason
                self._state["active_batch_id"] = batch_id
                self._state["active_session_ids"] = sids
                self._state["active_requested_count"] = count
                self._state["active_task_id"] = task_id
                self._state["total_runs"] = int(self._state.get("total_runs") or 0) + 1
                self._schedule_next_interval_locked(cfg)
                self._schedule_next_watermark_locked(cfg)
            self._persist_state()

            _record_auto_reg_task(
                task_id=task_id,
                status="running",
                summary=f"定时注册启动：{reason} · 计划 {count} 个账号",
                progress_done=0,
                progress_total=count,
                finished=False,
                detail={
                    "reason": reason,
                    "requested": count,
                    "batch_id": batch_id,
                    "session_ids": sids,
                    "started_at": now,
                    "mode": cfg.get("auto_register_mode"),
                    "phase": "started",
                },
            )
        except Exception as exc:
            err_msg = str(getattr(exc, "detail", None) or exc)[:200]
            print(f"[auto-reg-scheduler] Launch failed: {err_msg}", flush=True)
            err_task_id = f"auto_reg_{int(now)}"
            with self._lock:
                fails = int(self._state.get("consecutive_failures") or 0) + 1
                self._state["consecutive_failures"] = fails
                self._state["last_run_at"] = now
                self._state["last_finish_at"] = now
                self._state["last_trigger_reason"] = reason
                fail_item = {
                    "ok": False,
                    "requested": count,
                    "imported": 0,
                    "failed": count,
                    "duration_sec": 0,
                    "reason": reason,
                    "message": f"启动异常：{err_msg}",
                    "finished_at": now,
                }
                self._state["last_result"] = fail_item
                self._append_history_locked(fail_item)
                if fails >= int(cfg["auto_register_max_consecutive_failures"]):
                    self._state["circuit_broken"] = True
                    self._state["circuit_broken_reason"] = f"连续 {fails} 次启动失败：{err_msg}"
                    self._state["phase"] = "circuit_broken"
                else:
                    self._schedule_next_interval_locked(cfg)
                    self._schedule_next_watermark_locked(cfg)
                    self._state["phase"] = "waiting"
            self._persist_state()

            _record_auto_reg_task(
                task_id=err_task_id,
                status="error",
                summary=f"定时注册启动失败：{reason} · 异常: {err_msg}",
                progress_done=0,
                progress_total=count,
                finished=True,
                ok=False,
                detail=fail_item,
            )
