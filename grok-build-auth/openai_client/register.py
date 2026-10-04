# -*- coding: utf-8 -*-
"""
ChatGPT 协议注册执行器 (OpenAI Registration Engine)
从 turb-gpt-free-register 移植并解耦，支持 headless 独立调用与 sidecar 集成。
"""
from __future__ import annotations

import logging
import os
import sys
import time
from pathlib import Path
from typing import Any, Callable

# 确保 openai_client 目录在 sys.path 中以解析 config.* 与 core.*
_CURRENT_DIR = str(Path(__file__).resolve().parent)
if _CURRENT_DIR not in sys.path:
    sys.path.insert(0, _CURRENT_DIR)

from config import email as _email_cfg
from config import openai_protocol as _protocol_cfg
from config import register as _register_cfg
from config import twofa as _twofa_cfg
from config import codex as _codex_cfg
from core.chatgpt_auth import get_csrf_token, get_providers, signin_openai
from core.humanize import delay as human_delay
from core.openai_auth import (
    AccountUnusableError,
    EmailOtpInvalidError,
    build_sentinel_header,
    create_account,
    follow_authorize,
    generate_registration_password,
    navigate_about_you,
    navigate_create_account_password,
    navigate_email_otp_send,
    network_preflight,
    register_user,
    request_password_sentinel_bundle,
    request_sentinel_token,
    send_email_otp,
    validate_email_otp,
)
from core.name_samples import random_display_name
from core.profile_utils import generate_random_birthday
from core.session import BrowserSession
from core.account_export import fetch_session, follow_oauth_callback, setup_2fa

logger = logging.getLogger(__name__)

_FINALIZE_SESSION_MAX_ATTEMPTS = 3
_FINALIZE_SESSION_BACKOFF_BASE = 2.0


def _finalize_registration_session(
    session: BrowserSession,
    continue_url: str,
    email: str,
    callback_referer: str = "https://auth.openai.com/about-you",
) -> tuple[dict[str, Any], str]:
    """
    完成 OAuth 回调并拉取 accessToken。
    """
    if not continue_url:
        raise RuntimeError("create_account 响应缺少 continue_url，无法完成 OAuth 回调")

    last_exc: Exception | None = None
    for attempt in range(1, _FINALIZE_SESSION_MAX_ATTEMPTS + 1):
        try:
            logger.info(
                f"[登录态] 完成 OAuth 回调并拉取 Token：{email} "
                f"(尝试 {attempt}/{_FINALIZE_SESSION_MAX_ATTEMPTS})"
            )
            follow_oauth_callback(session, continue_url, referer=callback_referer)
            human_delay("post_auth")
            session_info = fetch_session(session)
            access_token = session_info.get("accessToken")
            if not access_token:
                raise RuntimeError("session 响应缺少 accessToken")
            logger.info(f"[登录态] 已拿到 accessToken：{email}")
            return session_info, access_token
        except Exception as exc:
            last_exc = exc
            if attempt >= _FINALIZE_SESSION_MAX_ATTEMPTS:
                break
            backoff = _FINALIZE_SESSION_BACKOFF_BASE ** (attempt - 1)
            logger.warning(
                f"[登录态] 回调或拉取 Token 失败：{email}，"
                f"{type(exc).__name__}: {str(exc)[:180]}，{backoff:.1f}s 后重试"
            )
            time.sleep(backoff)

    raise RuntimeError(
        f"OAuth 回调/拉取 Token 重试耗尽：{email}，"
        f"最后错误：{type(last_exc).__name__ if last_exc else 'Unknown'}: {last_exc}"
    ) from last_exc


