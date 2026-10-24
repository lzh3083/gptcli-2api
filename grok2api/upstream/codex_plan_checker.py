# -*- coding: utf-8 -*-
"""Codex / ChatGPT 账号套餐与优惠资格静默异步检测模块。

支持：
1. 注册完成后静默延时异步检测（方案 A）；
2. Web 后台单账号或批量一键检测与回写 PostgreSQL accounts 表。
"""
from __future__ import annotations

import json
import logging
import os
import sys
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Optional

logger = logging.getLogger("CodexPlanChecker")

# 引入 openai_client 核心模块
_OPENAI_CLIENT_DIR = Path(__file__).resolve().parents[2] / "grok-build-auth" / "openai_client"
if str(_OPENAI_CLIENT_DIR) not in sys.path:
    sys.path.insert(0, str(_OPENAI_CLIENT_DIR))


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def _get_pg_conn():
    import psycopg

    pg_dsn = os.environ.get(
        "DATABASE_URL",
        "postgresql://grok2api:grok2api_pass@postgres:5432/grok2api?sslmode=disable",
    )
    return psycopg.connect(pg_dsn, connect_timeout=10)


def update_account_plan_result(
    account_id: str,
    email: str,
    result: dict[str, Any],
    ok: bool,
    error: str | None = None,
) -> bool:
    """将套餐/优惠检测结果持久化回写到 PostgreSQL accounts 表的 payload 字段。"""
    try:
        now_str = _now_iso()
        with _get_pg_conn() as conn:
            cur = conn.cursor()
            cur.execute(
                "SELECT id, payload FROM accounts WHERE id = %s OR email = %s LIMIT 1",
                (account_id, email),
            )
            row = cur.fetchone()
            if not row:
                logger.warning(
                    "[PlanCheck] 数据库未找到账号: id=%s email=%s", account_id, email
                )
                return False

            aid, payload_raw = row[0], row[1]
            payload = payload_raw if isinstance(payload_raw, dict) else {}
            if isinstance(payload_raw, str):
                try:
                    payload = json.loads(payload_raw)
                except Exception:
                    payload = {}

            if ok:
                plan_type = str(result.get("current_plan_type") or "free").lower()
                plus_eligible = bool(result.get("plus_trial_eligible"))
                discount_pct = result.get("plus_trial_discount_percentage")
                duration_periods = result.get("plus_trial_duration_num_periods")
                duration_period = result.get("plus_trial_duration_period") or "month"
                title = result.get("plus_trial_title") or ""
                promos = result.get("eligible_promo_campaigns") or {}

                payload["plan_check_status"] = "success"
                payload["plan_check_error"] = None
                payload["plan_checked_at"] = now_str
                payload["current_plan_type"] = plan_type
                payload["plus_trial_eligible"] = plus_eligible
                payload["plus_trial_discount_percentage"] = discount_pct
                payload["plus_trial_duration_num_periods"] = duration_periods
                payload["plus_trial_duration_period"] = duration_period
                payload["plus_trial_title"] = title
                payload["eligible_promo_campaigns"] = promos
            else:
                payload["plan_check_status"] = "failed"
                payload["plan_check_error"] = str(error or "检测失败")[:300]
                payload["plan_checked_at"] = now_str

            cur.execute(
                "UPDATE accounts SET payload = %s, updated_at = now() WHERE id = %s",
                (json.dumps(payload, ensure_ascii=False), aid),
            )
            conn.commit()
            return True
    except Exception as exc:
        logger.exception("[PlanCheck] 回写 PostgreSQL 异常: %s", exc)
        return False


def mark_account_plan_status(account_id: str, email: str, status: str) -> None:
    """更新 accounts 状态为 running / queued。"""
    try:
        with _get_pg_conn() as conn:
            cur = conn.cursor()
            cur.execute(
                "SELECT id, payload FROM accounts WHERE id = %s OR email = %s LIMIT 1",
                (account_id, email),
            )
            row = cur.fetchone()
            if not row:
                return
            aid, payload_raw = row[0], row[1]
            payload = payload_raw if isinstance(payload_raw, dict) else {}
            if isinstance(payload_raw, str):
                try:
                    payload = json.loads(payload_raw)
                except Exception:
                    payload = {}
            payload["plan_check_status"] = status
            cur.execute(
                "UPDATE accounts SET payload = %s WHERE id = %s",
                (json.dumps(payload, ensure_ascii=False), aid),
            )
            conn.commit()
    except Exception:
        pass


