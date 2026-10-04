"""提供共享的 HTTP 请求、代理处理和浏览器启动参数。

浏览器引擎可通过 ``GROK2API_BROWSER_ENGINE`` 选择：

* ``camoufox``（默认）—— 加固版 Firefox，走 Juggler 协议，自带指纹伪造。
  2026-09-28 起 x.ai 上线 Cloudflare Bot Management JSD 检测后，Chromium
  （CDP 驱动 + 容器内无 GPU）一律拿不到 ``cf_clearance``，注册静默失败；
  Camoufox 实测可稳定通过。
* ``chromium`` —— 旧的 DrissionPage 实现，保留用于回退排查。
"""
import os
import urllib.parse

from camoufox_compat import CamoufoxOptions, parse_proxy
from curl_cffi import requests
from proxy_bridge import (
    LocalAuthProxyBridge,
    prepare_chromium_proxy,
    proxy_for_chromium,
)
from proxy_pool import (
    ProxyTransportError,
    current_proxy_lease,
    managed_proxy_active,
    safe_proxy_error_text,
)

_config = {}
_extension_path = ""


def _legacy_extension_path():
    return os.path.join(os.path.dirname(os.path.abspath(__file__)), "turnstilePatch")


def _resolve_extension_path(explicit=None):
    if explicit is not None:
        candidate = str(explicit or "").strip()
        return candidate if candidate and os.path.isdir(candidate) else ""
    configured = str(_extension_path or "").strip()
    if configured and os.path.isdir(configured):
        return configured
    legacy = _legacy_extension_path()
    return legacy if os.path.isdir(legacy) else ""


def configure_runtime(config_ref, extension_path=""):
    global _config, _extension_path
    _config = config_ref
    _extension_path = str(extension_path or "")


def get_configured_proxy():
    lease = current_proxy_lease()
    if lease is not None:
        return str(lease.proxy_url or "").strip()
    mode = str(_config.get("proxy_mode", "auto") or "auto").strip().lower()
    if mode == "direct" or mode in ("single", "pool"):
        return ""
    return str(_config.get("proxy", "") or "").strip()


def get_proxies():
    proxy = get_configured_proxy()
    return {"http": proxy, "https": proxy} if proxy else {}


def _parse_proxy_url(proxy):
    raw = str(proxy or "").strip()
    if not raw:
        return None
    if "://" not in raw:
        raw = "http://" + raw
    try:
        return urllib.parse.urlsplit(raw)
    except Exception:
        return None


def _safe_proxy_port(parsed):
    try:
        return parsed.port
    except Exception:
        return None


def _proxy_has_auth(proxy):
    parsed = _parse_proxy_url(proxy)
    return bool(parsed and parsed.hostname and (parsed.username is not None or parsed.password is not None))


def _strip_proxy_auth(proxy):
    raw = str(proxy or "").strip()
    parsed = _parse_proxy_url(raw)
    if not parsed or not parsed.hostname:
        return raw
    host = parsed.hostname
    if ":" in host and not host.startswith("["):
        host = "[%s]" % host
    port = _safe_proxy_port(parsed)
    netloc = "%s:%s" % (host, port) if port else host
    stripped = urllib.parse.urlunsplit((parsed.scheme or "http", netloc, parsed.path, parsed.query, parsed.fragment))
    return stripped.split("://", 1)[1] if "://" not in raw else stripped


def _proxy_endpoint_terms(proxy=None):
    parsed = _parse_proxy_url(proxy or get_configured_proxy())
    if not parsed or not parsed.hostname:
        return []
    terms = [parsed.hostname]
    port = _safe_proxy_port(parsed)
    if port:
        terms.extend(["%s:%s" % (parsed.hostname, port), "port %s" % port])
    return [item.lower() for item in terms if item]


def is_proxy_connection_error(exc):
    if not get_configured_proxy():
        return False
    err = str(exc or "").lower()
    if not err:
        return False
    if any(item in err for item in ("proxy", "tunnel", "socks")):
        return True
    markers = (
        "could not connect", "failed to connect", "connection refused",
        "connection reset", "connect error", "timed out", "timeout",
    )
    if any(item in err for item in markers):
        terms = _proxy_endpoint_terms()
        return not terms or any(term in err for term in terms)
    return False


