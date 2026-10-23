#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
run_batch_hotmail_pipeline.py
端到端全流程 Hotmail 批量注册与 Codex 手机接码授权流水线：
每个账号在同一浏览器实例内一次性跑完：住宅代理分配 -> 邮箱领取 -> ChatGPT注册 -> 手机接码绑定 -> Codex PKCE 授权 -> 换取 Access Token -> 落盘交付。
严格杜绝产生未绑手机号的孤儿 ChatGPT 账号，无二阶段注册。
"""

import sys
import os
import time
import json
import logging
import subprocess
import subprocess as _sp

LOG_FILE = "/app/data/batch_hotmail_10.log"
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    handlers=[
        logging.StreamHandler(sys.stdout),
        logging.FileHandler(LOG_FILE, mode="a", encoding="utf-8")
    ]
)
logger = logging.getLogger("HotmailBatch")
os.environ["SMS_CODE_WAIT"] = "180"
import config.codex
config.codex.SMS_CODE_WAIT = 180

PROXIES = [
    "socks5h://162.128.157.171:32116",  # StarHub SG
    "socks5h://162.128.157.171:33093",  # StarHub SG
    "socks5h://198.44.167.42:28821",   # StarHub SG
    "socks5h://198.44.167.42:35501",   # MyRepublic SG
    "socks5h://198.44.167.42:27066",   # SingTel SG
    "socks5h://198.44.167.42:27067",   # StarHub SG
    "socks5h://198.44.167.198:26553",  # SingTel SG
]

TARGET_COUNT = int(sys.argv[1]) if len(sys.argv) > 1 and sys.argv[1].isdigit() else 10
MAX_REG_ATTEMPTS = int(sys.argv[2]) if len(sys.argv) > 2 and sys.argv[2].isdigit() else (TARGET_COUNT * 3 + 10)

SINGLE_REG_SCRIPT = r"""
import sys, json, os
sys.path.insert(0, '/app/grok-build-auth/openai_client')

import config.codex
config.codex.ENABLE_CODEX_AUTO = True
config.codex.FORCE_PHONE_VERIFICATION = True
config.codex.SMS_CODE_WAIT = 180
config.codex.SMS_COUNTRY = "4,6,66,2,16,73"
config.codex.SMS_MAX_PRICE = "0.075"
os.environ["SMS_COUNTRY"] = "4,6,66,2,16,73"
os.environ["SMS_MAX_PRICE"] = "0.075"

from register import run_openai_registration

proxy = sys.argv[1] if len(sys.argv) > 1 and sys.argv[1].strip() else None

try:
    res = run_openai_registration(proxy=proxy, enable_codex=True)
    codex_info = res.get("codex") or {}
    codex_ok = bool(codex_info.get("ok"))
    cpa_file = codex_info.get("file_path") or res.get("cpa_file") or ""
    final_ok = bool(res.get("success")) and codex_ok and "codex-" in str(cpa_file)
    
    print("RESULT_JSON_START")
    print(json.dumps({
        "success": final_ok,
        "codex_ok": codex_ok,
        "email": res.get("email"),
        "password": res.get("password"),
        "account_id": res.get("account_id"),
        "cpa_file": cpa_file,
        "error": res.get("error") or (None if codex_ok else codex_info.get("message"))
    }, ensure_ascii=False))
    print("RESULT_JSON_END")
except Exception as e:
    print("RESULT_JSON_START")
    print(json.dumps({"success": False, "codex_ok": False, "error": str(e)}, ensure_ascii=False))
    print("RESULT_JSON_END")
