"""管理主注册浏览器生命周期并实现注册页面自动化操作。"""
import gc
import json
import random
import re
import secrets
import struct
import time

try:  # DrissionPage 仅在 GROK2API_BROWSER_ENGINE=chromium 回退时可用
    from DrissionPage import Chromium
    from DrissionPage.errors import ContextLostError, JavaScriptError, PageDisconnectedError
except Exception:  # pragma: no cover - Camoufox 模式下无需 DrissionPage
    Chromium = None

    class ContextLostError(Exception):
        """DrissionPage 缺失时的占位异常。"""

    class JavaScriptError(Exception):
        """DrissionPage 缺失时的占位异常。"""

    class PageDisconnectedError(Exception):
        """DrissionPage 缺失时的占位异常。"""

from curl_cffi import requests
from proxy_pool import ProxyTransportError, safe_proxy_error_text
# browser_runtime / mail_service / app_config own helpers this module calls.
# These imports (and the three cancel helpers below) were lost when the
# grok-register engine was folded into grokcli-2api: upstream, the grok_register_ttk
# module published them through bind_runtime(). Without them every browser start
# died with e.g.
#   "浏览器启动失败，已重试4次: name 'prepare_browser_proxy' is not defined"
#   "浏览器启动失败，已重试4次: name 'create_browser_options' is not defined"
# No module here imports registration_browser, so these imports are cycle-free.
import browser_runtime
from browser_runtime import (
    browser_engine,
    create_browser_options,
    get_configured_proxy,
    get_proxies,
    is_proxy_connection_error,
    page_has_proxy_error,
    prepare_browser_proxy,
)
from mail_service import get_email_and_token, get_oai_code, get_user_agent
from app_config import config
from cancel_utils import AccountRetryNeeded, raise_if_cancelled, sleep_with_cancel

browser = None
page = None
browser_proxy_bridge = None
browser_started_with_proxy = False
cf_clearance = ""
SIGNUP_URL = "https://accounts.x.ai/sign-up?redirect=grok-com"

TURNSTILE_ABSENT = "ABSENT"
TURNSTILE_LOADING = "LOADING"
TURNSTILE_WAITING = "WAITING"
TURNSTILE_SOLVED = "SOLVED"
TURNSTILE_FAILED = "FAILED"


def _mark_registration_stage(stage):
    # Function-local import avoids an import-time cycle while keeping the browser module reusable.
    from registration_flow import _set_registration_stage
    return _set_registration_stage(stage)


def _managed_proxy_mode():
    try:
        return str(config.get("proxy_mode", "auto") or "auto").strip().lower() in ("single", "pool")
    except Exception:
        return False


def _is_pre_submit_js_transient(exc):
    if isinstance(exc, (TimeoutError, ContextLostError, PageDisconnectedError)):
        return True
    if isinstance(exc, RuntimeError):
        message = str(exc or "").lower()
        return "js结果解析错误" in message or "js result parsing error" in message
    return False


def _run_pre_submit_js(script, *args):
    try:
        return page.run_js(script, *args)
    except Exception as exc:
        if _is_pre_submit_js_transient(exc):
            raise AccountRetryNeeded(f"提交前浏览器 JS 暂时失败: {exc}") from exc
        raise
_OWN_NAMES = {'is_cloudflare_block_response', 'response_preview', 'start_browser', 'enable_nsfw_for_token', 'stop_browser_proxy_bridge', 'set_tos_accepted', 'fill_email_and_submit', 'getTurnstileToken', 'set_birth_date', 'generate_random_birthdate', 'fill_profile_and_submit', 'click_email_signup_button', 'wait_for_sso_cookie', 'fill_code_and_submit', 'build_profile', 'cleanup_runtime_memory', 'open_signup_page', 'stop_browser', 'encode_grpc_nsfw_settings', 'restart_browser', 'has_profile_form', 'update_nsfw_settings', 'refresh_active_page', 'check_imagine_capability', 'browser_engine', 'browser_runtime'}
# 注意：grok_register_ttk 会用 _make_compat_proxy 把本模块的同名函数包装成
# 转发代理，再经 bind_runtime 把整个命名空间注回这里。凡是会被转发的名字
# 都必须列在 _OWN_NAMES 里，否则 bind_runtime 会用「指向本模块的代理」覆盖
# 掉真函数，代理再转发回自己 —— 无限递归。
# 实测踩过：check_imagine_capability 漏登记，调用时报
# "maximum recursion depth exceeded while calling a Python object"。
# tests/test_module_compatibility.py 里有测试守着这条。


def bind_runtime(namespace):
    for name, value in namespace.items():
        if name.startswith("__") or name in _OWN_NAMES or name in {
            "browser", "page", "browser_proxy_bridge", "browser_started_with_proxy", "cf_clearance",
        }:
            continue
        globals()[name] = value


def generate_random_birthdate():
    import datetime as dt

    today = dt.date.today()
    age = random.randint(20, 40)
    birth_year = today.year - age
    birth_month = random.randint(1, 12)
    birth_day = random.randint(1, 28)
    return f"{birth_year}-{birth_month:02d}-{birth_day:02d}T16:00:00.000Z"

def response_preview(res, limit=200):
    try:
        text = str(res.text or "")
    except Exception:
        text = ""
    text = re.sub(r"\s+", " ", text).strip()
    return text[:limit]

def is_cloudflare_block_response(res):
    try:
        headers = {str(k).lower(): str(v).lower() for k, v in dict(res.headers).items()}
        text = str(res.text or "").lower()
        server = headers.get("server", "")
        content_type = headers.get("content-type", "")
        return (
            res.status_code in (403, 429, 503)
            and (
                "cloudflare" in server
                or "cloudflare" in text
                or "cf-error" in text
                or "__cf_chl" in text
                or "text/html" in content_type
            )
        )
    except Exception:
        return False

def set_birth_date(session, log_callback=None):
    url = "https://grok.com/rest/auth/set-birth-date"
    new_headers = {
        "content-type": "application/json",
        "origin": "https://grok.com",
        "referer": "https://grok.com/",
    }
    payload = {"birthDate": generate_random_birthdate()}
    try:
        res = session.post(url, json=payload, headers=new_headers, timeout=15)
        if log_callback:
            log_callback(
                f"[Debug] set_birth_date status: {res.status_code}, body: {response_preview(res)}"
            )
        if 200 <= res.status_code < 300:
            return True, "ok"
        if is_cloudflare_block_response(res):
            return (
                False,
                "set_birth_date 被 grok.com 的 Cloudflare 防护拦截，HTTP "
                f"{res.status_code}",
            )
        return False, f"set_birth_date HTTP {res.status_code}: {response_preview(res)}"
    except Exception as e:
        if log_callback:
            log_callback(f"[set_birth_date] 异常: {e}")
        return False, f"set_birth_date 异常: {e}"

def set_tos_accepted(session, log_callback=None):
    url = "https://accounts.x.ai/auth_mgmt.AuthManagement/SetTosAcceptedVersion"
    payload = struct.pack("B", (2 << 3) | 0) + struct.pack("B", 1)
    data = b"\x00" + struct.pack(">I", len(payload)) + payload
    new_headers = {
        "content-type": "application/grpc-web+proto",
        "x-grpc-web": "1",
        "x-user-agent": "connect-es/2.1.1",
        "origin": "https://accounts.x.ai",
        "referer": "https://accounts.x.ai/accept-tos",
    }
    try:
        res = session.post(url, data=data, headers=new_headers, timeout=15)
        if log_callback:
            log_callback(f"[Debug] set_tos_accepted status: {res.status_code}")
        if 200 <= res.status_code < 300:
            return True, "ok"
        if is_cloudflare_block_response(res):
            return (
                False,
                "set_tos_accepted 被 accounts.x.ai 的 Cloudflare 防护拦截，HTTP "
                f"{res.status_code}",
            )
        return False, f"set_tos_accepted HTTP {res.status_code}: {response_preview(res)}"
    except Exception as e:
        if log_callback:
            log_callback(f"[set_tos_accepted] 异常: {e}")
        return False, f"set_tos_accepted 异常: {e}"

def encode_grpc_nsfw_settings():
    field1_content = bytes([0x10, 0x01])
    field1 = bytes([0x0A, len(field1_content)]) + field1_content
    nsfw_string = b"always_show_nsfw_content"
    field2_inner = bytes([0x0A, len(nsfw_string)]) + nsfw_string
    field2 = bytes([0x12, len(field2_inner)]) + field2_inner
    payload = field1 + field2
    return b"\x00" + struct.pack(">I", len(payload)) + payload

def update_nsfw_settings(session, log_callback=None):
    url = "https://grok.com/auth_mgmt.AuthManagement/UpdateUserFeatureControls"
    data = encode_grpc_nsfw_settings()
    new_headers = {
        "content-type": "application/grpc-web+proto",
        "x-grpc-web": "1",
        "origin": "https://grok.com",
        "referer": "https://grok.com/",
    }
    try:
        res = session.post(url, data=data, headers=new_headers, timeout=15)
        if log_callback:
            log_callback(
                f"[Debug] update_nsfw status: {res.status_code}, body: {response_preview(res)}"
            )
        if 200 <= res.status_code < 300:
            return True, "ok"
        if is_cloudflare_block_response(res):
            return (
                False,
                "update_nsfw_settings 被 grok.com 的 Cloudflare 防护拦截，HTTP "
                f"{res.status_code}",
            )
        return False, f"update_nsfw_settings HTTP {res.status_code}: {response_preview(res)}"
    except Exception as e:
        if log_callback:
            log_callback(f"[update_nsfw] 异常: {e}")
        return False, f"update_nsfw_settings 异常: {e}"

