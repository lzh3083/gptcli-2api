"""SSO import job helpers for the Python registration/SSO sidecar.

Extracted from the legacy Python admin_routes so the public API / admin
control plane can live in Go. The registration sidecar imports this module
for /internal/sso/v1/* only.

Go must not reimplement device-flow SSO conversion; that stays in
scripts/sso_to_auth_json.py.
"""
from __future__ import annotations

import json
import threading
import time
import uuid
from pathlib import Path
from typing import Any

import grok2api.config as _config
from grok2api.pool import accounts


def _sso_script():
    import scripts.sso_to_auth_json as sso_import

    return sso_import

try:
    from grok2api.upstream import grok_build_adapter as reg_adapter
except Exception:  # noqa: BLE001
    reg_adapter = None  # type: ignore[assignment]


def _account_sso_value(entry: dict) -> str:
    try:
        from grok2api.pool.accounts import get_sso_value

        return get_sso_value(entry)
    except Exception:
        if not isinstance(entry, dict):
            return ""
        return str(entry.get("sso") or entry.get("sso_cookie") or entry.get("sso_token") or "").strip()



# ── SSO import jobs (progress polling across multi-worker via Redis) ───────

_SSO_JOB_TTL_SEC = 3600
_sso_jobs_lock = threading.Lock()
_sso_jobs_local: dict[str, dict[str, Any]] = {}


def _sso_job_key(job_id: str) -> str:
    try:
        from grok2api.store.redis_client import key as rk

        return rk("sso_import", "job", job_id)
    except Exception:
        return f"g2a:sso_import:job:{job_id}"


def _sso_job_put(job_id: str, job: dict[str, Any]) -> None:
    payload = dict(job)
    with _sso_jobs_lock:
        _sso_jobs_local[job_id] = payload
    try:
        from grok2api.store.redis_client import set_json

        set_json(_sso_job_key(job_id), payload, _SSO_JOB_TTL_SEC)
    except Exception:
        pass


def _sso_job_get(job_id: str) -> dict[str, Any] | None:
    try:
        from grok2api.store.redis_client import get_json

        data = get_json(_sso_job_key(job_id))
        if isinstance(data, dict):
            return data
    except Exception:
        pass
    with _sso_jobs_lock:
        job = _sso_jobs_local.get(job_id)
        return dict(job) if isinstance(job, dict) else None


def _sso_job_patch(job_id: str, **fields: Any) -> dict[str, Any] | None:
    job = _sso_job_get(job_id)
    if not isinstance(job, dict):
        return None
    job.update(fields)
    job["updated_at"] = time.time()
    total = max(1, int(job.get("total") or 1))
    done = int(job.get("done") or 0)
    job["percent"] = min(100, int(round(100.0 * done / total)))
    _sso_job_put(job_id, job)
    return job


def _sso_public_job(job: dict[str, Any] | None) -> dict[str, Any]:
    if not isinstance(job, dict):
        return {"ok": False, "error": "job not found"}
    # Never leak full SSO cookies / tokens in progress responses.
    out = {
        "ok": True,
        "job_id": job.get("id"),
        "status": job.get("status") or "unknown",
        "phase": job.get("phase") or "",
        "message": job.get("message") or "",
        "total": int(job.get("total") or 0),
        "done": int(job.get("done") or 0),
        "success": int(job.get("success") or 0),
        "fail": int(job.get("fail") or 0),
        "converted": int(job.get("converted") or 0),
        "percent": int(job.get("percent") or 0),
        "workers": int(job.get("workers") or 0),
        "delay": int(job.get("delay") or 0),
        "created_at": job.get("created_at"),
        "updated_at": job.get("updated_at"),
        "finished_at": job.get("finished_at"),
        "results": job.get("results") or [],
        "imported": job.get("imported") or [],
        "error": job.get("error"),
    }
    return out


def _parse_sso_lines(lines: list[str]) -> list[tuple[str, str]]:
    out: list[tuple[str, str]] = []
    for raw in lines:
        for line in raw.splitlines():
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            email = ""
            if "----" in line:
                parts = line.split("----")
                email = parts[0].strip()
                line = parts[-1].strip()
            elif ":" in line and not line.startswith("eyJ"):
                parts = line.rsplit(":", 1)
                email = parts[0].strip()
                line = parts[-1].strip()
            out.append((email, line))
    return out


