"""美国住宅 IP 环境一致性模块。

背景
----
grok-register 原版不做任何浏览器指纹归一化，实测在容器/服务器上会出现三方矛盾：

    IP 国家:  US（美国住宅代理）
    浏览器时区: UTC            <- 与 IP 矛盾
    navigator.platform: Linux x86_64  <- 与 UA 声称的 Windows 矛盾

这类矛盾本身就是风控特征，会抵消住宅 IP 带来的"真实用户"收益。
本模块把时区、语言、platform、CPU/内存等指纹统一到"美国 Windows 桌面用户"。

设计原则
--------
1. 只做一致性，不做伪装强度竞赛：目标是消除自相矛盾，而非对抗专业指纹库。
2. 最小侵入：原项目文件只加一行调用，升级时冲突面最小。
3. 可关闭：配置 us_consistency_enabled=false 即完全退回原版行为。
"""

from __future__ import annotations

import json
import os
import time
import urllib.request

# 默认时区与语言（美国东部，亦作为无法识别时的通用默认）
DEFAULT_TIMEZONE = "America/New_York"
DEFAULT_LOCALE = "en-US"

# 常见国家代码 -> 默认首选时区与语言/Accept-Language 映射
_COUNTRY_PRESETS = {
    "US": {"timezone": "America/New_York", "locale": "en-US", "accept_lang": "en-US,en;q=0.9"},
    "JP": {"timezone": "Asia/Tokyo", "locale": "ja-JP", "accept_lang": "ja,en-US;q=0.9,en;q=0.8"},
    "GB": {"timezone": "Europe/London", "locale": "en-GB", "accept_lang": "en-GB,en;q=0.9"},
    "DE": {"timezone": "Europe/Berlin", "locale": "de-DE", "accept_lang": "de-DE,de;q=0.9,en-US;q=0.8"},
    "FR": {"timezone": "Europe/Paris", "locale": "fr-FR", "accept_lang": "fr-FR,fr;q=0.9,en-US;q=0.8"},
    "SG": {"timezone": "Asia/Singapore", "locale": "en-SG", "accept_lang": "en-SG,en;q=0.9,zh-CN;q=0.8"},
    "HK": {"timezone": "Asia/Hong_Kong", "locale": "zh-HK", "accept_lang": "zh-HK,zh;q=0.9,en-US;q=0.8"},
    "TW": {"timezone": "Asia/Taipei", "locale": "zh-TW", "accept_lang": "zh-TW,zh;q=0.9,en-US;q=0.8"},
    "KR": {"timezone": "Asia/Seoul", "locale": "ko-KR", "accept_lang": "ko-KR,ko;q=0.9,en-US;q=0.8"},
    "CA": {"timezone": "America/Toronto", "locale": "en-CA", "accept_lang": "en-CA,en;q=0.9"},
    "AU": {"timezone": "Australia/Sydney", "locale": "en-AU", "accept_lang": "en-AU,en;q=0.9"},
    "NL": {"timezone": "Europe/Amsterdam", "locale": "nl-NL", "accept_lang": "nl-NL,nl;q=0.9,en-US;q=0.8"},
}

# 与 UA 保持一致的 Windows 桌面参数。
DEFAULT_PLATFORM = "Win32"
DEFAULT_ACCEPT_LANGUAGE = "en-US,en;q=0.9"
DEFAULT_CORES = 8
DEFAULT_DEVICE_MEMORY = 8

# Client Hints：UA 声称 Windows + Chrome，这些头必须自洽，
# 否则服务端比对 Sec-CH-UA-Platform 与 navigator.platform 就能发现矛盾。
_CH_PLATFORM = '"Windows"'
_CH_PLATFORM_VERSION = '"15.0.0"'  # Windows 11 的 UA-CH 平台版本


def _chrome_version_from_ua() -> str:
    """从配置的 User-Agent 里取 Chrome 主版本号。

    Sec-CH-UA 必须与 User-Agent 里的版本一致：只改 UA 不改 UA-CH（或反过来）
    都会留下可被服务端比对的矛盾。版本号从 user_agent 动态提取，避免两处
    硬编码各自漂移。
    """
    ua = ""
    try:
        ua = str((_config or {}).get("user_agent") or "")
    except Exception:
        ua = ""
    if not ua:
        try:
            import app_config

            ua = str(getattr(app_config, "config", {}).get("user_agent") or "")
        except Exception:
            ua = ""
    marker = "Chrome/"
    if marker in ua:
        major = ua.split(marker, 1)[1].split(" ", 1)[0].strip().split(".", 1)[0]
        if major.isdigit():
            return major
    return "138"


