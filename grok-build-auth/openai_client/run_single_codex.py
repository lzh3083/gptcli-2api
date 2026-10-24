# -*- coding: utf-8 -*-
"""单账号 Codex 手机验证与 OAuth 授权，完成后仅同步至 CPA。支持 account_deactivated 自动废弃。"""
import json
import logging
import os
import sqlite3
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, "/app/grok-build-auth/openai_client")
from core.codex_oauth import run_codex_oauth
from register import _resolve_novproxy_one_ip

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    handlers=[logging.StreamHandler(sys.stdout)]
)
logger = logging.getLogger("SingleCodex")

EMAIL = sys.argv[1] if len(sys.argv) > 1 else "WilfridTremblayfoz@hotmail.com"
PROXY = sys.argv[2] if len(sys.argv) > 2 and sys.argv[2].strip() else None
CLI_PASSWORD = sys.argv[3] if len(sys.argv) > 3 and sys.argv[3].strip() else None

CPA_DIR = Path("/app/data/cpa_auth_files")
COOLING_DIR = Path("/app/data/cpa_cooling")
DB_PATH = Path("/app/grok-build-auth/openai_client/turb.sqlite3")
if not DB_PATH.exists():
    DB_PATH = Path(__file__).resolve().parent / "turb.sqlite3"


def find_account_password(email: str) -> str | None:
    """多渠道自动定位账号密码：命令行参数 -> 本地凭证文件 -> turb.sqlite3 数据库。"""
    if CLI_PASSWORD:
        return CLI_PASSWORD

    # 1. 检查凭据文件
    candidate_paths = [
        CPA_DIR / f"chatgpt-{email}.json",
        CPA_DIR / f"chatgpt-{email.lower()}.json",
        CPA_DIR / f"codex-{email}-free.json",
        CPA_DIR / f"codex-{email.lower()}-free.json",
        COOLING_DIR / f"codex-{email}-free.json",
        COOLING_DIR / f"codex-{email.lower()}-free.json",
    ]
    for p in candidate_paths:
        if p.exists():
            try:
                data = json.loads(p.read_text(encoding="utf-8"))
                pwd = data.get("password")
                if pwd:
                    return str(pwd)
            except Exception:
                pass

    # 2. 查询 turb.sqlite3
    if DB_PATH.exists():
        try:
            conn = sqlite3.connect(str(DB_PATH))
            cur = conn.cursor()
            # 从 accounts 查询
            cur.execute("SELECT payload FROM accounts WHERE LOWER(email) = LOWER(?) LIMIT 1", (email,))
            row = cur.fetchone()
            if row and row[0]:
                payload = json.loads(row[0]) if isinstance(row[0], str) else row[0]
                if isinstance(payload, dict) and payload.get("password"):
                    return str(payload["password"])

            # 从 email_pool 查询
            cur.execute("SELECT payload FROM email_pool WHERE LOWER(email) = LOWER(?) LIMIT 1", (email,))
            row = cur.fetchone()
            if row and row[0]:
                payload = json.loads(row[0]) if isinstance(row[0], str) else row[0]
                if isinstance(payload, dict) and payload.get("password"):
                    return str(payload["password"])
        except Exception as e:
            logger.warning("查询 SQLite 密码异常: %s", e)
        finally:
            try:
                conn.close()
            except Exception:
                pass

    return None


def discard_deactivated_account(email: str, reason: str = "account_deactivated") -> None:
    """彻底废弃已被官方注销/停用的账号：清理本地与生产凭证，更新数据库黑名单。"""
    logger.warning("=" * 70)
    logger.warning(f"🚫 [账号彻底废弃] {email} 已被官方注销/停用 ({reason})，执行清理流程...")
    logger.warning("=" * 70)

    # 1. 清理本地和 CPA 生产凭证
    for base_dir in [
        CPA_DIR,
        COOLING_DIR,
        Path("/www/wwwroot/cpa-manager-plus/cliproxyapi/auths"),
        Path("/www/wwwroot/gptcli-2api/data/cpa_auth_files"),
    ]:
        for pattern in [f"*{email}*", f"*{email.lower()}*"]:
            for f in base_dir.glob(pattern):
                try:
                    f.unlink()
                    logger.info(f"  > 已删除失效凭证: {f}")
                except Exception as de:
                    logger.warning(f"  > 删除文件失败 {f}: {de}")

    # 2. 更新 SQLite 状态
    if DB_PATH.exists():
        try:
            conn = sqlite3.connect(str(DB_PATH))
            cur = conn.cursor()
            cur.execute(
                "UPDATE accounts SET status = 'deactivated', updated_at = datetime('now') WHERE LOWER(email) = LOWER(?)",
                (email,)
            )
            cur.execute(
                "UPDATE email_pool SET status = 'deactivated', updated_at = datetime('now') WHERE LOWER(email) = LOWER(?)",
                (email,)
            )
            conn.commit()
            conn.close()
            logger.info(f"  > 已将 {email} 在 turb.sqlite3 中标记为 deactivated")
        except Exception as se:
            logger.warning(f"  > 更新 SQLite 状态失败: {se}")

    # 3. 记录到失败黑名单
    failed_file = Path("/app/data/codex_failed_accounts.json")
    if not failed_file.parent.exists():
        failed_file = Path("/www/wwwroot/gptcli-2api/data/codex_failed_accounts.json")
    try:
        data = {}
        if failed_file.exists():
            try:
                data = json.loads(failed_file.read_text(encoding="utf-8"))
            except Exception:
                data = {}
        data[email.lower()] = {
            "email": email,
            "status": "deactivated",
            "reason": reason,
            "discarded_at": time.strftime("%Y-%m-%d %H:%M:%S")
        }
        failed_file.write_text(json.dumps(data, indent=2, ensure_ascii=False), encoding="utf-8")
        logger.info(f"  > 已写入黑名单 {failed_file}")
    except Exception as fe:
        logger.warning(f"  > 写入黑名单失败: {fe}")


