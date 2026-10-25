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



# ==============================================================================
# 自然行为拟人对话激活模块 (Warm-up / Anti-Clustering)
# ==============================================================================
import random

WARMUP_PROMPTS = [
    # 英文生活与健康
    "What are some quick and healthy breakfast ideas that take under 10 minutes?",
    "How can I keep fresh vegetables crisp and last longer in the fridge?",
    "What is the best way to clean and maintain a cast-iron skillet?",
    "Can you suggest a few simple indoor plants that need minimal sunlight?",
    "What is the difference between cold brew and regular iced coffee?",
    "What are some good indoor activities for a rainy afternoon?",
    "Can you give me 3 practical tips for organizing a messy work desk?",
    "What is the healthiest way to cook eggs without losing nutrients?",
    # 英文常识与趣味
    "Could you recommend 3 classic mystery novels suitable for beginners?",
    "What are some practical tips to improve typing speed and accuracy?",
    "Explain the basic difference between stocks and mutual funds simply.",
    "What are some gentle neck and shoulder stretches I can do at my desk?",
    "Give me 3 creative themes for a weekend family movie night.",
    "Can you write a short, cozy haiku about an autumn afternoon?",
    "Why do leaves change color in the autumn? Explain like I am ten.",
    # 中文生活与常识
    "平时喝茶的话，绿茶、红茶和乌龙茶在冲泡温度上有什么讲究吗？",
    "想在家里做点简单清爽的夏日饮品，有什么零失败的搭配推荐？",
    "推荐几本通俗易懂的心理学入门书籍，适合碎片时间阅读的。",
    "长时间看电脑屏幕眼睛容易干涩，日常有什么缓解疲劳的小技巧？",
    "出差收拾行李时，如何快速整理衣物才能不占空间又防皱？",
    "能不能推荐三种简单易做、适合带饭的工作日便当菜谱？",
    "家庭养猫需要准备哪些基础用品？有什么新手容易忽略的细节？",
    "请简单科普一下为什么运动后会有肌肉酸痛，怎么恢复最快？",
    "想培养每天晨读的习惯，有什么容易长期坚持的方法？",
    "推荐几个适合周末短途自驾游的放松活动类型。"
]


def _dismiss_welcome_popups(driver: Any) -> None:
    """清理新账号首次登录 chatgpt.com 时可能遮挡输入框的欢迎引导与提示弹窗。"""
    dismiss_js = """
    try {
        const keywords = [
            "okay, let's go", "next", "done", "got it", "stay logged out", 
            "dismiss", "continue", "start chatting", "关闭", "知道了", "继续",
            "accept all", "agree", "agree and continue", "sounds good"
        ];
        // 优先在 dialog 中寻找并点击
        const dialogs = document.querySelectorAll('div[role="dialog"], div[role="alertdialog"], .modal, [data-state="open"]');
        for (const d of dialogs) {
            const close = d.querySelector('button[aria-label*="Close"], button[aria-label*="close"], button[data-testid*="close"]');
            if (close) { close.click(); }
            const btns = Array.from(d.querySelectorAll('button'));
            for (const b of btns) {
                const txt = (b.innerText || b.textContent || "").trim().toLowerCase();
                if (keywords.some(k => txt.includes(k))) {
                    b.click();
                }
            }
        }
        // 全局关闭按钮
        const globalClose = document.querySelectorAll('button[aria-label*="Close"], button[aria-label*="close"], button[aria-label*="Dismiss"]');
        for (const gc of globalClose) { gc.click(); }
        // 页面通用按钮
        const buttons = Array.from(document.querySelectorAll('button'));
        for (const btn of buttons) {
            const txt = (btn.innerText || btn.textContent || "").trim().toLowerCase();
            if (keywords.some(k => txt === k)) {
                btn.click();
            }
        }
    } catch(e) {}
    """
    try:
        if hasattr(driver, "page") and hasattr(driver.page, "evaluate"):
            driver.page.evaluate(dismiss_js)
        elif hasattr(driver, "execute_script"):
            driver.execute_script(dismiss_js)
    except Exception:
        pass