_CH_UA_BRANDS = '"Not(A:Brand";v="99", "Google Chrome";v="%s", "Chromium";v="%s"'

_config: dict = {}

# 浏览器路径探测顺序。容器/服务器上 DrissionPage 默认找不到 Chrome，
# 需要显式指定；这里做成"探测到就用"，避免把路径硬编码进配置文件。
#
# 说明：除常见的系统安装位置外，还支持通过环境变量
# GROK_BROWSER_PATH / PLAYWRIGHT_BROWSERS_PATH 指定，便于在
# 浏览器安装在非标准目录（如持久化数据卷）时复用。
_BROWSER_CANDIDATES = (
    "/usr/bin/google-chrome",
    "/usr/bin/google-chrome-stable",
    "/usr/bin/chromium",
    "/usr/bin/chromium-browser",
    "/opt/google/chrome/chrome",
    "/root/.cache/ms-playwright/chromium-1223/chrome-linux64/chrome",
)


def _candidate_from_env() -> str:
    """从环境变量推导浏览器路径。

    GROK_BROWSER_PATH 直接给出可执行文件；
    PLAYWRIGHT_BROWSERS_PATH 给出浏览器根目录，需再拼上 chromium 子路径。
    """
    direct = str(os.environ.get("GROK_BROWSER_PATH") or "").strip()
    if direct and os.path.isfile(direct) and os.access(direct, os.X_OK):
        return direct
    root = str(os.environ.get("PLAYWRIGHT_BROWSERS_PATH") or "").strip()
    if root and os.path.isdir(root):
        # 目录名带版本号，按名字排序取最新的一个。
        try:
            entries = sorted(
                name for name in os.listdir(root) if name.startswith("chromium-")
            )
        except OSError:
            entries = []
        for name in reversed(entries):
            candidate = os.path.join(root, name, "chrome-linux64", "chrome")
            if os.path.isfile(candidate) and os.access(candidate, os.X_OK):
                return candidate
            candidate = os.path.join(root, name, "chrome-linux", "chrome")
            if os.path.isfile(candidate) and os.access(candidate, os.X_OK):
                return candidate
    return ""


def detect_browser_path() -> str:
    """返回可用的 Chromium 可执行文件路径，找不到则返回空串。

    优先使用配置项 browser_path；其次读环境变量；最后按候选列表探测。
    """
    configured = str(_config.get("browser_path") or "").strip()
    if configured and os.path.isfile(configured) and os.access(configured, os.X_OK):
        return configured
    from_env = _candidate_from_env()
    if from_env:
        return from_env
    for candidate in _BROWSER_CANDIDATES:
        if os.path.isfile(candidate) and os.access(candidate, os.X_OK):
            return candidate
    return ""


def configure(config_ref) -> None:
    """由 app_config 注入配置。"""
    global _config
    _config = config_ref if isinstance(config_ref, dict) else {}


# 美国各州 → 时区。用于按代理真实出口自动对齐时区。
# 注意：住宅代理的 state 参数实际不生效（实测请求 New York
# 会分到 Nevada/Texas），所以不能依赖下单参数，必须以探测到的真实
# 出口地区为准，否则会出现「IP 在加州、时区却是纽约」这类矛盾特征。
_US_STATE_TIMEZONE = {
    "AL": "America/Chicago", "AK": "America/Anchorage", "AZ": "America/Phoenix",
    "AR": "America/Chicago", "CA": "America/Los_Angeles", "CO": "America/Denver",
    "CT": "America/New_York", "DE": "America/New_York", "DC": "America/New_York",
    "FL": "America/New_York", "GA": "America/New_York", "HI": "Pacific/Honolulu",
    "ID": "America/Boise", "IL": "America/Chicago", "IN": "America/Indiana/Indianapolis",
    "IA": "America/Chicago", "KS": "America/Chicago", "KY": "America/New_York",
    "LA": "America/Chicago", "ME": "America/New_York", "MD": "America/New_York",
    "MA": "America/New_York", "MI": "America/Detroit", "MN": "America/Chicago",
    "MS": "America/Chicago", "MO": "America/Chicago", "MT": "America/Denver",
    "NE": "America/Chicago", "NV": "America/Los_Angeles", "NH": "America/New_York",
    "NJ": "America/New_York", "NM": "America/Denver", "NY": "America/New_York",
    "NC": "America/New_York", "ND": "America/Chicago", "OH": "America/New_York",
    "OK": "America/Chicago", "OR": "America/Los_Angeles", "PA": "America/New_York",
    "RI": "America/New_York", "SC": "America/New_York", "SD": "America/Chicago",
    "TN": "America/Chicago", "TX": "America/Chicago", "UT": "America/Denver",
    "VT": "America/New_York", "VA": "America/New_York", "WA": "America/Los_Angeles",
    "WV": "America/New_York", "WI": "America/Chicago", "WY": "America/Denver",
}

