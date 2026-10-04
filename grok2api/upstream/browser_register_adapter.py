"""Browser registration adapter bridging grok-register engine with grokcli-2api.

Executes real-browser registration (Chromium via DrissionPage + Camoufox Solver),
extracts the resulting SSO cookie, and passes it back to grok2api for OIDC Device
Flow token conversion and PostgreSQL persistence.
"""
from __future__ import annotations

import os
import sys
import time
from pathlib import Path
from typing import Any, Callable

# Ensure browser_register package is in sys.path
_PKG_DIR = Path(__file__).resolve().parent / "browser_register"
if str(_PKG_DIR) not in sys.path:
    sys.path.insert(0, str(_PKG_DIR))

from grok2api.upstream.browser_register import (
    app_config,
    browser_runtime,
    captcha_solver,
    mail_service,
    registration_browser,
    sso_risk,
    us_consistency,
)
from curl_cffi import requests


def _preflight_registration_path(proxy_url: str = "", log_callback: Callable[[str], None] = None) -> bool:
    """非破坏性预检 accounts.x.ai / grok.com 的连通性与 Cloudflare 状态。"""
    targets = ("https://accounts.x.ai/", "https://grok.com/")
    req_proxies = {"http": proxy_url, "https": proxy_url} if proxy_url else None
    for url in targets:
        try:
            started = time.monotonic()
            resp = requests.get(
                url,
                proxies=req_proxies,
                impersonate="chrome124",
                timeout=15,
                allow_redirects=False,
            )
            latency = int((time.monotonic() - started) * 1000)
            status_code = int(resp.status_code)
            headers = {str(k).lower(): str(v).lower() for k, v in dict(getattr(resp, "headers", {}) or {}).items()}
            text = str(getattr(resp, "text", "") or "")[:4096].lower()
            cf_blocked = ("cloudflare" in headers.get("server", "") or "cf-error" in text) and status_code in (403, 429, 503)
            if cf_blocked:
                if log_callback:
                    log_callback(f"[!] 路径预检告警: {url} 遭遇 Cloudflare 阻断 (HTTP {status_code})")
                return False
            if log_callback:
                log_callback(f"[+] 路径预检正常: {url} (HTTP {status_code}, 延迟 {latency}ms)")
        except Exception as exc:
            if log_callback:
                log_callback(f"[!] 路径预检异常: {url} 连接失败: {exc}")
            return False
    return True


def _detect_chromium_path() -> str:
    candidates = [
        os.environ.get("GROK_BROWSER_PATH", ""),
        os.environ.get("CHROME_PATH", ""),
        "/usr/bin/chromium",
        "/usr/bin/chromium-browser",
        "/usr/bin/google-chrome",
        "/usr/bin/chrome",
    ]
    for c in candidates:
        if c and os.path.isfile(c) and os.access(c, os.X_OK):
            return c
    return ""


