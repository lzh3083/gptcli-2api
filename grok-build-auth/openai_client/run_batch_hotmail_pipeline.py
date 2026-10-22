#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
run_batch_hotmail_pipeline.py
全自动 Hotmail 批量注册与 Codex CPA 凭证导出流水线：
阶段 1: 批量注册纯 ChatGPT 账号直到达成指定成功数（默认 10）
阶段 2: 对阶段 1 成功的账号依次执行 Codex PKCE 授权与手机接码，生成 codex-*-free.json
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
MAX_REG_ATTEMPTS = int(sys.argv[2]) if len(sys.argv) > 2 and sys.argv[2].isdigit() else (TARGET_COUNT * 2 + 6)

SINGLE_REG_SCRIPT = r"""
import sys, json, os
sys.path.insert(0, '/app/grok-build-auth/openai_client')

import config.codex
config.codex.ENABLE_CODEX_AUTO = False
config.codex.FORCE_PHONE_VERIFICATION = False

from register import run_openai_registration

proxy = sys.argv[1] if len(sys.argv) > 1 and sys.argv[1].strip() else None

try:
    res = run_openai_registration(proxy=proxy, enable_codex=False)
    print("RESULT_JSON_START")
    print(json.dumps({
        "success": bool(res.get("success")),
        "email": res.get("email"),
        "password": res.get("password"),
        "account_id": res.get("account_id"),
        "cpa_file": res.get("cpa_file"),
        "error": res.get("error")
    }, ensure_ascii=False))
    print("RESULT_JSON_END")
except Exception as e:
    print("RESULT_JSON_START")
    print(json.dumps({"success": False, "error": str(e)}, ensure_ascii=False))
    print("RESULT_JSON_END")
"""

logger.info("=" * 65)
logger.info("=== 启动 Hotmail 批量注册与 Codex 授权流水线 ===")
logger.info(f"目标成功数: {TARGET_COUNT} 个账号 | 注册阶段最大轮次: {MAX_REG_ATTEMPTS}")
logger.info("=" * 65)

reg_success_list = []
reg_failed_list = []

codex_success_list = []
codex_failed_list = []