# 州全名 → 缩写，便于直接匹配 ip-api 的 regionName。
_US_STATE_NAME_ABBR = {
    "alabama": "AL", "alaska": "AK", "arizona": "AZ", "arkansas": "AR",
    "california": "CA", "colorado": "CO", "connecticut": "CT", "delaware": "DE",
    "district of columbia": "DC", "florida": "FL", "georgia": "GA", "hawaii": "HI",
    "idaho": "ID", "illinois": "IL", "indiana": "IN", "iowa": "IA", "kansas": "KS",
    "kentucky": "KY", "louisiana": "LA", "maine": "ME", "maryland": "MD",
    "massachusetts": "MA", "michigan": "MI", "minnesota": "MN", "mississippi": "MS",
    "missouri": "MO", "montana": "MT", "nebraska": "NE", "nevada": "NV",
    "new hampshire": "NH", "new jersey": "NJ", "new mexico": "NM", "new york": "NY",
    "north carolina": "NC", "north dakota": "ND", "ohio": "OH", "oklahoma": "OK",
    "oregon": "OR", "pennsylvania": "PA", "rhode island": "RI",
    "south carolina": "SC", "south dakota": "SD", "tennessee": "TN", "texas": "TX",
    "utah": "UT", "vermont": "VT", "virginia": "VA", "washington": "WA",
    "west virginia": "WV", "wisconsin": "WI", "wyoming": "WY",
}


def timezone_for_region(region) -> str:
    """按美国州名/缩写返回时区；无法识别时返回空串。"""
    raw = str(region or "").strip()
    if not raw:
        return ""
    upper = raw.upper()
    if upper in _US_STATE_TIMEZONE:
        return _US_STATE_TIMEZONE[upper]
    abbr = _US_STATE_NAME_ABBR.get(raw.lower())
    if abbr:
        return _US_STATE_TIMEZONE.get(abbr, "")
    return ""


def apply_region_timezone(region) -> str:
    """按代理真实出口地区对齐时区。

    这是「IP ↔ 时区」一致性的关键：住宅代理的实际落地州无法预先指定，
    必须以探测结果为准回写，否则时区会与 IP 地理位置矛盾。
    """
    zone = timezone_for_region(region)
    if not zone:
        return ""
    _config["us_consistency_timezone"] = zone
    apply_process_timezone()
    return zone


def probe_exit_region(proxy_url="", timeout=20):
    """经代理或直连探测出口的国家与地区，返回 dict（失败返回空 dict）。"""
    url = (
        "http://ip-api.com/json/?fields=status,country,countryCode,"
        "regionName,city,timezone,isp,hosting,proxy,mobile,query"
    )
    raw = str(proxy_url or "").strip()
    try:
        if raw:
            opener = urllib.request.build_opener(
                urllib.request.ProxyHandler({"http": raw, "https": raw})
            )
        else:
            opener = urllib.request.build_opener()
        request = urllib.request.Request(url, headers={"User-Agent": "curl/7.88.1"})
        with opener.open(request, timeout=timeout) as response:
            data = json.loads(response.read().decode("utf-8", "replace"))
    except Exception:
        return {}
    if not isinstance(data, dict) or data.get("status") != "success":
        return {}
    return {
        "ip": str(data.get("query") or ""),
        "country": str(data.get("countryCode") or ""),
        "country_name": str(data.get("country") or ""),
        "region": str(data.get("regionName") or ""),
        "city": str(data.get("city") or ""),
        "timezone": str(data.get("timezone") or ""),
        "isp": str(data.get("isp") or ""),
        "hosting": bool(data.get("hosting")),
        "proxy": bool(data.get("proxy")),
    }