def run_browser_registration(
    *,
    sid: str,
    sess: dict[str, Any],
    update: Callable[[str, str], None],
    check_cancel: Callable[[], None],
    proxy: str = "",
    receiver: Any = None,
) -> dict[str, Any]:
    """Execute one browser registration flow and return credentials dict."""
    check_cancel()
    update("registering", "preparing browser environment")

    # Define status update callbacks early
    def _log_cb(msg: str) -> None:
        check_cancel()
        text = str(msg or "").strip()
        print(f"[{sid}] {text}")
        # Map browser stages to grok2api UI phases
        if "打开注册页" in text or "visiting" in text.lower():
            update("registering", text)
        elif "验证码" in text or "邮箱" in text:
            update("registering", text)
        elif "Turnstile" in text or "打码" in text or "过盾" in text:
            update("solving_turnstile", text)
        elif "资料" in text or "profile" in text.lower():
            update("registering", text)
        elif "sso" in text.lower():
            update("registering", text)
        else:
            update("registering", text)

    def _cancel_cb() -> None:
        check_cancel()

    # 1. Load active config
    cfg = app_config.load_config()

    # Overlay user preferences from session/web form
    for k in (
        "proxy_pool_preflight_enabled",
        "us_consistency_enabled",
        "us_consistency_timezone",
        "us_consistency_locale",
        "enable_nsfw",
        "sso_risk_gate_enabled",
        "sso_risk_rejected_file",
    ):
        if k in sess and sess[k] is not None:
            cfg[k] = sess[k]

    # Detect browser executable
    chrome_bin = _detect_chromium_path()
    if chrome_bin:
        cfg["browser_path"] = chrome_bin
    else:
        # Let DrissionPage auto-detect if not found in standard paths
        cfg["browser_path"] = cfg.get("browser_path") or ""

    # 2. Configure proxy mode
    # An explicit per-job proxy always wins. When the caller passes nothing we
    # fall back to the operator-configured default line (config.json
    # proxy_mode="single" + proxy) instead of silently going direct, so the
    # registration egress can be switched from the admin console without a code
    # change. Unset config still means direct, which is the old behaviour.
    active_proxy = str(proxy or sess.get("proxy") or "").strip()

    # NovProxy 动态住宅代理自适应提取
    configured_mode = str(cfg.get("proxy_mode") or "").strip().lower()
    if active_proxy.lower() in ("novproxy", "residential") or (not active_proxy and configured_mode in ("novproxy", "residential")):
        try:
            from grok2api.upstream.browser_register import novproxy
            api_base = str(cfg.get("novproxy_api") or sess.get("novproxy_api") or "https://white.novproxy.com/white/api").strip()
            region = str(cfg.get("novproxy_region") or sess.get("novproxy_region") or "JP").strip()
            minutes = int(cfg.get("novproxy_minutes") or sess.get("novproxy_minutes") or 60)
            _log_cb(f"[*] 正在从 NovProxy 提取实时动态住宅代理 ({region})...")
            nodes = novproxy.fetch_nodes(
                api_base=api_base,
                region=region,
                num=1,
                minutes=minutes,
                attempts=2,
                timeout=12.0,
                log=_log_cb,
            )
            if nodes:
                node = nodes[0]
                active_proxy = node if "://" in node else f"socks5h://{node}"
                _log_cb(f"[+] 成功分配 NovProxy 住宅节点: {active_proxy}")
        except Exception as n_exc:
            _log_cb(f"[!] NovProxy 动态提取失败: {n_exc}")
            active_proxy = ""

    if not active_proxy:
        try:
            if configured_mode == "single":
                active_proxy = str(cfg.get("proxy") or "").strip()
        except Exception:
            active_proxy = ""
    is_direct = not active_proxy or active_proxy.lower() in ("direct", "none", "off", "0")
    if is_direct:
        cfg["proxy_mode"] = "direct"
        cfg["proxy"] = ""
    else:
        cfg["proxy_mode"] = "single"
        cfg["proxy"] = active_proxy

    # 3. Configure local Turnstile solver
    local_solver_url = (
        os.environ.get("GROK2API_LOCAL_SOLVER_URL")
        or os.environ.get("LOCAL_SOLVER_URL")
        or "http://127.0.0.1:5072"
    ).rstrip("/")
    # Honour the operator's switch instead of forcing the solver on. Both modes
    # work: with the solver enabled its token is published to the page's React
    # state (see registration_browser.inject_turnstile_token_into_react), and
    # with it disabled we rely on Turnstile clearing itself.
    cfg["captcha_solver_enabled"] = bool(cfg.get("captcha_solver_enabled", False))
    cfg["captcha_solver_provider"] = "yescaptcha"
    cfg["captcha_solver_api_base"] = local_solver_url

    # 4. Bind runtime configuration
    registration_browser.bind_runtime(cfg)
    browser_runtime.configure_runtime(cfg)
    # mail_service reads both cfg["config"] and flattened keys depending on the
    # call site, so bind both spellings.
    mail_service.bind_runtime({"config": cfg, **cfg})
    # browser_register/__init__.py registers top-level aliases for these
    # submodules, but one may already have been imported through the legacy
    # top-level path before that ran. Re-bind such duplicates so no reader is
    # left holding an unconfigured copy.
    try:
        import mail_service as _top_level_mail_service

        if _top_level_mail_service is not mail_service:
            _top_level_mail_service.bind_runtime({"config": cfg, **cfg})
    except Exception as exc:
        print(f"[{sid}] top-level mail_service bind skipped: {exc}", flush=True)
    try:
        import browser_runtime as _top_level_browser_runtime

        if _top_level_browser_runtime is not browser_runtime:
            _top_level_browser_runtime.configure_runtime(cfg)
    except Exception as exc:
        print(f"[{sid}] top-level browser_runtime bind skipped: {exc}", flush=True)
    captcha_solver.bind_runtime(cfg)
    sso_risk.configure_risk_runtime(cfg, browser_runtime.http_get)

    # 4b. Point the local solver at the same egress as the registration browser.
    # This must run after configure_runtime() so cfg is fully populated, and it
    # deliberately does not use captcha_solver.sync_solver_proxy(): that helper
    # resolves default_proxies_file() to a path inside the installed package,
    # whereas api_solver.py reads proxies.txt from its own CWD
    # (/app/turnstile-solver). A solver egress that differs from the browser's
    # makes Cloudflare reject the solved token.
    try:
        solver_proxies_file = (
            cfg.get("captcha_solver_proxies_file")
            or os.environ.get("GROK2API_SOLVER_PROXIES_FILE")
            or "/app/turnstile-solver/proxies.txt"
        )
        if is_direct:
            solver_proxy_line = ""
        else:
            solver_proxy_line = str(active_proxy).strip()
            # api_solver.py expects curl-style schemes, not Chromium's socks5h://.
            if solver_proxy_line.startswith("socks5h://"):
                solver_proxy_line = "socks5://" + solver_proxy_line[len("socks5h://"):]
            elif solver_proxy_line.startswith("socks4a://"):
                solver_proxy_line = "socks4://" + solver_proxy_line[len("socks4a://"):]
        Path(solver_proxies_file).parent.mkdir(parents=True, exist_ok=True)
        Path(solver_proxies_file).write_text(
            (solver_proxy_line + "\n") if solver_proxy_line else "", encoding="utf-8"
        )
        print(
            f"[{sid}] solver proxy synced -> {solver_proxies_file} line={solver_proxy_line!r}",
            flush=True,
        )
    except Exception as exc:
        print(f"[{sid}] solver proxy sync warning: {exc}", flush=True)

    # 6. Run the browser registration steps
    email = ""
    dev_token = ""
    sso = ""
    profile: dict[str, Any] = {}
    try:
        # A. 执行注册路径非破坏性预检
        if cfg.get("proxy_pool_preflight_enabled", True):
            _log_cb("[*] 正在执行注册路径预检 (accounts.x.ai / grok.com)...")
            # In direct mode active_proxy may hold a sentinel ("direct"/"none"/
            # "off"/"0"). Passing that to requests makes it resolve a host named
            # "direct" (curl: (5) Could not resolve proxy: direct) and the
            # preflight aborts before the browser is ever started.
            preflight_proxy = "" if is_direct else active_proxy
            preflight_ok = _preflight_registration_path(proxy_url=preflight_proxy, log_callback=_log_cb)
            if not preflight_ok:
                raise RuntimeError("注册路径预检失败：目标站点不可达或遭遇 Cloudflare 阻断，已终止本次尝试")

        # B. 出口国家与网络环境自适应对齐（多国支持）
        if us_consistency.enabled():
            expect_c = str(cfg.get("us_consistency_expect_country") or "").strip()
            if expect_c.upper() in ("AUTO", "ALL", "RAND", "ANY", "*"):
                expect_c = ""
            detected_zone = us_consistency.align_timezone_with_proxy(active_proxy, expect_country=expect_c)
            if detected_zone:
                _log_cb(f"[*] {us_consistency.describe()}")

        _log_cb("[*] 正在启动 Chromium 浏览器实例...")
        use_proxy = not is_direct
        registration_browser.start_browser(log_callback=_log_cb, use_proxy=use_proxy)
        check_cancel()

        _log_cb("[*] 打开 xAI 注册页面: https://accounts.x.ai/sign-up?redirect=grok-com")
        registration_browser.open_signup_page(log_callback=_log_cb, cancel_callback=_cancel_cb)
        check_cancel()

        _log_cb("[*] 创建邮箱并提交注册表单...")
        email, dev_token = registration_browser.fill_email_and_submit(
            timeout=45,
            log_callback=_log_cb,
            cancel_callback=_cancel_cb,
        )
        sess["email"] = email
        _log_cb(f"[+] 邮箱已提交: {email}")
        check_cancel()

        _log_cb("[*] 正在等待接收并提交验证码...")
        code = registration_browser.fill_code_and_submit(
            email=email,
            dev_token=dev_token,
            timeout=180,
            log_callback=_log_cb,
            cancel_callback=_cancel_cb,
        )
        _log_cb("[+] 验证码验证成功")
        check_cancel()

        _log_cb("[*] 填写个人资料并提交...")
        profile = registration_browser.fill_profile_and_submit(
            timeout=120,
            log_callback=_log_cb,
            cancel_callback=_cancel_cb,
        )
        _log_cb(f"[+] 资料提交完成: {profile.get('given_name', '')} {profile.get('family_name', '')}")
        check_cancel()

        _log_cb("[*] 正在提取 SSO Cookie...")
        sso = registration_browser.wait_for_sso_cookie(
            timeout=120,
            log_callback=_log_cb,
            cancel_callback=_cancel_cb,
        )
        if not sso:
            raise RuntimeError("未能从浏览器会话中提取到 sso cookie")
        _log_cb(f"[+] 成功获取 SSO Cookie (长度={len(sso)})")

        # 流量节省关键优化：SSO已获取，立即关闭浏览器并断开代理连接，避免后台长连接偷跑流量
        try:
            registration_browser.stop_browser()
            registration_browser.cleanup_runtime_memory(log_callback=_log_cb, reason="SSO已获取提前释放浏览器")
        except Exception:
            pass

        # C. SSO 风控早停检查 (读取 grok.com botFlagSource / policy=deny)
        if cfg.get("sso_risk_gate_enabled", True):
            _log_cb("[*] 正在执行 SSO 风控早停安全检查 (botFlagSource / policy)...")
            sso_risk.ensure_sso_eligible(
                raw_token=sso,
                email=email,
                proxy=active_proxy,
                log_callback=_log_cb,
                http_get=browser_runtime.http_get,
            )

        # D. 开启 NSFW 敏感内容权限 (后处理开关)
        if cfg.get("enable_nsfw", True):
            _log_cb("[*] 正在开启账号 NSFW 权限 (gRPC-web)...")
            try:
                nsfw_ok, nsfw_msg = registration_browser.enable_nsfw_for_token(sso, log_callback=_log_cb)
                if nsfw_ok:
                    _log_cb(f"[+] NSFW 权限开启成功: {nsfw_msg}")
                else:
                    _log_cb(f"[!] NSFW 权限开启失败 (不影响账号正常入库): {nsfw_msg}")
            except Exception as nsfw_exc:
                _log_cb(f"[!] NSFW 开启异常: {nsfw_exc}")

        # E. 破冰养号会话 (Warm-up Conversation: 模拟真实用户首轮日常闲聊)
        if cfg.get("warmup_conversation_enabled", True):
            _log_cb("[*] 正在执行新账号破冰对话 (Warm-up Session)...")
            try:
                from curl_cffi import requests as c_req
                warmup_headers = {
                    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
                    "Cookie": f"sso={sso}; sso-rw={sso}",
                    "Origin": "https://grok.com",
                    "Referer": "https://grok.com/",
                    "Content-Type": "application/json",
                }
                warmup_proxies = {"https": active_proxy} if active_proxy else None
                # 发送一轮轻量常规问候，激活会话树
                warm_res = c_req.post(
                    "https://grok.com/rest/app-chat/conversations/new",
                    headers=warmup_headers,
                    json={"modelName": "grok-3", "message": "Hi, how are you today?"},
                    proxies=warmup_proxies,
                    timeout=15,
                    impersonate="chrome124",
                )
                if warm_res.status_code in (200, 201):
                    _log_cb("[+] 破冰对话创建成功，账号已沉淀合法会话上下文")
                else:
                    _log_cb(f"[*] 破冰对话响应码: {warm_res.status_code} (不影响入库)")
            except Exception as w_exc:
                _log_cb(f"[*] 破冰会话跳过: {str(w_exc)[:90]}")

    finally:
        # Always reclaim browser process and memory after each registration
        try:
            registration_browser.cleanup_runtime_memory(
                log_callback=_log_cb,
                reason="单次注册结束",
            )
        except Exception as clean_exc:
            print(f"[{sid}] cleanup error: {clean_exc}")

    password = str(profile.get("password") or sess.get("password") or "")
    return {
        "ok": True,
        "sso": sso,
        "email": email,
        "password": password,
        "profile": profile,
        "session_cookies": {
            "sso": sso,
            "sso-rw": sso,
        },
    }
