"""Shared-store backends for high-concurrency multi-worker deployments.

Production mode requires Redis (hot state) + PostgreSQL (durable accounts/keys).
File JSON is only a migration source via migrate_json_to_pg.py.
"""

from __future__ import annotations

from typing import Any

from grok2api.config import DATABASE_URL, REDIS_URL, STORE_BACKEND, WORKERS


def store_status() -> dict[str, Any]:
    """Lightweight backend status for /health (no network unless clients exist)."""
    redis_info: dict[str, Any] = {"configured": bool(REDIS_URL), "ok": False}
    pg_info: dict[str, Any] = {"configured": bool(DATABASE_URL), "ok": False}
    try:
        from grok2api.store.redis_client import ping as redis_ping
        from grok2api.store.redis_client import redis_enabled

        if redis_enabled():
            redis_info["ok"] = bool(redis_ping())
            redis_info["enabled"] = True
        else:
            redis_info["enabled"] = False
    except Exception as e:  # noqa: BLE001
        redis_info["enabled"] = False
        redis_info["error"] = str(e)

    try:
        from grok2api.store.pg import pg_enabled, ping as pg_ping

        if pg_enabled():
            pg_info["enabled"] = True
            pg_info["ok"] = bool(pg_ping())
        else:
            pg_info["enabled"] = False
            if DATABASE_URL:
                pg_info["note"] = "URL set but driver/connect failed"
    except Exception as e:  # noqa: BLE001
        pg_info["enabled"] = False
        pg_info["error"] = str(e)

    multi_ok = bool(redis_info.get("ok")) and bool(pg_info.get("ok"))

    return {
        "backend": STORE_BACKEND,
        "workers": WORKERS,
        "redis": redis_info,
        "postgres": pg_info,
        "high_concurrency": True,
        "multi_worker_ready": multi_ok,
    }


def require_high_concurrency_stores() -> None:
    """Fail closed unless Redis + PostgreSQL are reachable (production default)."""
    try:
        import grok2api.config as _cfg

        workers = int(getattr(_cfg, "WORKERS", WORKERS) or 1)
        require = bool(getattr(_cfg, "REQUIRE_SHARED_STORES", True))
        backend = str(getattr(_cfg, "STORE_BACKEND", STORE_BACKEND) or "hybrid")
    except Exception:
        workers = WORKERS
        require = True
        backend = STORE_BACKEND

    if not require and backend == "file" and workers <= 1:
        return

    from grok2api.store.redis_client import ping as redis_ping
    from grok2api.store.redis_client import redis_enabled

    if not redis_enabled():
        raise RuntimeError(
            "High-concurrency mode requires REDIS_URL and package `redis`. "
            "Start Redis (docker compose --profile store up -d) and: "
            "pip install -r requirements-store.txt"
        )
    if not redis_ping(force=True):
        raise RuntimeError(
            "Redis unreachable. Check REDIS_URL / GROK2API_REDIS_URL "
            f"(workers={workers})."
        )

    try:
        from grok2api.store.pg import pg_enabled, ping as pg_ping
    except Exception as e:  # noqa: BLE001
        raise RuntimeError(
            "PostgreSQL driver missing. pip install -r requirements-store.txt"
        ) from e

    if not pg_enabled():
        raise RuntimeError(
            "High-concurrency mode requires DATABASE_URL and package `psycopg`. "
            "Start Postgres (docker compose --profile store up -d) and install "
            "requirements-store.txt"
        )
    if not pg_ping(force=True):
        raise RuntimeError(
            "PostgreSQL unreachable. Check DATABASE_URL / GROK2API_DATABASE_URL "
            f"(workers={workers})."
        )


# Backward-compatible alias
def require_multi_worker_stores() -> None:
    require_high_concurrency_stores()
