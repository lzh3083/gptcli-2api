# -*- coding: utf-8 -*-
"""Codex 微软邮箱池与收信接码管理模块。

提供微软邮箱素材的批量导入、状态追踪（available/used/already_registered/failed）、
实时存活与采购前历史注册检测，以及手动抓取最新 OTP 与收件箱邮件列表。
"""
from __future__ import annotations

import concurrent.futures
import json
import logging
import os
import re
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

# 确保路径解析正常
REPO_ROOT = Path(__file__).resolve().parents[2]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))
GBA_ROOT = REPO_ROOT / "grok-build-auth"
if str(GBA_ROOT) not in sys.path:
    sys.path.insert(0, str(GBA_ROOT))
OPENAI_CLIENT_DIR = GBA_ROOT / "openai_client"
if str(OPENAI_CLIENT_DIR) not in sys.path:
    sys.path.insert(0, str(OPENAI_CLIENT_DIR))

try:
    from core import db
    from core import outlook_client
except ImportError:
    from openai_client.core import db
    from openai_client.core import outlook_client

logger = logging.getLogger(__name__)


def _now_iso() -> str:
    return datetime.now().isoformat(timespec="seconds")


def get_email_pool_summary() -> dict[str, Any]:
    """汇总微软邮箱池中各种状态的账号数量。"""
    summary = db.outlook_pool_summary()
    return {
        "ok": True,
        "available": summary.get("available", 0),
        "used": summary.get("used", 0),
        "already_registered": summary.get("already_registered", 0),
        "failed": summary.get("failed", 0),
        "cooling": summary.get("cooling", 0),
        "disabled": summary.get("disabled", 0),
        "total": summary.get("total", 0),
    }


def list_email_pool(
    page: int = 1,
    limit: int = 50,
    status: str | None = None,
    search: str | None = None,
) -> dict[str, Any]:
    """分页查询微软邮箱池。"""
    page = max(1, int(page or 1))
    limit = max(1, min(200, int(limit or 50)))
    offset = (page - 1) * limit

    status_filter = str(status or "").strip().lower()
    if status_filter in ("all", "*", ""):
        status_filter = None

    q = str(search or "").strip() if search else None

    page_data = db.list_email_pool_page(
        source="outlook",
        status=status_filter,
        q=q,
        limit=limit,
        offset=offset,
    )

    items = []
    for raw in page_data.get("items") or []:
        email = str(raw.get("email") or "").strip()
        client_id = str(raw.get("client_id") or raw.get("clientId") or "").strip()
        masked_client_id = f"{client_id[:6]}...{client_id[-4:]}" if len(client_id) > 10 else client_id
        
        has_rt = bool(str(raw.get("refresh_token") or raw.get("refreshToken") or "").strip())
        reg_id = raw.get("registered_account_id")
        
        items.append({
            "id": raw.get("id"),
            "email": email,
            "status": raw.get("status") or "available",
            "source": raw.get("source") or "outlook",
            "note": raw.get("note") or "",
            "client_id": masked_client_id,
            "has_refresh_token": has_rt,
            "has_recovery": bool(raw.get("recovery_email") or raw.get("recovery_code")),
            "imported_at": raw.get("imported_at") or raw.get("created_at") or "",
            "used_at": raw.get("used_at"),
            "registered_account_id": reg_id,
            "is_registered": bool(reg_id),
            "codex_status": raw.get("codex_status"),
        })

    total = int(page_data.get("total") or 0)
    total_pages = (total + limit - 1) // limit if total > 0 else 1

    return {
        "ok": True,
        "page": page,
        "limit": limit,
        "total": total,
        "total_pages": total_pages,
        "items": items,
        "summary": get_email_pool_summary(),
    }