def enable_nsfw_for_token(token, cf_clearance="", log_callback=None):
    proxies = get_proxies()
    user_agent = get_user_agent()
    try:
        with requests.Session(impersonate="chrome120", proxies=proxies) as session:
            cookie_parts = [f"sso={token}", f"sso-rw={token}"]
            if cf_clearance:
                cookie_parts.append(f"cf_clearance={cf_clearance}")
            session.headers.update(
                {
                    "user-agent": user_agent,
                    "cookie": "; ".join(cookie_parts),
                }
            )
            ok, message = set_tos_accepted(session, log_callback)
            if not ok:
                return False, message
            ok, message = set_birth_date(session, log_callback)
            if not ok:
                return False, message
            ok, message = update_nsfw_settings(session, log_callback)
            if not ok:
                return False, message
            return True, "成功开启 NSFW"
    except Exception as e:
        return False, f"异常: {str(e)}"

def check_imagine_capability(log_callback=None, cancel_callback=None):
    """用当前已登录的浏览器会话实测 Grok Imagine 生图能力。

    为什么必须在注册流程里做：保存下来的 sso cookie 之后会被服务端轮换。
    实测拿它去调 grok.com 的 gRPC 接口，HTTP 状态码仍是 200，但
    grpc-status=16 + "Bad credentials [WKE=unauthenticated:bad-credentials]"
    —— 只看 HTTP 状态码会被骗。所以只有注册当时浏览器里的会话有效。

    判定方式是读 Imagine 页面本身，而不是自己拼 REST 请求：直接 POST
    /rest/app-chat/conversations/new 会被 xAI 以 403 code:7
    "This page is out of date. Reload to continue." 拒掉（缺少页面上下文
    令牌），那个 403 说明不了额度，只能说明请求构造得不对。

    返回 (ok, detail)：
      True  → 生图界面可用（没有额度/订阅拦截）
      False → 明确提示需要 credits 或订阅
      None  → 探测本身没做成（页面没就绪等），不代表账号能力
    """
    global page
    if page is None:
        return None, "浏览器未就绪"
    raise_if_cancelled(cancel_callback)
    try:
        page.get("https://grok.com/imagine", timeout=45)
        page.wait.doc_loaded()
    except Exception as exc:
        return None, f"打开 Imagine 页面失败: {exc}"
    sleep_with_cancel(7, cancel_callback)

    try:
        raw = page.run_js(r"""
const body = document.body ? document.body.innerText : '';
return JSON.stringify({
  url: location.href,
  text: body.slice(0, 4000),
  hasTextarea: !!document.querySelector('textarea, [contenteditable="true"]'),
  hasUpgradeCta: /out of credits|need a grok subscription|upgrade to|subscribe to|spending limit/i.test(body),
});
""")
        data = json.loads(raw) if isinstance(raw, str) else (raw or {})
    except Exception as exc:
        return None, f"读取 Imagine 页面失败: {exc}"

    text = str(data.get("text") or "")
    lowered = text.lower()
    if data.get("hasUpgradeCta"):
        return False, "无生图额度：页面提示需要 credits 或 Grok 订阅"
    if "sign in" in lowered and "sign up" in lowered and not data.get("hasTextarea"):
        return None, "会话未被识别为已登录，未能判定"
    if data.get("hasTextarea"):
        # 有输入框只是界面层面，说明不了额度。再实际提交一次生成请求，
        # 看额度是否真的放行 —— 拦截通常要到点生成时才弹出来。
        gen_ok, gen_detail = _try_generate_image(cancel_callback)
        if gen_ok is not None:
            return gen_ok, gen_detail
        return True, "Imagine 界面可用（有生图输入框，未见额度拦截）；实际生成未判定: %s" % gen_detail
    return None, f"未判定（url={data.get('url')}）：{text[:160]}"


def _try_generate_image(cancel_callback=None):
    """在已打开的 Imagine 页面上真的提交一次生图，看额度是否放行。

    返回 (True, ...) 生成了图片 / (False, ...) 被额度拦住 / (None, ...) 没判定出来。
    """
    global page
    if page is None:
        return None, "浏览器未就绪"
    before = set()
    try:
        state = page.run_js(r"""
const imgs = Array.from(document.querySelectorAll('img'))
  .map(i => i.currentSrc || i.src || '')
  .filter(Boolean);
return JSON.stringify({imgs: imgs});
""")
        before = set(json.loads(state).get("imgs") or [])
    except Exception:
        pass

    box = None
    box_loc = ""
    for loc in ("tag:textarea", "@contenteditable=true", "tag:input@@type=text"):
        try:
            box = page.ele(loc, timeout=3)
        except Exception:
            box = None
        if box:
            box_loc = loc
            break
    if box is None:
        return None, "找不到生图输入框"

    try:
        box.clear()
        box.input("a red apple on a white background")
        sleep_with_cancel(1, cancel_callback)
        # contenteditable 上敲 "\n" 并不会触发发送，得点发送按钮或真按回车。
        sent = ""
        for loc in ("tag:button@@aria-label*=end", "tag:button@@aria-label*=Send",
                    "tag:button@@type=submit", "tag:button@@text()=Send"):
            try:
                btn = page.ele(loc, timeout=2)
            except Exception:
                btn = None
            if btn:
                btn.click()
                sent = "按钮 %s" % loc
                break
        if not sent:
            try:
                from DrissionPage.common import Keys
                box.input(Keys.ENTER)
                sent = "回车键"
            except Exception as exc:
                return None, f"没找到发送按钮且回车失败: {exc}"
    except Exception as exc:
        return None, f"提交生图请求失败: {exc}"

    blocked_re = "out of credits|need a grok subscription|upgrade to|spending limit|not available in your"
    last_tail = ""
    for _ in range(50):
        sleep_with_cancel(3, cancel_callback)
        raise_if_cancelled(cancel_callback)
        try:
            state = page.run_js(r"""
const body = document.body ? document.body.innerText : '';
const imgs = Array.from(document.querySelectorAll('img'))
  .map(i => i.currentSrc || i.src || '')
  .filter(Boolean);
return JSON.stringify({
  imgs: imgs,
  blocked: /%s/i.test(body),
  tail: body.slice(-500),
});
""" % blocked_re)
            st = json.loads(state) if isinstance(state, str) else (state or {})
        except Exception:
            continue
        if st.get("blocked"):
            return False, "提交后提示需要 credits 或订阅"
        last_tail = str(st.get("tail") or last_tail)
        new_imgs = [u for u in (st.get("imgs") or []) if u not in before]
        if new_imgs:
            return True, "已成功生成图片（页面出现新的生成结果）"
    return None, "提交后 150 秒内没看到新图片（提交方式=%s，输入框=%s，提交前图片数=%d）；页面尾部: %s" % (
        sent, box_loc, len(before), last_tail.replace("\n", " ")[:220])

def stop_browser_proxy_bridge():
    global browser_proxy_bridge
    if browser_proxy_bridge is not None:
        try:
            browser_proxy_bridge.stop()
        except Exception:
            pass
    browser_proxy_bridge = None

def start_browser(log_callback=None, use_proxy=True):
    global browser, page, browser_proxy_bridge, browser_started_with_proxy
    last_exc = None
    proxy_enabled = bool(use_proxy and get_configured_proxy())
    for attempt in range(1, 5):
        bridge = None
        try:
            browser_proxy, bridge = prepare_browser_proxy(use_proxy=use_proxy, log_callback=log_callback)
            # 时区必须与代理真实出口地区一致，且必须在 Chromium 启动前确定：
            # Chromium 在启动时读取 TZ 环境变量，启动后再改不会生效。
            # 住宅代理的实际落地州无法预先指定（实测下单参数不生效），
            # 所以这里以探测到的真实出口地区回写时区，避免出现
            # 「IP 在加州、时区却是纽约」这类矛盾特征。
            if browser_proxy:
                try:
                    import us_consistency
                    if us_consistency.enabled():
                        expect = "AUTO"
                        try:
                            expect = str(config.get("us_consistency_expect_country") or "AUTO")
                        except Exception:
                            pass
                        zone = us_consistency.align_timezone_with_proxy(browser_proxy, expect_country=expect)
                        if zone and log_callback:
                            log_callback("[*] 已按代理实际出口对齐时区: %s" % zone)
                except Exception:
                    pass
            engine = browser_runtime.browser_engine()
            options = create_browser_options(browser_proxy=browser_proxy)
            if engine == "camoufox":
                # Camoufox（加固版 Firefox，Juggler 协议 + 指纹伪造）能拿到
                # cf_clearance；Chromium 走 CDP，2026-09-28 起必被 Cloudflare
                # Bot Management 拦截，注册静默失败。
                from camoufox_compat import CamoufoxBrowser
                browser = CamoufoxBrowser(options)
            else:
                browser = Chromium(options)
            browser_proxy_bridge = bridge
            browser_started_with_proxy = bool(browser_proxy)
            tabs = browser.get_tabs()
            page = tabs[-1] if tabs else browser.new_tab()
            # 美国环境一致性：注入 navigator 覆盖脚本与 Client Hints，
            # 使 JS 可见指纹与 UA 声称的 Windows 桌面环境保持一致。
            # Camoufox 走 Juggler 且自带完整指纹伪造，没有 run_cdp，
            # 手动注入 Chromium 风格的 Client Hints 反而与 Firefox 冲突。
            if engine != "camoufox":
                try:
                    import us_consistency
                    if us_consistency.enabled():
                        us_consistency.apply_page_overrides(page)
                        if log_callback:
                            log_callback("[*] %s" % us_consistency.describe())
                except Exception:
                    pass
            if log_callback and getattr(browser, "user_data_path", None):
                log_callback(f"[Debug] 当前浏览器资料目录: {browser.user_data_path}")
            if log_callback and get_configured_proxy():
                mode = "代理" if browser_started_with_proxy else "直连"
                log_callback(f"[*] 浏览器网络模式: {mode}")
            if log_callback and attempt > 1:
                log_callback(f"[*] 浏览器第 {attempt} 次启动成功")
            return browser, page
        except Exception as exc:
            last_exc = exc
            if bridge is not None:
                try:
                    bridge.stop()
                except Exception:
                    pass
            if log_callback:
                mode = "代理" if proxy_enabled else "直连"
                log_callback(f"[Debug] 浏览器{mode}启动失败(第{attempt}/4次): {exc}")
            try:
                if browser is not None:
                    browser.quit(del_data=True)
            except Exception:
                pass
            browser = None
            page = None
            browser_proxy_bridge = None
            browser_started_with_proxy = False
            time.sleep(min(1.5 * attempt, 4))
    if _managed_proxy_mode() and is_proxy_connection_error(last_exc):
        raise ProxyTransportError("Chromium 通过当前代理启动失败: %s" % safe_proxy_error_text(last_exc)) from last_exc
    raise Exception(f"浏览器启动失败，已重试4次: {last_exc}")

