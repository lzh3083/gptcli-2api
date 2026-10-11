# -*- coding: utf-8 -*-
"""通过 CloakBrowser + Playwright 适配层执行 ChatGPT 注册。"""
from __future__ import annotations

import json
import logging
import os
import time
from pathlib import Path
from typing import Callable

from config import cloakbrowser as _cfg
from config import twofa as _twofa_cfg
from core.account_export import save_account_data, post_register_dwell
from core.browser_data_saver import BrowserDataSaver
from core.browser_traffic import PlaywrightTrafficTracker
from core.cloakbrowser_driver import build_cloak_driver
from core.email_provider import acquire_email_after_input, wait_for_otp, resolve_email_source
from core.humanize import delay as human_delay

# 复用 Roxy 注册流程里已维护好的页面操作函数。
from core.roxy_registration import (  # noqa: F401
    _maybe_accept, _submit_email_and_wait_next, _fill_password_page_if_present,
    _clear_otp_inputs, _type_otp, _click_continue, _wait_after_email_otp_submit,
    _click_resend_email_otp, _complete_profile_page, _fetch_chatgpt_session, _check_manual_stop,
)

logger = logging.getLogger(__name__)


def run_cloak_registration(
    email: str | None,
    name: str,
    birthday: str,
    proxy: str = None,
    otp_code: str = None,
    batch_dir: Path | None = None,
    on_email_acquired: Callable[[str], None] | None = None,
    password: str | None = None,
    otp_provider: Callable[[str, float], str] | None = None,
    on_step: Callable[[str, str], None] | None = None,
    check_cancel: Callable[[], None] | None = None,
    enable_codex: bool | None = None,
) -> dict:
    """CloakBrowser 自动化注册入口。"""
    def _step(status: str, msg: str) -> None:
        logger.info("[Cloak注册][%s] %s", status, msg)
        if on_step:
            try:
                on_step(status, msg)
            except Exception:
                pass

    def _cancel() -> None:
        _check_manual_stop()
        if check_cancel:
            check_cancel()

    effective_otp_provider = otp_provider or wait_for_otp
    driver = None
    opened = None
    create_acknowledged = False
    openai_password: str | None = None
    traffic_tracker: PlaywrightTrafficTracker | None = None
    data_saver: BrowserDataSaver | None = None
    network_traffic: dict | None = None
    try:
        _step("browser_launch", f"启动 CloakBrowser 真实反检测浏览器: {email or '待分配'}")
        driver, opened = build_cloak_driver(proxy=proxy)
        effective_proxy = ((opened.raw or {}).get("proxy_pool_target") if opened else None) or proxy or None
        try:
            traffic_tracker = PlaywrightTrafficTracker(driver.context, label="Cloak")
        except Exception as exc:
            # 统计失败不应影响注册主流程。
            logger.warning("[Cloak注册] 初始化浏览器流量统计失败，继续注册：%s: %s", type(exc).__name__, str(exc)[:180])
        data_saver = BrowserDataSaver(label="Cloak")
        if traffic_tracker is not None:
            traffic_tracker.attach_data_saver(data_saver)
        data_saver.install_playwright(driver.context)
        logger.info("[Cloak注册] 开始：%s，profile=%s，proxy=%s", email, opened.profile_id, effective_proxy)

        otp_after_ts = time.time()
        _step("navigate_login", "CloakBrowser 打开 https://chatgpt.com/auth/login")
        driver.get("https://chatgpt.com/auth/login")
        human_delay("navigate")
        _maybe_accept(driver)
        _cancel()

        def _email_supplier_after_input() -> str:
            nonlocal email
            _cancel()
            email = acquire_email_after_input(email)
            if on_email_acquired:
                on_email_acquired(email)
            return email

        _step("submit_email", f"填写并提交注册邮箱: {email or '自动领取'}")
        next_state = _submit_email_and_wait_next(
            driver,
            email,
            attempts=3,
            email_supplier=_email_supplier_after_input,
        )
        _cancel()

        # 如果邮箱提交后直接进入验证码页，也尝试点击“使用密码继续”进入密码创建页；
        # _fill_password_page_if_present 会在设置成功后返回本次 OpenAI 注册密码。
        _step("password_step", f"处理密码设置页 (state={next_state})")
        openai_password = _fill_password_page_if_present(driver, email, timeout=25)
        effective_password = str(openai_password or password or "").strip()
        if effective_password:
            create_acknowledged = True
        _cancel()

        current_otp = otp_code
        max_otp_attempts = 3
        for otp_attempt in range(1, max_otp_attempts + 1):
            _cancel()
            if current_otp is None:
                _step("waiting_otp", f"等待接收 6 位邮箱验证码 ({email}, 第 {otp_attempt}/{max_otp_attempts} 次)")
                try:
                    current_otp = effective_otp_provider(email, after_ts=otp_after_ts)
                except TypeError:
                    current_otp = effective_otp_provider(email, otp_after_ts)
                except Exception as exc:
                    if otp_attempt >= max_otp_attempts:
                        raise
                    logger.warning(
                        "[Cloak注册][OTP] 一直未收到验证码，点击“重新发送电子邮件”后继续等待（下一轮 %s/%s）：%s: %s",
                        otp_attempt + 1,
                        max_otp_attempts,
                        type(exc).__name__,
                        str(exc)[:180],
                    )
                    otp_after_ts = time.time()
                    _click_resend_email_otp(driver, timeout=25)
                    human_delay("api")
                    current_otp = None
                    continue
            _step("validating_otp", f"CloakBrowser 填写并提交验证码: {current_otp}")
            _clear_otp_inputs(driver)
            _type_otp(driver, current_otp)
            human_delay("otp_input")
            try:
                _click_continue(driver)
            except Exception as exc:
                logger.info("[Cloak注册][OTP] 未找到显式提交按钮，继续等待页面状态：%s", str(exc)[:120])

            outcome = _wait_after_email_otp_submit(driver, timeout=10)
            if outcome == "accepted":
                create_acknowledged = True
                break
            if otp_attempt >= max_otp_attempts:
                raise RuntimeError("邮箱验证码连续错误/过期，已达到最大重试次数")
            otp_after_ts = time.time()
            _click_resend_email_otp(driver, timeout=25)
            human_delay("api")
            current_otp = None

        _step("about_you", f"CloakBrowser 填写个人资料 ({name}, {birthday})")
        profile_submitted = _complete_profile_page(driver, name, birthday, timeout=60)
        if profile_submitted:
            create_acknowledged = True
            human_delay("post_auth")

        _step("fetch_session", "CloakBrowser 提取 ChatGPT 登录 Session 与 accessToken")
        session_info = _fetch_chatgpt_session(driver, timeout=120)
        access_token = session_info["accessToken"]
        logger.info("[Cloak注册] 已拿到 accessToken：%s", email)

        cookies_dict = driver.get_cookies_dict() if hasattr(driver, "get_cookies_dict") else {}

        if _twofa_cfg.ENABLE_2FA:
            logger.warning("[Cloak注册] 当前 CloakBrowser 自动化路径暂不执行 2FA 设置，已跳过")
        totp_secret = None

        cpa_chatgpt_file = ""
        try:
            data_root = os.environ.get("GROK2API_DATA_DIR") or (
                "/app/data" if Path("/app/data").is_dir() else str(Path(__file__).resolve().parents[3] / "data")
            )
            cpa_dir = Path(data_root) / "cpa_auth_files"
            cpa_dir.mkdir(parents=True, exist_ok=True)
            cpa_proxy_url = os.environ.get("CODEX_CPA_PROXY_URL", "socks5h://warp-relay:1080").strip()
            chatgpt_cpa_record = {
                "access_token": access_token,
                "email": email,
                "type": "chatgpt",
                "account_id": (session_info.get("account") or {}).get("id") or "",
                "user_id": (session_info.get("user") or {}).get("id") or "",
                "plan_type": (session_info.get("account") or {}).get("planType") or "free",
                "expired": session_info.get("expires") or "",
                "last_refresh": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "password": effective_password,
                "totp_secret": totp_secret,
                "cookies": cookies_dict,
            }
            if cpa_proxy_url:
                chatgpt_cpa_record["proxy_url"] = cpa_proxy_url
            chatgpt_file_path = cpa_dir / f"chatgpt-{email}.json"
            chatgpt_file_path.write_text(json.dumps(chatgpt_cpa_record, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
            cpa_chatgpt_file = str(chatgpt_file_path)
            logger.info("[Cloak注册][CPA] 已生成 ChatGPT CPA 凭证文件: %s", cpa_chatgpt_file)
        except Exception as e:
            logger.warning("[Cloak注册][CPA] 保存 ChatGPT 凭证文件异常: %s", e)

        codex_result = {
            "status": "skipped",
            "ok": True,
            "message": "ENABLE_CODEX_AUTO=False，跳过 Codex",
        }
        try:
            from config import codex as _codex_cfg
            should_run_codex = bool(getattr(_codex_cfg, "ENABLE_CODEX_AUTO", True)) if enable_codex is None else bool(enable_codex)
            if should_run_codex:
                from core.roxy_codex_oauth import run_roxy_codex_oauth
                _step("codex_oauth", "复用当前 CloakBrowser 窗口执行 Codex OAuth 授权与手机验证")
                _cancel()
                codex_result = run_roxy_codex_oauth(
                    email,
                    otp_provider=effective_otp_provider,
                    proxy=effective_proxy,
                    reuse_existing_profile=True,
                    existing_driver=driver,
                    existing_opened=opened,
                    force=True,
                    clear_existing_state=True,
                    password=effective_password,
                )
            else:
                logger.info("[Cloak注册][Codex] ENABLE_CODEX_AUTO=False，注册后跳过 Codex OAuth")
        except Exception as exc:
            codex_result = {"status": "failed", "ok": False, "message": f"{type(exc).__name__}: {str(exc)[:180]}"}

        # 统计注册浏览器关闭前的完整会话；注册后停留期间的网络请求也计入。
        post_register_dwell(email, label="Cloak注册")
        if traffic_tracker is not None:
            network_traffic = traffic_tracker.stop()
        if data_saver is not None:
            data_saver.stop()
        account_id = save_account_data(
            email=email,
            access_token=access_token,
            totp_secret=totp_secret,
            email_source=resolve_email_source(email),
            proxy_used=effective_proxy,
            batch_dir=batch_dir,
            extra={
                "user": session_info.get("user"),
                "account": session_info.get("account"),
                "expires": session_info.get("expires"),
                "cloakbrowser": {"profile_id": opened.profile_id, "open_result": opened.raw},
                "registration_password": effective_password,
                "codex": codex_result,
                "network_traffic": network_traffic,
            },
        )
        cpa_primary_file = (
            (codex_result.get("file_path") if isinstance(codex_result, dict) else None)
            or cpa_chatgpt_file
        )
        codex_ok = codex_result.get("ok") or codex_result.get("status") == "skipped"
        if codex_ok:
            _step("success", f"CloakBrowser 真实浏览器注册与 Codex 授权成功: {email}")
        return {
            "success": bool(codex_ok),
            "email": email,
            "password": effective_password,
            "account_id": account_id,
            "access_token": access_token,
            "session_info": session_info,
            "cookies": cookies_dict,
            "totp_secret": totp_secret,
            "codex": codex_result,
            "cpa_file": cpa_primary_file,
            "network_traffic": network_traffic,
            "error": None if codex_ok else f"Codex 未完成: {codex_result.get('message')}",
        }
    except Exception as exc:
        if traffic_tracker is not None:
            try:
                network_traffic = traffic_tracker.stop()
            except Exception:
                pass
        if data_saver is not None:
            data_saver.stop()
        logger.error("[Cloak注册] 失败：%s: %s", type(exc).__name__, exc)
        logger.debug("[Cloak注册] 失败详情", exc_info=True)
        try:
            if email:
                from core.email_provider import release_email
                release_email(email, status="failed" if create_acknowledged else "available", note=f"Cloak注册失败: {str(exc)[:180]}")
        except Exception:
            pass
        return {
            "success": False,
            "email": email,
            "password": str(openai_password or password or ""),
            "access_token": "",
            "session_info": {},
            "cookies": {},
            "totp_secret": None,
            "codex": {"status": "failed", "ok": False, "message": str(exc)[:180]},
            "network_traffic": network_traffic,
            "create_acknowledged": create_acknowledged,
            "error": f"{type(exc).__name__}: {str(exc)[:300]}",
        }
    finally:
        if traffic_tracker is not None:
            try:
                traffic_tracker.stop()
            except Exception:
                pass
        if data_saver is not None:
            data_saver.stop()
        if driver and not bool(_cfg.CLOAK_KEEP_BROWSER_OPEN):
            try:
                driver.quit()
            except Exception:
                pass