def page_has_proxy_error(page_obj):
    try:
        url = str(getattr(page_obj, "url", "") or "")
        title = str(page_obj.run_js("return document.title || ''") or "")
        body = str(page_obj.run_js("return document.body ? document.body.innerText.slice(0, 2000) : ''") or "")
    except Exception:
        return False
    text = "%s\n%s\n%s" % (url, title, body)
    text = text.lower()
    # Chromium 的错误页把错误码写成 ERR_XXX_YYY（下划线），而不是自然语言，
    # 所以 "tunnel connection failed" 这类词永远匹配不到
    # ERR_TUNNEL_CONNECTION_FAILED。实测踩过：住宅节点跑着跑着死掉，页面
    # 停在错误页，检测没认出来 → 没抛 ProxyTransportError → 被上层当成
    # 普通应用异常直接 raise，整批 20 个账号全被拖死。
    # 这里同时按「下划线归一化后的自然语言」和「ERR_ 错误码」两路匹配。
    normalized = text.replace("_", " ")
    failed = any(marker in normalized for marker in (
        "err proxy", "proxy connection failed", "proxy server",
        "proxy authentication", "tunnel connection failed",
        "无法连接到代理服务器", "代理服务器",
    )) or "chrome-error" in text or any(marker in text for marker in (
        # 网络层错误码：走代理时出现，一律按「这个节点不可用」处理，
        # 交给上层释放租约换节点重试。
        "err_tunnel_connection_failed", "err_socks_connection_failed",
        "err_proxy_connection_failed", "err_connection_timed_out",
        "err_connection_reset", "err_connection_closed",
        "err_connection_refused", "err_connection_failed",
        "err_name_not_resolved", "err_address_unreachable",
        "err_internet_disconnected", "err_network_changed",
        "err_empty_response", "err_timed_out", "err_ssl_protocol_error",
        "err_http2_protocol_error", "err_quic_protocol_error",
    ))
    if failed and managed_proxy_active():
        raise ProxyTransportError("Chromium 检测到代理连接错误页面")
    return failed


def prepare_browser_proxy(use_proxy=True, log_callback=None):
    # Managed registration leases must never silently switch to another IP in
    # the middle of an account. Legacy auto mode keeps the historical direct
    # fallback behavior when callers explicitly pass use_proxy=False.
    if managed_proxy_active():
        use_proxy = True
    proxy = get_configured_proxy()
    if not use_proxy or not proxy:
        return "", None
    logger = None
    if log_callback:
        logger = lambda message: log_callback("[*] 已为 Chromium启动本地认证代理桥: %s" % message.split(": ", 1)[-1]) if "started authenticated proxy bridge" in message else log_callback(message)
    return prepare_chromium_proxy(proxy, log=logger)


def apply_browser_proxy_option(options, proxy):
    """把代理写进浏览器启动配置。

    DrissionPage 的 ChromiumOptions.set_proxy() **不支持 socks5** —— 它只会
    打印 "this proxy is not supported for the time being" 然后静默忽略，
    浏览器实际走直连（表现为 ERR_CONNECTION_RESET，极难排查）。

    而 Chromium 原生的 --proxy-server 是支持 socks5 的。所以这里对
    socks* 一律绕开 set_proxy，直接用启动参数。

    CamoufoxOptions 走 Playwright 的 proxy 字典，原生支持 socks5，直接
    set_proxy 即可（兼容层会做 socks5h→socks5 归一化）。
    """
    if not proxy:
        return
    if isinstance(options, CamoufoxOptions):
        options.set_proxy(proxy)
        return
    scheme = ""
    try:
        parsed = urllib.parse.urlsplit(str(proxy))
        scheme = (parsed.scheme or "").lower()
    except Exception:
        scheme = ""
    is_socks = scheme.startswith("socks")
    if hasattr(options, "set_proxy") and not is_socks:
        try:
            options.set_proxy(proxy)
            return
        except Exception:
            pass
    if not hasattr(options, "set_argument"):
        raise AttributeError("当前浏览器 options 不支持设置代理")
    # Chromium's --proxy-server understands socks5://, socks4:// and http(s)://
    # only. The curl-style socks5h:// / socks4a:// spellings (remote DNS) are
    # rejected outright with ERR_NO_SUPPORTED_PROXIES, which renders a Chromium
    # error page instead of the sign-up page — making every proxied registration
    # look like "未找到「使用邮箱注册」按钮". Chromium's socks5 already resolves
    # names through the proxy, so the rewrite is behaviour-safe.
    chromium_proxy = str(proxy).strip()
    lowered = chromium_proxy.lower()
    if lowered.startswith("socks5h://"):
        chromium_proxy = "socks5://" + chromium_proxy[len("socks5h://"):]
    elif lowered.startswith("socks4a://"):
        chromium_proxy = "socks4://" + chromium_proxy[len("socks4a://"):]
    try:
        options.set_argument("--proxy-server=%s" % chromium_proxy)
    except TypeError:
        options.set_argument("--proxy-server", chromium_proxy)


def browser_engine():
    """当前浏览器引擎：``camoufox``（默认）或 ``chromium``。"""
    raw = str(os.environ.get("GROK2API_BROWSER_ENGINE") or "").strip().lower()
    if not raw:
        try:
            raw = str((_config or {}).get("browser_engine") or "").strip().lower()
        except Exception:
            raw = ""
    return "chromium" if raw in ("chromium", "chrome", "drission") else "camoufox"


