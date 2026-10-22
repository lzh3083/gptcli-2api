# -*- coding: utf-8 -*-
"""
run_batch_existing_codex.py
方案 A: 为 10-21/10-22 最新注册的 ChatGPT 基础账号执行 Codex 手机验证与 OAuth PKCE 授权，
优先选择最新注册的账号，生成 codex-*-free.json 并自动双端落盘 (PostgreSQL + CPA目录 + chatgpt2api)。
"""

import sys
import os
import time
import json
import logging
import subprocess
from pathlib import Path

LOG_FILE = "/app/data/batch_existing_codex.log"
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    handlers=[
        logging.StreamHandler(sys.stdout),
        logging.FileHandler(LOG_FILE, mode="a", encoding="utf-8")
    ]
)
logger = logging.getLogger("ExistingCodex")

sys.path.insert(0, "/app/grok-build-auth/openai_client")
from register import _resolve_novproxy_one_ip
from core.codex_oauth import run_codex_oauth
from deliver_codex import sync_to_chatgpt2api

TARGET_SUCCESS = int(sys.argv[1]) if len(sys.argv) > 1 and sys.argv[1].isdigit() else 20
CPA_DIR = Path("/app/data/cpa_auth_files")
FAILED_DB = Path("/app/data/codex_failed_accounts.json")
# 同一账号累计失败达到该阈值后跳过，避免重启后反复卡在同一批坏号上
FAIL_SKIP_THRESHOLD = int(os.environ.get("CODEX_FAIL_SKIP", "2"))


def _load_failed() -> dict:
    try:
        if FAILED_DB.is_file():
            return json.loads(FAILED_DB.read_text(encoding="utf-8")) or {}
    except Exception:
        pass
    return {}


