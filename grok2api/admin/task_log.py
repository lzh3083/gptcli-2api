"""Thin helper to record background/task outcomes into task_logs."""

from __future__ import annotations

from typing import Any


def record(
    kind: str,
    *,
    summary: str = "",
    status: str = "done",
    task_id: str | None = None,
    detail: dict[str, Any] | None = None,
    ok: bool | None = None,
    progress_done: int = 0,
    progress_total: int = 0,
    finished: bool = True,
) -> int | None:
    try:
        from grok2api.store.task_logs_pg import write_task

        return write_task(
            kind=kind,
            summary=summary,
            status=status,
            task_id=task_id,
            detail=detail,
            ok=ok,
            progress_done=progress_done,
            progress_total=progress_total,
            finished=finished,
        )
    except Exception:
        return None