def stop_browser():
    global browser, page, browser_started_with_proxy
    if browser is not None:
        try:
            browser.quit(del_data=True)
        except Exception:
            pass
    stop_browser_proxy_bridge()
    browser = None
    page = None
    browser_started_with_proxy = False

def restart_browser(log_callback=None, use_proxy=True):
    stop_browser()
    return start_browser(log_callback=log_callback, use_proxy=use_proxy)

def cleanup_runtime_memory(log_callback=None, reason="定期清理"):
    if log_callback:
        log_callback(f"[*] {reason}: 关闭浏览器并清理内存")
    stop_browser()
    try:
        from cpa_xai.browser_confirm import shutdown_mint_browsers
        shutdown_mint_browsers()
    except Exception as exc:
        if log_callback:
            log_callback(f"[Debug] CPA 浏览器清理失败: {exc}")
    collected = gc.collect()
    if log_callback:
        log_callback(f"[*] Python GC 已回收对象数: {collected}")

def refresh_active_page():
    global browser, page
    if browser is None:
        restart_browser()
    try:
        tabs = browser.get_tabs()
        if tabs:
            page = tabs[-1]
        else:
            page = browser.new_tab()
    except Exception:
        restart_browser()
    return page

def click_email_signup_button(timeout=10, log_callback=None, cancel_callback=None):
    global page
    deadline = time.time() + timeout
    while time.time() < deadline:
        raise_if_cancelled(cancel_callback)
        if log_callback:
            log_callback("[Debug] 尝试查找“使用邮箱注册”按钮...")

        clicked = _run_pre_submit_js(r"""
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}
function nodeText(node) {
    return [
        node.innerText,
        node.textContent,
        node.getAttribute('aria-label'),
        node.getAttribute('title'),
        node.getAttribute('href'),
    ].filter(Boolean).join(' ').replace(/\s+/g, ' ').trim();
}
function scoreEntry(node) {
    const compact = nodeText(node).replace(/\s+/g, '');
    const lower = compact.toLowerCase();
    if (compact.includes('使用邮箱注册')) return 100;
    if (lower.includes('signupwithemail')) return 95;
    if (lower.includes('continuewithemail')) return 90;
    if (lower.includes('email') && (lower.includes('sign') || lower.includes('continue') || lower.includes('use') || lower.includes('with'))) return 80;
    if (lower === 'email' || lower.includes('邮箱')) return 70;
    return 0;
}
const candidates = Array.from(document.querySelectorAll('button, a, [role="button"]'))
    .filter((node) => isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true')
    .map((node) => ({ node, score: scoreEntry(node), text: nodeText(node) }))
    .filter((item) => item.score > 0)
    .sort((a, b) => b.score - a.score);
const target = candidates[0]?.node || null;
if (!target) {
    return false;
}
target.click();
return candidates[0].text || true;
        """)

        if clicked:
            if log_callback:
                detail = f": {clicked}" if isinstance(clicked, str) else ""
                log_callback(f"[*] 已点击「使用邮箱注册」按钮{detail}")
            sleep_with_cancel(2, cancel_callback)
            return True

        if log_callback:
            current_url = page.url if page else "none"
            log_callback(f"[Debug] 当前URL: {current_url}")

        sleep_with_cancel(1, cancel_callback)

    if log_callback:
        page_html = page.html[:500] if page else "no page"
        log_callback(f"[Debug] 页面内容片段: {page_html}")

    raise Exception("未找到「使用邮箱注册」按钮")

def open_signup_page(log_callback=None, cancel_callback=None):
    global browser, page
    raise_if_cancelled(cancel_callback)
    if browser is None:
        start_browser(log_callback=log_callback)
        if log_callback:
            log_callback("[*] 浏览器已启动")

    def _open_with_current_browser():
        global page
        try:
            page = browser.get_tab(0)
            page.get(SIGNUP_URL)
        except Exception as e:
            if log_callback:
                log_callback(f"[Debug] 打开URL异常: {e}")
            page = browser.new_tab(SIGNUP_URL)
        page.wait.doc_loaded()

    try:
        _open_with_current_browser()
    except Exception as e:
        if browser_started_with_proxy and get_configured_proxy():
            if _managed_proxy_mode():
                raise ProxyTransportError("当前代理访问注册页失败: %s" % safe_proxy_error_text(e)) from e
            if log_callback:
                log_callback(f"[!] 浏览器代理访问注册页失败，自动回退直连: {e}")
            restart_browser(log_callback=log_callback, use_proxy=False)
            _open_with_current_browser()
        else:
            raise

    if browser_started_with_proxy and page_has_proxy_error(page):
        if _managed_proxy_mode():
            raise ProxyTransportError("Chromium 页面显示代理连接错误")
        if log_callback:
            log_callback("[!] 浏览器页面显示代理错误，自动回退直连")
        restart_browser(log_callback=log_callback, use_proxy=False)
        _open_with_current_browser()

    sleep_with_cancel(2, cancel_callback)
    if log_callback:
        log_callback(f"[*] 当前URL: {page.url}")
    # 拟人化：打开注册页后模拟人类视读停留
    human_read_sec = random.uniform(1.2, 2.5)
    if log_callback:
        log_callback(f"[拟人化] 视读页面停留 ({human_read_sec:.1f}s)...")
    sleep_with_cancel(human_read_sec, cancel_callback)
    click_email_signup_button(
        log_callback=log_callback, cancel_callback=cancel_callback
    )

def has_profile_form(log_callback=None):
    refresh_active_page()
    try:
        return bool(
            page.run_js(
                """
const givenInput = document.querySelector('input[data-testid="givenName"], input[name="givenName"], input[autocomplete="given-name"]');
const familyInput = document.querySelector('input[data-testid="familyName"], input[name="familyName"], input[autocomplete="family-name"]');
const passwordInput = document.querySelector('input[data-testid="password"], input[name="password"], input[type="password"]');
return !!(givenInput && familyInput && passwordInput);
            """
            )
        )
    except Exception:
        return False