def run_openai_registration(
    email: str | None = None,
    name: str | None = None,
    birthday: str | None = None,
    password: str | None = None,
    proxy: str | None = None,
    otp_code: str | None = None,
    otp_provider: Callable[[str, float], str] | None = None,
    on_step: Callable[[str, str], None] | None = None,
    check_cancel: Callable[[], None] | None = None,
    enable_codex: bool | None = None,
    enable_twofa: bool | None = None,
) -> dict[str, Any]:
    """
    执行完整的 ChatGPT 纯协议注册流程。

    Args:
        email: 目标邮箱（若为空则从配置的邮箱池或 provider 领取）
        name: 用户显示名（为空则随机生成符合规则的英文名）
        birthday: 生日 YYYY-MM-DD（为空随机生成成年人日期）
        password: 注册密码（为空自动生成强密码）
        proxy: 代理 URL (socks5/http)
        otp_code: 预先传入的验证码（如果有）
        otp_provider: 动态获取验证码函数 fn(email, after_ts) -> code
        on_step: 进度通知回调 fn(status, message)
        check_cancel: 检查是否取消任务 fn() -> raises Exception if cancelled
        enable_codex: 是否开启 Codex OAuth 授权提取
        enable_twofa: 是否配置 2FA TOTP

    Returns:
        dict 包含:
            success: bool
            email: str
            access_token: str
            session_info: dict
            cookies: dict
            password: str
            totp_secret: str | None
            codex: dict
            error: str | None
    """
    def _step(status: str, msg: str):
        logger.info(f"[{status}] {msg}")
        if on_step:
            try:
                on_step(status, msg)
            except Exception:
                pass

    def _cancel():
        if check_cancel:
            check_cancel()

    # 1. 准备注册参数
    if not email:
        if _email_cfg.USE_EMAIL_SERVICE:
            from core.email_provider import acquire_email
            email = acquire_email()
            _step("email_acquired", f"已从邮箱池获取邮箱: {email}")
        else:
            raise RuntimeError("未提供邮箱且未开启邮箱池服务")

    if not name:
        name = random_display_name()
    if not birthday:
        birthday = generate_random_birthday()
    if not password:
        password = generate_registration_password()

    if enable_codex is None:
        enable_codex = bool(getattr(_codex_cfg, "ENABLE_CODEX_AUTO", True))
    if enable_twofa is None:
        enable_twofa = bool(getattr(_twofa_cfg, "ENABLE_2FA", False))

    fingerprint_seed = None
    if bool(getattr(_register_cfg, "PROTOCOL_REUSE_FINGERPRINT_BY_EMAIL", False)):
        fingerprint_seed = f"registration:{str(email).strip().lower()}"

    session = BrowserSession(
        proxy=proxy,
        fingerprint_seed=fingerprint_seed,
    )

    proxy_label = "直连"
    if session.proxy:
        proxy_label = session.proxy.split("@")[-1] if "@" in session.proxy else session.proxy

    _step("starting", f"开始 ChatGPT 协议注册: {email} (代理: {proxy_label})")
    _cancel()

    create_acknowledged = False
    access_token = ""
    session_info: dict[str, Any] = {}
    totp_secret = None
    codex_result: dict[str, Any] = {"status": "skipped", "ok": False, "message": "未触发"}

    try:
        # 网络预检
        _step("preflight", "执行网络与 TLS 预检")
        network_preflight(session)
        human_delay("navigate")
        _cancel()

        # 匿名预热
        if getattr(_protocol_cfg, "CHATGPT_ANON_BOOTSTRAP_ENABLED", True):
            _step("bootstrap", "执行匿名状态首页预热")
            from core.chatgpt_bootstrap import anonymous_bootstrap
            anonymous_bootstrap(
                session,
                strict=bool(getattr(_protocol_cfg, "CHATGPT_BOOTSTRAP_STRICT", False)),
            )
            human_delay("navigate")
            _cancel()

        # 阶段 1: ChatGPT 认证初始化
        _step("auth_init", "获取 providers 与 CSRF")
        get_providers(session)
        human_delay("api")
        csrf_token = get_csrf_token(session)
        human_delay("api")
        _cancel()

        _step("signin", "发起 OAuth Signin")
        authorize_url = signin_openai(session, csrf_token, email)
        human_delay("api")
        _cancel()

        # 阶段 2: OpenAI 认证流
        _step("authorize", "跟随 Authorize 建立会话")
        authorize_final_url = follow_authorize(session, authorize_url)
        human_delay("navigate")
        _cancel()

        _step("password_page", "导航到密码注册页")
        navigate_create_account_password(session, authorize_final_url)
        human_delay("navigate")
        _cancel()

        _step("sentinel", "计算 Sentinel Token 挑战")
        password_sentinel = request_password_sentinel_bundle(session)
        password_sentinel_header, password_so_header = build_sentinel_header(
            session, password_sentinel, "username_password_create"
        )
        human_delay("challenge")
        _cancel()

        _step("register_user", "提交账号邮箱与密码")
        register_result = register_user(
            session,
            email,
            password,
            password_sentinel_header,
            password_so_header,
        )
        create_acknowledged = True
        _cancel()

        # 发送邮箱验证码
        _step("otp_send", "请求发送邮箱验证码")
        otp_after_ts = time.time()
        navigate_email_otp_send(session, register_result.get("continue_url"))
        human_delay("navigate")
        _cancel()

        # 阶段 3: 获取并验证邮箱验证码
        validate_result = None
        max_otp_attempts = 3
        current_otp = otp_code

        for otp_attempt in range(1, max_otp_attempts + 1):
            _cancel()
            if not current_otp:
                _step("waiting_otp", f"等待接收 6 位验证码 (第 {otp_attempt}/{max_otp_attempts} 次)")
                if otp_provider:
                    current_otp = otp_provider(email, otp_after_ts)
                elif _email_cfg.USE_EMAIL_SERVICE:
                    from core.email_provider import wait_for_otp
                    current_otp = wait_for_otp(email, after_ts=otp_after_ts)
                else:
                    raise RuntimeError("未提供验证码获取函数且未配置邮箱服务")

            if not current_otp:
                raise RuntimeError(f"未获取到邮箱验证码: {email}")

            _step("validating_otp", f"提交验证码: {current_otp}")
            human_delay("otp_input")
            try:
                sentinel_header_9 = None
                so_header_9 = None
                if getattr(_protocol_cfg, "SEND_SENTINEL_ON_EMAIL_OTP_VALIDATE", False):
                    sentinel_resp_9 = request_sentinel_token(session, "authorize_continue")
                    sentinel_header_9, so_header_9 = build_sentinel_header(session, sentinel_resp_9, "authorize_continue")
                    human_delay("challenge")

                validate_result = validate_email_otp(session, current_otp, sentinel_header_9, so_header_9)
                break
            except EmailOtpInvalidError as exc:
                if otp_attempt >= max_otp_attempts:
                    raise
                logger.warning(f"[OTP] 验证码错误: {exc}，重新发送中...")
                otp_after_ts = time.time()
                send_email_otp(session)
                human_delay("api")
                current_otp = None

        if not validate_result:
            raise RuntimeError("邮箱验证码校验未完成")

        _cancel()
        human_delay("api")

        page = validate_result.get("page") if isinstance(validate_result, dict) else {}
        page = page if isinstance(page, dict) else {}
        page_type = str(page.get("type") or "")
        otp_continue_url = (
            validate_result.get("continue_url")
            or validate_result.get("external_url")
            or validate_result.get("url")
            or page.get("continue_url")
            or page.get("external_url")
            or page.get("url")
        )

        otp_continue_text = str(otp_continue_url or "")
        direct_oauth = bool(
            otp_continue_text
            and "about-you" not in otp_continue_text
            and (
                "chatgpt.com/api/auth/callback" in otp_continue_text
                or "auth.openai.com/authorize/continue" in otp_continue_text
                or page_type == "external_url"
            )
        )

        if page_type == "external_url" or direct_oauth:
            _step("oauth_callback", "OTP 验证后直接进入 OAuth 回调")
            session_info, access_token = _finalize_registration_session(
                session,
                otp_continue_url,
                email,
                callback_referer="https://auth.openai.com/email-verification",
            )
        else:
            about_url = str(otp_continue_url) if otp_continue_url and "about-you" in str(otp_continue_url) else None
            _step("about_you", "提交个人资料 (姓名/生日)")
            navigate_about_you(session, about_url)
            human_delay("navigate")
            _cancel()

            sentinel_resp_11 = request_sentinel_token(session, "oauth_create_account")
            sentinel_header_11, so_header_11 = build_sentinel_header(session, sentinel_resp_11, "oauth_create_account")
            human_delay("challenge")

            create_result = create_account(session, name, birthday, sentinel_header_11, so_header_11)
            continue_url = create_result.get("continue_url")
            if not continue_url:
                raise RuntimeError(f"create_account 响应缺少 continue_url: {create_result}")

            _step("oauth_callback", "完成 OAuth 回调并提取登录态")
            session_info, access_token = _finalize_registration_session(session, continue_url, email)

        if getattr(_protocol_cfg, "CHATGPT_AUTH_BOOTSTRAP_ENABLED", True):
            try:
                from core.chatgpt_bootstrap import authenticated_bootstrap
                authenticated_bootstrap(
                    session,
                    access_token,
                    strict=bool(getattr(_protocol_cfg, "CHATGPT_BOOTSTRAP_STRICT", False)),
                )
            except Exception as e:
                logger.warning(f"认证后预热跳过: {e}")

        _cancel()

        # 阶段 4: 可选配置 2FA
        if enable_twofa:
            _step("setup_2fa", "配置 TOTP 2FA")
            try:
                totp_secret = setup_2fa(session, email)
                _step("setup_2fa", f"2FA 配置成功, secret={totp_secret[:8]}...")
            except Exception as exc:
                logger.warning(f"2FA 设置失败(非阻断): {exc}")

        # 阶段 5: 可选 Codex OAuth 授权
        if enable_codex:
            _step("codex_oauth", "执行 Codex OAuth 自动授权")
            try:
                from core.codex_oauth import run_codex_oauth
                codex_result = run_codex_oauth(email, proxy=proxy, force=True)
                if codex_result.get("ok"):
                    _step("codex_oauth", f"Codex OAuth 成功: {codex_result.get('file_path')}")
                else:
                    _step("codex_oauth", f"Codex OAuth 结果: {codex_result.get('status')} - {codex_result.get('message')}")
            except Exception as exc:
                codex_result = {
                    "status": "failed",
                    "ok": False,
                    "message": f"{type(exc).__name__}: {str(exc)[:180]}",
                }
                logger.warning(f"Codex OAuth 异常(非阻断): {exc}")

        # 提取 cookies
        cookies_dict: dict[str, str] = {}
        try:
            for c in session.cookies:
                cookies_dict[c.name] = c.value
        except Exception:
            pass

        # 同时为 ChatGPT 会话落盘一份标准 CPA 凭证文件，确保随时可导出/对接
        cpa_chatgpt_file = ""
        try:
            import json
            cpa_dir = Path("/app/data/cpa_auth_files")
            if not cpa_dir.exists():
                cpa_dir = Path(__file__).resolve().parents[3] / "data" / "cpa_auth_files"
            cpa_dir.mkdir(parents=True, exist_ok=True)
            chatgpt_cpa_record = {
                "access_token": access_token,
                "email": email,
                "type": "chatgpt",
                "account_id": (session_info.get("account") or {}).get("id") or "",
                "user_id": (session_info.get("user") or {}).get("id") or "",
                "plan_type": (session_info.get("account") or {}).get("planType") or "free",
                "expired": session_info.get("expires") or "",
                "last_refresh": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "password": password,
                "totp_secret": totp_secret,
                "cookies": cookies_dict,
            }
            chatgpt_file_path = cpa_dir / f"chatgpt-{email}.json"
            chatgpt_file_path.write_text(json.dumps(chatgpt_cpa_record, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
            cpa_chatgpt_file = str(chatgpt_file_path)
            logger.info(f"[CPA] 已生成 ChatGPT CPA 凭证文件: {cpa_chatgpt_file}")
        except Exception as e:
            logger.warning(f"[CPA] 保存 ChatGPT 凭证文件异常: {e}")

        cpa_primary_file = (
            (codex_result.get("file_path") if isinstance(codex_result, dict) else None)
            or cpa_chatgpt_file
        )

        _step("success", f"注册成功完成: {email}")

        return {
            "success": True,
            "email": email,
            "password": password,
            "access_token": access_token,
            "session_info": session_info,
            "cookies": cookies_dict,
            "totp_secret": totp_secret,
            "codex": codex_result,
            "cpa_file": cpa_primary_file,
            "error": None,
        }

    except Exception as exc:
        err_msg = str(exc)
        logger.error(f"[注册失败] {email}: {err_msg}", exc_info=True)
        return {
            "success": False,
            "email": email,
            "password": password,
            "access_token": "",
            "session_info": {},
            "cookies": {},
            "totp_secret": None,
            "codex": codex_result,
            "error": err_msg,
            "create_acknowledged": create_acknowledged,
        }
