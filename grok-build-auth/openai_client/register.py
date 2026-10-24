# -*- coding: utf-8 -*-
"""
ChatGPT 协议注册执行器 (OpenAI Registration Engine)
从 turb-gpt-free-register 移植并解耦，支持 headless 独立调用与 sidecar 集成。
"""
from __future__ import annotations

import logging
import os
import subprocess
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
_USED_NOVPROXY_EXIT_IPS: set[str] = set()
_USED_NOVPROXY_NODES: set[str] = set()


def _resolve_dataimpulse_one_ip(
    proxy: str | None = None,
    step_cb: Callable[[str, str], None] | None = None,
    cfg_data: dict[str, Any] | None = None,
) -> str | None:
    """
    若未传入具体代理 URL 或传入 dataimpulse/residential 哨兵词，自动从 DataImpulse 提取并验证
    一个未使用过的独立住宅 IP（一号一IP，校验 hosting=False）。
    """
    raw = str(proxy or "").strip()
    if raw.lower() in ("direct", "none", "off", "0"):
        return None
    if raw and "://" in raw and not any(k in raw.lower() for k in ("dataimpulse", "novproxy", "residential", "dynamic")):
        return raw

    if cfg_data is None:
        cfg_data = {}
        try:
            import json
            for cand_path in ("/app/data/config.json", str(Path(__file__).resolve().parents[2] / "data" / "config.json")):
                if Path(cand_path).is_file():
                    cfg_data = json.loads(Path(cand_path).read_text(encoding="utf-8")) or {}
                    break
        except Exception:
            cfg_data = {}

    user = str(
        cfg_data.get("dataimpulse_user")
        or os.environ.get("DATAIMPULSE_USER")
        or ""
    ).strip()
    password = str(
        cfg_data.get("dataimpulse_pass")
        or os.environ.get("DATAIMPULSE_PASS")
        or ""
    ).strip()
    country = str(
        cfg_data.get("dataimpulse_country")
        or os.environ.get("DATAIMPULSE_COUNTRY")
        or cfg_data.get("novproxy_region")
        or os.environ.get("NOVPROXY_REGION")
        or "JP"
    ).strip().upper()

    if "-" in raw and raw.lower().startswith("dataimpulse-"):
        country = raw.split("-", 1)[1].strip().upper() or country
    if country in ("AUTO", "*", "ANY", "ALL", ""):
        country = "JP"

    ttl = int(cfg_data.get("dataimpulse_ttl") or os.environ.get("DATAIMPULSE_TTL") or 30)
    api_base = str(
        cfg_data.get("dataimpulse_api")
        or os.environ.get("DATAIMPULSE_API")
        or "https://gw.dataimpulse.com:777/api/list"
    ).strip()

    repo_root = str(Path(__file__).resolve().parents[2])
    if repo_root not in sys.path:
        sys.path.insert(0, repo_root)
    from grok2api.upstream.browser_register import dataimpulse

    data_root = os.environ.get("GROK2API_DATA_DIR") or (
        "/app/data" if Path("/app/data").is_dir() else str(Path(__file__).resolve().parents[2] / "data")
    )
    return dataimpulse.resolve_one_ip(
        user=user,
        password=password,
        country=country,
        session_ttl=ttl,
        api_base=api_base,
        data_dir=data_root,
        step_cb=step_cb,
    )


