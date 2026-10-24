# -*- coding: utf-8 -*-
"""Codex 重新授权与强制重新登录授权服务。

支持：
1. “重新授权”（mode="reauth"）：优先使用已有 Refresh Token 向 https://auth.openai.com/oauth/token 换取新令牌；
   刷新成功后更新 PostgreSQL accounts 表、Redis 缓存及本地 CPA JSON 凭证文件。
2. “重新登录并授权”（mode="relogin"）：跳过刷新令牌和旧检查点，强制使用账号密码 + 邮箱实时 OTP 验证码 + TOTP 完成完整重登与授权。
"""
from __future__ import annotations

import base64
import concurrent.futures
import json
import logging
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
import urllib.parse
import urllib.request

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
    from core import codex_oauth
    from core import codex_retry_service
except ImportError:
    from openai_client.core import db
    from openai_client.core import codex_oauth
    from openai_client.core import codex_retry_service

import grok2api.pool.accounts as account_pool

logger = logging.getLogger(__name__)

OPENAI_OIDC_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
OPENAI_TOKEN_URL = "https://auth.openai.com/oauth/token"


def _b64url_decode(s: str) -> bytes:
    s = s.strip()
    s += "=" * (-len(s) % 4)
    return base64.urlsafe_b64decode(s)


def _parse_jwt(token: str) -> dict[str, Any]:
    try:
        parts = token.split(".")
        if len(parts) < 2:
            return {}
        return json.loads(_b64url_decode(parts[1]))
    except Exception:
        return {}


def _get_account_details(email: str = "", account_id: str = "") -> dict[str, Any] | None:
    """从本地 Store (PostgreSQL / Redis / SQLite) 获取账号记录。"""
    acc_map = account_pool.read_auth_map()
    email_clean = email.strip().lower()
    acc_id_clean = account_id.strip()

    # 1. 尝试按 account_id 查找
    if acc_id_clean and acc_id_clean in acc_map:
        return dict(acc_map[acc_id_clean])

    # 2. 尝试按 email 查找
    if email_clean:
        for k, v in acc_map.items():
            em = str(v.get("email") or "").strip().lower()
            if em == email_clean:
                v_copy = dict(v)
                v_copy["_account_id"] = k
                return v_copy

    # 3. 尝试从 SQLite accounts 查找
    if email_clean:
        try:
            sqlite_row = db.get_account_by_email(email_clean)
            if sqlite_row:
                return sqlite_row
        except Exception:
            pass

    return None


