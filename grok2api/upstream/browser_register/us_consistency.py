"""全球住宅家宽与代理网络环境一致性模块 (us_consistency)。

设计原则
--------
1. 全球自适应一致性：真实家宽出口属于哪个国家与地区，时区、语言、Accept-Language 就必须与该地 100% 同步对齐。
2. 杜绝硬编码美国：默认采用 AUTO 自动探测，家宽地区不要默认美国，优先 OpenAI 模型部署国家（如 JP / US / SG / GB 等）。
3. 消除指纹撕裂：抹平 IP 地理位置与浏览器时区、Locale、Client Hints 之间的矛盾。
4. 最小侵入与向后兼容：保持现有接口命名完全一致（align_timezone_with_proxy, describe, enabled 等）。
5. 可关闭：配置 us_consistency_enabled=false 即完全退回原版行为。
"""

from __future__ import annotations

import json
import os
import time
import urllib.request
from typing import Any

# OpenAI 模型官方主力部署与低延迟支持国家列表（按优先级推荐）
PREFERRED_MODEL_COUNTRIES = ["JP", "US", "SG", "GB", "DE", "FR", "CA", "AU", "KR", "NL"]

# 默认通用时区与语言（未识别时安全兜底，优先模型主力部署国 JP / US）
DEFAULT_TIMEZONE = "Asia/Tokyo"
DEFAULT_LOCALE = "ja-JP"

# 全球 40+ 常见国家代码 -> 官方权威时区、系统 Locale 与 Accept-Language 映射表
_COUNTRY_PRESETS = {
    "JP": {"timezone": "Asia/Tokyo", "locale": "ja-JP", "accept_lang": "ja,en-US;q=0.9,en;q=0.8", "name": "Japan"},
    "US": {"timezone": "America/New_York", "locale": "en-US", "accept_lang": "en-US,en;q=0.9", "name": "United States"},
    "SG": {"timezone": "Asia/Singapore", "locale": "en-SG", "accept_lang": "en-SG,en;q=0.9,zh-CN;q=0.8", "name": "Singapore"},
    "GB": {"timezone": "Europe/London", "locale": "en-GB", "accept_lang": "en-GB,en;q=0.9", "name": "United Kingdom"},
    "DE": {"timezone": "Europe/Berlin", "locale": "de-DE", "accept_lang": "de-DE,de;q=0.9,en-US;q=0.8", "name": "Germany"},
    "FR": {"timezone": "Europe/Paris", "locale": "fr-FR", "accept_lang": "fr-FR,fr;q=0.9,en-US;q=0.8", "name": "France"},
    "CA": {"timezone": "America/Toronto", "locale": "en-CA", "accept_lang": "en-CA,en;q=0.9", "name": "Canada"},
    "AU": {"timezone": "Australia/Sydney", "locale": "en-AU", "accept_lang": "en-AU,en;q=0.9", "name": "Australia"},
    "KR": {"timezone": "Asia/Seoul", "locale": "ko-KR", "accept_lang": "ko-KR,ko;q=0.9,en-US;q=0.8", "name": "South Korea"},
    "NL": {"timezone": "Europe/Amsterdam", "locale": "nl-NL", "accept_lang": "nl-NL,nl;q=0.9,en-US;q=0.8", "name": "Netherlands"},
    "SE": {"timezone": "Europe/Stockholm", "locale": "sv-SE", "accept_lang": "sv-SE,sv;q=0.9,en-US;q=0.8", "name": "Sweden"},
    "CH": {"timezone": "Europe/Zurich", "locale": "de-CH", "accept_lang": "de-CH,de;q=0.9,en-US;q=0.8", "name": "Switzerland"},
    "IT": {"timezone": "Europe/Rome", "locale": "it-IT", "accept_lang": "it-IT,it;q=0.9,en-US;q=0.8", "name": "Italy"},
    "ES": {"timezone": "Europe/Madrid", "locale": "es-ES", "accept_lang": "es-ES,es;q=0.9,en-US;q=0.8", "name": "Spain"},
    "TW": {"timezone": "Asia/Taipei", "locale": "zh-TW", "accept_lang": "zh-TW,zh;q=0.9,en-US;q=0.8", "name": "Taiwan"},
    "HK": {"timezone": "Asia/Hong_Kong", "locale": "zh-HK", "accept_lang": "zh-HK,zh;q=0.9,en-US;q=0.8", "name": "Hong Kong"},
    "IN": {"timezone": "Asia/Kolkata", "locale": "en-IN", "accept_lang": "en-IN,en;q=0.9", "name": "India"},
    "BR": {"timezone": "America/Sao_Paulo", "locale": "pt-BR", "accept_lang": "pt-BR,pt;q=0.9,en-US;q=0.8", "name": "Brazil"},
    "MX": {"timezone": "America/Mexico_City", "locale": "es-MX", "accept_lang": "es-MX,es;q=0.9,en-US;q=0.8", "name": "Mexico"},
    "NZ": {"timezone": "Pacific/Auckland", "locale": "en-NZ", "accept_lang": "en-NZ,en;q=0.9", "name": "New Zealand"},
    "IE": {"timezone": "Europe/Dublin", "locale": "en-IE", "accept_lang": "en-IE,en;q=0.9", "name": "Ireland"},
    "NO": {"timezone": "Europe/Oslo", "locale": "no-NO", "accept_lang": "no-NO,no;q=0.9,en-US;q=0.8", "name": "Norway"},
    "FI": {"timezone": "Europe/Helsinki", "locale": "fi-FI", "accept_lang": "fi-FI,fi;q=0.9,en-US;q=0.8", "name": "Finland"},
    "DK": {"timezone": "Europe/Copenhagen", "locale": "da-DK", "accept_lang": "da-DK,da;q=0.9,en-US;q=0.8", "name": "Denmark"},
    "PL": {"timezone": "Europe/Warsaw", "locale": "pl-PL", "accept_lang": "pl-PL,pl;q=0.9,en-US;q=0.8", "name": "Poland"},
    "BE": {"timezone": "Europe/Brussels", "locale": "nl-BE", "accept_lang": "nl-BE,nl;q=0.9,en-US;q=0.8", "name": "Belgium"},
    "AT": {"timezone": "Europe/Vienna", "locale": "de-AT", "accept_lang": "de-AT,de;q=0.9,en-US;q=0.8", "name": "Austria"},
    "MY": {"timezone": "Asia/Kuala_Lumpur", "locale": "en-MY", "accept_lang": "en-MY,en;q=0.9,ms;q=0.8", "name": "Malaysia"},
    "TH": {"timezone": "Asia/Bangkok", "locale": "th-TH", "accept_lang": "th-TH,th;q=0.9,en-US;q=0.8", "name": "Thailand"},
    "PH": {"timezone": "Asia/Manila", "locale": "en-PH", "accept_lang": "en-PH,en;q=0.9,tl;q=0.8", "name": "Philippines"},
    "VN": {"timezone": "Asia/Ho_Chi_Minh", "locale": "vi-VN", "accept_lang": "vi-VN,vi;q=0.9,en-US;q=0.8", "name": "Vietnam"},
    "ID": {"timezone": "Asia/Jakarta", "locale": "id-ID", "accept_lang": "id-ID,id;q=0.9,en-US;q=0.8", "name": "Indonesia"},
    "IL": {"timezone": "Asia/Jerusalem", "locale": "he-IL", "accept_lang": "he-IL,he;q=0.9,en-US;q=0.8", "name": "Israel"},
    "AE": {"timezone": "Asia/Dubai", "locale": "ar-AE", "accept_lang": "ar-AE,ar;q=0.9,en-US;q=0.8", "name": "United Arab Emirates"},
    "TR": {"timezone": "Europe/Istanbul", "locale": "tr-TR", "accept_lang": "tr-TR,tr;q=0.9,en-US;q=0.8", "name": "Turkey"},
    "ZA": {"timezone": "Africa/Johannesburg", "locale": "en-ZA", "accept_lang": "en-ZA,en;q=0.9", "name": "South Africa"},
}