def fill_email_and_submit(timeout=45, log_callback=None, cancel_callback=None, on_mail_created=None):
    raise_if_cancelled(cancel_callback)
    email, dev_token = get_email_and_token()
    if not email or not dev_token:
        raise Exception("获取邮箱失败")
    if log_callback:
        log_callback(f"[*] 已创建邮箱: {email}")
    if on_mail_created is not None:
        try:
            persisted = on_mail_created(email, dev_token)
            if persisted is False and log_callback:
                log_callback("[!] 邮箱凭据提前保存失败；注册继续，提交成功后还会再次尝试保存")
        except Exception as exc:
            if log_callback:
                log_callback(f"[!] 邮箱凭据提前保存异常；注册继续，提交成功后还会再次尝试保存: {exc}")
    deadline = time.time() + timeout
    last_diag_time = 0
    last_reclick_time = 0
    last_snapshot = None
    while time.time() < deadline:
        raise_if_cancelled(cancel_callback)
        filled_raw = _run_pre_submit_js(
            """
const email = arguments[0];
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}
function textOf(node) {
    return [
        node.innerText,
        node.textContent,
        node.getAttribute('aria-label'),
        node.getAttribute('title'),
        node.getAttribute('placeholder'),
        node.getAttribute('data-testid'),
        node.getAttribute('name'),
        node.getAttribute('id'),
        node.getAttribute('autocomplete'),
    ].filter(Boolean).join(' ').replace(/\\s+/g, ' ').trim();
}
function describeInput(node) {
    return [
        `type=${node.getAttribute('type') || ''}`,
        `name=${node.getAttribute('name') || ''}`,
        `id=${node.getAttribute('id') || ''}`,
        `placeholder=${node.getAttribute('placeholder') || ''}`,
        `aria=${node.getAttribute('aria-label') || ''}`,
        `testid=${node.getAttribute('data-testid') || ''}`,
    ].join(' ').replace(/\\s+/g, ' ').trim().slice(0, 160);
}
function describeAction(node) {
    return textOf(node).slice(0, 120);
}
function emailCandidates() {
    const direct = Array.from(document.querySelectorAll('input[data-testid="email"], input[name="email"], input[type="email"], input[autocomplete="email"], input[placeholder*="mail" i], input[aria-label*="mail" i]'));
    const all = Array.from(document.querySelectorAll('input, textarea'));
    for (const node of all) {
        const type = (node.getAttribute('type') || '').toLowerCase();
        if (['hidden', 'submit', 'button', 'checkbox', 'radio', 'file', 'search'].includes(type)) continue;
        const meta = textOf(node).toLowerCase();
        if (meta.includes('email') || meta.includes('e-mail') || meta.includes('mail') || meta.includes('邮箱') || meta.includes('电子邮件')) {
            direct.push(node);
        }
    }
    return Array.from(new Set(direct));
}
const visibleInputs = Array.from(document.querySelectorAll('input, textarea'))
    .filter((node) => isVisible(node) && !node.disabled && !node.readOnly)
    .map(describeInput)
    .slice(0, 8);
const visibleActions = Array.from(document.querySelectorAll('button, a, [role="button"]'))
    .filter((node) => isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true')
    .map(describeAction)
    .filter(Boolean)
    .slice(0, 10);
const input = emailCandidates().find((node) => isVisible(node) && !node.disabled && !node.readOnly) || null;
if (!input) {
    return JSON.stringify({
        state: 'not-ready',
        url: location.href,
        title: document.title,
        inputs: visibleInputs,
        buttons: visibleActions,
    });
}
input.focus(); input.click();
const valueProto = input instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
const valueSetter = Object.getOwnPropertyDescriptor(valueProto, 'value')?.set;
const tracker = input._valueTracker;
if (tracker) tracker.setValue('');
if (valueSetter) valueSetter.call(input, email); else input.value = email;
input.dispatchEvent(new InputEvent('beforeinput', { bubbles: true, data: email, inputType: 'insertText' }));
input.dispatchEvent(new InputEvent('input', { bubbles: true, data: email, inputType: 'insertText' }));
input.dispatchEvent(new Event('change', { bubbles: true }));
const inputType = (input.getAttribute('type') || '').toLowerCase();
const isValid = inputType !== 'email' || input.checkValidity();
if ((input.value || '').trim() !== email || !isValid) {
    return JSON.stringify({
        state: 'fill-failed',
        value: input.value || '',
        valid: isValid,
        input: describeInput(input),
        url: location.href,
    });
}
input.blur();
return JSON.stringify({
    state: 'filled',
    input: describeInput(input),
    url: location.href,
});
            """,
            email,
        )
        filled = json.loads(filled_raw)
        state = filled.get("state")
        last_snapshot = filled
        if state == "not-ready":
            now = time.time()
            if now - last_reclick_time >= 3:
                reclicked = _run_pre_submit_js(r"""
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}
function nodeText(node) {
    return [
        node.innerText,
        node.textContent,
        node.getAttribute('aria-label'),
        node.getAttribute('title'),
        node.getAttribute('href'),
    ].filter(Boolean).join(' ').replace(/\s+/g, ' ').trim();
}
function scoreEntry(node) {
    const compact = nodeText(node).replace(/\s+/g, '');
    const lower = compact.toLowerCase();
    if (compact.includes('使用邮箱注册')) return 100;
    if (lower.includes('signupwithemail')) return 95;
    if (lower.includes('continuewithemail')) return 90;
    if (lower.includes('email') && (lower.includes('sign') || lower.includes('continue') || lower.includes('use') || lower.includes('with'))) return 80;
    if (lower === 'email' || lower.includes('邮箱')) return 70;
    return 0;
}
const candidates = Array.from(document.querySelectorAll('button, a, [role="button"]'))
    .filter((node) => isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true')
    .map((node) => ({ node, score: scoreEntry(node), text: nodeText(node) }))
    .filter((item) => item.score > 0)
    .sort((a, b) => b.score - a.score);
if (!candidates.length) return false;
candidates[0].node.click();
return candidates[0].text || true;
                """)
                last_reclick_time = now
                if reclicked and log_callback:
                    detail = f": {reclicked}" if isinstance(reclicked, str) else ""
                    log_callback(f"[Debug] 邮箱输入框未出现，已再次触发邮箱注册入口{detail}")
            if log_callback and now - last_diag_time >= 5:
                last_diag_time = now
                inputs = " | ".join((filled or {}).get("inputs", [])[:6])
                buttons = " | ".join((filled or {}).get("buttons", [])[:8])
                url = (filled or {}).get("url", page.url if page else "")
                log_callback(f"[Debug] 等待邮箱输入框: url={url}; inputs={inputs or 'none'}; buttons={buttons or 'none'}")
            sleep_with_cancel(0.5, cancel_callback)
            continue
        if state != "filled":
            if log_callback:
                log_callback(f"[Debug] 邮箱输入框已出现，但写入失败: {filled}")
            sleep_with_cancel(0.5, cancel_callback)
            continue
        sleep_with_cancel(0.8, cancel_callback)
        ready_to_submit = _run_pre_submit_js(
            r"""
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}
function textOf(node) {
    return [node.getAttribute('placeholder'), node.getAttribute('data-testid'), node.getAttribute('name'),
            node.getAttribute('id'), node.getAttribute('autocomplete'), node.getAttribute('aria-label')]
        .filter(Boolean).join(' ').toLowerCase();
}
const direct = Array.from(document.querySelectorAll('input[data-testid="email"], input[name="email"], input[type="email"], input[autocomplete="email"], input[placeholder*="mail" i], input[aria-label*="mail" i]'));
for (const node of Array.from(document.querySelectorAll('input, textarea'))) {
    const type = (node.getAttribute('type') || '').toLowerCase();
    if (['hidden', 'submit', 'button', 'checkbox', 'radio', 'file', 'search'].includes(type)) continue;
    const meta = textOf(node);
    if (meta.includes('email') || meta.includes('e-mail') || meta.includes('mail') || meta.includes('邮箱') || meta.includes('电子邮件')) direct.push(node);
}
const input = Array.from(new Set(direct)).find((node) => isVisible(node) && !node.disabled && !node.readOnly) || null;
if (!input || !(input.value || '').trim()) return false;
return (input.getAttribute('type') || '').toLowerCase() !== 'email' || input.checkValidity();
            """
        )
        if not ready_to_submit:
            sleep_with_cancel(0.5, cancel_callback)
            continue
        _mark_registration_stage("email_submit")
        clicked = page.run_js(
            r"""
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}
function textOf(node) {
    return [
        node.innerText,
        node.textContent,
        node.getAttribute('aria-label'),
        node.getAttribute('title'),
        node.getAttribute('placeholder'),
        node.getAttribute('data-testid'),
        node.getAttribute('name'),
        node.getAttribute('id'),
        node.getAttribute('autocomplete'),
    ].filter(Boolean).join(' ').replace(/\s+/g, ' ').trim();
}
function emailCandidates() {
    const direct = Array.from(document.querySelectorAll('input[data-testid="email"], input[name="email"], input[type="email"], input[autocomplete="email"], input[placeholder*="mail" i], input[aria-label*="mail" i]'));
    const all = Array.from(document.querySelectorAll('input, textarea'));
    for (const node of all) {
        const type = (node.getAttribute('type') || '').toLowerCase();
        if (['hidden', 'submit', 'button', 'checkbox', 'radio', 'file', 'search'].includes(type)) continue;
        const meta = textOf(node).toLowerCase();
        if (meta.includes('email') || meta.includes('e-mail') || meta.includes('mail') || meta.includes('邮箱') || meta.includes('电子邮件')) {
            direct.push(node);
        }
    }
    return Array.from(new Set(direct));
}
const input = emailCandidates().find((node) => isVisible(node) && !node.disabled && !node.readOnly) || null;
if (!input || !(input.value || '').trim()) return false;
const inputType = (input.getAttribute('type') || '').toLowerCase();
if (inputType === 'email' && !input.checkValidity()) return false;
const buttons = Array.from(document.querySelectorAll('button[type="submit"], button, [role="button"], input[type="submit"]'))
    .filter((node) => isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true');
const submitButton = buttons.find((node) => {
    const text = textOf(node).replace(/\s+/g, '');
    const lower = text.toLowerCase();
    return (
        text === '注册' ||
        text.includes('注册') ||
        text.includes('继续') ||
        text.includes('下一步') ||
        text.includes('确认') ||
        lower.includes('signup') ||
        lower.includes('sign up') ||
        lower.includes('continue') ||
        lower.includes('next') ||
        lower.includes('createaccount') ||
        lower.includes('submit')
    );
});
if (submitButton) {
    submitButton.click();
    return textOf(submitButton) || true;
}
const form = input.closest('form');
if (form) {
    if (form.requestSubmit) form.requestSubmit();
    else form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    return 'form-submit';
}
input.focus();
input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', code: 'Enter', bubbles: true, cancelable: true }));
input.dispatchEvent(new KeyboardEvent('keyup', { key: 'Enter', code: 'Enter', bubbles: true, cancelable: true }));
return 'enter';
            """
        )
        if clicked:
            if log_callback:
                detail = f" ({clicked})" if isinstance(clicked, str) else ""
                log_callback(f"[*] 已填写邮箱并提交: {email}{detail}")
            return email, dev_token
        sleep_with_cancel(0.5, cancel_callback)
    if last_snapshot:
        inputs = " | ".join(last_snapshot.get("inputs", [])[:6])
        buttons = " | ".join(last_snapshot.get("buttons", [])[:8])
        url = last_snapshot.get("url", page.url if page else "")
        raise Exception(
            f"未找到邮箱输入框或注册按钮，最后页面: url={url}; inputs={inputs or 'none'}; buttons={buttons or 'none'}"
        )
    raise Exception("未找到邮箱输入框或注册按钮")