def is_deactivated_error(res: dict) -> bool:
    """判断是否为官方注销/停用/封禁错误。"""
    if not isinstance(res, dict):
        return False
    status = str(res.get("status") or "").lower()
    msg = str(res.get("message") or "").lower()
    err_code = str(res.get("error_code") or "").lower()

    if status == "deactivated":
        return True
    if err_code in ("account_deactivated", "account_deleted", "account_banned"):
        return True
    if any(k in msg for k in ("account_deactivated", "account was deactivated", "account has been deactivated", "account_deleted", "account_banned")):
        return True
    return False


if not PROXY:
    try:
        PROXY = _resolve_novproxy_one_ip(None)
    except Exception:
        PROXY = "socks5h://162.128.157.171:32116"

password = find_account_password(EMAIL)
if not password:
    logger.error("未能找到账号 %s 的密码，请检查数据库或凭证文件！", EMAIL)
    print(f"RESULT_JSON_START\n{json.dumps({'success': False, 'status': 'failed', 'email': EMAIL, 'error': 'missing_password'})}\nRESULT_JSON_END")
    sys.exit(1)

logger.info("=" * 70)
logger.info(">>> 启动单账号 Codex 授权: %s | 代理: %s", EMAIL, PROXY)
logger.info("=" * 70)

t0 = time.time()
try:
    res = run_codex_oauth(EMAIL, force=False, password=password, proxy=PROXY)
    el = time.time() - t0
    ok = bool(res and (res.get("ok") or res.get("status") == "success"))
    logger.info("=" * 70)

    # 1. 检查是否为 account_deactivated 注销
    if is_deactivated_error(res):
        reason = res.get("message") or "account_deactivated"
        discard_deactivated_account(EMAIL, reason=reason)
        print(f"RESULT_JSON_START\n{json.dumps({'success': False, 'status': 'deactivated', 'email': EMAIL, 'reason': reason})}\nRESULT_JSON_END")
        sys.exit(2)

    # 2. 授权成功
    if ok:
        file_path = res.get("file_path")
        logger.info("★ 授权成功 %s 耗时 %.1fs | 凭证: %s", EMAIL, el, file_path)
        try:
            logger.info("正在执行 CPA 凭据落盘与同步...")
            subprocess.run([sys.executable, "/app/grok-build-auth/openai_client/deliver_codex.py", EMAIL], capture_output=True, timeout=120)
            subprocess.run(["bash", "/app/scripts/sync_codex_to_cpa.sh"], capture_output=True, timeout=120)
            logger.info("★ 已成功将 %s 凭据同步至 CPA 生产目录！", EMAIL)
        except Exception as de:
            logger.warning("CPA 交付同步异常: %s", de)
        print(f"RESULT_JSON_START\n{json.dumps({'success': True, 'status': 'success', 'email': EMAIL, 'file_path': file_path})}\nRESULT_JSON_END")
        sys.exit(0)

    # 3. 其他失败
    logger.info("✕ 授权未完成 %s 耗时 %.1fs | 状态: %s | 信息: %s", EMAIL, el, res.get("status"), res.get("message"))
    logger.info("=" * 70)
    print(f"RESULT_JSON_START\n{json.dumps({'success': False, 'status': res.get('status') or 'failed', 'email': EMAIL, 'message': str(res.get('message'))[:200]})}\nRESULT_JSON_END")
    sys.exit(1)

except Exception as e:
    err_str = f"{type(e).__name__}: {e}"
    logger.error("✕ 授权异常 %s 耗时 %.1fs | %s", EMAIL, time.time() - t0, err_str)
    if "account_deactivated" in err_str.lower():
        discard_deactivated_account(EMAIL, reason=err_str)
        print(f"RESULT_JSON_START\n{json.dumps({'success': False, 'status': 'deactivated', 'email': EMAIL, 'reason': err_str})}\nRESULT_JSON_END")
        sys.exit(2)
    print(f"RESULT_JSON_START\n{json.dumps({'success': False, 'status': 'error', 'email': EMAIL, 'error': err_str[:200]})}\nRESULT_JSON_END")
    sys.exit(1)