def import_outlook_batch(text: str, auto_check: bool = False) -> dict[str, Any]:
    """批量导入微软邮箱文本（支持 ---- 或 ==== 分隔，4段或6段）。

    若 auto_check=True，则在导入后对新加入的邮箱自动进行测活与历史注册检测。
    """
    if not text or not str(text).strip():
        return {"ok": False, "error": "导入文本为空", "inserted": 0, "skipped": 0}

    lines = str(text).strip().splitlines()
    records = []
    skipped_format = 0

    for raw in lines:
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        parts = line.split("----") if "----" in line else line.split("====")
        if len(parts) not in (4, 6):
            skipped_format += 1
            continue
        email, password, client_id, refresh_token = (p.strip() for p in parts[:4])
        if not email or "@" not in email:
            skipped_format += 1
            continue
        rec: dict[str, Any] = {
            "email": email,
            "password": password,
            "client_id": client_id,
            "refresh_token": refresh_token,
        }
        if len(parts) == 6:
            rec["recovery_email"] = parts[4].strip()
            rec["recovery_code"] = parts[5].strip()
        records.append(rec)

    if not records:
        return {
            "ok": False,
            "error": f"未解析到合规格式的邮箱素材（格式错误行数: {skipped_format}）",
            "inserted": 0,
            "skipped": skipped_format,
        }

    inserted, skipped_exist = db.import_outlook_accounts(records)
    total_skipped = skipped_format + skipped_exist

    check_results = []
    if auto_check and records:
        imported_emails = [r["email"] for r in records]
        check_res = check_emails(imported_emails[:100])  # 最多一次自动检测 100 个
        check_results = check_res.get("results") or []

    return {
        "ok": True,
        "message": f"成功导入 {inserted} 个邮箱，跳过 {total_skipped} 个（格式不合规 {skipped_format}，已存在 {skipped_exist}）",
        "inserted": inserted,
        "skipped": total_skipped,
        "total_parsed": len(records),
        "auto_checked": auto_check,
        "check_results": check_results,
        "summary": get_email_pool_summary(),
    }


def check_single_mailbox(email: str) -> dict[str, Any]:
    """单邮箱实时测活与采购前历史注册检测。"""
    email = str(email or "").strip()
    if not email:
        return {"ok": False, "email": email, "error": "邮箱为空"}

    account = outlook_client.get_account_context(email)
    if account is None:
        return {"ok": False, "email": email, "error": "未找到邮箱素材（缺失 password/client_id/refresh_token）"}

    alive_ok, detail = outlook_client._verify_outlook_token_alive(account)
    is_pre_registered = any(k in detail for k in ("historical_openai", "chatgpt_locked", "deactivated", "already_registered"))

    if is_pre_registered:
        new_status = "already_registered"
        note = f"采购前收件箱含历史OpenAI记录: {detail[:160]}"
    elif alive_ok:
        new_status = "available"
        note = f"测活通过: {detail[:100]}"
    else:
        new_status = "failed"
        note = f"凭证失效: {detail[:160]}"

    db.release_outlook(email, status=new_status, note=note)

    return {
        "ok": alive_ok and not is_pre_registered,
        "email": email,
        "status": new_status,
        "is_already_registered": is_pre_registered,
        "detail": detail,
        "note": note,
    }


def check_emails(emails: list[str]) -> dict[str, Any]:
    """并发批量测活与历史注册检测。"""
    clean_emails = [str(e).strip() for e in emails if str(e).strip()]
    if not clean_emails:
        return {"ok": False, "error": "邮箱列表为空", "results": []}

    results: list[dict[str, Any]] = []
    max_workers = min(16, max(2, len(clean_emails)))
    with concurrent.futures.ThreadPoolExecutor(max_workers=max_workers, thread_name_prefix="email-check") as executor:
        future_map = {executor.submit(check_single_mailbox, e): e for e in clean_emails}
        for future in concurrent.futures.as_completed(future_map):
            try:
                res = future.result()
                results.append(res)
            except Exception as exc:
                em = future_map[future]
                results.append({"ok": False, "email": em, "error": str(exc), "status": "failed"})

    passed = sum(1 for r in results if r.get("status") == "available")
    already_reg = sum(1 for r in results if r.get("status") == "already_registered")
    failed = sum(1 for r in results if r.get("status") == "failed")

    return {
        "ok": True,
        "total": len(results),
        "passed": passed,
        "already_registered": already_reg,
        "failed": failed,
        "results": results,
        "summary": get_email_pool_summary(),
    }