"""

logger.info("=" * 65)
logger.info("=== 启动 Hotmail 端到端 Codex 手机验证与授权流水线 (无二阶段) ===")
logger.info(f"目标 Codex 账号数: {TARGET_COUNT} 个 | 最大尝试轮次: {MAX_REG_ATTEMPTS}")
logger.info("=" * 65)

codex_success_list = []
codex_failed_list = []

for attempt in range(1, MAX_REG_ATTEMPTS + 1):
    if len(codex_success_list) >= TARGET_COUNT:
        logger.info(f"🎉 达成目标：已成功完成 {len(codex_success_list)}/{TARGET_COUNT} 个 Codex 账号！")
        break

    proxy = None
    try:
        sys.path.insert(0, "/app/grok-build-auth/openai_client")
        from register import _resolve_novproxy_one_ip
        proxy = _resolve_novproxy_one_ip(None)
    except Exception as _pe:
        logger.warning(f"[NovProxy] 动态提取异常: {_pe}，降级轮换备用池")
        proxy = PROXIES[(attempt - 1) % len(PROXIES)]

    logger.info("-" * 65)
    logger.info(f">>> [流水线] 轮次 {attempt}/{MAX_REG_ATTEMPTS} (Codex 成功: {len(codex_success_list)}/{TARGET_COUNT}) | 代理: {proxy} <<<")
    logger.info("-" * 65)

    t0 = time.time()
    res_data = {}
    full_output = []

    try:
        proc = subprocess.Popen(
            [sys.executable, "-u", "-c", SINGLE_REG_SCRIPT, proxy],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1
        )

        timeout_sec = 650
        deadline = time.time() + timeout_sec

        while True:
            if time.time() > deadline:
                proc.kill()
                raise subprocess.TimeoutExpired(proc.args, timeout_sec)

            line = proc.stdout.readline()
            if not line:
                if proc.poll() is not None:
                    break
                time.sleep(0.1)
                continue

            line_str = line.strip()
            full_output.append(line_str)
            if any(k in line_str for k in (
                "[NovProxy]", "[proxy_assigned]", "[email_acquired]", "OTP=",
                "[Save]", "[CPA]", "RESULT_JSON", "SUCCESS", "FAILED",
                "[Codex]", "[Codex][Cloak]", "[Codex][Browser]", "[SMS]", "[SMSBower]",
                "手机验证尝试", "手机 OTP 收到", "换 token 成功", "已填写手机 OTP",
                "已提交手机 OTP", "已重新设置手机号", "left_phone_flow",
                "已提交邮箱", "已填写并提交登录密码", "已填写并点击密码页",
                "CloakBrowser", "preflight", "starting:", "submit_email:",
                "password_step:", "waiting_otp:", "validating_otp:", "about_you:",
                "fetch_session:", "注册成功完成", "Error:", "Exception:", "Traceback", "failed:", "失败"
            )):
                logger.info(f"  > {line_str}")

        proc.wait(timeout=10)
        elapsed = time.time() - t0
        output_text = "\n".join(full_output)

        if "RESULT_JSON_START" in output_text and "RESULT_JSON_END" in output_text:
            part = output_text.split("RESULT_JSON_START")[1].split("RESULT_JSON_END")[0]
            try:
                res_data = json.loads(part.strip())
            except Exception:
                pass

        ok = bool(res_data.get("success")) and bool(res_data.get("codex_ok")) and os.path.isfile(res_data.get("cpa_file", ""))
        email = res_data.get("email") or "unknown"
        cpa_file = res_data.get("cpa_file") or ""

        if ok and "codex-" in cpa_file:
            logger.info(f"★ [Codex 全流程成功] {email} 耗时: {elapsed:.1f}s | CPA 凭证: {cpa_file}")
            codex_success_list.append({
                "email": email,
                "account_id": res_data.get("account_id"),
                "cpa_file": cpa_file,
                "elapsed": round(elapsed, 1)
            })
            try:
                _sp.run([sys.executable, '/app/grok-build-auth/openai_client/deliver_codex.py', email],
                        capture_output=True, timeout=120)
                _sp.run(['bash', '/app/scripts/sync_codex_to_cpa.sh'],
                        capture_output=True, timeout=120)
                logger.info(f"  > [阶段 7 交付] 已落盘 PostgreSQL + CPA 目录并热同步 2api: {email}")
            except Exception as _de:
                logger.warning(f"  > [阶段 7 交付] 落盘异常: {_de}")
        else:
            err = res_data.get("error") or (f"non_zero_exit_{proc.returncode}" if proc.returncode != 0 else "codex_phone_unverified")
            logger.warning(f"✕ [Codex 流程未完成] {email} 耗时: {elapsed:.1f}s | 错误原因: {err}")
            codex_failed_list.append({"email": email, "error": str(err)[:200]})

    except subprocess.TimeoutExpired:
        elapsed = time.time() - t0
        logger.error(f"⏱ [超时] 轮次 {attempt} 超时 > {timeout_sec}s")
        codex_failed_list.append({"email": "timeout", "error": f"timeout > {timeout_sec}s"})
    except Exception as exc:
        elapsed = time.time() - t0
        logger.error(f"⚠ [异常] 轮次 {attempt} 发生异常: {exc}")
        codex_failed_list.append({"email": "exception", "error": str(exc)[:200]})

    subprocess.run(["pkill", "-9", "-f", "chrome"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.run(["pkill", "-9", "-f", "chromium"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(3)

logger.info("\n" + "=" * 65)
logger.info("=== Codex 流水线最终执行总结 ===")
logger.info(f"Codex 账号达成数: {len(codex_success_list)}/{TARGET_COUNT}")
logger.info(f"失败轮次: {len(codex_failed_list)}")
logger.info(json.dumps({
    "target_count": TARGET_COUNT,
    "codex_success": codex_success_list,
    "codex_failed": codex_failed_list
}, indent=2, ensure_ascii=False))
logger.info("=" * 65)