def _save_failed(db: dict) -> None:
    try:
        FAILED_DB.parent.mkdir(parents=True, exist_ok=True)
        FAILED_DB.write_text(json.dumps(db, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    except Exception as exc:
        logger.warning(f"失败库写入异常: {exc}")


def _mark_failed(email: str, reason: str) -> int:
    db = _load_failed()
    key = email.strip().lower()
    rec = db.get(key) or {"count": 0, "reasons": []}
    rec["count"] = int(rec.get("count", 0)) + 1
    reasons = list(rec.get("reasons") or [])
    reasons.append(str(reason)[:200])
    rec["reasons"] = reasons[-5:]
    rec["last_failed_at"] = time.strftime("%Y-%m-%d %H:%M:%S")
    db[key] = rec
    _save_failed(db)
    return rec["count"]


def _mark_success(email: str) -> None:
    db = _load_failed()
    db.pop(email.strip().lower(), None)
    _save_failed(db)


def get_candidate_accounts():
    chatgpt_files = list(CPA_DIR.glob("chatgpt-*.json"))
    codex_lower = set(f.name.replace("codex-", "").replace("-free.json", "").replace(".json", "").lower() for f in CPA_DIR.glob("codex-*.json"))
    
    items = []
    for f in chatgpt_files:
        name = f.name.replace("chatgpt-", "").replace("-free.json", "").replace(".json", "")
        if name.lower() in codex_lower:
            continue
        try:
            mtime = os.path.getmtime(f)
            from datetime import datetime
            dt = datetime.fromtimestamp(mtime)
            # 严格筛选 10-21 与 10-22 的最新账号，且必须为支持本地收件箱直连的 hotmail/outlook 域名
            if dt.strftime("%Y-%m-%d") in ("2026-10-21", "2026-10-22") and (name.lower().endswith("@hotmail.com") or name.lower().endswith("@outlook.com")):
                d = json.loads(f.read_text(encoding="utf-8"))
                pwd = d.get("password")
                if pwd:
                    items.append((mtime, name, pwd, dt.strftime("%Y-%m-%d %H:%M:%S")))
        except Exception:
            pass
    # 按时间倒序（最新的在前）
    items.sort(key=lambda x: x[0], reverse=True)
    return [(name, pwd, dt_str) for (_, name, pwd, dt_str) in items]

def kill_stray():
    subprocess.run("pkill -9 -f chrome 2>/dev/null || true", shell=True)
    subprocess.run("pkill -9 -f chromium 2>/dev/null || true", shell=True)
    subprocess.run("pkill -9 -f node 2>/dev/null || true", shell=True)

def main():
    candidates = get_candidate_accounts()
    logger.info("=" * 70)
    logger.info("=== 启动现成 ChatGPT 账号批量 Codex 授权流水线 (方案 A: 最新优先) ===")
    logger.info(f"筛选出 10-21/10-22 待处理候选账号总数: {len(candidates)} | 目标成功转化数: {TARGET_SUCCESS}")
    logger.info("=" * 70)

    if not candidates:
        logger.warning("没有可转化的账号，退出。")
        return

    # 依据历史失败次数降权：失败 >= 阈值的直接跳过，其余按失败次数升序优先
    failed_db = _load_failed()
    kept, skipped = [], []
    for item in candidates:
        cnt = int((failed_db.get(item[0].strip().lower()) or {}).get("count", 0))
        if cnt >= FAIL_SKIP_THRESHOLD:
            skipped.append((item[0], cnt))
        else:
            kept.append((cnt, item))
    kept.sort(key=lambda x: x[0])
    candidates = [it for _, it in kept]
    if skipped:
        logger.info(f"已跳过历史失败 >= {FAIL_SKIP_THRESHOLD} 次的账号 {len(skipped)} 个（避免重复消耗）")
    logger.info(f"本轮实际可尝试候选: {len(candidates)} 个")
    if not candidates:
        logger.warning("候选全部落入失败黑名单，请补充新账号。")
        return

    success_count = 0
    failed_count = 0

    for idx, (email, password, reg_time) in enumerate(candidates, 1):
        if success_count >= TARGET_SUCCESS:
            logger.info("🎉 已达到目标成功数 %s/%s，平稳结束！", success_count, TARGET_SUCCESS)
            break

        logger.info("-" * 65)
        logger.info(f">>> [Codex 验证] 进度: {idx}/{len(candidates)} (已成功: {success_count}/{TARGET_SUCCESS}) | 账号: {email} (注册: {reg_time}) <<<")
        logger.info("-" * 65)

        kill_stray()
        t0 = time.time()
        proxy = None
        try:
            proxy = _resolve_novproxy_one_ip(None)
            logger.info(f"分配纯净住宅代理: {proxy}")
        except Exception as pe:
            logger.warning(f"代理获取异常: {pe}，降级使用默认配置")

        try:
            res = run_codex_oauth(email, force=True, password=password, proxy=proxy)
            elapsed = time.time() - t0
            ok = bool(res and (res.get("ok") or res.get("status") == "success"))
            
            if ok:
                success_count += 1
                _mark_success(email)
                codex_file = res.get("file_path") or str(CPA_DIR / f"codex-{email}-free.json")
                logger.info(f"★ [Codex 成功] {email} 耗时: {elapsed:.1f}s | 凭证: {codex_file}")
                
                # 双端落盘: 1. PostgreSQL + 2. CPA 目录 + 3. chatgpt2api 端口 3000
                try:
                    subprocess.run([sys.executable, "/app/grok-build-auth/openai_client/deliver_codex.py", email], capture_output=True, timeout=120)
                    subprocess.run(["bash", "/app/scripts/sync_codex_to_cpa.sh"], capture_output=True, timeout=120)
                    logger.info(f"  > [阶段 7 双端落盘] 已成功入库 PostgreSQL 与 CPA 目录: {email}")
                except Exception as de:
                    logger.warning(f"  > [阶段 7 双端落盘异常]: {de}")

                try:
                    cred_data = json.loads(Path(codex_file).read_text(encoding="utf-8"))
                    sync_res = sync_to_chatgpt2api(cred_data)
                    logger.info(f"  > [chatgpt2api 同步] 状态: {sync_res.get('ok')} | 信息: {sync_res.get('message')}")
                except Exception as se:
                    logger.warning(f"  > [chatgpt2api 同步异常]: {se}")
            else:
                failed_count += 1
                err_msg = res.get("message") if res else "未知错误"
                fc = _mark_failed(email, err_msg)
                logger.warning(f"✕ [Codex 失败] {email} 耗时: {elapsed:.1f}s | 累计失败 {fc} 次 | 原因: {err_msg}")

        except Exception as exc:
            elapsed = time.time() - t0
            failed_count += 1
            fc = _mark_failed(email, f"{type(exc).__name__}: {exc}")
            logger.error(f"✕ [Codex 异常] {email} 耗时: {elapsed:.1f}s | 累计失败 {fc} 次 | 错误: {exc}")

        kill_stray()
        logger.info("账号间休息 6 秒防风控...")
        time.sleep(6)

    logger.info("=" * 70)
    logger.info(f"=== Codex 批量验证完成总结: 成功 {success_count} 个，失败 {failed_count} 个 ===")
    logger.info("=" * 70)

if __name__ == "__main__":
    main()
