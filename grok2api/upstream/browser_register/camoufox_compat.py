"""DrissionPage → Playwright/Camoufox 兼容层。

背景
----
2026-09-28 起 x.ai 在 accounts.x.ai 上启用了 Cloudflare Bot Management 的
JSD（JavaScript Detection）检测。窗口化的 Chromium（无论直连、WARP 还是
住宅代理）一律拿不到 ``cf_clearance``，Turnstile widget 不渲染，注册表单
提交被静默丢弃——表现为「点 Sign up 后页面毫无变化、永远收不到验证码邮件」。

根因是 Chromium 必须由 CDP（Chrome DevTools Protocol）驱动，而 CDP 连接本身
就是 Cloudflare 的确定性检测点；容器内又缺 GPU，``navigator.platform`` 报
Linux 而 UA 声称 Windows，``WebGL`` 直接不可用。

Camoufox（加固过的 Firefox）走 Juggler 协议，自带指纹伪造（含 WebGL、
platform、字体、时区），实测可稳定拿到 ``cf_clearance`` 并正常注册。

本模块提供一层薄适配：把上游 ``browser_register`` 代码用到的 DrissionPage
API（``Chromium`` / ``ChromiumOptions`` / ``page.run_js`` / ``page.ele`` …）
映射到 Playwright + Camoufox。上游主流程的 DOM 操作绝大多数是通过
``page.run_js()`` 注入 JS 完成的，所以 JS 逻辑可以原样复用。
"""

from __future__ import annotations

import json
import os
import re
import time
import urllib.parse

__all__ = [
    "CamoufoxOptions",
    "CamoufoxBrowser",
    "CamoufoxPage",
    "CamoufoxElement",
    "translate_locator",
]


# ───────────────────────────── 定位语法转换 ─────────────────────────────


def _split_attrs(body: str):
    """把 ``type=text@@aria-label*=Send`` 拆成属性条件列表。"""
    parts = [p for p in str(body or "").split("@@") if p.strip()]
    return [p.strip() for p in parts]


def _attr_selector(cond: str) -> str:
    """把 DrissionPage 属性条件翻译成 CSS 属性选择器。

    支持 ``name=value``、``name*=value``、``name^=value``、``name$=value``。
    ``text()=Send`` 这类伪属性交给 ``:has-text()`` 处理，返回空串。
    """
    cond = cond.strip()
    if not cond:
        return ""
    if cond.startswith("text()"):
        return ""
    m = re.match(r"^([A-Za-z_:][-A-Za-z0-9_:.]*)\s*(\*=|\\^=|\\$=|=)\s*(.*)$", cond)
    if not m:
        # 裸属性名，例如 contenteditable
        return '[%s]' % cond
    name, op, value = m.group(1), m.group(2), m.group(3)
    value = value.strip().strip("'\"")
    if op == "=":
        return '[%s="%s"]' % (name, value)
    return '[%s%s"%s"]' % (name, op, value)


def translate_locator(locator: str) -> str:
    """DrissionPage 定位串 → Playwright 选择器。

    支持的形式（覆盖上游全部用法）::

        tag:input@@type=text        → input[type="text"]
        tag:button@@text()=Send     → button:has-text("Send")
        tag:button@@aria-label*=end → button[aria-label*="end"]
        @contenteditable=true       → [contenteditable="true"]
        text:Sign up                → text=Sign up
        css:div.foo                 → div.foo
        xpath://div                 → xpath=//div
    """
    loc = str(locator or "").strip()
    if not loc:
        return "*"

    if loc.startswith("xpath:"):
        return "xpath=" + loc[len("xpath:"):]
    if loc.startswith("css:"):
        return loc[len("css:"):]
    if loc.startswith("text:"):
        return "text=" + loc[len("text:"):]
    if loc.startswith("tag:"):
        body = loc[len("tag:"):]
        tag, _, attr_part = body.partition("@@")
        tag = tag.strip() or "*"
        conds = _split_attrs(attr_part)
        css = tag
        has_text = ""
        for cond in conds:
            if cond.startswith("text()"):
                _, _, value = cond.partition("=")
                has_text = value.strip().strip("'\"")
                continue
            css += _attr_selector(cond)
        if has_text:
            return '%s:has-text("%s")' % (css, has_text)
        return css
    if loc.startswith("@"):
        css = _attr_selector(loc[1:])
        return css or "*"
    # 已是 CSS 或 Playwright 原生选择器
    return loc


