"""PostgreSQL task log (registration / SSO / probe / renew / …).

Distinct from admin_audit_logs (operator actions). This table records
background or long-running jobs and their outcomes for the admin「任务日志」page.
"""

from __future__ import annotations

import json
import time
from typing import Any

from grok2api.store.pg import connection, json_dump, pg_enabled


def enabled() -> bool:
    return pg_enabled()


def write_task(
    *,
    kind: str,
    summary: str = "",
    status: str = "done",
    task_id: str | None = None,
    detail: dict[str, Any] | None = None,
    ok: bool | None = None,
    progress_done: int = 0,
    progress_total: int = 0,
    finished: bool = True,
) -> int | None:
    """Write one task log row.

    When ``task_id`` is set, **update** the latest existing row for the same
    ``(kind, task_id)`` so start/progress/finish of one job stays a single row.
    A brand-new task_id always inserts a fresh row — previous jobs never merge.
    Without task_id, always inserts (one-shot events).
    """
    if not enabled() or not kind:
        return None
    payload = detail if isinstance(detail, dict) else {}
    st = (status or "done").strip().lower() or "done"
    tid = (str(task_id).strip()[:128] if task_id else None) or None
    try:
        done_i = max(0, int(progress_done or 0))
    except Exception:
        done_i = 0
    try:
        total_i = max(0, int(progress_total or 0))
    except Exception:
        total_i = 0
    if ok is None:
        if st in {"done", "success", "completed", "ok", "partial"}:
            ok = True
        elif st in {"error", "failed", "cancelled", "stopped"}:
            ok = False
    try:
        with connection() as conn:
            with conn.cursor() as cur:
                # Upsert-by-task_id: keep one row per running job lifecycle.
                if tid:
                    cur.execute(
                        """
                        UPDATE task_logs SET
                          status = %s,
                          summary = %s,
                          detail = %s::jsonb,
                          ok = %s,
                          progress_done = %s,
                          progress_total = %s,
                          updated_at = now(),
                          finished_at = CASE
                            WHEN %s THEN COALESCE(finished_at, now())
                            ELSE finished_at
                          END
                        WHERE id = (
                          SELECT id FROM task_logs
                          WHERE kind = %s AND task_id = %s
                          ORDER BY id DESC
                          LIMIT 1
                        )
                        RETURNING id
                        """,
                        (
                            st[:64],
                            (summary or "")[:500],
                            json_dump(payload),
                            ok,
                            done_i,
                            total_i,
                            bool(finished),
                            str(kind)[:64],
                            tid,
                        ),
                    )
                    row = cur.fetchone()
                    if row:
                        conn.commit()
                        return int(row[0])

                cur.execute(
                    """
                    INSERT INTO task_logs (
                      kind, task_id, status, summary, detail, ok,
                      progress_done, progress_total, created_at, updated_at, finished_at
                    ) VALUES (
                      %s, %s, %s, %s, %s::jsonb, %s,
                      %s, %s, now(), now(),
                      CASE WHEN %s THEN now() ELSE NULL END
                    )
                    RETURNING id
                    """,
                    (
                        str(kind)[:64],
                        tid,
                        st[:64],
                        (summary or "")[:500],
                        json_dump(payload),
                        ok,
                        done_i,
                        total_i,
                        bool(finished),
                    ),
                )
                row = cur.fetchone()
            conn.commit()
        return int(row[0]) if row else None
    except Exception:
        return None


