# -*- coding: utf-8 -*-
"""
ChatGPT 注册流程基础协议与流程骨架 (Base Registration Flow)。
收敛 browser_use、cloakbrowser、roxy、skyvern 四套驱动的同构生命周期：
1. 环境预检与住宅代理绑定
2. 浏览器与过盾环境初始化
3. 邮箱分配 (延迟分配以防空耗)
4. OTP 接收与验证
5. 个人资料/密码配置
6. Codex OAuth 授权与 CPA 凭证导出
"""
from __future__ import annotations

import logging
from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Callable

logger = logging.getLogger(__name__)


class FlowStage(str, Enum):
    STARTING = "starting"
    SOLVING_CAPTCHA = "solving_turnstile"
    EMAIL_ACQUIRED = "email_acquired"
    EMAIL_SUBMITTED = "email_submitted"
    OTP_WAITING = "otp_waiting"
    OTP_VERIFIED = "otp_verified"
    PROFILE_SUBMITTING = "profile_submitting"
    CODEX_AUTHORIZING = "codex_authorizing"
    SUCCESS = "success"
    FAILED = "failed"


@dataclass
class RegistrationResult:
    success: bool
    email: str = ""
    access_token: str = ""
    session_info: dict[str, Any] = field(default_factory=dict)
    cookies: dict[str, Any] = field(default_factory=dict)
    password: str = ""
    totp_secret: str | None = None
    codex: dict[str, Any] = field(default_factory=dict)
    error: str | None = None
    failure_category: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "success": self.success,
            "email": self.email,
            "access_token": self.access_token,
            "session_info": self.session_info,
            "cookies": self.cookies,
            "password": self.password,
            "totp_secret": self.totp_secret,
            "codex": self.codex,
            "error": self.error,
            "failure_category": self.failure_category,
        }


class BaseRegistrationFlow:
    """ChatGPT 自动化注册通用流程骨架。"""

    def __init__(
        self,
        *,
        on_step: Callable[[str, str], None] | None = None,
        check_cancel: Callable[[], None] | None = None,
    ) -> None:
        self.on_step = on_step
        self.check_cancel = check_cancel

    def step(self, stage: FlowStage | str, message: str) -> None:
        val = stage.value if isinstance(stage, FlowStage) else str(stage)
        logger.info(f"[{val}] {message}")
        if self.on_step:
            try:
                self.on_step(val, message)
            except Exception:
                pass

    def cancel_check(self) -> None:
        if self.check_cancel:
            self.check_cancel()

    def classify_error(self, err_msg: str) -> str:
        s = str(err_msg or "").lower()
        if any(k in s for k in ["turnstile", "captcha", "cf_clearance", "cloudflare"]):
            return "turnstile_solver_failed"
        if any(k in s for k in ["sentinel", "pow", "sdk.js"]):
            return "sentinel_challenge_failed"
        if any(k in s for k in ["otp", "mail", "inbox", "验证码", "取信超时"]):
            return "email_otp_timeout"
        if any(k in s for k in ["phone", "sms", "接码"]):
            return "sms_phone_failed"
        if any(k in s for k in ["dirty", "recycled", "deactivated", "已存在", "二手"]):
            return "mailbox_dirty_recycled"
        if any(k in s for k in ["fraud", "blocked", "403", "denied"]):
            return "openai_risk_blocked"
        if any(k in s for k in ["proxy", "socks5", "timeout", "connect"]):
            return "proxy_network_error"
        return "other_error"