_sso_backup_lock = threading.Lock()
_sso_backup_reconcile_at = 0.0


def _sso_backup_dirs() -> list[Path]:
    dirs: list[Path] = []
    for base in (
        getattr(_config, "DATA_DIR", None),
        Path(__file__).resolve().parents[2] / "data",
    ):
        if not base:
            continue
        root = Path(base)
        for name in ("import_sso", "register_sso"):
            p = root / name
            if p not in dirs:
                dirs.append(p)
    return dirs


def _persist_import_sso_backup(*, email: str = "", sso: str = "", source: str = "sso-import") -> str:
    cookie = str(sso or "").strip()
    if not cookie:
        return ""
    try:
        root = Path(getattr(_config, "DATA_DIR", Path(__file__).resolve().parents[2] / "data")) / "import_sso"
        root.mkdir(parents=True, exist_ok=True)
        ts = time.strftime("%Y%m%d-%H%M%S", time.localtime())
        safe_email = "".join(
            ch if ch.isalnum() or ch in "._@+-" else "_"
            for ch in str(email or "unknown")
        )[:80]
        path = root / f"{ts}_{safe_email}_{uuid.uuid4().hex[:8]}.json"
        payload = {
            "email": email,
            "sso": cookie,
            "sso_cookie": cookie,
            "source": source,
            "created_at": ts,
        }
        path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
        return str(path)
    except Exception as e:  # noqa: BLE001
        print(f"[sso-import] WARN: save SSO backup failed: {e}")
        return ""


def _load_saved_sso_records() -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    for root in _sso_backup_dirs():
        try:
            files = sorted(root.glob("*.json"), key=lambda p: p.stat().st_mtime, reverse=True)
        except Exception:
            continue
        for path in files[:5000]:
            try:
                obj = json.loads(path.read_text(encoding="utf-8"))
            except Exception:
                continue
            if not isinstance(obj, dict):
                continue
            sso = _account_sso_value(obj)
            if not sso:
                continue
            rec = dict(obj)
            rec["sso"] = sso
            rec.setdefault("sso_cookie", sso)
            rec["sso_backup_path"] = str(path)
            records.append(rec)

    # Registration-session export can see SSO cookies that predate account-payload
    # persistence. Pull those live/Redis sessions too, then reconcile them back to
    # the account table by imported account id, registration_session_id, or email.
    try:
        adapter = reg_adapter
        if adapter is not None:
            listed = adapter.list_registration_sessions() or {}
            sessions = listed.get("sessions") if isinstance(listed, dict) else listed
            if isinstance(sessions, list):
                for sess in sessions:
                    if not isinstance(sess, dict):
                        continue
                    sso = str(sess.get("sso") or "").strip()
                    if not sso:
                        cookies = sess.get("session_cookies")
                        if isinstance(cookies, dict):
                            sso = str(cookies.get("sso") or cookies.get("sso-rw") or "").strip()
                    if not sso:
                        continue
                    account_ids: list[str] = []
                    for aid in sess.get("imported_account_ids") or []:
                        if aid:
                            account_ids.append(str(aid))
                    imported_accounts = sess.get("imported_accounts")
                    if isinstance(imported_accounts, list):
                        for acc in imported_accounts:
                            if not isinstance(acc, dict):
                                continue
                            aid = acc.get("id")
                            if aid:
                                account_ids.append(str(aid))
                    rec = {
                        "source": "registration-session",
                        "session_id": str(sess.get("id") or ""),
                        "registration_session_id": str(sess.get("id") or ""),
                        "batch_id": sess.get("batch_id"),
                        "registration_batch_id": sess.get("batch_id"),
                        "email": str(sess.get("email") or "").strip(),
                        "password": str(sess.get("password") or "").strip(),
                        "register_password": str(sess.get("password") or "").strip(),
                        "sso": sso,
                        "sso_cookie": sso,
                        "account_ids": sorted(set(account_ids)),
                    }
                    records.append(rec)
    except Exception as e:  # noqa: BLE001
        print(f"[sso-reconcile] registration sessions skipped: {e}")
    return records


