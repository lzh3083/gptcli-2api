"""Shared cancellation / retry helpers for the browser registration engine.

Why this module exists
----------------------
The upstream grok-register engine published ``AccountRetryNeeded``,
``raise_if_cancelled``, ``sleep_with_cancel``, ``RemoteTokenRequestError``,
``RemoteTokenCompatibilityError`` and ``log_exception`` from its top-level
``grok_register_ttk`` module, which fanned them out to the submodules through
``bind_runtime()``.

That top-level module was not carried over when the engine was folded into
grokcli-2api, so ``registration_browser`` and ``mail_service`` referenced names
that no longer existed. The failures only surfaced at runtime, e.g.::

    浏览器启动失败，已重试4次: name 'prepare_browser_proxy' is not defined
    浏览器启动失败，已重试4次: name 'create_browser_options' is not defined
    name 'sleep_with_cancel' is not defined

Putting the helpers in one dependency-free module keeps every consumer a single
import away from them, with no import cycles (nothing here imports any other
browser_register module).
"""
from __future__ import annotations

import time

# Cancel polling granularity: short enough to feel instant, long enough that a
# multi-minute wait does not spin the CPU.
_SLEEP_SLICE_SEC = 0.25


class AccountRetryNeeded(RuntimeError):
    """当前账号需要整体重试（浏览器处于可恢复的瞬时状态）。"""


class RemoteTokenRequestError(RuntimeError):
    """远程 token / 管理接口请求失败。"""


class RemoteTokenCompatibilityError(RuntimeError):
    """远程 token / 管理接口返回了不兼容的响应格式。"""


def raise_if_cancelled(cancel_callback=None):
    """调用方请求取消时立即抛出 —— cancel_callback 自身负责抛异常。

    ``cancel_callback`` 由适配器传入，内部调用 ``check_cancel()``；一旦用户
    在界面上点了停止，它就会抛出取消异常，这里只需让它自然传播。
    """
    if cancel_callback is not None:
        cancel_callback()


def sleep_with_cancel(seconds, cancel_callback=None):
    """分片睡眠，保证取消能被及时响应（对齐原 grok_register_ttk 行为）。

    没有回调时退化为普通 ``time.sleep``；有回调时每片都检查一次取消。
    """
    total = max(0.0, float(seconds or 0))
    if cancel_callback is None:
        time.sleep(total)
        return
    deadline = time.time() + total
    while True:
        cancel_callback()
        remaining = deadline - time.time()
        if remaining <= 0:
            return
        time.sleep(min(_SLEEP_SLICE_SEC, remaining))


def log_exception(prefix, exc=None, log_callback=None):
    """轻量异常日志，返回可读文本供调用方写入结果字段。

    对齐原 grok_register_ttk 的三参数调用：
    ``log_exception("写入 grok2api 本地池失败", exc, log_callback)``。
    """
    if exc is None:
        text = str(prefix or "")
    else:
        text = f"{prefix}: {exc.__class__.__name__}: {exc}"
    if log_callback is not None:
        try:
            log_callback(f"[!] {text}")
        except Exception:
            pass
    return text