for attempt in range(1, MAX_REG_ATTEMPTS + 1):
    if len(codex_success_list) >= TARGET_COUNT:
        logger.info(f"🎉 闭环完成：已达成 Codex 凭证目标 {len(codex_success_list)}/{TARGET_COUNT} 个账号！")
        break

    proxy = None
    try:
        sys.path.insert(0, "/app/grok-build-auth/openai_client")
        from register import _resolve_novproxy_one_ip
        proxy = _resolve_novproxy_one_ip(None)
    except Exception as _pe:
        logger.warning(f"[阶段 1: NovProxy] 动态提取异常: {_pe}，降级轮换备用池")
        proxy = PROXIES[(attempt - 1) % len(PROXIES)]

    logger.info("-" * 65)
    logger.info(f">>> [阶段 1: 动态住宅代理] 轮次 {attempt}/{MAX_REG_ATTEMPTS} (已成功: {len(codex_success_list)}/{TARGET_COUNT}) | 代理: {proxy} <<<")
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

        timeout_sec = 320
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

        ok = bool(res_data.get("success")) and bool(res_data.get("cpa_file"))
        email = res_data.get("email") or "unknown"
        if ok and os.path.isfile(res_data.get("cpa_file", "")):
            logger.info(f"★ [阶段 1 注册成功] {email} 耗时: {elapsed:.1f}s | CPA: {res_data.get('cpa_file')}")
            reg_success_list.append({
                "email": email,
                "password": res_data.get("password"),
                "account_id": res_data.get("account_id"),
                "cpa_file": res_data.get("cpa_file"),
                "elapsed": round(elapsed, 1)
            })
            # === 串行闭环：注册成功立即接码（阶段5-6）+ 双端落盘（阶段7）===
            try:
                from core.codex_oauth import run_codex_oauth
                logger.info("-" * 65)
                logger.info(f">>> [阶段 5-6: 接码授权] {email} | 代理: {proxy} <<<")
                logger.info("-" * 65)
                t_codex = time.time()
                c_res = run_codex_oauth(email, force=True, password=res_data.get("password"), proxy=proxy)
                c_el = time.time() - t_codex
                if c_res and c_res.get('ok'):
                    logger.info(f"★ [Codex 成功] {email} 耗时 {c_el:.1f}s | 凭证: {c_res.get('file_path')}")
                    codex_success_list.append({"email": email, "cpa_file": c_res.get('file_path'), "elapsed": round(c_el, 1)})
                    try:
                        _sp.run([sys.executable, '/app/grok-build-auth/openai_client/deliver_codex.py', email],
                                capture_output=True, timeout=120)
                        _sp.run(['bash', '/app/scripts/sync_codex_to_cpa.sh'],
                                capture_output=True, timeout=120)
                        logger.info(f"  > [阶段 7] 已落盘 PostgreSQL + CPA 目录: {email}")
                    except Exception as _de:
                        logger.warning(f"  > [阶段 7] 落盘异常: {_de}")
                else:
                    msg = (c_res or {}).get('message') or 'unknown'
                    logger.warning(f"✕ [Codex 失败] {email} 耗时 {c_el:.1f}s | {msg}")
                    codex_failed_list.append({"email": email, "error": str(msg)[:200]})
            except Exception as _ce:
                logger.error(f"⚠ [接码异常] {email}: {type(_ce).__name__}: {_ce}")
                codex_failed_list.append({"email": email, "error": str(_ce)[:200]})
            finally:
                subprocess.run(["pkill", "-9", "-f", "chrome"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                time.sleep(2)
        else:
            err = res_data.get("error") or f"non_zero_exit_{proc.returncode}"
            logger.warning(f"✕ [阶段 1 未完成] {email} 耗时: {elapsed:.1f}s | 错误: {err}")
            reg_failed_list.append({"email": email, "error": str(err)[:200]})

    except subprocess.TimeoutExpired:
        elapsed = time.time() - t0
        logger.error(f"⏱ [超时] 轮次 {attempt} 超时 > {timeout_sec}s")
        reg_failed_list.append({"email": "timeout", "error": f"timeout > {timeout_sec}s"})
    except Exception as exc:
        elapsed = time.time() - t0
        logger.error(f"⚠ [异常] 轮次 {attempt} 发生异常: {exc}")
        reg_failed_list.append({"email": "exception", "error": str(exc)[:200]})

    subprocess.run(["pkill", "-9", "-f", "chrome"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.run(["pkill", "-9", "-f", "chromium"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(3)

logger.info("\n" + "=" * 65)
logger.info("=== 阶段 1 注册总结 ===")
logger.info(f"成功注册数量: {len(reg_success_list)}/{TARGET_COUNT}")
logger.info(f"失败尝试数量: {len(reg_failed_list)}")
logger.info("=" * 65)

# 阶段 2: 兜底处理注册成功但接码失败的账号
_serial_done = {x.get('email') for x in codex_success_list}

if reg_success_list:
    logger.info("\n" + "=" * 65)
    logger.info(f"=== 启动阶段 2: Codex OAuth 授权与凭证导出 (待处理: {len(reg_success_list)} 个账号) ===")
    logger.info("=" * 65)

    _pending = [x for x in reg_success_list if x.get('email') not in _serial_done]
    for idx, acc in enumerate(_pending, 1):
        email = acc["email"]
        password = acc.get("password") or ""
        logger.info("-" * 65)
        logger.info(f">>> [阶段 2: Codex] 账号 {idx}/{len(reg_success_list)}: {email} <<<")
        logger.info("-" * 65)

        codex_code = f"""
import sys, json, time, logging
logging.basicConfig(level=logging.INFO, stream=sys.stdout, format='%(asctime)s [%(levelname)s] %(message)s')
sys.path.insert(0, '/app/grok-build-auth/openai_client')
from register import _resolve_novproxy_one_ip
from core.codex_oauth import run_codex_oauth

email = {json.dumps(email)}
password = {json.dumps(password)}

proxy = _resolve_novproxy_one_ip(None)
print('[Subprocess] Allocated proxy:', proxy, flush=True)

try:
    res = run_codex_oauth(email, force=True, password=password, proxy=proxy)
    print('RESULT_JSON_START', flush=True)
    print(json.dumps({{
        'success': bool(res.get('ok')),
        'email': email,
        'cpa_file': res.get('file_path'),
        'error': res.get('message')
    }}, ensure_ascii=False), flush=True)
    print('RESULT_JSON_END', flush=True)
except Exception as e:
    print('RESULT_JSON_START', flush=True)
    print(json.dumps({{'success': False, 'email': email, 'error': str(e)}}, ensure_ascii=False), flush=True)
    print('RESULT_JSON_END', flush=True)
"""

        t0 = time.time()
        c_proc = subprocess.Popen(
            [sys.executable, "-u", "-c", codex_code],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1
        )

        c_output = []
        c_res = {}
        c_timeout = 480
        c_deadline = time.time() + c_timeout

        try:
            while True:
                if time.time() > c_deadline:
                    c_proc.kill()
                    raise subprocess.TimeoutExpired(c_proc.args, c_timeout)

                l = c_proc.stdout.readline()
                if not l:
                    if c_proc.poll() is not None:
                        break
                    time.sleep(0.1)
                    continue

                l_str = l.strip()
                c_output.append(l_str)
                if any(k in l_str for k in (
                    "[NovProxy]", "[Cloak]", "[Codex]", "[SMS]", "RESULT_JSON", "[Subprocess]", "Error", "Exception"
                )):
                    logger.info(f"  > {l_str}")

            c_proc.wait(timeout=10)
            c_elapsed = time.time() - t0
            c_out_text = "\n".join(c_output)

            if "RESULT_JSON_START" in c_out_text and "RESULT_JSON_END" in c_out_text:
                part = c_out_text.split("RESULT_JSON_START")[1].split("RESULT_JSON_END")[0]
                try:
                    c_res = json.loads(part.strip())
                except Exception:
                    pass

            if c_res.get("success"):
                logger.info(f"★ [阶段 2 Codex 成功] {email} 耗时: {c_elapsed:.1f}s | CPA 凭证: {c_res.get('cpa_file')}")
                codex_success_list.append({
                    "email": email,
                    "cpa_file": c_res.get("cpa_file"),
                    "elapsed": round(c_elapsed, 1)
                })
            else:
                c_err = c_res.get("error") or f"exit_{c_proc.returncode}"
                logger.warning(f"✕ [阶段 2 Codex 未完成] {email} 耗时: {c_elapsed:.1f}s | 错误: {c_err}")
                codex_failed_list.append({"email": email, "error": str(c_err)[:200]})

        except subprocess.TimeoutExpired:
            c_elapsed = time.time() - t0
            logger.error(f"⏱ [超时] Codex 授权超时 > {c_timeout}s on {email}")
            codex_failed_list.append({"email": email, "error": f"timeout > {c_timeout}s"})
        except Exception as exc:
            c_elapsed = time.time() - t0
            logger.error(f"⚠ [异常] Codex 授权异常: {exc} on {email}")
            codex_failed_list.append({"email": email, "error": str(exc)[:200]})

        subprocess.run(["pkill", "-9", "-f", "chrome"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(["pkill", "-9", "-f", "node"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(5)

logger.info("\n" + "=" * 65)
logger.info("=== 流水线最终执行总结 ===")
logger.info(f"阶段 1 注册成功: {len(reg_success_list)}/{TARGET_COUNT}")
logger.info(f"阶段 2 Codex 授权成功: {len(codex_success_list)}/{len(reg_success_list)}")
logger.info(json.dumps({
    "registration_success": reg_success_list,
    "codex_success": codex_success_list,
    "registration_failed": reg_failed_list,
    "codex_failed": codex_failed_list
}, indent=2, ensure_ascii=False))
logger.info("=" * 65)