def _reconcile_saved_sso_to_accounts(*, force: bool = False) -> int:
    """Attach saved import/register SSO backup files to matching account rows."""
    global _sso_backup_reconcile_at
    now = time.time()
    if not force and now - _sso_backup_reconcile_at < 30.0:
        return 0
    with _sso_backup_lock:
        now = time.time()
        if not force and now - _sso_backup_reconcile_at < 30.0:
            return 0
        _sso_backup_reconcile_at = now
        records = _load_saved_sso_records()
    if not records:
        return 0

    by_id: dict[str, dict[str, Any]] = {}
    by_email: dict[str, dict[str, Any]] = {}
    by_session: dict[str, dict[str, Any]] = {}
    for rec in records:
        for aid in rec.get("account_ids") or []:
            if aid and str(aid) not in by_id:
                by_id[str(aid)] = rec
        for id_key in ("account_id", "id", "auth_key"):
            aid = str(rec.get(id_key) or "").strip()
            if aid and aid not in by_id:
                by_id[aid] = rec
        email = str(rec.get("email") or "").strip().lower()
        if email and email not in by_email:
            by_email[email] = rec
        sid = str(rec.get("session_id") or rec.get("registration_session_id") or "").strip()
        if sid and sid not in by_session:
            by_session[sid] = rec

    data = accounts.read_auth_map()
    if not isinstance(data, dict) or not data:
        return 0
    changed = 0
    try:
        from grok2api.pool.accounts import get_sso_value, merge_durable_account_fields
    except Exception:
        get_sso_value = _account_sso_value  # type: ignore[assignment]
        merge_durable_account_fields = None  # type: ignore[assignment]

    for _aid, entry in data.items():
        if not isinstance(entry, dict):
            continue
        aid = str(_aid)
        sid = str(entry.get("registration_session_id") or "").strip()
        email = str(entry.get("email") or "").strip().lower()
        rec = by_id.get(aid) or (by_session.get(sid) if sid else None) or (by_email.get(email) if email else None)
        if not rec:
            continue
        before = json.dumps(entry, sort_keys=True, ensure_ascii=False, default=str)
        if merge_durable_account_fields is not None:
            merge_durable_account_fields(entry, rec)
        else:
            sso = _account_sso_value(rec)
            if sso and not get_sso_value(entry):
                entry["sso"] = sso
                entry.setdefault("sso_cookie", sso)
        after = json.dumps(entry, sort_keys=True, ensure_ascii=False, default=str)
        if after != before:
            changed += 1

    if changed:
        from grok2api.pool.auth_store import write_auth_map

        write_auth_map(data)
    return changed