DEFAULT_PLATFORM = "Win32"
DEFAULT_ACCEPT_LANGUAGE = "ja,en-US;q=0.9,en;q=0.8"
DEFAULT_CORES = 8
DEFAULT_DEVICE_MEMORY = 8

_CH_PLATFORM = '"Windows"'
_CH_PLATFORM_VERSION = '"15.0.0"'

def _chrome_version_from_ua() -> str:
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
_config: dict[str, Any] = {}

_BROWSER_CANDIDATES = (
    "/usr/bin/google-chrome",
    "/usr/bin/google-chrome-stable",
    "/usr/bin/chromium",
    "/usr/bin/chromium-browser",
    "/opt/google/chrome/chrome",
    "/root/.cache/ms-playwright/chromium-1223/chrome-linux64/chrome",
)

def _candidate_from_env() -> str:
    direct = str(os.environ.get("GROK_BROWSER_PATH") or "").strip()
    if direct and os.path.isfile(direct) and os.access(direct, os.X_OK):
        return direct
    root = str(os.environ.get("PLAYWRIGHT_BROWSERS_PATH") or "").strip()
    if root and os.path.isdir(root):
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
    global _config
    _config = config_ref if isinstance(config_ref, dict) else {}

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
    zone = timezone_for_region(region)
    if not zone:
        return ""
    _config["us_consistency_timezone"] = zone
    apply_process_timezone()
    return zone