def fill_code_and_submit(email, dev_token, timeout=180, transition_timeout=15, log_callback=None, cancel_callback=None):
    def _resend_code():
        page.run_js(
            r"""
const nodes = Array.from(document.querySelectorAll('button, a, [role="button"]'));
const target = nodes.find((node) => {
  const t = (node.innerText || node.textContent || '').replace(/\s+/g, '').toLowerCase();
  return t.includes('重新发送') || t.includes('resend') || t.includes('再次发送');
});
if (target && !target.disabled) { target.click(); return true; }
return false;
            """
        )

    _mark_registration_stage("code_wait")
    code = get_oai_code(
        dev_token,
        email,
        log_callback=log_callback,
        cancel_callback=cancel_callback,
        resend_callback=_resend_code,
    )
    if not code:
        raise Exception("获取验证码失败")
    clean_code = str(code).replace("-", "").strip()

    # 拟人化：收到验证码后模拟人类切屏/看邮件延时，避免0秒光速写入
    view_code_delay = random.uniform(1.8, 3.2)
    if log_callback:
        log_callback(f"[拟人化] 模拟查收验证码视读延时 ({view_code_delay:.1f}s)...")
    sleep_with_cancel(view_code_delay, cancel_callback)

    deadline = time.time() + timeout

    while time.time() < deadline:
        raise_if_cancelled(cancel_callback)
        ready = page.run_js(
            """
const code = String(arguments[0] || '').trim();
if (!code) return 'not-ready';
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}
const aggregate = Array.from(document.querySelectorAll(
  'input[data-input-otp=\"true\"], input[name=\"code\"], input[autocomplete=\"one-time-code\"], input[inputmode=\"numeric\"], input[inputmode=\"text\"]'
)).find((node) => isVisible(node) && !node.disabled && !node.readOnly && Number(node.maxLength || 6) > 1);
if (aggregate) return 'aggregate';
const otpBoxes = Array.from(document.querySelectorAll('input')).filter((node) => {
    if (!isVisible(node) || node.disabled || node.readOnly) return false;
    const maxLength = Number(node.maxLength || 0);
    const ac = String(node.autocomplete || '').toLowerCase();
    return maxLength === 1 || ac === 'one-time-code';
});
return otpBoxes.length >= code.length ? 'boxes' : 'not-ready';
            """,
            clean_code,
        )
        if ready == "not-ready":
            sleep_with_cancel(0.5, cancel_callback)
            continue
        _mark_registration_stage("code_submit")
        filled = page.run_js(
            """
const code = String(arguments[0] || '').trim();
if (!code) return 'empty-code';

function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}

function setInputValue(input, value) {
    const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
    const tracker = input._valueTracker;
    if (tracker) tracker.setValue('');
    if (nativeSetter) nativeSetter.call(input, value);
    else input.value = value;
    input.dispatchEvent(new InputEvent('beforeinput', { bubbles: true, data: value, inputType: 'insertText' }));
    input.dispatchEvent(new InputEvent('input', { bubbles: true, data: value, inputType: 'insertText' }));
    input.dispatchEvent(new Event('change', { bubbles: true }));
}

const aggregate = Array.from(document.querySelectorAll(
  'input[data-input-otp=\"true\"], input[name=\"code\"], input[autocomplete=\"one-time-code\"], input[inputmode=\"numeric\"], input[inputmode=\"text\"]'
)).find((node) => isVisible(node) && !node.disabled && !node.readOnly && Number(node.maxLength || 6) > 1);

if (aggregate) {
    aggregate.focus();
    aggregate.click();
    setInputValue(aggregate, code);
    return String(aggregate.value || '').replace(/\\s+/g, '') ? 'filled-aggregate' : 'aggregate-failed';
}

const otpBoxes = Array.from(document.querySelectorAll('input')).filter((node) => {
    if (!isVisible(node) || node.disabled || node.readOnly) return false;
    const maxLength = Number(node.maxLength || 0);
    const ac = String(node.autocomplete || '').toLowerCase();
    return maxLength === 1 || ac === 'one-time-code';
});

if (otpBoxes.length >= code.length) {
    for (let i = 0; i < code.length; i += 1) {
        const ch = code[i] || '';
        const box = otpBoxes[i];
        box.focus();
        box.click();
        setInputValue(box, ch);
        box.dispatchEvent(new KeyboardEvent('keydown', { bubbles: true, key: ch }));
        box.dispatchEvent(new KeyboardEvent('keyup', { bubbles: true, key: ch }));
    }
    const merged = otpBoxes.slice(0, code.length).map((x) => String(x.value || '').trim()).join('');
    return merged.length ? 'filled-boxes' : 'boxes-failed';
}

return 'not-ready';
            """,
            clean_code,
        )

        if filled == "not-ready":
            sleep_with_cancel(0.5, cancel_callback)
            continue
        if "failed" in str(filled):
            if log_callback:
                log_callback(f"[Debug] 验证码填写失败: {filled}")
            sleep_with_cancel(0.5, cancel_callback)
            continue

        clicked = page.run_js(
            r"""
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}

const buttons = Array.from(document.querySelectorAll('button[type=\"submit\"], button')).filter((node) => {
    return isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true';
});

const btn = buttons.find((node) => {
    const t = (node.innerText || node.textContent || '').replace(/\\s+/g, '').toLowerCase();
    return (
        t.includes('确认邮箱') ||
        t.includes('继续') ||
        t.includes('下一步') ||
        t.includes('confirm') ||
        t.includes('continue') ||
        t.includes('next')
    );
});

if (!btn) return 'no-button';
btn.focus();
btn.click();
return 'clicked';
            """
        )

        if clicked == "clicked" or clicked == "no-button":
            if log_callback:
                if clicked == "clicked":
                    log_callback(f"[*] 已填写验证码并触发提交，等待资料页确认: {code}")
                else:
                    log_callback(f"[*] 已填写验证码，页面未显示提交按钮，等待自动提交确认: {code}")
            transition_deadline = min(deadline, time.time() + max(1.0, float(transition_timeout)))
            while time.time() < transition_deadline:
                raise_if_cancelled(cancel_callback)
                if has_profile_form(log_callback=log_callback):
                    if log_callback:
                        log_callback("[*] 验证码提交已确认，资料页已出现")
                    return code
                try:
                    rejection = page.run_js(
                        r"""
const text = (document.body ? document.body.innerText : '').replace(/\s+/g, ' ').toLowerCase();
const markers = [
  'invalid code', 'incorrect code', 'wrong code', 'code expired',
  '验证码错误', '验证码无效', '验证码已过期', '验证码不正确'
];
return markers.find((item) => text.includes(item)) || '';
                        """
                    )
                except (ContextLostError, JavaScriptError) as exc:
                    # A navigation can invalidate the old execution context
                    # just before the profile form becomes observable.
                    if log_callback:
                        log_callback(f"[Debug] 验证码提交后页面正在切换，继续等待: {exc}")
                    try:
                        refresh_active_page()
                    except Exception:
                        pass
                    rejection = ""
                if rejection:
                    raise RuntimeError(f"验证码被页面明确拒绝: {rejection}")
                sleep_with_cancel(0.5, cancel_callback)
            from registration_flow import VerificationSubmissionUnconfirmed
            raise VerificationSubmissionUnconfirmed(
                f"验证码已填写，但在 {transition_timeout}s 内未确认进入资料页"
            )

        sleep_with_cancel(0.5, cancel_callback)

    from registration_flow import VerificationSubmissionUnconfirmed
    raise VerificationSubmissionUnconfirmed("验证码已获取，但自动填写/提交结果无法确认")

def _read_turnstile_state():
    """只读 Turnstile 状态，不重置、不点击、不修改页面。"""
    if page is None:
        return {
            "state": TURNSTILE_ABSENT,
            "present": False,
            "token": "",
            "token_length": 0,
            "widget_present": False,
            "iframe_present": False,
            "script_present": False,
            "visible": False,
        }

    state_raw = page.run_js(
        """
try {
  function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
  }

  const input = document.querySelector('input[name="cf-turnstile-response"]');
  let token = String((input && input.value) || '').trim();
  if (!token && window.turnstile && typeof window.turnstile.getResponse === 'function') {
    try {
      token = String(window.turnstile.getResponse() || '').trim();
    } catch (e) {
      token = '';
    }
  }

  const iframe = document.querySelector('iframe[src*="turnstile"]');
  const widget = document.querySelector('div.cf-turnstile, [data-sitekey]');
  const scriptPresent = !!document.querySelector('script[src*="turnstile"]');
  const widgetPresent = !!input || !!iframe || !!widget;
  const visible = isVisible(iframe) || isVisible(widget);

  const nearbyText = [widget, iframe && iframe.parentElement, input && input.parentElement]
    .filter(Boolean)
    .map((node) => String(node.innerText || node.textContent || node.getAttribute?.('title') || ''))
    .join(' ')
    .toLowerCase();
  const failed = widgetPresent && [
    'verification failed',
    'challenge failed',
    'challenge expired',
    '验证失败',
    '验证已过期',
    '校验失败',
  ].some((marker) => nearbyText.includes(marker));

  let state = 'ABSENT';
  if (token) state = 'SOLVED';
  else if (failed) state = 'FAILED';
  else if (input || iframe) state = 'WAITING';
  else if (widget) state = 'LOADING';

  return JSON.stringify({
    state,
    token,
    widget_present: widgetPresent,
    iframe_present: !!iframe,
    script_present: scriptPresent,
    visible,
  });
} catch (e) {
  return JSON.stringify({
    state: 'ABSENT',
    token: '',
    widget_present: false,
    iframe_present: false,
    script_present: false,
    visible: false,
  });
}
        """
    )
    try:
        state = json.loads(state_raw)
    except (TypeError, ValueError):
        state = {}

    token = str(state.get("token") or "").strip()
    status = str(state.get("state") or TURNSTILE_ABSENT).strip().upper()
    if token:
        status = TURNSTILE_SOLVED
    if status not in {
        TURNSTILE_ABSENT,
        TURNSTILE_LOADING,
        TURNSTILE_WAITING,
        TURNSTILE_SOLVED,
        TURNSTILE_FAILED,
    }:
        status = TURNSTILE_ABSENT

    return {
        "state": status,
        "present": status != TURNSTILE_ABSENT,
        "token": token,
        "token_length": len(token),
        "widget_present": bool(state.get("widget_present")),
        "iframe_present": bool(state.get("iframe_present")),
        "script_present": bool(state.get("script_present")),
        "visible": bool(state.get("visible")),
    }


# x.ai 注册页的 Turnstile sitekey。页面 HTML 与内联脚本里都能读到，这里作为
# 最后兜底，避免因页面尚未渲染完而解析不到。
_DEFAULT_TURNSTILE_SITEKEY = "0x4AAAAAAAhr9JGVDZbrZOo0"