# ───────────────────────────── 元素包装 ─────────────────────────────


class CamoufoxElement:
    """模拟 DrissionPage 的 ``ChromiumElement``。

    只实现上游实际用到的方法：``click`` / ``input`` / ``clear`` /
    ``text`` / ``attr`` / ``attrs`` / ``html`` / ``rect``。
    """

    def __init__(self, locator, timeout: float = 10):
        self._loc = locator
        self._timeout_ms = max(int(float(timeout or 10) * 1000), 1000)

    # -- 交互 ---------------------------------------------------------
    def click(self, by_js: bool = False):
        self._loc.click(timeout=self._timeout_ms)
        return True

    def input(self, text, clear: bool = False, by_js: bool = False):
        value = str(text)
        try:
            self._loc.fill(value, timeout=self._timeout_ms)
        except Exception:
            # contenteditable / 非标准 input 退化为键盘输入
            self._loc.click(timeout=self._timeout_ms)
            self._loc.type(value, timeout=self._timeout_ms)
        return True

    def clear(self, by_js: bool = False):
        try:
            self._loc.fill("", timeout=self._timeout_ms)
        except Exception:
            pass
        return True

    def focus(self):
        try:
            self._loc.focus(timeout=self._timeout_ms)
        except Exception:
            pass

    def scroll_to_see(self):
        try:
            self._loc.scroll_into_view_if_needed(timeout=self._timeout_ms)
        except Exception:
            pass

    # -- 属性 ---------------------------------------------------------
    @property
    def text(self) -> str:
        try:
            return str(self._loc.inner_text(timeout=self._timeout_ms) or "")
        except Exception:
            try:
                return str(self._loc.text_content(timeout=self._timeout_ms) or "")
            except Exception:
                return ""

    @property
    def html(self) -> str:
        try:
            return str(self._loc.evaluate("e => e.outerHTML") or "")
        except Exception:
            return ""

    @property
    def attrs(self) -> dict:
        try:
            return self._loc.evaluate(
                "e => { const o = {}; for (const a of e.attributes) o[a.name] = a.value; return o; }"
            ) or {}
        except Exception:
            return {}

    def attr(self, name: str):
        try:
            return self._loc.get_attribute(str(name), timeout=self._timeout_ms)
        except Exception:
            return None

    @property
    def rect(self):
        try:
            box = self._loc.bounding_box()
        except Exception:
            box = None
        return _Rect(box)

    @property
    def tag(self) -> str:
        try:
            return str(self._loc.evaluate("e => e.tagName.toLowerCase()") or "")
        except Exception:
            return ""

    def run_js(self, script: str, *args):
        return self._loc.evaluate(script, *args) if args else self._loc.evaluate(script)


class _Rect:
    """模拟 DrissionPage 的 ``rect`` 对象（``.size`` / ``.location``）。"""

    def __init__(self, box):
        self._box = box or {}

    @property
    def size(self):
        return (self._box.get("width", 0), self._box.get("height", 0))

    @property
    def location(self):
        return (self._box.get("x", 0), self._box.get("y", 0))

    @property
    def midpoint(self):
        x, y = self.location
        w, h = self.size
        return (x + w / 2, y + h / 2)

    def __iter__(self):
        return iter(self.size)


# ───────────────────────────── 页面包装 ─────────────────────────────