def fetch_mailbox_otp_and_messages(email: str) -> dict[str, Any]:
    """抓取指定邮箱的最新 OTP 验证码与最近收件箱邮件列表。"""
    email = str(email or "").strip()
    if not email:
        return {"ok": False, "error": "邮箱为空"}

    account = outlook_client.get_account_context(email)
    if account is None:
        return {"ok": False, "error": f"未找到 {email} 的邮箱凭据（请确认已在邮箱池导入）"}

    session = outlook_client._http_session()
    raw_messages: list[dict] = []
    fetch_source = "none"

    # 1. 尝试使用 Graph API 读取
    try:
        raw_messages = outlook_client._fetch_via(session, "graph", account)
        fetch_source = "graph"
    except Exception as exc_graph:
        logger.debug("[FetchOTP] Graph API 拉信失败 (%s): %s，尝试 IMAP 直连", email, exc_graph)
        # 2. 失败后回退至 IMAP
        try:
            raw_messages = outlook_client._fetch_via(session, "imap", account)
            fetch_source = "imap"
        except Exception as exc_imap:
            logger.debug("[FetchOTP] IMAP 直连拉信失败 (%s): %s，尝试 REST 兜底", email, exc_imap)
            try:
                raw_messages = outlook_client._fetch_via(session, "rest", account)
                fetch_source = "rest"
            except Exception as exc_rest:
                return {
                    "ok": False,
                    "error": f"收取邮件失败（Graph/IMAP/REST 均不可达）: {exc_graph} | {exc_imap} | {exc_rest}",
                    "email": email,
                }

    # 解析邮件列表与提取 OTP
    normalized_list = []
    latest_otp = None
    latest_otp_subject = None
    latest_otp_time = None

    for msg in raw_messages:
        subj = str(msg.get("subject") or "").strip()
        sender = str(msg.get("from") or "").strip()
        date_str = str(msg.get("receivedDateTime") or msg.get("date") or "").strip()
        preview = str(msg.get("bodyPreview") or msg.get("snippet") or "").strip()
        html_body = str(msg.get("html") or msg.get("body") or "")

        # 尝试从该邮件提取 OTP
        otp = outlook_client._extract_otp(msg)
        is_openai = "openai" in subj.lower() or "chatgpt" in subj.lower() or "openai" in sender.lower()

        if otp and not latest_otp:
            latest_otp = otp
            latest_otp_subject = subj
            latest_otp_time = date_str

        text_body = str(msg.get("text") or msg.get("snippet") or preview or "")
        normalized_list.append({
            "id": msg.get("id"),
            "subject": subj,
            "from": sender,
            "date": date_str,
            "preview": preview or (html_body[:100] if html_body else ""),
            "body_html": html_body,
            "body_text": text_body,
            "has_html": bool(html_body),
            "otp": otp,
            "is_openai": is_openai,
        })

    return {
        "ok": True,
        "email": email,
        "latest_otp": latest_otp,
        "latest_otp_subject": latest_otp_subject,
        "latest_otp_time": latest_otp_time,
        "fetch_source": fetch_source,
        "messages_count": len(normalized_list),
        "messages": normalized_list[:15],
    }


def update_email_status(
    emails: list[str],
    status: str,
    note: str | None = None,
) -> dict[str, Any]:
    """批量或单个修改邮箱池中的邮箱状态。"""
    valid_statuses = {"available", "used", "already_registered", "failed", "cooling", "disabled"}
    status = str(status or "").strip().lower()
    if status not in valid_statuses:
        return {"ok": False, "error": f"不支持的状态: {status}，允许: {list(valid_statuses)}"}

    clean_emails = [str(e).strip() for e in emails if str(e).strip()]
    if not clean_emails:
        return {"ok": False, "error": "邮箱列表为空"}

    updated = 0
    for email in clean_emails:
        try:
            db.release_outlook(email, status=status, note=note)
            updated += 1
        except Exception as exc:
            logger.warning("[EmailPool] 更新状态失败 (%s): %s", email, exc)

    return {
        "ok": True,
        "message": f"已将 {updated} 个邮箱状态更新为 {status}",
        "updated": updated,
        "status": status,
        "summary": get_email_pool_summary(),
    }


def delete_emails(emails: list[str]) -> dict[str, Any]:
    """批量或单个从邮箱池中物理删除邮箱记录。"""
    clean_emails = [str(e).strip() for e in emails if str(e).strip()]
    if not clean_emails:
        return {"ok": False, "error": "邮箱列表为空"}

    deleted = 0
    for email in clean_emails:
        try:
            if db.delete_email_pool(email, source="outlook"):
                deleted += 1
        except Exception as exc:
            logger.warning("[EmailPool] 删除邮箱记录失败 (%s): %s", email, exc)

    return {
        "ok": True,
        "message": f"已从邮箱池删除 {deleted} 个邮箱",
        "deleted": deleted,
        "summary": get_email_pool_summary(),
    }