def _resolve_turnstile_sitekey(log_callback=None):
    """解析 Turnstile sitekey：配置 → 页面 → 内置兜底。"""
    try:
        import captcha_solver
        settings = captcha_solver.settings_from_config()
        if settings:
            key = str(settings.get("sitekey") or "").strip()
            if key:
                return key
    except Exception:
        pass
    if page is not None:
        try:
            found = page.run_js(
                r"""
try {
  const node = document.querySelector('[data-sitekey]');
  if (node) {
    const v = node.getAttribute('data-sitekey');
    if (v) return String(v).trim();
  }
  const html = document.documentElement ? document.documentElement.innerHTML : '';
  const m = html.match(/0x4AAAAAA[A-Za-z0-9_-]{10,}/);
  if (m) return m[0];
  return '';
} catch (e) { return ''; }
                """
            )
            found = str(found or "").strip()
            if found:
                return found
        except Exception:
            pass
    return _DEFAULT_TURNSTILE_SITEKEY


def _try_solve_turnstile_via_solver(log_callback=None):
    """解 Turnstile 并把 token 注入页面。

    优先走「页内自建组件 + 点击」：x.ai 在 Camoufox 下不会自己渲染 Turnstile
    （window.turnstile 永不出现），但注入 .cf-turnstile[data-sitekey] 后点击它，
    挑战会正常跑完并把 token 写进 input[name="cf-turnstile-response"]——这条路径
    与本地 solver 的成功路径完全一致，且不依赖外部打码服务。失败再退回打码平台。

    两条路径拿到 token 后都会调 activate_turnstile_bridge：x.ai 的提交守卫读的是
    React state（由 Turnstile 组件的 onToken 写入），不是 input.value，只填 input
    点提交只会触发 ValidatePassword。
    """
    try:
        import captcha_solver
    except Exception as exc:
        if log_callback:
            log_callback(f"[*] 打码模块不可用，继续被动等待: {str(exc)[:80]}")
        return False

    sitekey = _resolve_turnstile_sitekey(log_callback)

    # 路径 1：配置了打码服务（含本地 Camoufox solver）时优先调用
    settings = captcha_solver.settings_from_config()
    if settings:
        if log_callback:
            target = settings.get("api_base") or "默认端点"
            log_callback(f"[*] 触发打码求解（{target}）...")
        try:
            captcha_solver.sync_solver_proxy(log=log_callback)
        except Exception:
            pass
        try:
            result = captcha_solver.solve_and_inject(
                page,
                settings["api_key"],
                log=log_callback,
                api_base=settings["api_base"],
                timeout_sec=settings["timeout_sec"],
                sitekey=settings.get("sitekey") or sitekey or "",
            )
        except Exception as exc:
            if log_callback:
                log_callback(f"[*] 打码异常，继续被动等待: {str(exc)[:100]}")
            return False
        if result.get("ok"):
            if log_callback:
                log_callback(
                    f"[*] 打码已解出并注入（token {int(result.get('token_len') or 0)} 字符）"
                )
                if not result.get("ua_matched", True):
                    log_callback("[!] 打码 UA 与浏览器不一致，token 可能不被接受")
            token_val = result.get("token") or (_read_turnstile_state() or {}).get("token") or ""
            if token_val:
                try:
                    res = captcha_solver.activate_turnstile_bridge(page, token_val)
                    if log_callback:
                        log_callback(f"[*] 打码 token 已桥接到 React bridge: {res}")
                except Exception as exc:
                    if log_callback:
                        log_callback(f"[!] 打码 token 桥接异常: {exc}")
                try:
                    f_res = inject_turnstile_token_into_react(token_val, log_callback)
                    if log_callback:
                        log_callback(f"[*] React Fiber onToken 注入结果: {f_res}")
                except Exception as exc:
                    if log_callback:
                        log_callback(f"[!] React Fiber 注入异常: {exc}")
            return True
        if log_callback:
            log_callback(f"[*] 打码未成功，继续被动等待: {result.get('reason') or '未知原因'}")
        return False

    # 路径 2：未配置打码时的备用方案：页内自建组件 + 点击
    if sitekey:
        if log_callback:
            log_callback("[*] 未配置打码服务，尝试页内自建 Turnstile 组件求解...")
        token = ""
        try:
            token = captcha_solver.solve_turnstile_in_page(
                page,
                sitekey,
                log=log_callback,
                timeout_sec=12,
            )
        except Exception as exc:
            if log_callback:
                log_callback(f"[*] 页内求解异常: {str(exc)[:100]}")
        if token:
            try:
                captcha_solver.inject_turnstile_token(page, token)
            except Exception:
                pass
            try:
                captcha_solver.activate_turnstile_bridge(page, token)
            except Exception:
                pass
            try:
                inject_turnstile_token_into_react(token, log_callback)
            except Exception:
                pass
            if log_callback:
                log_callback("[*] 页内 Turnstile 求解成功并已桥接到 React state")
            return True

    return False


def _wait_for_turnstile(log_callback=None, cancel_callback=None, timeout=60):
    """等待 Turnstile 自动完成；自动失败则交给打码，最后才判超时。"""
    global page
    if page is None:
        raise Exception("页面未就绪，无法执行 Turnstile")

    timeout = max(float(timeout or 0), 0.0)
    deadline = time.time() + timeout
    started = time.time()
    last_state = None
    last_log_at = 0.0

    # 打码只尝试一次：反复调用既烧点数也拖时间，失败就退回被动等待。
    try:
        import captcha_solver as _cs
        solver_after = _cs.auto_wait_sec()
        if browser_runtime.browser_engine() == "camoufox":
            solver_after = 0.0
    except Exception:
        solver_after = 10.0
    solver_attempted = False
    in_page_attempted = False

    while time.time() < deadline:
        raise_if_cancelled(cancel_callback)
        state = _read_turnstile_state()
        status = state["state"]
        token = state["token"]

        if status == TURNSTILE_SOLVED and token:
            if log_callback:
                log_callback(f"[*] Cloudflare 人机验证已完成，token长度={len(token)}")
            return token

        if status == TURNSTILE_ABSENT:
            # 页面里根本没有 Turnstile 组件。x.ai 在 Camoufox 下不会自己渲染，
            # 被动等待永远等不到 token，所以主动注入一个组件并点击求解。
            if not in_page_attempted:
                in_page_attempted = True
                if _try_solve_turnstile_via_solver(log_callback):
                    st = _read_turnstile_state()
                    if st.get("token"):
                        if log_callback:
                            log_callback(f"[*] Cloudflare 人机验证已完成，token长度={len(st['token'])}")
                        return st["token"]
                    last_log_at = 0.0
                    last_state = None
                    continue
            return ""

        if status == TURNSTILE_FAILED:
            raise Exception("Cloudflare 人机验证失败")

        now = time.time()
        state_changed = status != last_state
        if log_callback and (state_changed or now - last_log_at >= 5):
            if status == TURNSTILE_LOADING:
                log_callback("[*] Cloudflare 人机验证组件正在加载...")
            elif status == TURNSTILE_WAITING:
                if state_changed and state.get("visible"):
                    log_callback("[*] Cloudflare 人机验证等待完成，请在当前浏览器窗口完成验证")
                else:
                    log_callback(
                        f"[*] 仍在等待 Cloudflare 人机验证... {int(now - started)}s"
                    )
            last_log_at = now
            last_state = status

        # 给 Cloudflare 自动完成的机会；超过阈值仍无 token 才上打码。
        if (not solver_attempted) and (now - started) >= solver_after:
            solver_attempted = True
            if _try_solve_turnstile_via_solver(log_callback):
                st = _read_turnstile_state()
                if st.get("token"):
                    if log_callback:
                        log_callback(f"[*] Cloudflare 人机验证已完成，token长度={len(st['token'])}")
                    return st["token"]
            last_log_at = 0.0
            last_state = None
            continue

        sleep_with_cancel(min(1.0, max(deadline - now, 0.0)), cancel_callback)

    final_st = _read_turnstile_state()
    if final_st.get("token"):
        if log_callback:
            log_callback(f"[*] Cloudflare 人机验证已完成，token长度={len(final_st['token'])}")
        return final_st["token"]
    raise Exception("Turnstile 验证超时")


def getTurnstileToken(log_callback=None, cancel_callback=None, timeout=60):
    """兼容入口：等待当前页面的 Turnstile 验证完成。"""
    return _wait_for_turnstile(
        log_callback=log_callback,
        cancel_callback=cancel_callback,
        timeout=timeout,
    )

def build_profile():
    given_name_pool = [
        "Neo", "Ethan", "Liam", "Noah", "Lucas", "Mason", "Ryan", "Leo",
        "Owen", "Aiden", "Elio", "Aron", "Ivan", "Nolan", "Evan", "Kai",
        "Caleb", "Adam", "Ezra", "Miles", "Logan", "Carter", "Hunter", "Jason",
        "Brian", "Dylan", "Alex", "Colin", "Blake", "Gavin", "Henry", "Julian",
        "Kevin", "Louis", "Marcus", "Nathan", "Oscar", "Peter", "Quinn", "Robin",
        "Simon", "Tristan", "Victor", "Wesley", "Xavier", "Yuri", "Zane", "Felix",
        "Aaron", "Damian",
    ]
    family_name_pool = [
        "Lin", "Wang", "Zhao", "Liu", "Chen", "Zhang", "Xu", "Sun",
        "Guo", "He", "Yang", "Wu", "Zhou", "Tang", "Qin", "Shi",
        "Fang", "Peng", "Cao", "Deng", "Fan", "Fu", "Gao", "Han",
        "Hu", "Jiang", "Kong", "Lu", "Ma", "Nie", "Pan", "Qiao",
        "Ren", "Shao", "Tian", "Xie", "Yan", "Yao", "Yu", "Zeng",
        "Bai", "Duan", "Hou", "Jin", "Kang", "Luo", "Mao", "Song",
        "Wei", "Xiong",
    ]
    given_name = random.choice(given_name_pool)
    family_name = random.choice(family_name_pool)
    password = "N" + secrets.token_hex(4) + "!a7#" + secrets.token_urlsafe(6)
    return given_name, family_name, password