class _PageWaiter:
    """DrissionPage 风格 ``page.wait``。

    DrissionPage 把 ``wait`` 做成对象（``page.wait.doc_loaded()``），而兼容层最初
    把它实现成普通方法 ``wait(seconds)``，于是生产流程一进门就炸在
    ``'function' object has no attribute 'doc_loaded'``。这里两种用法都支持：
    ``page.wait.doc_loaded()`` 与 ``page.wait(2)``。
    """

    def __init__(self, page: "CamoufoxPage"):
        self._page = page

    def __call__(self, seconds: float = 0):
        time.sleep(max(float(seconds or 0), 0.0))
        return self

    def doc_loaded(self, timeout: float = 30, **kwargs):
        """等待文档加载完成（对应 Playwright 的 domcontentloaded）。"""
        try:
            self._page.native.wait_for_load_state(
                "domcontentloaded", timeout=max(float(timeout or 30), 1.0) * 1000
            )
        except Exception:
            pass
        return True

    def load_start(self, timeout: float = 30, **kwargs):
        """等待页面开始加载；Playwright 无等价原语，退化为 doc_loaded。"""
        return self.doc_loaded(timeout=timeout)

    def ele_displayed(self, locator, timeout: float = 10, **kwargs):
        try:
            self._page.native.locator(str(locator)).first.wait_for(
                state="visible", timeout=max(float(timeout or 10), 0.1) * 1000
            )
            return True
        except Exception:
            return False

    def ele_deleted(self, locator, timeout: float = 10, **kwargs):
        try:
            self._page.native.locator(str(locator)).first.wait_for(
                state="detached", timeout=max(float(timeout or 10), 0.1) * 1000
            )
            return True
        except Exception:
            return False

    def title_change(self, text: str = "", timeout: float = 10, **kwargs):
        return True

    def url_change(self, text: str = "", timeout: float = 10, **kwargs):
        return True

    def download_begin(self, timeout: float = 10, **kwargs):
        return False


class CamoufoxPage:
    """模拟 DrissionPage 的 ``ChromiumTab``。"""

    def __init__(self, pw_page, browser=None, index: int = 0):
        self._page = pw_page
        self._browser = browser
        self.index = index

    # -- 原生对象 -----------------------------------------------------
    @property
    def native(self):
        return self._page

    # -- 导航 ---------------------------------------------------------
    def get(self, url: str, timeout: float = 45, **kwargs):
        self._page.goto(str(url), timeout=max(int(float(timeout or 45) * 1000), 5000),
                        wait_until="domcontentloaded")
        return True

    def refresh(self):
        try:
            self._page.reload(wait_until="domcontentloaded")
        except Exception:
            pass

    def back(self):
        try:
            self._page.go_back()
        except Exception:
            pass

    # -- JS -----------------------------------------------------------
    def run_js(self, script: str, *args):
        """执行 JS。

        DrissionPage 的 ``run_js`` 语义是「脚本体 + 位置参数」，脚本里用
        ``arguments[0]`` / ``arguments[1]`` 读取。Playwright 的 ``evaluate``
        只接受一个参数，所以多参数时把它打包成数组再用 ``apply`` 展开，
        这样 ``arguments`` 的位置语义与 DrissionPage 一致。
        """
        wrapped = _wrap_js(script)
        try:
            if not args:
                return self._page.evaluate(wrapped)
            if len(args) == 1:
                return self._page.evaluate(wrapped, args[0])
            return self._page.evaluate(
                "function(payload) { return (%s).apply(null, payload); }" % wrapped,
                list(args),
            )
        except Exception as exc:
            # 少数脚本可能是表达式或依赖 IIFE，直接透传重试一次
            try:
                return self._page.evaluate(script)
            except Exception:
                raise exc

    # -- 元素 ---------------------------------------------------------
    def ele(self, locator: str, timeout: float = 10, index: int = 1):
        selector = translate_locator(locator)
        loc = self._page.locator(selector)
        if index and index > 1:
            loc = loc.nth(index - 1)
        else:
            loc = loc.first
        loc.wait_for(state="attached", timeout=max(int(float(timeout or 10) * 1000), 1000))
        return CamoufoxElement(loc, timeout)

    def eles(self, locator: str, timeout: float = 10):
        selector = translate_locator(locator)
        loc = self._page.locator(selector)
        try:
            loc.first.wait_for(state="attached",
                               timeout=max(int(float(timeout or 10) * 1000), 1000))
        except Exception:
            return []
        count = loc.count()
        return [CamoufoxElement(loc.nth(i), timeout) for i in range(count)]

    def s_ele(self, locator: str, timeout: float = 1):
        """找不到返回 ``None``（DrissionPage 的静默查找）。"""
        try:
            return self.ele(locator, timeout=timeout)
        except Exception:
            return None

    # -- 属性 ---------------------------------------------------------
    @property
    def url(self) -> str:
        try:
            return str(self._page.url or "")
        except Exception:
            return ""

    @property
    def title(self) -> str:
        try:
            return str(self._page.title() or "")
        except Exception:
            return ""

    @property
    def html(self) -> str:
        try:
            return str(self._page.content() or "")
        except Exception:
            return ""

    def cookies(self, all_domains: bool = True, all_info: bool = False, **kwargs):
        """返回 cookie 列表。

        DrissionPage 默认返回 ``[{name, value, domain, path, ...}]``；
        ``all_info=True`` 时带 ``expires`` / ``httpOnly`` / ``secure``。
        """
        try:
            raw = self._page.context.cookies()
        except Exception:
            return []
        out = []
        for c in raw or []:
            item = {
                "name": c.get("name", ""),
                "value": c.get("value", ""),
                "domain": c.get("domain", ""),
                "path": c.get("path", "/"),
            }
            if all_info:
                item["expires"] = c.get("expires", -1)
                item["httpOnly"] = c.get("httpOnly", False)
                item["secure"] = c.get("secure", False)
                item["sameSite"] = c.get("sameSite", "Lax")
            out.append(item)
        return out

    def set_cookies(self, cookies):
        try:
            self._page.context.add_cookies(cookies)
        except Exception:
            pass

    @property
    def wait(self):
        """DrissionPage 兼容：``page.wait.doc_loaded()`` 与 ``page.wait(2)`` 都可用。"""
        waiter = getattr(self, "_waiter", None)
        if waiter is None:
            waiter = _PageWaiter(self)
            self._waiter = waiter
        return waiter

    def screenshot(self, path: str = "", full_page: bool = False):
        try:
            return self._page.screenshot(path=path or None, full_page=full_page)
        except Exception:
            return None

    def close(self):
        try:
            self._page.close()
        except Exception:
            pass

    def __repr__(self):
        return "<CamoufoxPage index=%s url=%s>" % (self.index, self.url[:60])


