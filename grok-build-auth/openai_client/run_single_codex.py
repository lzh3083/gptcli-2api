# -*- coding: utf-8 -*-
"""单账号 Codex 手机验证与 OAuth 授权，完成后仅同步至 CPA。"""
import json
import logging
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

if not PROXY:
    try:
        PROXY = _resolve_novproxy_one_ip(None)
    except Exception:
        PROXY = "socks5h://162.128.157.171:32116"

CPA_DIR = Path("/app/data/cpa_auth_files")
src = CPA_DIR / f"chatgpt-{EMAIL}.json"
if not src.exists():
    src_lower = CPA_DIR / f"chatgpt-{EMAIL.lower()}.json"
    if src_lower.exists():
        src = src_lower

if not src.exists():
    logger.error("未找到基础 ChatGPT 凭证文件: %s", src)
    sys.exit(1)

d = json.loads(src.read_text(encoding="utf-8"))
password = d.get("password")

logger.info("=" * 70)
logger.info(">>> 启动单账号 Codex 授权: %s | 代理: %s", EMAIL, PROXY)
logger.info("=" * 70)

t0 = time.time()
try:
    res = run_codex_oauth(EMAIL, force=False, password=password, proxy=PROXY)
    el = time.time() - t0
    ok = bool(res and (res.get("ok") or res.get("status") == "success"))
    logger.info("=" * 70)
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
    else:
        logger.info("✕ 授权失败 %s 耗时 %.1fs | 状态: %s | 信息: %s", EMAIL, el, res.get("status"), res.get("message"))
    logger.info("=" * 70)
except Exception as e:
    logger.error("✕ 授权异常 %s 耗时 %.1fs | %s: %s", EMAIL, time.time() - t0, type(e).__name__, e)