# x.ai's sign-up page renders Turnstile through React, and its submit guard reads
# a React state (called F in the bundle) rather than the
# input[name="cf-turnstile-response"] value:
#
#     eL = async e => {
#         if (H || eo || ex || eS) return;
#         if (!F) return (0,b.track)("account_signup_error",
#                      {error_type:"turnstile_failed", step:"credentials"}), eA(), !1;
#         ...
#     }
#
# That state is written by the Turnstile component's onToken callback:
#
#     ["Turnstile", 0, ({onToken: e, ...}) => {
#         let _ = useContext(a.TurnstileCtx), E = useRef(null);
#         useEffect(() => {
#             if (void 0 === _ || null === E.current) return;
#             _.turnstile.render(E.current, {
#                 sitekey: _.sitekey, size: "flexible",
#                 callback: t => { m(null), e(t) },   // <- e is onToken
#                 "error-callback": ...
#             }), () => { _.turnstile.remove(t) }
#         }, [...]);
#     }]
#
# Writing the solver token into the hidden input alone is therefore not enough:
# the click is accepted, the button is enabled, and the page still refuses to
# POST, emitting only the account_signup_error analytics event above.
#
# onToken is a component prop, so it hangs off the React fiber's memoizedProps.
# Walking up from the widget container via __reactFiber$ lets us call it
# directly, which sidesteps Cloudflare's closed shadow DOM entirely (the widget
# is not reachable as ordinary DOM, so there is nothing to fill in).
_REACT_ONTOKEN_JS = r"""
try {
  const token = %s;
  function targetOf(el) {
    if (!el) return null;
    return el.wrappedJSObject || el;
  }
  function fiberKey(el) {
    const t = targetOf(el);
    if (!t) return null;
    // React attaches __reactFiber$<random> as a NON-enumerable own property,
    // so Object.keys() never sees it — getOwnPropertyNames() is required.
    // On Firefox (Camoufox), expando properties sit on wrappedJSObject.
    for (const name of Object.getOwnPropertyNames(t)) {
      if (name.indexOf('__reactFiber$') === 0) return name;
      if (name.indexOf('__reactInternalInstance$') === 0) return name;
    }
    return null;
  }
  // 定位 React 根：React 18+ 在容器上挂 __reactContainer$，旧版本挂 __reactFiber$。
  // Cloudflare 的 widget 位于 closed shadow DOM 中，cf-turnstile-response 这个
  // input 在主文档里可能根本不存在，从它向上爬是走不到 React 树的，所以改为
  // 从根 fiber 出发做一次有界 BFS，遍历整棵树找 onToken。
  function rootFiberOf(el) {
    const t = targetOf(el);
    if (!t) return null;
    for (const name of Object.getOwnPropertyNames(t)) {
      if (name.indexOf('__reactContainer$') === 0) return t[name];
      if (name.indexOf('__reactFiber$') === 0) return t[name];
      if (name.indexOf('__reactInternalInstance$') === 0) return t[name];
    }
    return null;
  }
  const hosts = [document.querySelector('form'), document.querySelector('#root'), document.body, document.documentElement]
    .filter(Boolean);
  let rootFiber = null;
  for (const host of hosts) {
    rootFiber = rootFiberOf(host);
    if (rootFiber) break;
  }
  // 兜底：任意一个带 fiber 的元素，沿 return 走到顶。
  if (!rootFiber) {
    for (const el of document.querySelectorAll('*')) {
      const f = rootFiberOf(el);
      if (f) {
        let top = f, guard = 0;
        while (top && top.return && guard < 5000) { top = top.return; guard += 1; }
        rootFiber = top;
        break;
      }
    }
  }
  if (!rootFiber) return 'not-found:no-react-root';
  const seen = new Set();
  const queue = [rootFiber];
  let visited = 0;
  const propNames = new Set();
  while (queue.length && visited < 20000) {
    const fiber = queue.shift();
    if (!fiber || seen.has(fiber)) continue;
    seen.add(fiber);
    visited += 1;
    const props = fiber.memoizedProps;
    if (props && typeof props === 'object') {
      for (const k of Object.keys(props)) {
        if (/token|success|callback|verify|solve/i.test(k)) propNames.add(k);
      }
      for (const k of ['onToken', 'onSuccess', 'onVerify', 'onSolved', 'callback', 'onCallback']) {
        if (typeof props[k] === 'function') {
          try { props[k](token); return 'called-' + k + '@' + visited; }
          catch (e) { return k + '-threw:' + String(e).slice(0, 80); }
        }
      }
    }
    // hooks 链：部分实现把回调藏在 memoizedState 里。
    let hook = fiber.memoizedState;
    let hookGuard = 0;
    while (hook && hookGuard < 40) {
      const hs = hook.memoizedState;
      if (hs && typeof hs === 'object') {
        for (const k of ['onToken', 'onSuccess', 'callback']) {
          if (typeof hs[k] === 'function') {
            try { hs[k](token); return 'called-hook-' + k + '@' + visited; }
            catch (e) {}
          }
        }
      }
      hook = hook.next;
      hookGuard += 1;
    }
    if (fiber.child) queue.push(fiber.child);
    if (fiber.sibling) queue.push(fiber.sibling);
  }
  return 'not-found:visited=' + visited + ',props=' + Array.from(propNames).slice(0, 12).join(',');
} catch (e) { return 'fatal:' + String(e).slice(0, 100); }
"""


def inject_turnstile_token_into_react(token, log_callback=None):
    """Publish a solved Turnstile token to x.ai's React state.

    Returns the JS probe result string (useful for logs and tests).
    """
    if page is None or not token:
        return ""
    try:
        result = page.run_js(_REACT_ONTOKEN_JS % json.dumps(str(token)))
    except Exception as exc:
        if log_callback:
            log_callback(f"[*] Turnstile token 注入 React 失败: {type(exc).__name__}: {str(exc)[:140]}")
        return ""
    if log_callback:
        log_callback(f"[*] Turnstile token 注入 React 结果: {result}")
    return str(result or "")


def _publish_solver_token_to_page_state(log_callback=None):
    """Read the solver token off the page and hand it to the React component.

    主路径是 activate_turnstile_bridge：调用 x.ai 的 api.js?onload 回调并触发
    window.turnstile 垫片的 render，让 Turnstile 组件的 onToken 拿到 token，
    从而写进提交守卫读的那个 React state。

    原来的 fiber 注入（inject_turnstile_token_into_react）保留为兜底，但实测
    x.ai 页面在 Camoufox 下没有任何 React fiber 属性，它必然返回 no-react-root。
    """
    try:
        token = page.run_js(
            'try{var i=document.querySelector(\'input[name="cf-turnstile-response"]\');'
            'return i?String(i.value||""):"";}catch(e){return "";}'
        )
    except Exception:
        token = ""
    token = str(token or "")
    if log_callback:
        log_callback(f"[*] 提交前读取 solver token 长度={len(token)}")
    if not token:
        return ""
    try:
        import captcha_solver
        bridge = captcha_solver.activate_turnstile_bridge(page, token)
        if log_callback:
            log_callback(f"[*] Turnstile 桥接结果: {str(bridge)[:180]}")
    except Exception as exc:
        if log_callback:
            log_callback(f"[!] Turnstile 桥接异常: {str(exc)[:100]}")
    return inject_turnstile_token_into_react(token, log_callback)