def list_tasks(
    *,
    q: str = "",
    kind: str = "",
    status: str = "",
    page: int = 1,
    page_size: int = 50,
) -> dict[str, Any]:
    if not enabled():
        return {
            "ok": True,
            "items": [],
            "total": 0,
            "page": 1,
            "page_size": page_size,
            "total_pages": 1,
            "store_source": "none",
            "log_type": "task",
        }

    try:
        page_i = max(1, int(page))
    except Exception:
        page_i = 1
    try:
        size_i = int(page_size)
    except Exception:
        size_i = 50
    size_i = max(1, min(200, size_i if size_i > 0 else 50))

    where: list[str] = []
    params: list[Any] = []
    qq = (q or "").strip()
    if qq:
        where.append(
            "("
            "kind ILIKE %s OR status ILIKE %s OR summary ILIKE %s "
            "OR COALESCE(task_id,'') ILIKE %s"
            ")"
        )
        like = f"%{qq}%"
        params.extend([like, like, like, like])
    kk = (kind or "").strip()
    if kk and kk != "all":
        where.append("kind = %s")
        params.append(kk)
    ss = (status or "").strip()
    if ss and ss != "all":
        where.append("status = %s")
        params.append(ss)
    wh = (" WHERE " + " AND ".join(where)) if where else ""

    with connection() as conn:
        with conn.cursor() as cur:
            cur.execute(f"SELECT COUNT(*) FROM task_logs{wh}", params)
            total = int((cur.fetchone() or [0])[0] or 0)
            total_pages = max(1, (total + size_i - 1) // size_i) if total else 1
            page_i = min(page_i, total_pages)
            offset = (page_i - 1) * size_i
            cur.execute(
                f"""
                SELECT id, created_at, updated_at, finished_at, kind, task_id,
                       status, summary, detail, ok, progress_done, progress_total
                FROM task_logs
                {wh}
                ORDER BY created_at DESC, id DESC
                LIMIT %s OFFSET %s
                """,
                [*params, size_i, offset],
            )
            rows = cur.fetchall()

    items: list[dict[str, Any]] = []
    for r in rows:
        detail = r[8]
        if isinstance(detail, str):
            try:
                detail = json.loads(detail)
            except json.JSONDecodeError:
                detail = {"raw": detail}

        def _ts(v: Any) -> float | None:
            if v is None:
                return None
            try:
                return v.timestamp() if hasattr(v, "timestamp") else float(v)
            except Exception:
                return None

        created_ts = _ts(r[1]) or time.time()
        items.append(
            {
                "id": int(r[0]),
                "created_at": created_ts,
                "updated_at": _ts(r[2]),
                "finished_at": _ts(r[3]),
                "kind": r[4],
                "task_id": r[5],
                "status": r[6],
                "summary": r[7],
                "detail": detail if isinstance(detail, dict) else {},
                "ok": None if r[9] is None else bool(r[9]),
                "progress_done": int(r[10] or 0),
                "progress_total": int(r[11] or 0),
                # Compatibility aliases for older logs UI columns.
                "action": r[4],
                "target_type": "task",
                "target_id": r[5],
                "actor": "system",
                "ip": None,
            }
        )
    return {
        "ok": True,
        "items": items,
        "total": total,
        "page": page_i,
        "page_size": size_i,
        "total_pages": total_pages,
        "q": qq,
        "kind": kk or "all",
        "status": ss or "all",
        "action": kk or "all",
        "store_source": "postgres",
        "log_type": "task",
    }


# Always surface these in the admin filter even before any rows exist.
_KNOWN_KINDS = (
    "register",
    "sso_import",
    "json_import",
    "json_export",
    "probe",
    "renew",
)


def list_kinds(limit: int = 50) -> list[str]:
    known = [k for k in _KNOWN_KINDS]
    if not enabled():
        return known[: max(1, min(200, int(limit or 50)))]
    try:
        with connection() as conn:
            with conn.cursor() as cur:
                cur.execute(
                    """
                    SELECT kind, COUNT(*) AS c
                    FROM task_logs
                    GROUP BY kind
                    ORDER BY c DESC, kind ASC
                    LIMIT %s
                    """,
                    (max(1, min(200, int(limit))),),
                )
                found = [str(r[0]) for r in cur.fetchall() if r and r[0]]
        # Prefer DB-ordered kinds, then fill missing known kinds so filters stay useful.
        out: list[str] = []
        for k in found + known:
            if k and k not in out:
                out.append(k)
        return out[: max(1, min(200, int(limit or 50)))]
    except Exception:
        return known