def _run_sso_import_job(
    job_id: str,
    *,
    sso_items: list[tuple[str, str]],
    merge: bool,
    delay: int,
    max_workers: int,
) -> None:
    """Background worker: convert SSO cookies then bulk-import with progress updates."""
    from concurrent.futures import ThreadPoolExecutor, as_completed

    total = len(sso_items)
    try:
        from grok2api.config import SSO_IMPORT_WORKERS
    except Exception:
        SSO_IMPORT_WORKERS = 8
    # Device-code conversion is rate-limited by xAI; default hard-cap was 6 which
    # made bulk SSO (hundreds) crawl. Cap at 12; still throttle when delay>=2.
    workers = min(int(max_workers), int(SSO_IMPORT_WORKERS), max(1, total), 12)
    if delay and delay >= 2:
        workers = min(workers, 3)
    elif delay and delay >= 1:
        workers = min(workers, 6)

    _sso_job_patch(
        job_id,
        status="running",
        phase="converting",
        message=f"正在转换 SSO → token（{workers} 线程）…",
        workers=workers,
        done=0,
        success=0,
        fail=0,
        converted=0,
        results=[],
        imported=[],
    )

    pending_entries: list[dict[str, Any]] = []
    results: list[dict[str, Any]] = []
    converted_count = 0
    fail = 0
    progress_lock = threading.Lock()
    last_progress_at = 0.0
    # Throttle Redis/progress writes under high concurrency.
    progress_every = 1 if total <= 20 else max(2, total // 25)

    def _convert_one(args: tuple[int, str, str]) -> dict[str, Any]:
        i, email_hint, sso = args
        if delay > 0 and i > 1:
            # tiny staggered start only (seconds), not cumulative per index
            time.sleep(
                min(float(delay), 2.0)
                * (((i - 1) % max(1, workers)) / max(1.0, float(workers)))
            )
        item: dict[str, Any] = {
            "index": i,
            "sso_hint": (sso[:12] + "...") if len(sso) > 12 else sso,
        }
        try:
            # quiet=True: less stdout lock contention under multi-thread import
            token = _sso_script().sso_to_token(sso, quiet=True)
            if not token:
                item["status"] = "failed"
                item["error"] = "device flow failed or invalid sso (often xAI rate limit; retry with lower concurrency)"
                return item
            _key, entry = _sso_script().token_to_auth_entry(token, email=email_hint)
            saved_sso_path = _persist_import_sso_backup(
                email=str(entry.get("email") or email_hint or ""),
                sso=sso,
            )
            item["status"] = "converted"
            item["email"] = entry.get("email", email_hint)
            item["entry"] = {
                "key": entry["key"],
                "auth_mode": entry.get("auth_mode", "oidc"),
                "email": entry.get("email", email_hint),
                "refresh_token": entry.get("refresh_token", ""),
                "expires_at": entry.get("expires_at"),
                "oidc_issuer": entry.get("oidc_issuer", _sso_script().OIDC_ISSUER),
                "oidc_client_id": entry.get(
                    "oidc_client_id", _sso_script().GROK_CLI_CLIENT_ID
                ),
                "user_id": entry.get("user_id") or entry.get("principal_id"),
                # Keep original SSO cookie on the account so admin UI can show/export it.
                "sso": sso,
                "sso_cookie": sso,
                "source": "sso-import",
                "sso_backup_path": saved_sso_path,
            }
            return item
        except TypeError:
            # Older sso_to_token without quiet=
            try:
                token = _sso_script().sso_to_token(sso)
                if not token:
                    item["status"] = "failed"
                    item["error"] = "device flow failed or invalid sso (often xAI rate limit; retry with lower concurrency)"
                    return item
                _key, entry = _sso_script().token_to_auth_entry(token, email=email_hint)
                saved_sso_path = _persist_import_sso_backup(
                    email=str(entry.get("email") or email_hint or ""),
                    sso=sso,
                )
                item["status"] = "converted"
                item["email"] = entry.get("email", email_hint)
                item["entry"] = {
                    "key": entry["key"],
                    "auth_mode": entry.get("auth_mode", "oidc"),
                    "email": entry.get("email", email_hint),
                    "refresh_token": entry.get("refresh_token", ""),
                    "expires_at": entry.get("expires_at"),
                    "oidc_issuer": entry.get("oidc_issuer", _sso_script().OIDC_ISSUER),
                    "oidc_client_id": entry.get(
                        "oidc_client_id", _sso_script().GROK_CLI_CLIENT_ID
                    ),
                    "user_id": entry.get("user_id") or entry.get("principal_id"),
                    "sso": sso,
                    "sso_cookie": sso,
                    "source": "sso-import",
                    "sso_backup_path": saved_sso_path,
                }
                return item
            except Exception as e:  # noqa: BLE001
                item["status"] = "failed"
                item["error"] = str(e)
                return item
        except Exception as e:  # noqa: BLE001
            item["status"] = "failed"
            item["error"] = str(e)
            return item

    try:
        with ThreadPoolExecutor(
            max_workers=workers, thread_name_prefix="sso-import-"
        ) as ex:
            futs = [
                ex.submit(_convert_one, (i, e, s))
                for i, (e, s) in enumerate(sso_items, 1)
            ]
            for fut in as_completed(futs):
                item = fut.result()
                with progress_lock:
                    if item.get("status") == "converted":
                        converted_count += 1
                        entry = item.get("entry") or {}
                        # Guarantee SSO lands in durable payload (PG accounts.payload).
                        if not entry.get("sso") and not entry.get("sso_cookie"):
                            sso_raw = ""
                            # recover from results item index mapping if needed
                            sso_raw = str((sso_items[int(item.get("index") or 1) - 1][1] if sso_items else "") or "")
                            if sso_raw:
                                entry["sso"] = sso_raw
                                entry["sso_cookie"] = sso_raw
                        if not entry.get("source"):
                            entry["source"] = "sso-import"
                        pending_entries.append(entry)
                        pub = {k: v for k, v in item.items() if k != "entry"}
                        # Keep status as converting until bulk import finishes.
                        pub["status"] = "converted"
                        results.append(pub)
                    else:
                        fail += 1
                        results.append(
                            {k: v for k, v in item.items() if k != "entry"}
                        )
                    done = converted_count + fail
                    # Sort for stable UI order.
                    results.sort(key=lambda x: int(x.get("index") or 0))
                    now = time.time()
                    should_publish = (
                        done >= total
                        or done % progress_every == 0
                        or (now - last_progress_at) >= 0.8
                    )
                    if should_publish:
                        last_progress_at = now
                        _sso_job_patch(
                            job_id,
                            status="running",
                            phase="converting",
                            message=(
                                f"转换中 {done}/{total}"
                                f"（成功 {converted_count} · 失败 {fail}）"
                            ),
                            done=done,
                            converted=converted_count,
                            success=0,
                            fail=fail,
                            results=list(results),
                        )

        # Stage 2: one storage write for all converted accounts.
        imported: list[dict[str, Any]] = []
        ok = 0
        if pending_entries:
            _sso_job_patch(
                job_id,
                status="running",
                phase="importing",
                message=f"正在写入账号池（{len(pending_entries)} 个）…",
                done=total,  # convert phase finished
                converted=converted_count,
                fail=fail,
                results=list(results),
            )
            bulk = accounts.import_auth_payloads_bulk(pending_entries, merge=merge)
            storage = bulk.get("storage") or ""
            if storage and storage != "postgres":
                print(f"[sso-import] WARN: import storage={storage} (expected postgres)")
            if not bulk.get("ok"):
                err = bulk.get("error") or "bulk import failed"
                for item in results:
                    if item.get("status") == "converted":
                        item["status"] = "failed"
                        item["error"] = err
                        fail += 1
                converted_count = 0
            else:
                imp = bulk.get("imported") or []
                imp_iter = iter(imp)
                for item in results:
                    if item.get("status") != "converted":
                        continue
                    info = next(imp_iter, None)
                    if not info:
                        item["status"] = "failed"
                        item["error"] = "account omitted by storage deduplication or partial insert error"
                        fail += 1
                        continue
                    item["status"] = "ok"
                    item["account_id"] = info.get("id")
                    item["email"] = info.get("email") or item.get("email")
                    item["user_id"] = info.get("user_id")
                    item["expires_at"] = info.get("expires_at")
                    item["has_refresh_token"] = info.get("has_refresh_token")
                    ok += 1
                    imported.append(
                        {
                            "id": info.get("id"),
                            "email": info.get("email"),
                            "user_id": info.get("user_id"),
                            "expires_at": info.get("expires_at"),
                            "has_refresh_token": info.get("has_refresh_token"),
                        }
                    )

        fail = sum(1 for x in results if x.get("status") != "ok")
        ok = sum(1 for x in results if x.get("status") == "ok")
        msg = f"SSO 导入完成：{ok} 成功, {fail} 失败（workers={workers}）"
        _sso_job_patch(
            job_id,
            status="done",
            phase="done",
            message=msg,
            done=total,
            success=ok,
            fail=fail,
            converted=converted_count,
            imported=imported,
            results=results,
            finished_at=time.time(),
            percent=100,
            ok=fail == 0,
            storage=storage if "storage" in locals() else None,
        )
        try:
            import grok2api.admin.task_log as task_log

            task_log.record(
                "sso_import",
                task_id=job_id,
                summary=msg,
                status="done" if fail == 0 else ("partial" if ok else "error"),
                ok=fail == 0,
                progress_done=total,
                progress_total=total,
                detail={
                    "success": ok,
                    "fail": fail,
                    "converted": converted_count,
                    "workers": workers,
                    "imported_count": len(imported),
                },
            )
        except Exception:
            pass
    except Exception as e:  # noqa: BLE001
        msg = f"SSO 导入失败：{e}"
        _sso_job_patch(
            job_id,
            status="error",
            phase="error",
            message=msg,
            error=str(e),
            finished_at=time.time(),
            ok=False,
        )
        try:
            import grok2api.admin.task_log as task_log

            task_log.record(
                "sso_import",
                task_id=job_id,
                summary=msg,
                status="error",
                ok=False,
                progress_done=0,
                progress_total=total,
                detail={"error": str(e)[:400]},
            )
        except Exception:
            pass