def align_timezone_with_proxy(proxy_url="", expect_country="") -> str:
    """探测代理或直连真实出口地区，并将时区与语言自动对齐到该地区（支持全球多国自适应）。"""
    info = probe_exit_region(proxy_url)
    if not info:
        return ""
    country = str(info.get("country") or "").upper()
    expect = str(expect_country or "").strip().upper()
    if expect and expect not in ("AUTO", "ANY", "*") and country != expect:
        return ""

    # 1. 优先使用 ip-api 接口返回的官方权威 timezone
    zone = str(info.get("timezone") or "").strip()
    # 2. 若无 timezone 且属于美国，使用州时区表补全
    if not zone and country == "US":
        zone = timezone_for_region(info.get("region"))
    # 3. 兜底查询常见国家预设表
    if not zone and country in _COUNTRY_PRESETS:
        zone = _COUNTRY_PRESETS[country]["timezone"]

    if zone:
        _config["us_consistency_timezone"] = zone
        # 自适应更新 locale 与 accept_language
        if country in _COUNTRY_PRESETS:
            preset = _COUNTRY_PRESETS[country]
            _config["us_consistency_locale"] = preset["locale"]
            globals()["DEFAULT_ACCEPT_LANGUAGE"] = preset["accept_lang"]
        _config["_detected_country"] = country
        _config["_detected_city"] = info.get("city")
        apply_process_timezone()
        return zone
    return ""


def enabled() -> bool:
    return bool(_config.get("us_consistency_enabled", True))


def timezone_name() -> str:
    return str(_config.get("us_consistency_timezone") or DEFAULT_TIMEZONE).strip() or DEFAULT_TIMEZONE


def locale_name() -> str:
    return str(_config.get("us_consistency_locale") or DEFAULT_LOCALE).strip() or DEFAULT_LOCALE


def apply_process_timezone() -> str:
    """在进程级设置 TZ，使 Chromium 子进程继承正确时区。

    Chromium 在 Linux 上读取 TZ 环境变量决定本地时区；这是最可靠、
    且不依赖 CDP 的注入方式（CDP 的 Emulation.setTimezoneOverride 只在
    已建立的 target 上生效，新开的 tab 需要重新注入）。

    返回实际生效的时区名。
    """
    if not enabled():
        return ""
    tz = timezone_name()
    os.environ["TZ"] = tz
    try:
        time.tzset()  # POSIX: 让 C 库立即感知新的 TZ
    except Exception:
        pass
    return tz


def apply_browser_options(options) -> None:
    """把一致性参数写进 DrissionPage ChromiumOptions。

    必须在 Chromium 启动前调用。同时负责指定浏览器可执行文件路径 ——
    容器/服务器环境里 DrissionPage 默认找不到 Chrome，不指定会直接报
    "Browser not found"。
    """
    if not enabled():
        return
    locale = locale_name()

    # 浏览器路径：容器内通常需要显式指定，否则 DrissionPage 找不到可执行文件。
    browser_path = detect_browser_path()
    if browser_path:
        try:
            options.set_browser_path(browser_path)
        except Exception:
            pass

    # --lang 影响 navigator.language 与 Accept-Language 的默认值。
    try:
        options.set_argument("--lang=%s" % locale)
    except Exception:
        pass

    # 关闭可能暴露自动化/精简环境的开关，并统一语言列表。
    # --no-sandbox / --disable-dev-shm-usage 是容器内运行的必需项：
    # 容器通常无特权且 /dev/shm 偏小，缺失会导致浏览器启动失败。
    for argument in (
        "--no-sandbox",
        "--disable-dev-shm-usage",
        "--accept-lang=%s" % DEFAULT_ACCEPT_LANGUAGE,
        "--disable-features=Translate,BackForwardCache,AcceptCHFrame,MediaRouter,OptimizationHints",
        "--disable-blink-features=AutomationControlled",
    ):
        try:
            options.set_argument(argument)
        except Exception:
            pass