def fill_profile_and_submit(timeout=180, log_callback=None, cancel_callback=None):
    given_name, family_name, password = build_profile()
    deadline = time.time() + timeout
    form_filled_once = False

    while time.time() < deadline:
        raise_if_cancelled(cancel_callback)
        if not form_filled_once:
            filled = page.run_js(
                """
const givenName = arguments[0];
const familyName = arguments[1];
const password = arguments[2];

function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}

function pickInput(selector) {
    return Array.from(document.querySelectorAll(selector)).find((node) => {
        return isVisible(node) && !node.disabled && !node.readOnly;
    }) || null;
}

function setInputValue(input, value) {
    if (!input) return false;
    input.focus();
    input.click();
    const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
    const tracker = input._valueTracker;
    if (tracker) tracker.setValue('');
    if (nativeSetter) nativeSetter.call(input, value);
    else input.value = value;
    input.dispatchEvent(new InputEvent('beforeinput', { bubbles: true, data: value, inputType: 'insertText' }));
    input.dispatchEvent(new InputEvent('input', { bubbles: true, data: value, inputType: 'insertText' }));
    input.dispatchEvent(new Event('change', { bubbles: true }));
    input.blur();
    return String(input.value || '').trim() === String(value || '').trim();
}

const givenInput = pickInput('input[data-testid="givenName"], input[name="givenName"], input[autocomplete="given-name"], input[aria-label*="名"]');
const familyInput = pickInput('input[data-testid="familyName"], input[name="familyName"], input[autocomplete="family-name"], input[aria-label*="姓"]');
const passwordInput = pickInput('input[data-testid="password"], input[name="password"], input[type="password"], input[autocomplete="new-password"]');

if (!givenInput || !familyInput || !passwordInput) return 'not-ready';

const ok1 = setInputValue(givenInput, givenName);
const ok2 = setInputValue(familyInput, familyName);
const ok3 = setInputValue(passwordInput, password);

if (!ok1 || !ok2 || !ok3) return 'fill-failed';

const buttons = Array.from(document.querySelectorAll('button[type="submit"], button, [role="button"], input[type="submit"]')).filter((node) => {
    return isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true';
});
const submitBtn = buttons.find((node) => {
    const t = (node.innerText || node.textContent || '').replace(/\\s+/g, '').toLowerCase();
    return t.includes('完成注册') || t.includes('创建账户') || t.includes('signup') || t.includes('createaccount');
});

if (submitBtn) {
    return 'ready-to-submit';
}
return 'filled-no-submit';
            """,
                given_name,
                family_name,
                password,
            )

            if filled in ("ready-to-submit", "filled-no-submit"):
                form_filled_once = True
            elif filled == "fill-failed" and log_callback:
                log_callback("[Debug] 资料输入失败，重试中...")
                sleep_with_cancel(0.5, cancel_callback)
                continue
            elif filled == "not-ready":
                sleep_with_cancel(0.5, cancel_callback)
                continue

        turnstile_state = _read_turnstile_state()
        if turnstile_state["state"] in {
            TURNSTILE_LOADING,
            TURNSTILE_WAITING,
            TURNSTILE_FAILED,
        }:
            remaining = max(deadline - time.time(), 60.0)
            got = getTurnstileToken(
                log_callback=log_callback,
                cancel_callback=cancel_callback,
                timeout=remaining,
            )
            if got:
                deadline = max(deadline, time.time() + 60.0)
            continue

        submit_state = page.run_js(
            r"""
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}

function buttonText(node) {
    return [
        node.innerText,
        node.textContent,
        node.getAttribute('value'),
        node.getAttribute('aria-label'),
        node.getAttribute('title'),
    ].filter(Boolean).join(' ').replace(/\s+/g, ' ').trim();
}
const buttons = Array.from(document.querySelectorAll('button[type="submit"], button, [role="button"], input[type="submit"]')).filter((node) => {
    return isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true';
});
const submitBtn = buttons.find((node) => {
    const t = buttonText(node).replace(/\s+/g, '').toLowerCase();
    return t.includes('完成注册') || t.includes('创建账户') || t.includes('signup') || t.includes('createaccount');
});
if (!submitBtn) {
    const visibleTexts = buttons.map(buttonText).filter(Boolean).slice(0, 8).join(' | ');
    return 'no-submit-button:' + visibleTexts;
}
return 'ready-to-submit';
            """
        )

        if submit_state == "ready-to-submit":
            _mark_registration_stage("profile_submit")
            # The solver has already put its token in the hidden input; publish
            # it to React state as well, otherwise the submit guard sees no
            # token and silently refuses to POST (see _REACT_ONTOKEN_JS).
            _publish_solver_token_to_page_state(log_callback)
            sleep_with_cancel(0.8, cancel_callback)
            # 拟人化：提交前停顿与微动，模拟人工核对资料
            pre_submit_delay = random.uniform(1.0, 2.2)
            if log_callback:
                log_callback(f"[拟人化] 提交注册前模拟人工核对 ({pre_submit_delay:.1f}s)...")
            sleep_with_cancel(pre_submit_delay, cancel_callback)
            submit_state = page.run_js(
                r"""
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}
function buttonText(node) {
    return [node.innerText, node.textContent, node.getAttribute('value'), node.getAttribute('aria-label'), node.getAttribute('title')]
      .filter(Boolean).join(' ').replace(/\s+/g, ' ').trim();
}
const submitBtn = Array.from(document.querySelectorAll('button[type="submit"], button, [role="button"], input[type="submit"]'))
    .filter((node) => isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true')
    .find((node) => {
        const t = buttonText(node).replace(/\s+/g, '').toLowerCase();
        return t.includes('完成注册') || t.includes('创建账户') || t.includes('signup') || t.includes('createaccount');
    });
if (!submitBtn) return 'submit-disappeared';
submitBtn.focus();
submitBtn.click();
return 'submitted';
                """
            )
            if submit_state == "submitted":
                if log_callback:
                    log_callback(f"[*] 已填写注册资料并提交: {given_name} {family_name}")
                return {"given_name": given_name, "family_name": family_name, "password": password}
        if isinstance(submit_state, str) and submit_state.startswith("no-submit-button") and log_callback:
            visible_buttons = submit_state.split(":", 1)[1] if ":" in submit_state else ""
            suffix = f" 可见按钮: {visible_buttons}" if visible_buttons else ""
            log_callback(f"[Debug] 未找到提交按钮，继续等待页面稳定...{suffix}")

        sleep_with_cancel(0.5, cancel_callback)

    raise Exception("最终注册页资料填写失败")

def wait_for_sso_cookie(timeout=120, log_callback=None, cancel_callback=None):
    deadline = time.time() + timeout
    last_seen_names = set()
    last_submit_retry = 0.0
    final_no_submit_state = ""
    final_no_submit_since = None
    final_no_submit_timeout = 25
    last_wait_exception_message = ""
    last_wait_exception_at = 0.0
    consecutive_wait_errors = 0
    last_consecutive_error_message = ""
    last_wait_exception = None

    while time.time() < deadline:
        raise_if_cancelled(cancel_callback)
        try:
            refresh_active_page()
            if page is None:
                sleep_with_cancel(1, cancel_callback)
                continue

            # 仍停留在“完成注册”页时，若 Cloudflare 已通过，周期性重试点击提交
            now = time.time()
            if now - last_submit_retry >= 2.5:
                retried = page.run_js(
                    r"""
const titleHit = !!Array.from(document.querySelectorAll('h1,h2,div,span')).find((el) => {
    const t = (el.textContent || '').replace(/\s+/g, '');
    const lower = t.toLowerCase();
    return t.includes('完成注册') || lower.includes('completeyoursignup') || lower.includes('completesignup');
});
return titleHit ? 'final-page' : 'not-final-page';
                    """
                )

                if retried == "final-page":
                    turnstile_state = _read_turnstile_state()
                    if turnstile_state["state"] in {
                        TURNSTILE_LOADING,
                        TURNSTILE_WAITING,
                        TURNSTILE_FAILED,
                    }:
                        if log_callback:
                            log_callback(
                                f"[Debug] 最终页 Cloudflare 状态: {turnstile_state['state']}, "
                                f"token长度={turnstile_state['token_length']}"
                            )
                        remaining = max(deadline - time.time(), 0.0)
                        getTurnstileToken(
                            log_callback=log_callback,
                            cancel_callback=cancel_callback,
                            timeout=remaining,
                        )
                        last_submit_retry = now
                        continue

                    # 重试点击前把 token 重新桥接一次。token 是一次性的、约 120 秒
                    # 过期，首次提交若因 React state 未拿到 token 而失败，重试时
                    # 必须重新喂给 onToken，否则点多少次都不会发出注册请求。
                    _publish_solver_token_to_page_state(log_callback)
                    sleep_with_cancel(0.8, cancel_callback)

                    retried = page.run_js(
                        r"""
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}
function buttonText(node) {
    return [
        node.innerText,
        node.textContent,
        node.getAttribute('value'),
        node.getAttribute('aria-label'),
        node.getAttribute('title'),
    ].filter(Boolean).join(' ').replace(/\s+/g, ' ').trim();
}
const buttons = Array.from(document.querySelectorAll('button[type="submit"], button, [role="button"], input[type="submit"]')).filter((node) => {
    return isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true';
});
const submitBtn = buttons.find((node) => {
    const t = buttonText(node).replace(/\s+/g, '').toLowerCase();
    return t.includes('完成注册') || t.includes('创建账户') || t.includes('signup') || t.includes('createaccount');
});
if (!submitBtn) {
    const visibleTexts = buttons.map(buttonText).filter(Boolean).slice(0, 8).join(' | ');
    return 'final-page-no-submit:' + visibleTexts;
}
submitBtn.focus();
submitBtn.click();
return 'final-page-clicked-submit';
                        """
                    )
                last_submit_retry = now
                if log_callback and (retried == "final-page-clicked-submit" or (isinstance(retried, str) and retried.startswith("final-page-no-submit"))):
                    log_callback(f"[Debug] 最终页状态: {retried}")
                if isinstance(retried, str) and retried.startswith("final-page-no-submit"):
                    if retried != final_no_submit_state:
                        final_no_submit_state = retried
                        final_no_submit_since = now
                    elif final_no_submit_since and now - final_no_submit_since >= final_no_submit_timeout:
                        raise AccountRetryNeeded(
                            f"最终注册页状态 {final_no_submit_timeout}s 未变化且未找到提交按钮，重试当前账号: {retried}"
                        )
                else:
                    final_no_submit_state = ""
                    final_no_submit_since = None

            cookies = page.cookies(all_domains=True, all_info=True) or []
            for item in cookies:
                if isinstance(item, dict):
                    name = str(item.get("name", "")).strip()
                    value = str(item.get("value", "")).strip()
                else:
                    name = str(getattr(item, "name", "")).strip()
                    value = str(getattr(item, "value", "")).strip()

                if name:
                    last_seen_names.add(name)

                if name == "sso" and value:
                    if log_callback:
                        log_callback("[*] 已获取到 sso cookie")
                    return value
            # A completed polling iteration breaks any previous exception streak.
            consecutive_wait_errors = 0
            last_consecutive_error_message = ""
        except PageDisconnectedError:
            consecutive_wait_errors = 0
            last_consecutive_error_message = ""
            refresh_active_page()
        except AccountRetryNeeded:
            raise
        except ProxyTransportError:
            raise
        except (ContextLostError, JavaScriptError) as exc:
            last_wait_exception = exc
            message = f"{exc.__class__.__name__}: {exc}"
            if message == last_consecutive_error_message:
                consecutive_wait_errors += 1
            else:
                consecutive_wait_errors = 1
                last_consecutive_error_message = message
            if log_callback:
                now = time.time()
                if message != last_wait_exception_message or now - last_wait_exception_at >= 10:
                    log_callback(f"[Debug] 等待 sso cookie 时出现瞬时异常 ({consecutive_wait_errors}/3): {message}")
                    last_wait_exception_message = message
                    last_wait_exception_at = now
            if consecutive_wait_errors >= 3:
                raise RuntimeError(f"等待 sso cookie 连续瞬时异常: {last_wait_exception.__class__.__name__}: {last_wait_exception}") from last_wait_exception
        except Exception:
            raise

        sleep_with_cancel(1, cancel_callback)

    cause = "" if last_wait_exception is None else f"; last_error={last_wait_exception.__class__.__name__}: {last_wait_exception}"
    raise Exception(
        f"等待超时：未获取到 sso cookie。已看到 cookies: {sorted(last_seen_names)}{cause}"
    )