def _warmup_chatgpt_conversation(driver: Any, email: str, step_cb: Any = None) -> bool:
    """在当前已登录的指纹浏览器上下文里完成一轮拟人问答激活 (闭环沉淀云端真实会话记录)。"""
    try:
        if step_cb:
            step_cb("warmup", "准备在 ChatGPT 页面执行拟人首聊对话激活 (关键防封，沉淀真实会话)...")
        logger.info("[Warmup] 开始为 %s 执行原地对话激活 (模拟自然行为，沉淀真实交互会话)...", email)
        
        page = getattr(driver, "page", None)
        if not page:
            logger.warning("[Warmup] 未获取到浏览器 Page 对象，跳过对话激活")
            return False

        # 1. 确认当前在 chatgpt.com 主界面
        try:
            cur_url = str(getattr(driver, "current_url", "") or getattr(page, "url", "") or "")
            if "chatgpt.com" not in cur_url or "auth.openai.com" in cur_url:
                logger.info("[Warmup] 当前页面为 %s，导航至 https://chatgpt.com/", cur_url)
                page.goto("https://chatgpt.com/", timeout=40000, wait_until="domcontentloaded")
                time.sleep(random.uniform(3.0, 5.0))
        except Exception as _nav_e:
            logger.warning("[Warmup] 导航至 chatgpt.com 提示: %s，继续尝试探测输入框", _nav_e)

        # 关掉任何可能遮挡界面的弹窗
        _dismiss_welcome_popups(driver)
        time.sleep(random.uniform(1.0, 2.0))
        _dismiss_welcome_popups(driver)
        
        # 2. 定位并聚焦输入框 (优先定位 ProseMirror contenteditable div，绕过 pointer-events 遮挡)
        focus_js = """
        () => {
            const sels = [
                'div#prompt-textarea[contenteditable="true"]',
                'div[contenteditable="true"]',
                'textarea#prompt-textarea',
                'textarea[placeholder*="Message"]',
                'textarea[placeholder*="Ask"]',
                'textarea'
            ];
            for (const s of sels) {
                const els = Array.from(document.querySelectorAll(s));
                for (const el of els) {
                    const r = el.getBoundingClientRect();
                    const st = window.getComputedStyle(el);
                    if (r.width > 20 && r.height > 10 && st.visibility !== 'hidden' && st.display !== 'none') {
                        el.scrollIntoView({block: 'center'});
                        el.focus();
                        el.click();
                        return {ok: true, selector: s, tag: el.tagName};
                    }
                }
            }
            return {ok: false};
        }
        """
        focus_res = {"ok": False}
        for _ in range(5):
            try:
                _dismiss_welcome_popups(driver)
                focus_res = page.evaluate(focus_js) or {"ok": False}
                if focus_res.get("ok"):
                    break
                page.keyboard.press("Escape")
            except Exception:
                pass
            time.sleep(1.2)

        if not focus_res.get("ok"):
            logger.warning("[Warmup] 未在当前页面找到可见的 ChatGPT 输入框，当前 URL: %s", getattr(page, "url", ""))
            return False

        # 3. 拟人打字输入 (直接通过 keyboard.type 向当前焦点输入，避免 ElementHandle.click 遮挡超时)
        prompt = random.choice(WARMUP_PROMPTS)
        if step_cb:
            step_cb("warmup", f"拟人键入首条问题: {prompt[:30]}...")
        logger.info("[Warmup] 已聚焦输入框 (%s)，选取问答题目: %s", focus_res.get("selector"), prompt)

        try:
            time.sleep(random.uniform(0.2, 0.5))
            for ch in prompt:
                page.keyboard.type(ch, delay=random.randint(20, 60))
                if ch in (",", "，", ".", "。", "?", "？", " ", "!"):
                    time.sleep(random.uniform(0.06, 0.18))
            time.sleep(random.uniform(0.5, 0.9))
        except Exception as _type_e:
            logger.warning("[Warmup] keyboard.type 异常: %s", _type_e)

        # 校验输入框内容是否已成功写入，若为空则通过 execCommand / InputEvent 兜底注入
        ensure_text_js = """
        (text) => {
            const el = document.querySelector('div#prompt-textarea[contenteditable="true"]')
                    || document.querySelector('div[contenteditable="true"]')
                    || document.querySelector('textarea#prompt-textarea')
                    || document.querySelector('textarea');
            if (!el) return {ok: false, len: 0};
            el.focus();
            const cur = (el.innerText || el.value || '').trim();
            if (cur.length < 5) {
                try {
                    document.execCommand('selectAll', false, null);
                    document.execCommand('insertText', false, text);
                } catch(e) {}
                if ((el.innerText || el.value || '').trim().length < 5) {
                    if (el.tagName === 'TEXTAREA') {
                        const setter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set;
                        setter.call(el, text);
                    } else {
                        el.innerHTML = '<p>' + text + '</p>';
                    }
                    el.dispatchEvent(new InputEvent('input', {bubbles: true, inputType: 'insertText', data: text}));
                }
            }
            const finalLen = (el.innerText || el.value || '').trim().length;
            return {ok: finalLen >= 5, len: finalLen};
        }
        """
        try:
            text_state = page.evaluate(ensure_text_js, prompt) or {}
            logger.info("[Warmup] 输入框文本校验: ok=%s, len=%s", text_state.get("ok"), text_state.get("len"))
        except Exception as _ens_e:
            logger.warning("[Warmup] 输入框文本校验异常: %s", _ens_e)

        # 4. 触发发送并校验发送动作
        if step_cb:
            step_cb("warmup", "提交首聊问题，等待模型流式响应生成...")
        logger.info("[Warmup] 提交问题，等待 ChatGPT 响应生成...")

        click_send_js = """
        () => {
            const selectors = [
                'button[data-testid="send-button"]:not([disabled])',
                'button[data-testid="fruitjuice-send-button"]:not([disabled])',
                'button[aria-label*="Send"]:not([disabled])',
                'button[aria-label*="发送"]:not([disabled])'
            ];
            for (const s of selectors) {
                const btn = document.querySelector(s);
                if (btn && !btn.disabled) {
                    btn.click();
                    return {clicked: true, selector: s};
                }
            }
            return {clicked: false};
        }
        """
        sent = False
        for _ in range(5):
            try:
                res_click = page.evaluate(click_send_js) or {}
                if res_click.get("clicked"):
                    sent = True
                    logger.info("[Warmup] 已通过 JS 点击发送按钮 (%s)", res_click.get("selector"))
                    break
            except Exception:
                pass
            time.sleep(0.4)

        if not sent:
            try:
                page.keyboard.press("Enter")
                logger.info("[Warmup] 已触发键盘 Enter 发送")
                sent = True
            except Exception as _e:
                logger.warning("[Warmup] 键盘 Enter 异常: %s", _e)

        # 5. 等待模型开始生成 (捕获 stop-button 或 assistant 角色消息)
        generating_started = False
        start_wait = time.time()
        for _ in range(20):
            try:
                stop_btn = page.query_selector('button[data-testid="stop-button"]')
                assistant_node = page.query_selector('div[data-message-author-role="assistant"], article[data-testid*="conversation-turn"]')
                if stop_btn or assistant_node:
                    generating_started = True
                    logger.info("[Warmup] 检测到模型已开始生成回答 (stop_button / assistant_node 出现)")
                    break
            except Exception:
                pass
            time.sleep(0.5)

        if not generating_started:
            # 补点一次可能存在的发送按钮
            try:
                retry_send = page.query_selector('button[data-testid="send-button"]')
                if retry_send:
                    retry_send.click()
                    time.sleep(2)
            except Exception:
                pass

        # 6. 等待模型流式回复完全结束 (stop-button 消失)
        max_generate_wait = 35
        wait_deadline = time.time() + max_generate_wait
        while time.time() < wait_deadline:
            try:
                stop_btn = page.query_selector('button[data-testid="stop-button"]')
                if generating_started and not stop_btn:
                    # stop 按钮已消失，生成完毕
                    time.sleep(1.5)
                    break
            except Exception:
                pass
            time.sleep(1.0)

        # 7. 捕获模型真实回答文本与会话 URL 进行权威存活性校验
        reply_snippet = ""
        try:
            reply_el = page.query_selector('div[data-message-author-role="assistant"] .markdown, div[data-message-author-role="assistant"]')
            if reply_el:
                reply_snippet = (reply_el.inner_text() or "").strip()
        except Exception:
            pass

        final_url = str(getattr(page, "url", "") or "")
        elapsed_warmup = round(time.time() - start_wait, 1)

        if reply_snippet or "/c/" in final_url:
            logger.info(
                "[Warmup] ★ 对话首聊成功沉淀！耗时: %.1fs | 会话URL: %s | 模型回复前40字: %s",
                elapsed_warmup, final_url, reply_snippet[:40].replace("\n", " ") if reply_snippet else "(已捕获会话ID)"
            )
            if step_cb:
                step_cb("warmup", f"★ 对话激活完成 (耗时 {elapsed_warmup}s): {reply_snippet[:25]}...")
            return True
        else:
            logger.warning("[Warmup] 首聊等待完成但未提取到模型文本 (url=%s, 耗时=%.1fs)，继续后续流程", final_url, elapsed_warmup)
            return False

    except Exception as exc:
        logger.warning("[Warmup] 对话激活过程遇到非致命异常: %s (跳过，不阻断主流程)", exc)
        return False


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
    fingerprint_seed: int | str | None = None,
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
        driver, opened = build_cloak_driver(proxy=proxy, fingerprint_seed=fingerprint_seed)
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

        # 阶段：同源指纹浏览器原地对话激活 (Warm-up 模拟自然行为，关键防封)
        # 铁律：必须在当前活跃会话、处于 chatgpt.com 且登录态完备时立即执行！
        if str(os.environ.get("CODEX_ENABLE_WARMUP", "1")).strip() in ("1", "true", "True"):
            try:
                _step("warmup", "复用当前同源指纹浏览器执行原地首聊对话激活 (关键防封，沉淀真实会话)")
                _warmup_chatgpt_conversation(driver, email, step_cb=_step)
            except Exception as _w_err:
                logger.warning("[Cloak注册][Warmup] 对话激活跳过或遇到非致命异常: %s", _w_err)

        cookies_dict = driver.get_cookies_dict() if hasattr(driver, "get_cookies_dict") else {}

        if _twofa_cfg.ENABLE_2FA:
            logger.warning("[Cloak注册] 当前 CloakBrowser 自动化路径暂不执行 2FA 设置，已跳过")
        totp_secret = None

        cpa_chatgpt_file = ""
        # 铁律：只要开启了 enable_codex，严禁提前生成无手机号的单独 chatgpt-*.json 孤儿文件！
        if not enable_codex:
            try:
                data_root = os.environ.get("GROK2API_DATA_DIR") or (
                    "/app/data" if Path("/app/data").is_dir() else str(Path(__file__).resolve().parents[3] / "data")
                )
                cpa_dir = Path(data_root) / "cpa_auth_files"
                cpa_dir.mkdir(parents=True, exist_ok=True)
                from core.codex_oauth import resolve_cpa_proxy_url
                cpa_proxy_url = resolve_cpa_proxy_url(effective_proxy)
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
        codex_ok = bool(codex_result.get("ok")) if enable_codex else True
        cpa_primary_file = (
            (codex_result.get("file_path") if isinstance(codex_result, dict) else "")
            if enable_codex else cpa_chatgpt_file
        )
        if enable_codex and not codex_ok:
            cpa_primary_file = ""
        if codex_ok:
            _step("success", f"CloakBrowser 真实浏览器注册与 Codex 授权成功: {email}")
        return {
            "success": bool(codex_ok),
            "email": email,
            "password": effective_password,
            "account_id": account_id if codex_ok else None,
            "access_token": access_token if codex_ok else "",
            "session_info": session_info if codex_ok else {},
            "cookies": cookies_dict,
            "totp_secret": totp_secret,
            "codex": codex_result,
            "cpa_file": cpa_primary_file,
            "network_traffic": network_traffic,
            "error": None if codex_ok else f"Codex 手机验证或授权未完成: {codex_result.get('message')}",
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
