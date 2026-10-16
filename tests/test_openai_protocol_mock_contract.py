# -*- coding: utf-8 -*-
"""
ChatGPT 协议注册各步骤请求契约与回放测试 (Mock Protocol Contract Replay)
模拟 auth.openai.com 与 sentinel.openai.com 关键请求交互，验证协议参数与错误检测。
"""
import json
import unittest
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "grok-build-auth"))
sys.path.insert(0, str(ROOT / "grok-build-auth" / "openai_client"))

from openai_client.core.openai_auth import (
    AccountUnusableError,
    EmailOtpInvalidError,
    detect_account_unusable_response_body,
    detect_account_unusable_text,
    _extract_error_code,
)


class DummyResponse:
    def __init__(self, data):
        self._data = data

    def json(self):
        return self._data


class TestOpenAIProtocolContract(unittest.TestCase):
    def test_account_unusable_detection(self):
        # 1. 明确被封/停用账号响应
        resp_deactivated = json.dumps({"error": {"code": "account_deactivated", "message": "Your account has been deactivated."}})
        self.assertEqual(detect_account_unusable_response_body(resp_deactivated), "account_deactivated")

        resp_deleted = json.dumps({"error": {"code": "account_deleted"}})
        self.assertEqual(detect_account_unusable_response_body(resp_deleted), "account_deleted")

        # 2. 正常失败响应（如临时频率限制或验证码错）不应判定为账号作废
        resp_rate_limit = json.dumps({"error": {"message": "Rate limit reached, please try later."}})
        self.assertEqual(detect_account_unusable_response_body(resp_rate_limit), "")

        resp_otp_wrong = json.dumps({"error": {"message": "Invalid verification code entered."}})
        self.assertEqual(detect_account_unusable_response_body(resp_otp_wrong), "")

    def test_error_code_extraction(self):
        resp = DummyResponse({"error": {"code": "invalid_otp_code", "message": "code expired"}})
        self.assertEqual(_extract_error_code(resp), "invalid_otp_code")

        resp_empty = DummyResponse("plain text error")
        self.assertEqual(_extract_error_code(resp_empty), "")

    def test_unusable_text_detection(self):
        self.assertEqual(detect_account_unusable_text("your account has been deleted by administrator"), "account_deleted")
        self.assertEqual(detect_account_unusable_text("account was deactivated"), "account_deactivated")
        self.assertEqual(detect_account_unusable_text("Timeout waiting for response from server"), "")


if __name__ == "__main__":
    unittest.main()