def _wrap_js(script: str) -> str:
    """把 DrissionPage 风格的脚本包成 Playwright 可执行的函数。

    必须用 ``function(){}`` 而不是箭头函数：上游脚本大量使用 ``arguments[0]``
    读取传入参数，而箭头函数没有自己的 ``arguments`` 绑定，包成 ``() => {}``
    会直接报 "arguments is not defined"。

    另外，以 ``function foo(){}`` 开头的脚本是**函数声明序列 + 语句**（不是
    单个表达式），原样交给 ``page.evaluate`` 会报
    "missing ) in parenthetical"，同样必须包一层。
    """
    src = str(script or "").strip()
    if not src:
        return "function() { return null; }"
    # 已经是箭头函数表达式，原样使用
    if re.match(r"^(async\s+)?(\([^)]*\)|[A-Za-z_$][\w$]*)\s*=>", src):
        return src
    # 其余（含 function 声明序列、裸语句）一律包成普通函数，保留 arguments 语义
    return "function() { %s }" % src


# ───────────────────────────── 浏览器包装 ─────────────────────────────


class CamoufoxOptions:
    """模拟 DrissionPage 的 ``ChromiumOptions``，收集参数供 Camoufox 使用。"""

    def __init__(self):
        self._args = []
        self._headless = False
        self._browser_path = ""
        self._proxy = ""
        self._user_agent = ""
        self._extensions = []
        self._user_data_path = ""
        self._timezone = ""
        self._locale = ""
        self._prefs = {}
        self._extra = {}

    # -- DrissionPage 链式接口 ---------------------------------------
    def auto_port(self, *a, **kw):
        return self

    def set_timeouts(self, **kwargs):
        return self

    def headless(self, on: bool = True, **kwargs):
        self._headless = bool(on)
        return self

    def set_argument(self, *args, **kwargs):
        if len(args) == 2:
            self._args.append("%s=%s" % (args[0], args[1]))
        elif args:
            self._args.append(str(args[0]))
        return self

    def set_browser_path(self, path):
        self._browser_path = str(path or "")
        return self

    def set_proxy(self, proxy):
        self._proxy = str(proxy or "")
        return self

    def set_user_agent(self, ua):
        self._user_agent = str(ua or "")
        return self

    def add_extension(self, path):
        if path:
            self._extensions.append(str(path))
        return self

    def set_user_data_path(self, path):
        self._user_data_path = str(path or "")
        return self

    def set_timezone(self, tz):
        self._timezone = str(tz or "")
        return self

    def set_locale(self, locale):
        self._locale = str(locale or "")
        return self

    def set_pref(self, name, value):
        self._prefs[str(name)] = value
        return self

    def set_paths(self, **kwargs):
        return self

    def existing_only(self, on=True):
        return self

    def set_retry(self, *a, **kw):
        return self

    def set_load_mode(self, *a, **kw):
        return self

    # -- 只读属性 -----------------------------------------------------
    @property
    def headless_enabled(self):
        return self._headless

    @property
    def user_data_path(self):
        return self._user_data_path

    @property
    def arguments(self):
        return list(self._args)

    @property
    def proxy(self):
        return self._proxy

    @property
    def browser_path(self):
        return self._browser_path

    @property
    def user_agent(self):
        return self._user_agent

    @property
    def timezone(self):
        return self._timezone

    @property
    def locale(self):
        return self._locale

    @property
    def extensions(self):
        return list(self._extensions)

    # -- Camoufox 启动参数 --------------------------------------------
    def to_launch_options(self) -> dict:
        """转成 Camoufox 的启动参数。

        注意：Camoufox 的 ``launch_options()`` 没有 ``timezone`` 参数，时区必须
        通过 ``env["TZ"]`` 交给 Firefox 自己读；而 ``privacy.resistFingerprinting``
        正是 Camoufox 指纹保护的基础，绝不能被关掉。
        """
        opts = {
            "headless": self._headless,
            # 不再强制伪装成 Windows：实测在 Linux 容器里生成 Windows 指纹会让
            # Cloudflare Turnstile 拒绝初始化（api.js 请求成功、onload 触发，但
            # 脚本体不执行、window.turnstile 永不出现）。Camoufox 的 os 默认值是
            # ["windows", "macos", "linux"] 随机取一个，solver 正是随机到 linux 才
            # 能解题。这里显式固定为 linux，与容器真实环境一致，保证指纹自洽。
            "os": ["linux"],
            "humanize": True,
            "geoip": False,
            "locale": self._locale or "en-US",
            "enable_cache": True,
            "i_know_what_im_doing": True,
        }
        if self._proxy:
            parsed = parse_proxy(self._proxy)
            if parsed:
                opts["proxy"] = parsed
        # 时区走进程级 TZ（Firefox 启动时自然继承）。这里刻意不使用 Camoufox 的
        # env= 参数：它语义不明，可能整包替换启动环境，把 DISPLAY 一起弄丢。
        if self._timezone:
            os.environ["TZ"] = self._timezone
            try:
                time.tzset()
            except Exception:
                pass
        # 默认省流量首选项：禁用网页外置字体、遥测上报与自动更新
        default_prefs = {
            "browser.display.use_document_fonts": 0,
            "datareporting.policy.dataSubmissionEnabled": False,
            "toolkit.telemetry.enabled": False,
            "browser.safebrowsing.downloads.enabled": False,
            "extensions.update.enabled": False,
        }
        merged_prefs = dict(default_prefs)
        if self._prefs:
            merged_prefs.update(self._prefs)
        opts["firefox_user_prefs"] = merged_prefs
        if self._user_data_path:
            opts["persistent_context"] = True
            opts["user_data_dir"] = self._user_data_path
        if self._args:
            opts["args"] = list(self._args)
        if self._extensions:
            opts["addons"] = list(self._extensions)
        return opts