def _configure_camoufox_options(options, browser_proxy="", extension_path=None):
    """把注册流程需要的环境一致性设置写进 CamoufoxOptions。

    注意：这里**不**调用 ``us_consistency.apply_browser_options()``。那套逻辑是
    为 Chromium 写的（改 --lang、设可执行文件路径、注入 Chromium 风格的
    Sec-CH-UA Client Hints）。Camoufox 的 ``os=["windows"]`` 已经生成了一整套
    自洽的 Windows 指纹（UA / navigator.platform / WebGL / 字体 / 时区），
    再叠一层 Chromium 的 Client Hints 只会制造新的矛盾——Firefox 本来就不发
    Sec-CH-UA 头。
    """
    # x.ai 的 Cloudflare 对 headless 指纹判定极严，始终用窗口化 + Xvfb。
    display = str(os.environ.get("DISPLAY") or "").strip()
    force_headless = str((_config or {}).get("cpa_headless") or "").strip().lower() in ("1", "true", "yes")
    if force_headless or not display:
        options.headless(True)
    apply_browser_proxy_option(options, browser_proxy)
    # 时区仍需与代理出口地区对齐：Camoufox 支持显式指定，交给它而不是改进程 TZ。
    try:
        import us_consistency
        us_consistency.configure(_config)
        if us_consistency.enabled():
            timezone = str(us_consistency.timezone_name() or "").strip()
            locale = str(us_consistency.locale_name() or "").strip()
            if timezone:
                options.set_timezone(timezone)
            if locale:
                options.set_locale(locale)
    except Exception:
        pass
    return options


def create_browser_options(browser_proxy="", extension_path=None):
    if browser_engine() == "camoufox":
        return _configure_camoufox_options(
            CamoufoxOptions(), browser_proxy=browser_proxy, extension_path=extension_path
        )
    from DrissionPage import ChromiumOptions
    options = ChromiumOptions()
    options.auto_port()
    options.set_timeouts(base=1)
    # Prefer a windowed Chromium on the Xvfb display. x.ai's Cloudflare returns
    # "Sorry, you have been blocked" for --headless=new from every IP class tried
    # (Oracle VPS, Cloudflare WARP, US Comcast residential, Brazil residential),
    # i.e. the block is headless-fingerprint driven, not IP driven. Fall back to
    # headless only when no X display exists, because a windowed Chromium cannot
    # be reached over CDP without one. A root-owned Chromium always needs
    # --no-sandbox. browser_path comes from
    # browser_register_adapter._detect_chromium_path().
    display = str(os.environ.get("DISPLAY") or "").strip()
    force_headless = str((_config or {}).get("cpa_headless") or "").strip().lower() in ("1", "true", "yes")
    if force_headless or not display:
        options.headless(True)
    options.set_argument("--no-sandbox")
    options.set_argument("--disable-dev-shm-usage")
    # us_consistency rewrites navigator.platform to Win32 and sends Windows
    # Sec-CH-UA-* headers, so the User-Agent must claim Windows too. Chromium's
    # own UA says "Linux", which contradicts those overrides and is enough for
    # Cloudflare to reject the Turnstile token.
    user_agent = str((_config or {}).get("user_agent") or "").strip()
    if user_agent:
        try:
            options.set_argument("--user-agent=%s" % user_agent)
        except TypeError:
            options.set_argument("--user-agent", user_agent)
    browser_path = str((_config or {}).get("browser_path") or "").strip()
    if browser_path:
        options.set_browser_path(browser_path)
    apply_browser_proxy_option(options, browser_proxy)
    effective_extension = _resolve_extension_path(extension_path)
    if effective_extension:
        options.add_extension(effective_extension)
    # 美国住宅 IP 环境一致性：统一时区/语言/platform，消除 IP=US 但
    # 时区=UTC、platform=Linux 这类自相矛盾特征。失败不影响主流程。
    try:
        import us_consistency
        us_consistency.configure(_config)
        us_consistency.apply_process_timezone()
        us_consistency.apply_browser_options(options)
    except Exception:
        pass
    return options


def _build_request_kwargs(**kwargs):
    request_kwargs = dict(kwargs)
    proxies = request_kwargs.pop("proxies", None)
    if proxies is None:
        proxies = get_proxies()
    if proxies:
        request_kwargs["proxies"] = proxies
    request_kwargs.setdefault("timeout", 15)
    return request_kwargs


def http_get(url, **kwargs):
    request_kwargs = _build_request_kwargs(**kwargs)
    try:
        return requests.get(url, **request_kwargs)
    except Exception as exc:
        if is_proxy_connection_error(exc):
            if managed_proxy_active():
                raise ProxyTransportError(safe_proxy_error_text(exc)) from exc
            direct = dict(request_kwargs)
            direct.pop("proxies", None)
            return requests.get(url, **direct)
        raise


def http_post(url, **kwargs):
    replay_safe = bool(kwargs.pop("replay_safe", False))
    request_kwargs = _build_request_kwargs(**kwargs)
    try:
        return requests.post(url, **request_kwargs)
    except Exception as exc:
        if is_proxy_connection_error(exc):
            if managed_proxy_active() or not replay_safe:
                raise ProxyTransportError(safe_proxy_error_text(exc)) from exc
            direct = dict(request_kwargs)
            direct.pop("proxies", None)
            return requests.post(url, **direct)
        raise