def page_override_script() -> str:
    """返回注入到每个新文档的 JS：抹平 navigator 上的平台矛盾。

    注意 platform 不能只靠 CDP setUserAgentOverride —— 那个接口会顺带
    把 navigator.languages 覆盖成 ["en-US","en;q=0.9"] 这种非法形态，
    反而制造出新的异常。这里用 JS 直接定义 getter，更干净可控。
    """
    locale = locale_name()
    return """
(() => {
  const define = (obj, prop, value) => {
    try {
      Object.defineProperty(obj, prop, { get: () => value, configurable: true });
    } catch (e) {}
  };
  define(navigator, 'platform', '%(platform)s');
  define(navigator, 'hardwareConcurrency', %(cores)d);
  define(navigator, 'deviceMemory', %(memory)d);
  define(navigator, 'languages', ['%(locale)s', 'en']);
  define(navigator, 'language', '%(locale)s');
  define(navigator, 'webdriver', undefined);
  // 只改请求头不改 JS 是没用的：页面脚本照样能从 navigator 读到真实版本，
  // 服务端比对 User-Agent / Sec-CH-UA / navigator.userAgentData 就能发现伪造。
  define(navigator, 'userAgent', '%(ua)s');
  try {
    if (navigator.userAgentData) {
      const major = '%(major)s';
      const brands = [
        { brand: 'Not(A:Brand', version: '99' },
        { brand: 'Google Chrome', version: major },
        { brand: 'Chromium', version: major },
      ];
      Object.defineProperty(navigator, 'userAgentData', {
        get: () => ({
          brands: brands,
          mobile: false,
          platform: 'Windows',
          getHighEntropyValues: () => Promise.resolve({
            architecture: 'x86',
            bitness: '64',
            brands: brands,
            fullVersionList: brands.map((b) => ({ brand: b.brand, version: b.version + '.0.0.0' })),
            mobile: false,
            model: '',
            platform: 'Windows',
            platformVersion: '15.0.0',
            uaFullVersion: major + '.0.0.0',
          }),
        }),
        configurable: true,
      });
    }
  } catch (e) {}
  // Chrome 对象存在性：headless 下可能缺失，补齐以免被识别。
  if (!window.chrome) { window.chrome = {}; }
  if (!window.chrome.runtime) { window.chrome.runtime = {}; }
})();
""" % {
        "platform": DEFAULT_PLATFORM,
        "cores": DEFAULT_CORES,
        "memory": DEFAULT_DEVICE_MEMORY,
        "locale": locale,
        "ua": str((_config or {}).get("user_agent") or ""),
        "major": _chrome_version_from_ua(),
    }


def apply_page_overrides(page) -> bool:
    """对已打开的 page 注入一致性脚本与 Client Hints 覆盖。

    在每次导航前调用；DrissionPage 的 run_cdp 直连 CDP，注入后对
    后续导航持续生效（Page.addScriptToEvaluateOnNewDocument 是持久的）。
    """
    if not enabled() or page is None:
        return False
    ok = False
    try:
        page.run_cdp("Page.addScriptToEvaluateOnNewDocument", source=page_override_script())
        ok = True
    except Exception:
        pass
    # Client Hints 与 UA 自洽：UA 声称 Windows/Chrome，这些头必须跟上。
    # 必须一次性提交整组头：Network.setExtraHTTPHeaders 是"替换"语义，
    # 逐个调用只会留下最后一个，等于前面几个头从未生效。
    major = _chrome_version_from_ua()
    headers = {
        "Sec-CH-UA": _CH_UA_BRANDS % (major, major),
        "Sec-CH-UA-Platform": _CH_PLATFORM,
        "Sec-CH-UA-Platform-Version": _CH_PLATFORM_VERSION,
        "Accept-Language": DEFAULT_ACCEPT_LANGUAGE,
    }
    try:
        page.run_cdp("Network.setExtraHTTPHeaders", headers=headers)
    except Exception:
        pass
    return ok


def describe() -> str:
    """返回人类可读的一致性摘要，用于启动日志。"""
    if not enabled():
        return "网络环境一致性: 已关闭（原版行为）"
    country = _config.get("_detected_country") or "AUTO"
    return "网络环境一致性: 已启用 (出口国家=%s, 时区=%s, 语言=%s, 平台=%s)" % (
        country,
        timezone_name(),
        locale_name(),
        DEFAULT_PLATFORM,
    )