def reauth_single_account(
    email: str = "",
    account_id: str = "",
    proxy: str | None = None,
) -> dict[str, Any]:
    """使用已有 Refresh Token 重新授权单账号。"""
    email_clean = email.strip().lower()
    acc_details = _get_account_details(email=email_clean, account_id=account_id)
    if not acc_details:
        return {
            "ok": False,
            "email": email_clean,
            "account_id": account_id,
            "error": "未找到该账号记录，请确认账号存在",
            "needs_relogin": True,
        }

    email_val = str(acc_details.get("email") or email_clean).strip()
    refresh_token = str(acc_details.get("refresh_token") or "").strip()

    # 如果 payload 没存 refresh_token，尝试从 CPA 授权文件读取
    if not refresh_token:
        try:
            data_root = os.environ.get("GROK2API_DATA_DIR") or ("/app/data" if Path("/app/data").is_dir() else str(REPO_ROOT / "data"))
            cpa_dir = Path(data_root) / "cpa_auth_files"
            for p in cpa_dir.glob(f"codex-{email_val}*.json"):
                try:
                    cdata = json.loads(p.read_text(encoding="utf-8"))
                    rt = str(cdata.get("refresh_token") or "").strip()
                    if rt:
                        refresh_token = rt
                        break
                except Exception:
                    pass
        except Exception:
            pass

    if not refresh_token:
        return {
            "ok": False,
            "email": email_val,
            "account_id": account_id,
            "error": "该账号未保存 Refresh Token（刷新令牌），无法直接重新授权。请使用「重新登录并授权」",
            "needs_relogin": True,
        }

    # 构造换 Token 请求
    post_data = urllib.parse.urlencode({
        "grant_type": "refresh_token",
        "refresh_token": refresh_token,
        "client_id": OPENAI_OIDC_CLIENT_ID,
    }).encode("utf-8")

    req = urllib.request.Request(
        OPENAI_TOKEN_URL,
        data=post_data,
        headers={
            "Content-Type": "application/x-www-form-urlencoded",
            "User-Agent": "OpenAI/Codex (cli-proxy; macOS)",
            "Accept": "application/json",
        },
    )

    handlers = []
    if proxy and str(proxy).strip():
        handlers.append(urllib.request.ProxyHandler({"http": proxy, "https": proxy}))
    opener = urllib.request.build_opener(*handlers)

    try:
        with opener.open(req, timeout=30) as resp:
            body = resp.read().decode("utf-8")
            data = json.loads(body)
    except urllib.error.HTTPError as exc:
        err_body = ""
        try:
            err_body = exc.read().decode("utf-8")
        except Exception:
            pass
        status_code = exc.code
        err_msg = f"HTTP {status_code}: {err_body[:200]}"
        logger.warning("[Codex ReAuth] %s 刷新令牌失败: %s", email_val, err_msg)
        
        # 刷新令牌失效
        return {
            "ok": False,
            "email": email_val,
            "account_id": account_id,
            "status_code": status_code,
            "error": f"刷新令牌已失效或过期 ({err_msg})，请使用「重新登录并授权」强制重新登录",
            "needs_relogin": True,
        }
    except Exception as exc:
        logger.exception("[Codex ReAuth] %s 请求异常", email_val)
        return {
            "ok": False,
            "email": email_val,
            "account_id": account_id,
            "error": f"请求刷新端点网络异常: {exc}",
            "needs_relogin": False,
        }

    new_access_token = str(data.get("access_token") or "").strip()
    new_refresh_token = str(data.get("refresh_token") or refresh_token).strip()
    new_id_token = str(data.get("id_token") or "").strip()
    expires_in = int(data.get("expires_in") or 864000)

    if not new_access_token:
        return {
            "ok": False,
            "email": email_val,
            "error": "OpenAI 返回数据缺少 access_token",
            "needs_relogin": True,
        }

    # 解析 claims
    id_claims = _parse_jwt(new_id_token) if new_id_token else {}
    auth_claim = id_claims.get("https://api.openai.com/auth", {}) or {}
    user_id = (
        auth_claim.get("chatgpt_user_id")
        or auth_claim.get("user_id")
        or id_claims.get("sub")
        or acc_details.get("user_id")
        or email_val
    )
    plan_type = (
        id_claims.get("plan_type")
        or auth_claim.get("plan_type")
        or acc_details.get("plan_type")
        or "free"
    )

    # 组装 storage 并保存至 CPA 凭证目录
    storage = codex_oauth.build_codex_storage(data, id_claims)
    if not storage.get("email"):
        storage["email"] = email_val
    if not storage.get("account_id"):
        storage["account_id"] = user_id

    cpa_file_path = ""
    try:
        cpa_file_path = codex_oauth.save_codex_credential(storage, email_val, plan_type)
    except Exception as exc:
        logger.warning("[Codex ReAuth] 保存 CPA 文件异常: %s", exc)

    # 同步更新 PostgreSQL 和 Redis 缓存
    now_iso = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    import_payload = dict(acc_details)
    import_payload.update({
        "key": new_access_token,
        "type": "codex",
        "cliproxyapi_type": "codex",
        "auth_mode": "codex_oauth",
        "email": email_val,
        "user_id": user_id,
        "refresh_token": new_refresh_token,
        "id_token": new_id_token,
        "plan_type": plan_type,
        "expires_at": time.time() + expires_in,
        "sso": new_access_token,
        "sso_token": new_access_token,
        "sso_cookie": new_access_token,
        "codex_cpa_file": cpa_file_path,
        "codex_status": "success",
        "last_reauth_at": now_iso,
        "last_renew_at": time.time(),
    })

    try:
        import_res = account_pool.import_auth_payload(import_payload, merge=True)
        if not import_res.get("ok"):
            logger.warning("[Codex ReAuth] import_auth_payload 警告: %s", import_res.get("error"))
    except Exception as exc:
        logger.warning("[Codex ReAuth] 更新 account_pool 异常: %s", exc)

    # 同步更新 SQLite accounts
    try:
        db.update_account_codex_status(email_val, "success", None)
    except Exception:
        pass

    logger.info("[Codex ReAuth] %s 重新授权成功！", email_val)
    return {
        "ok": True,
        "email": email_val,
        "message": f"账号 {email_val} 重新授权成功！刷新令牌已更新并持久化",
        "plan_type": plan_type,
        "cpa_file": cpa_file_path,
        "expires_in": expires_in,
        "last_reauth_at": now_iso,
    }