def check_single_account(
    email: str,
    access_token: str | None = None,
    account_id: str | None = None,
    proxy: str | None = None,
) -> dict[str, Any]:
    """执行单个账号的套餐与资格检测并回写 PG。"""
    from core.chatgpt_plan import check_account_plan

    aid = account_id or ""
    token = (access_token or "").strip()
    em = (email or "").strip()

    if not token or not aid or not em:
        try:
            with _get_pg_conn() as conn:
                cur = conn.cursor()
                if aid:
                    cur.execute(
                        "SELECT id, email, payload FROM accounts WHERE id = %s LIMIT 1",
                        (aid,),
                    )
                else:
                    cur.execute(
                        "SELECT id, email, payload FROM accounts WHERE email = %s LIMIT 1",
                        (em,),
                    )
                row = cur.fetchone()
                if row:
                    aid = row[0]
                    em = row[1] or em
                    payload = row[2] if isinstance(row[2], dict) else {}
                    if isinstance(row[2], str):
                        try:
                            payload = json.loads(row[2])
                        except Exception:
                            payload = {}
                    if not token:
                        token = str(
                            payload.get("sso")
                            or payload.get("sso_cookie")
                            or payload.get("key")
                            or ""
                        ).strip()
        except Exception as exc:
            logger.error("[PlanCheck] 查询账号信息失败: %s", exc)

    if not token:
        err = "缺少有效 access_token / sso token，无法检测"
        if aid or em:
            update_account_plan_result(aid, em, {}, ok=False, error=err)
        return {"ok": False, "error": err}

    mark_account_plan_status(aid, em, "running")

    try:
        res = check_account_plan(
            token,
            proxy=proxy,
            timeout=25.0,
            max_attempts=2,
            retry_delay=2.0,
        )
        ok = bool(res.get("ok"))
        if ok:
            update_account_plan_result(aid, em, res, ok=True)
            logger.info(
                "★ [PlanCheck 成功] %s | plan=%s | plus_trial=%s | discount=%s%%",
                em,
                res.get("current_plan_type"),
                res.get("plus_trial_eligible"),
                res.get("plus_trial_discount_percentage"),
            )
        else:
            err = res.get("error") or "HTTP 请求失败"
            update_account_plan_result(aid, em, res, ok=False, error=err)
            logger.warning("✕ [PlanCheck 失败] %s: %s", em, err)
        return res
    except Exception as exc:
        err = f"{type(exc).__name__}: {str(exc)[:180]}"
        update_account_plan_result(aid, em, {}, ok=False, error=err)
        logger.exception("[PlanCheck 异常] %s: %s", em, exc)
        return {"ok": False, "error": err}


def trigger_async_plan_check(
    email: str,
    access_token: str | None = None,
    account_id: str | None = None,
    proxy: str | None = None,
    delay_seconds: float = 6.0,
) -> None:
    """在后台独立线程延时执行检测（方案 A：注册完成后静默异步查）。"""

    def _worker():
        try:
            if delay_seconds > 0:
                time.sleep(delay_seconds)
            check_single_account(
                email=email,
                access_token=access_token,
                account_id=account_id,
                proxy=proxy,
            )
        except Exception as e:
            logger.warning("[PlanCheck] 异步线程异常: %s, %s", email, e)

    t = threading.Thread(
        target=_worker, name=f"plan-check-{email[:15]}", daemon=True
    )
    t.start()


def batch_plan_check(
    account_ids: list[str] | None = None,
    all_accounts: bool = False,
    proxy: str | None = None,
) -> dict[str, Any]:
    """批量异步启动套餐资格检测。"""
    try:
        targets = []
        with _get_pg_conn() as conn:
            cur = conn.cursor()
            if all_accounts or not account_ids:
                cur.execute(
                    "SELECT id, email, payload FROM accounts ORDER BY updated_at DESC LIMIT 500"
                )
            else:
                cur.execute(
                    "SELECT id, email, payload FROM accounts WHERE id = ANY(%s)",
                    (account_ids,),
                )
            rows = cur.fetchall()
            for r in rows:
                aid, em, raw_payload = r[0], r[1], r[2]
                payload = raw_payload if isinstance(raw_payload, dict) else {}
                if isinstance(raw_payload, str):
                    try:
                        payload = json.loads(raw_payload)
                    except Exception:
                        payload = {}
                tok = str(
                    payload.get("sso")
                    or payload.get("sso_cookie")
                    or payload.get("key")
                    or ""
                ).strip()
                if tok:
                    targets.append({"id": aid, "email": em or "", "token": tok})

        if not targets:
            return {"ok": True, "count": 0, "message": "没有找到可检测的账号"}

        def _batch_worker():
            for idx, item in enumerate(targets):
                try:
                    check_single_account(
                        email=item["email"],
                        access_token=item["token"],
                        account_id=item["id"],
                        proxy=proxy,
                    )
                except Exception as _e:
                    logger.warning(
                        "[BatchPlanCheck] %s 检测异常: %s", item["email"], _e
                    )
                if idx < len(targets) - 1:
                    time.sleep(2.0)  # 错峰慢速，避免并发撞风控

        t = threading.Thread(
            target=_batch_worker, name="batch-plan-check", daemon=True
        )
        t.start()

        return {
            "ok": True,
            "accepted": True,
            "count": len(targets),
            "message": f"已将 {len(targets)} 个账号加入后台静默检测队列",
        }
    except Exception as exc:
        logger.exception("[BatchPlanCheck] 启动异常: %s", exc)
        return {"ok": False, "error": str(exc)}