def parse_proxy(proxy: str):
    """把代理 URL 解析成 Playwright 的 proxy 字典。

    Playwright/Camoufox 认 ``socks5://``，不认 curl 风格的 ``socks5h://``
    （h 只表示远程 DNS，socks5 本身已由代理解析域名），所以这里做归一化。
    """
    raw = str(proxy or "").strip()
    if not raw:
        return None
    normalized = raw
    lowered = raw.lower()
    if lowered.startswith("socks5h://"):
        normalized = "socks5://" + raw[len("socks5h://"):]
    elif lowered.startswith("socks4a://"):
        normalized = "socks4://" + raw[len("socks4a://"):]
    try:
        parsed = urllib.parse.urlsplit(normalized)
    except Exception:
        return None
    if not parsed.hostname:
        return None
    scheme = (parsed.scheme or "http").lower()
    server = "%s://%s:%s" % (scheme, parsed.hostname, parsed.port or _default_port(scheme))
    out = {"server": server}
    if parsed.username:
        out["username"] = urllib.parse.unquote(parsed.username)
    if parsed.password:
        out["password"] = urllib.parse.unquote(parsed.password)
    return out


def _default_port(scheme: str) -> int:
    return {"http": 80, "https": 443, "socks5": 1080, "socks4": 1080}.get(scheme, 1080)