def probe_exit_region(proxy_url="", timeout=15) -> dict[str, Any]:
    """经代理或直连探测出口的国家与地区，返回真实地理信息 dict。"""
    urls = [
        "http://ip-api.com/json/?fields=status,country,countryCode,regionName,city,timezone,isp,hosting,proxy,mobile,query",
        "https://ipwho.is/",
    ]
    raw = str(proxy_url or "").strip()
    for url in urls:
        try:
            if raw:
                opener = urllib.request.build_opener(
                    urllib.request.ProxyHandler({"http": raw, "https": raw})
                )
            else:
                opener = urllib.request.build_opener()
            request = urllib.request.Request(url, headers={"User-Agent": "curl/7.88.1", "Accept": "application/json"})
            with opener.open(request, timeout=timeout) as response:
                data = json.loads(response.read().decode("utf-8", "replace"))
            if not isinstance(data, dict):
                continue
            if data.get("status") == "success" or data.get("success") is True or "timezone" in data:
                tz_val = data.get("timezone")
                if isinstance(tz_val, dict):
                    tz_str = tz_val.get("id") or tz_val.get("name") or ""
                else:
                    tz_str = str(tz_val or "")
                country_code = str(data.get("countryCode") or data.get("country_code") or "").upper()
                return {
                    "ip": str(data.get("query") or data.get("ip") or ""),
                    "country": country_code,
                    "country_name": str(data.get("country") or ""),
                    "region": str(data.get("regionName") or data.get("region") or ""),
                    "city": str(data.get("city") or ""),
                    "timezone": tz_str,
                    "isp": str(data.get("isp") or ""),
                    "hosting": bool(data.get("hosting")),
                    "proxy": bool(data.get("proxy")),
                }
        except Exception:
            continue
    return {}

def align_timezone_with_proxy(proxy_url="", expect_country="") -> str:
    """探测代理或直连真实出口地区，并将时区与语言自动同区对齐（支持全球家宽自适应）。"""
    info = probe_exit_region(proxy_url)
    country = str(info.get("country") or "").upper()
    expect = str(expect_country or "").strip().upper()

    # 当未指定或为 AUTO / ANY / * 时，无条件对齐真实家宽出口国家地区
    if expect and expect not in ("AUTO", "ALL", "RAND", "ANY", "*") and country and country != expect:
        return ""

    zone = ""
    # 1. 优先使用探测接口返回的官方权威 timezone
    if info.get("timezone"):
        zone = str(info.get("timezone")).strip()

    # 2. 若未返回时区且出口为美国，根据州全称/缩写精准映射
    if not zone and country == "US":
        zone = timezone_for_region(info.get("region"))

    # 3. 若仍无时区，查预设主流国家时区字典
    if not zone and country in _COUNTRY_PRESETS:
        zone = _COUNTRY_PRESETS[country]["timezone"]

    # 4. 如果连国家都没探测出来（极罕见），按模型部署首选国（JP/US）智能兜底
    if not zone:
        preferred = _config.get("us_consistency_preferred_country") or "JP"
        if preferred in _COUNTRY_PRESETS:
            zone = _COUNTRY_PRESETS[preferred]["timezone"]
            country = preferred
        else:
            zone = DEFAULT_TIMEZONE
            country = "JP"

    if zone:
        _config["us_consistency_timezone"] = zone
        # 自适应更新 locale 与 Accept-Language
        if country in _COUNTRY_PRESETS:
            preset = _COUNTRY_PRESETS[country]
            _config["us_consistency_locale"] = preset["locale"]
            globals()["DEFAULT_ACCEPT_LANGUAGE"] = preset["accept_lang"]
        else:
            # 未在字典中的国家，构建标准该国 locale
            derived_locale = f"en-{country}" if country else "en-US"
            _config["us_consistency_locale"] = derived_locale
            globals()["DEFAULT_ACCEPT_LANGUAGE"] = f"{derived_locale},en;q=0.9"

        _config["_detected_country"] = country
        _config["_detected_city"] = info.get("city") or info.get("region") or country
        _config["_detected_region"] = info.get("region") or info.get("city") or country
        apply_process_timezone()
        return zone
    return ""

def enabled() -> bool:
    return bool(_config.get("us_consistency_enabled", True))

def timezone_name() -> str:
    return str(_config.get("us_consistency_timezone") or DEFAULT_TIMEZONE).strip() or DEFAULT_TIMEZONE

def locale_name() -> str:
    return str(_config.get("us_consistency_locale") or DEFAULT_LOCALE).strip() or DEFAULT_LOCALE

def location_summary() -> str:
    """返回对齐的出口位置摘要，例如 'Tokyo / Asia/Tokyo' 或 'Florida / America/New_York'。"""
    region = _config.get("_detected_region") or _config.get("_detected_city") or _config.get("_detected_country") or "AUTO"
    return f"{region} / {timezone_name()}"

def apply_process_timezone() -> str:
    if not enabled():
        return ""
    tz = timezone_name()
    os.environ["TZ"] = tz
    try:
        time.tzset()
    except Exception:
        pass
    return tz

def apply_browser_options(options) -> None:
    if not enabled():
        return
    locale = locale_name()
    browser_path = detect_browser_path()
    if browser_path:
        try:
            options.set_browser_path(browser_path)
        except Exception:
            pass

    try:
        options.set_argument("--lang=%s" % locale)
    except Exception:
        pass

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
    if not enabled() or page is None:
        return False
    ok = False
    try:
        page.run_cdp("Page.addScriptToEvaluateOnNewDocument", source=page_override_script())
        ok = True
    except Exception:
        pass
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
    loc_desc = location_summary()
    return "网络环境一致性: 自动对齐出口所在地 (%s) 时区与语言 (%s, 平台=%s)" % (
        loc_desc,
        locale_name(),
        DEFAULT_PLATFORM,
    )