def relogin_single_account(email: str, force: bool = True) -> dict[str, Any]:
    """强制重新登录并授权单账号（跳过 Refresh Token，走完整 OAuth 浏览器登录与邮箱接码）。"""
    email_clean = email.strip().lower()
    if not email_clean:
        return {"ok": False, "error": "email 为空"}

    # 检查进程锁，避免同一账号并发冲突
    if not codex_retry_service.reserve(email_clean):
        return {"ok": False, "email": email_clean, "error": "该账号已有重试/补跑任务正在进行中，请稍候"}

    try:
        logger.info("[Codex ReLogin] 开始对 %s 执行强制重新登录并授权...", email_clean)
        res = codex_retry_service.run_worker(email_clean, batch_label="WebUI-ReLogin-Force")
        is_ok = bool(res.get("ok") or res.get("status") == "success")
        return {
            "ok": is_ok,
            "email": email_clean,
            "status": res.get("status"),
            "message": res.get("message") or ("重新登录并授权成功" if is_ok else "重新登录并授权失败"),
            "file_path": res.get("file_path"),
            "callback_url": res.get("callback_url"),
        }
    except Exception as exc:
        logger.exception("[Codex ReLogin] %s 异常: %s", email_clean, exc)
        return {
            "ok": False,
            "email": email_clean,
            "error": f"重新登录并授权执行异常: {exc}",
        }


def batch_reauth(
    emails: list[str] | None = None,
    account_ids: list[str] | None = None,
    all_expired: bool = False,
    proxy: str | None = None,
) -> dict[str, Any]:
    """批量使用 Refresh Token 重新授权。"""
    target_emails: set[str] = set()

    if emails:
        for e in emails:
            if str(e).strip():
                target_emails.add(str(e).strip().lower())

    if account_ids:
        acc_map = account_pool.read_auth_map()
        for aid in account_ids:
            rec = acc_map.get(str(aid).strip())
            if rec and rec.get("email"):
                target_emails.add(str(rec["email"]).strip().lower())

    if all_expired or not target_emails:
        acc_map = account_pool.read_auth_map()
        now_ts = time.time()
        for _, rec in acc_map.items():
            if not isinstance(rec, dict):
                continue
            em = str(rec.get("email") or "").strip().lower()
            if not em:
                continue
            exp = float(rec.get("expires_at") or 0)
            status = str(rec.get("codex_status") or "").lower()
            # 已过期或即将（1天内）过期，或者 codex_status 为 expired / failed
            if exp < now_ts + 86400 or status in ("expired", "failed"):
                target_emails.add(em)

    clean_list = list(target_emails)
    if not clean_list:
        return {"ok": True, "message": "没有需要重新授权的账号", "total": 0, "success": 0, "failed": 0, "results": []}

    results: list[dict[str, Any]] = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=min(8, len(clean_list)), thread_name_prefix="codex-reauth") as executor:
        future_map = {executor.submit(reauth_single_account, email=e, proxy=proxy): e for e in clean_list}
        for future in concurrent.futures.as_completed(future_map):
            try:
                res = future.result()
                results.append(res)
            except Exception as exc:
                em = future_map[future]
                results.append({"ok": False, "email": em, "error": str(exc), "needs_relogin": True})

    success_count = sum(1 for r in results if r.get("ok"))
    failed_count = len(results) - success_count

    return {
        "ok": True,
        "message": f"批量重新授权完成：成功 {success_count} 个，失败 {failed_count} 个",
        "total": len(results),
        "success": success_count,
        "failed": failed_count,
        "results": results,
    }


def batch_relogin(emails: list[str]) -> dict[str, Any]:
    """批量强制重新登录并授权（受控串行或双线程执行，防止浏览器和接码冲突）。"""
    clean_list = [str(e).strip().lower() for e in emails if str(e).strip()]
    if not clean_list:
        return {"ok": False, "error": "待重新登录邮箱列表为空", "results": []}

    results: list[dict[str, Any]] = []
    # 强制重登需要启动无头浏览器，设置并发为 1-2，避免占满内存
    with concurrent.futures.ThreadPoolExecutor(max_workers=2, thread_name_prefix="codex-relogin") as executor:
        future_map = {executor.submit(relogin_single_account, e): e for e in clean_list}
        for future in concurrent.futures.as_completed(future_map):
            try:
                res = future.result()
                results.append(res)
            except Exception as exc:
                em = future_map[future]
                results.append({"ok": False, "email": em, "error": str(exc)})

    success_count = sum(1 for r in results if r.get("ok"))
    failed_count = len(results) - success_count

    return {
        "ok": True,
        "message": f"批量重新登录并授权完成：成功 {success_count} 个，失败 {failed_count} 个",
        "total": len(results),
        "success": success_count,
        "failed": failed_count,
        "results": results,
    }