# 在每个页面加载前安装 window.turnstile 垫片。
#
# 背景：x.ai 启用了 Cloudflare Bot Management 之后，Turnstile 的 api.js 虽然请求
# 成功、onload 也会触发，但脚本体在 accounts.x.ai 上不执行，window.turnstile 永不
# 出现。x.ai 的注册守卫因此拿不到 token，sign-up 请求根本不会发出（点击按钮只会
# 触发 ValidatePassword）。
#
# 这里用 defineProperty 把 window.turnstile 变成"永远存在"的访问器：
#   * 真实 API 出现时（脚本能执行的场景）自动挂上 getResponse 覆写，优先返回我们
#     通过外部 solver 拿到的 token，拿不到再回落到真实实现；
#   * 真实 API 始终不出现时，返回一个最小 mock，getResponse 同样返回 solver token。
# 这样无论 Turnstile 是否执行，x.ai 守卫读到的都是有效 token。
_TURNSTILE_SHIM_JS = r"""
(function () {
    if (window.__g2aTurnstileShim) { return; }
    window.__g2aTurnstileShim = true;
    var _real = null;

    function _recordCallback(opts) {
        if (!opts) return;
        window.__g2aRenderCalls = (window.__g2aRenderCalls || 0) + 1;
        if (typeof opts.callback === 'function') {
            window.__g2aPendingCallbacks = window.__g2aPendingCallbacks || [];
            if (window.__g2aPendingCallbacks.indexOf(opts.callback) < 0) {
                window.__g2aPendingCallbacks.push(opts.callback);
            }
            var t = window.__solverToken;
            if (t) {
                setTimeout(function () { try { opts.callback(t); } catch (e) {} }, 0);
            }
        }
        if (typeof opts['error-callback'] === 'function') {
            window.__g2aErrorCallbacks = window.__g2aErrorCallbacks || [];
            window.__g2aErrorCallbacks.push(opts['error-callback']);
        }
    }

    function _wrapReal(r) {
        if (!r || r.__g2aHooked) return r;
        r.__g2aHooked = true;
        try {
            var origRender = r.render;
            r.render = function (target, opts) {
                if (target) { window.__g2aWidget = target; }
                _recordCallback(opts);
                try {
                    return origRender ? origRender.apply(r, arguments) : 'g2a-mock-widget';
                } catch (e) {
                    return 'g2a-mock-widget';
                }
            };
            var origGetResp = r.getResponse;
            r.getResponse = function (widget) {
                var t = window.__solverToken;
                if (t) { return t; }
                try { return origGetResp ? origGetResp.call(r, widget) : ''; } catch (e) { return ''; }
            };
            var origExec = r.execute;
            r.execute = function (target, opts) {
                if (target) { window.__g2aWidget = target; }
                _recordCallback(opts);
                try {
                    return origExec ? origExec.apply(r, arguments) : undefined;
                } catch (e) {
                    return undefined;
                }
            };
        } catch (e) {}
        return r;
    }

    function _mock() {
        return {
            getResponse: function () { return window.__solverToken || ''; },
            render: function (target, opts) {
                if (target) { window.__g2aWidget = target; }
                _recordCallback(opts);
                return 'g2a-mock-widget';
            },
            reset: function () {},
            remove: function () {},
            ready: function (cb) { if (typeof cb === 'function') { try { cb(); } catch (e) {} } },
            execute: function (target, opts) {
                if (target) { window.__g2aWidget = target; }
                _recordCallback(opts);
            },
            isExecuted: function () { return !!window.__solverToken; }
        };
    }

    try {
        Object.defineProperty(window, 'turnstile', {
            configurable: true,
            enumerable: true,
            get: function () {
                if (_real) {
                    return _wrapReal(_real);
                }
                return _mock();
            },
            set: function (v) {
                _real = _wrapReal(v);
            }
        });
    } catch (e) {}
})();
"""