def _resolve_novproxy_one_ip(
    proxy: str | None,
    step_cb: Callable[[str, str], None] | None = None,
) -> str | None:
    """
    若未传入具体代理 URL 或传入 novproxy/residential 哨兵词，自动从 NovProxy 提取并验证
    一个未使用过的独立住宅 IP（一号一IP，校验 hosting=False）。
    """
    raw = str(proxy or "").strip()
    if raw.lower() in ("direct", "none", "off", "0"):
        return None
    if raw and "://" in raw and not any(k in raw.lower() for k in ("novproxy", "residential", "dynamic")):
        return raw

    # 读取 data/config.json 中的 NovProxy 配置（若存在）
    cfg_data: dict[str, Any] = {}
    try:
        import json
        for cand_path in ("/app/data/config.json", str(Path(__file__).resolve().parents[2] / "data" / "config.json")):
            if Path(cand_path).is_file():
                cfg_data = json.loads(Path(cand_path).read_text(encoding="utf-8")) or {}
                break
    except Exception:
        cfg_data = {}

    proxy_mode = str(
        cfg_data.get("proxy_mode")
        or os.environ.get("PROXY_MODE")
        or os.environ.get("NOVPROXY_MODE")
        or "novproxy"
    ).strip().lower()

    # 若指定或配置了 DataImpulse，分流至 DataImpulse 动态分配流程
    if (
        proxy_mode in ("dataimpulse", "data_impulse")
        or (raw and "dataimpulse" in raw.lower())
        or bool(os.environ.get("DATAIMPULSE_USER") or cfg_data.get("dataimpulse_user"))
    ):
        return _resolve_dataimpulse_one_ip(proxy, step_cb=step_cb, cfg_data=cfg_data)

    if not raw and proxy_mode in ("direct", "none", "off"):
        return None

    api_base = str(
        cfg_data.get("novproxy_api")
        or os.environ.get("NOVPROXY_API")
        or "https://white.novproxy.com/white/api"
    ).strip()
    region = str(
        cfg_data.get("novproxy_region")
        or os.environ.get("NOVPROXY_REGION")
        or "SG"
    ).strip().upper()
    if "-" in raw and raw.lower().startswith("novproxy-"):
        region = raw.split("-", 1)[1].strip().upper() or region
    if region in ("AUTO", "*", "ANY", "ALL", ""):
        region = "SG"
    minutes = int(cfg_data.get("novproxy_minutes") or os.environ.get("NOVPROXY_MINUTES") or 120)

    # 优先引入项目内置的 novproxy 模块
    repo_root = str(Path(__file__).resolve().parents[2])
    if repo_root not in sys.path:
        sys.path.insert(0, repo_root)
    from grok2api.upstream.browser_register import novproxy

    if step_cb:
        step_cb("proxy_extract", f"正在从 NovProxy 提取一号一IP独立住宅代理 ({region}, {minutes}min)...")

    data_root = os.environ.get("GROK2API_DATA_DIR") or (
        "/app/data" if Path("/app/data").is_dir() else str(Path(__file__).resolve().parents[2] / "data")
    )
    used_ip_file = Path(data_root) / "used_novproxy_ips.json"
    try:
        if used_ip_file.is_file():
            persisted = json.loads(used_ip_file.read_text(encoding="utf-8") or "{}")
            for ip_item in persisted.get("exit_ips") or []:
                if ip_item:
                    _USED_NOVPROXY_EXIT_IPS.add(str(ip_item).strip())
            for nd_item in persisted.get("nodes") or []:
                if nd_item:
                    _USED_NOVPROXY_NODES.add(str(nd_item).strip())
    except Exception:
        pass

    last_err = ""
    fallback_candidate: tuple[str, str, dict[str, Any]] | None = None
    for round_idx in range(1, 5):
        try:
            nodes = novproxy.fetch_nodes(
                api_base=api_base,
                region=region,
                num=8,
                minutes=minutes,
                attempts=2,
                timeout=15.0,
                log=lambda m: logger.info(f"[NovProxy] {m}"),
            )
            for node in nodes:
                clean_node = node.split("://", 1)[-1].strip()
                probe = novproxy.probe_node(clean_node, expect_country=None, timeout=15.0)
                if not probe.get("ok"):
                    last_err = str(probe.get("reason") or "probe_failed")
                    continue
                code = str(probe.get("country") or "").upper()
                allowed_countries = {
                    "US", "GB", "DE", "FR", "JP", "SG", "TW", "KR", "BR", "ES",
                    "IT", "NL", "AU", "CA", "SE", "NO", "DK", "FI", "CH", "AT",
                    "IE", "NZ", "PT"
                }
                if code not in allowed_countries:
                    last_err = f"节点出口国家不在高质量支持列表 ({code})"
                    continue
                if probe.get("hosting"):
                    last_err = f"节点出口为机房 IP ({probe.get('exit_ip')} / {probe.get('isp')})"
                    logger.warning(f"[NovProxy] 跳过机房属性节点 {clean_node}: {last_err}")
                    continue
                # 预检节点与 ip-api 的可用性（无需直接 curl OpenAI，因为由指纹浏览器 CloakBrowser 处理交互）
                pass
                exit_ip = str(probe.get("exit_ip") or "").strip()
                if fallback_candidate is None:
                    fallback_candidate = (clean_node, exit_ip, probe)
                if clean_node in _USED_NOVPROXY_NODES:
                    last_err = f"节点端口已使用 ({clean_node})"
                    continue
                if exit_ip and exit_ip in _USED_NOVPROXY_EXIT_IPS:
                    last_err = f"出口 IP {exit_ip} 已在历史记录中"
                    logger.info(f"[NovProxy] 跳过已使用过的出口 IP {exit_ip} (优先寻找全新住宅IP)")
                    continue
                _USED_NOVPROXY_NODES.add(clean_node)
                if exit_ip:
                    _USED_NOVPROXY_EXIT_IPS.add(exit_ip)
                try:
                    used_ip_file.parent.mkdir(parents=True, exist_ok=True)
                    used_ip_file.write_text(
                        json.dumps(
                            {
                                "exit_ips": list(_USED_NOVPROXY_EXIT_IPS)[-200:],
                                "nodes": list(_USED_NOVPROXY_NODES)[-200:],
                            },
                            ensure_ascii=False,
                            indent=2,
                        )
                        + "\n",
                        encoding="utf-8",
                    )
                except Exception:
                    pass
                assigned = f"socks5h://{clean_node}"
                msg = (
                    f"已分配 NovProxy 一号一IP 住宅节点: {assigned} "
                    f"(出口IP={exit_ip}, 国家={probe.get('country')}, ISP={probe.get('isp')})"
                )
                if step_cb:
                    step_cb("proxy_assigned", msg)
                else:
                    logger.info(f"[NovProxy] {msg}")
                return assigned
        except Exception as exc:
            last_err = str(exc)
            logger.warning(f"[NovProxy] 第 {round_idx} 轮提取异常: {exc}")
        time.sleep(1.5)

    if fallback_candidate is not None:
        clean_node, exit_ip, probe = fallback_candidate
        _USED_NOVPROXY_NODES.add(clean_node)
        assigned = f"socks5h://{clean_node}"
        msg = (
            f"已分配 NovProxy 住宅节点(池内复用): {assigned} "
            f"(出口IP={exit_ip}, 国家={probe.get('country')}, ISP={probe.get('isp')})"
        )
        if step_cb:
            step_cb("proxy_assigned", msg)
        else:
            logger.info(f"[NovProxy] {msg}")
        return assigned

    raise RuntimeError(f"无法从 NovProxy 获取可用的独立住宅代理节点 (region={region}): {last_err}")


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
            from core.codex_oauth import _do_phone_verification, _extract_continue_url, _is_phone_step
            if _is_phone_step({"url": continue_url}):
                logger.info(f"[登录态] 注册回调前命中手机验证步，开始接码验证：{email}")
                phone_res = _do_phone_verification(session, proactive=False)
                continue_url = _extract_continue_url(phone_res) or continue_url
                callback_referer = "https://auth.openai.com/phone-verification"

            landed_url = follow_oauth_callback(session, continue_url, referer=callback_referer)
            if _is_phone_step({"url": landed_url}):
                logger.info(f"[登录态] OAuth 回调重定向落入手机验证页 ({landed_url})，开始接码验证：{email}")
                phone_res = _do_phone_verification(session, proactive=False)
                phone_continue = _extract_continue_url(phone_res)
                if phone_continue:
                    continue_url = phone_continue
                    landed_url = follow_oauth_callback(
                        session, continue_url, referer="https://auth.openai.com/phone-verification"
                    )
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

    # 0. 优先提取并校验 NovProxy 一号一IP 独立住宅代理（避免代理异常时锁死邮箱）
    proxy = _resolve_novproxy_one_ip(proxy, step_cb=_step)

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

    # 默认使用 CloakBrowser 真实反检测浏览器驱动（彻底替代容易触发 6~10 分钟异步封号的纯协议 Node VM 模拟模式）
    from config import roxybrowser as _roxy_cfg
    reg_driver = str(
        os.environ.get("REGISTRATION_DRIVER")
        or getattr(_roxy_cfg, "REGISTRATION_DRIVER", "cloak")
        or "cloak"
    ).strip().lower()

    if reg_driver in ("cloak", "cloakbrowser", "browser", "roxy"):
        from core.cloakbrowser_registration import run_cloak_registration
        proxy_label = (proxy.split("@")[-1] if "@" in proxy else proxy) if proxy else "直连"
        _step("starting", f"开始 CloakBrowser 真实浏览器注册: {email or '待分配'} (代理: {proxy_label})")
        return run_cloak_registration(
            email=email,
            name=name,
            birthday=birthday,
            proxy=proxy,
            otp_code=otp_code,
            on_email_acquired=lambda acq: _step("email_acquired", f"已从邮箱池获取邮箱: {acq}"),
            password=password,
            otp_provider=otp_provider,
            on_step=on_step,
            check_cancel=check_cancel,
            enable_codex=enable_codex,
        )

    # 1. 准备注册参数（仅当显式指定 REGISTRATION_DRIVER=protocol 时回退纯协议）
    if not email:
        if _email_cfg.USE_EMAIL_SERVICE:
            from core.email_provider import acquire_email
            email = acquire_email()
            _step("email_acquired", f"已从邮箱池获取邮箱: {email}")
        else:
            raise RuntimeError("未提供邮箱且未开启邮箱池服务")

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

        # 提取 cookies 并提前保存 ChatGPT CPA 凭证文件，以便 Codex OAuth 步骤随时读取账号密码
        cookies_dict: dict[str, str] = {}
        try:
            for c in session.cookies:
                cookies_dict[c.name] = c.value
        except Exception:
            pass

        cpa_chatgpt_file = ""
        try:
            import json
            data_root = os.environ.get("GROK2API_DATA_DIR") or ("/app/data" if Path("/app/data").is_dir() else str(Path(__file__).resolve().parents[2] / "data"))
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
                "password": password,
                "totp_secret": totp_secret,
                "cookies": cookies_dict,
            }
            if cpa_proxy_url:
                chatgpt_cpa_record["proxy_url"] = cpa_proxy_url
            if not enable_codex:
                chatgpt_file_path = cpa_dir / f"chatgpt-{email}.json"
                chatgpt_file_path.write_text(json.dumps(chatgpt_cpa_record, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
                cpa_chatgpt_file = str(chatgpt_file_path)
                logger.info(f"[CPA] 已生成 ChatGPT CPA 凭证文件: {cpa_chatgpt_file}")
        except Exception as e:
            logger.warning(f"[CPA] 保存 ChatGPT 凭证文件异常: {e}")

        # 阶段 5: 可选 Codex OAuth 授权
        if enable_codex:
            _step("codex_oauth", "执行 Codex OAuth 自动授权")
            try:
                from core.codex_oauth import run_codex_oauth
                codex_result = run_codex_oauth(
                    email,
                    otp_provider=otp_provider,
                    proxy=session.proxy or proxy,
                    force=True,
                    password=password,
                )
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
                logger.warning(f"Codex OAuth 异常: {exc}")

        codex_ok = bool(codex_result.get("ok")) if enable_codex else True
        cpa_primary_file = (
            (codex_result.get("file_path") if isinstance(codex_result, dict) else "")
            if enable_codex else cpa_chatgpt_file
        )

        if codex_ok:
            _step("success", f"注册与 Codex 授权成功完成: {email}")

        return {
            "success": bool(codex_ok),
            "email": email,
            "password": password,
            "access_token": access_token if codex_ok else "",
            "session_info": session_info if codex_ok else {},
            "cookies": cookies_dict,
            "totp_secret": totp_secret,
            "codex": codex_result,
            "cpa_file": cpa_primary_file,
            "error": None if codex_ok else f"Codex 授权未完成: {codex_result.get('message')}",
        }

    except Exception as exc:
        err_msg = str(exc)
        logger.error(f"[注册失败] {email}: {err_msg}", exc_info=True)
        try:
            from core.email_provider import release_email, release_unconsumed_email
            is_already_registered = any(k in err_msg for k in (
                "登录密码页",
                "/log-in/password",
                "auth.openai.com/log-in/password",
                "user_already_exists",
                "already_registered",
            ))
            if is_already_registered:
                logger.warning(f"[注册拦截] 邮箱已被第三方注册过 OpenAI ({email})，标记为 already_registered，防止重新进入可用池")
                release_email(email, status="already_registered", note=f"采购前已被注册OpenAI: {err_msg[:140]}")
            elif not create_acknowledged:
                release_unconsumed_email(email, note=f"pre_register_fail: {err_msg[:120]}")
            else:
                release_email(email, status="failed", note=f"post_register_fail: {err_msg[:120]}")
        except Exception as rel_exc:
            logger.warning(f"[注册失败] 释放邮箱状态异常 ({email}): {rel_exc}")
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