class CamoufoxBrowser:
    """模拟 DrissionPage 的 ``Chromium`` 浏览器对象。"""

    def __init__(self, options: CamoufoxOptions = None, **kwargs):
        self._options = options or CamoufoxOptions()
        self._camoufox = None
        self._playwright = None
        self._browser = None
        self._context = None
        self._pages = []
        self._counter = 0
        self._user_data_path = self._options.user_data_path or ""
        self._start()

    # -- 启动 / 关闭 ---------------------------------------------------
    def _start(self):
        from camoufox.sync_api import Camoufox

        launch_opts = self._options.to_launch_options()
        self._camoufox = Camoufox(**launch_opts)
        handle = self._camoufox.start()
        self._playwright = getattr(self._camoufox, "_playwright", None)
        # Camoufox 视 persistent_context 返回 BrowserContext，否则返回 Browser
        if hasattr(handle, "new_context") and hasattr(handle, "contexts"):
            self._browser = handle
            self._context = handle.contexts[0] if handle.contexts else handle.new_context()
        else:
            self._context = handle
            self._browser = getattr(handle, "browser", None)
        self._user_data_path = (
            getattr(handle, "user_data_dir", None)
            or self._options.user_data_path
            or ""
        )
        # 每个页面加载前安装 Turnstile 垫片，详见 _TURNSTILE_SHIM_JS 的说明。
        try:
            self._context.add_init_script(_TURNSTILE_SHIM_JS)
        except Exception:
            pass

        # 流量与反爬平衡：仅拦截大体积多媒体与纯外部统计，绝不拦截字体和图片以保护 Cloudflare JSD 指纹
        try:
            def _traffic_filter(route):
                try:
                    req = route.request
                    url = req.url.lower()
                    rt = req.resource_type

                    # 1. 核心白名单：Cloudflare、Turnstile、x.ai 核心业务绝对放行
                    if "challenges.cloudflare.com" in url or "cloudflare" in url or "turnstile" in url or "accounts.x.ai" in url or "auth.x.ai" in url:
                        route.continue_()
                        return

                    # 2. 外部纯遥测与重型分析埋点（安全拦截）
                    telemetry = (
                        "statsig.com",
                        "datadoghq.com",
                        "sentry.io",
                        "google-analytics.com",
                        "googletagmanager.com",
                    )
                    if any(t in url for t in telemetry):
                        route.abort()
                        return

                    # 3. 大体积视频/音频媒体流（页面无此类内容，拦截防意外浪费）
                    if rt == "media" or any(url.endswith(ext) for ext in (".mp4", ".webm", ".mp3", ".ogg")):
                        route.abort()
                        return

                    route.continue_()
                except Exception:
                    try:
                        route.continue_()
                    except Exception:
                        pass

            self._context.route("**/*", _traffic_filter)
        except Exception:
            pass

    def _wrap(self, pw_page):
        self._counter += 1
        wrapper = CamoufoxPage(pw_page, browser=self, index=self._counter)
        self._pages.append(wrapper)
        return wrapper

    # -- DrissionPage 接口 --------------------------------------------
    @property
    def user_data_path(self):
        return self._user_data_path

    @property
    def context(self):
        return self._context

    @property
    def native_browser(self):
        return self._browser

    def get_tabs(self):
        """返回当前所有标签页（保持 DrissionPage 的「至少一个」语义）。"""
        try:
            live = list(self._context.pages)
        except Exception:
            live = []
        # 同步新建但未登记的页面
        known = {w.native for w in self._pages}
        for pw_page in live:
            if pw_page not in known:
                self._wrap(pw_page)
        self._pages = [w for w in self._pages if w.native in live]
        if not self._pages and live:
            self._pages = [self._wrap(live[0])]
        return list(self._pages)

    def new_tab(self, url: str = "", **kwargs):
        pw_page = self._context.new_page()
        wrapper = self._wrap(pw_page)
        if url:
            try:
                wrapper.get(url)
            except Exception:
                pass
        return wrapper

    def get_tab(self, index: int = 0, **kwargs):
        tabs = self.get_tabs()
        if not tabs:
            return self.new_tab()
        try:
            return tabs[int(index)]
        except Exception:
            return tabs[0]

    def latest_tab(self):
        tabs = self.get_tabs()
        return tabs[-1] if tabs else self.new_tab()

    def quit(self, del_data: bool = False, **kwargs):
        try:
            if self._camoufox is not None:
                self._camoufox.__exit__(None, None, None)
        except Exception:
            pass
        finally:
            self._camoufox = None
            self._browser = None
            self._context = None
            self._pages = []
            if del_data and self._user_data_path:
                try:
                    import shutil
                    shutil.rmtree(self._user_data_path, ignore_errors=True)
                except Exception:
                    pass

    def close(self):
        self.quit()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.quit()
