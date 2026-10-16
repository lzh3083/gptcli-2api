# -*- coding: utf-8 -*-
import unittest
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "grok-build-auth"))
sys.path.insert(0, str(ROOT / "grok-build-auth" / "openai_client"))

from openai_client.core.email_provider import _get_registry, parse_email_sources, _VALID_SOURCES
from openai_client.core.registration_flow import BaseRegistrationFlow, FlowStage, RegistrationResult
from scripts.registration_service import _classify_session_failure


class TestRegistrationComponents(unittest.TestCase):
    def test_email_strategy_registry(self):
        reg = _get_registry()
        for src in _VALID_SOURCES:
            self.assertIn(src, reg, f"来源 {src} 未在策略注册表中注册")
            strategy = reg[src]
            self.assertEqual(strategy.name, src)
            self.assertTrue(callable(strategy._pick))
            self.assertTrue(callable(strategy._otp))
            self.assertTrue(callable(strategy._release))

    def test_parse_email_sources(self):
        sources = parse_email_sources("outlook,cloudflare,unknown_src,gptmail")
        self.assertEqual(sources, ["outlook", "cloudflare", "gptmail"])

    def test_registration_flow_error_classification(self):
        flow = BaseRegistrationFlow()
        self.assertEqual(flow.classify_error("Cloudflare challenge 5073 timeout"), "turnstile_solver_failed")
        self.assertEqual(flow.classify_error("Sentinel runner POW proof invalid"), "sentinel_challenge_failed")
        self.assertEqual(flow.classify_error("取信超时：未收到验证码"), "email_otp_timeout")
        self.assertEqual(flow.classify_error("hero-sms: NO_NUMBERS available"), "sms_phone_failed")
        self.assertEqual(flow.classify_error("account already exists, dirty"), "mailbox_dirty_recycled")
        self.assertEqual(flow.classify_error("OpenAI fraud guard 403 Forbidden"), "openai_risk_blocked")
        self.assertEqual(flow.classify_error("SOCKS5 connection reset by peer"), "proxy_network_error")
        self.assertEqual(flow.classify_error("Random runtime error"), "other_error")

    def test_service_session_classification(self):
        sess_cf = {"error": "Turnstile solver failed to solve token"}
        self.assertEqual(_classify_session_failure(sess_cf), "turnstile_solver_failed")

        sess_otp = {"stage": "waiting_otp", "error": "取信超时"}
        self.assertEqual(_classify_session_failure(sess_otp), "email_otp_timeout")

    def test_registration_result_dataclass(self):
        res = RegistrationResult(
            success=True,
            email="test@domain.com",
            access_token="tok_123",
            password="pass",
            codex={"access_token": "codex_123"},
        )
        d = res.to_dict()
        self.assertTrue(d["success"])
        self.assertEqual(d["email"], "test@domain.com")
        self.assertEqual(d["codex"]["access_token"], "codex_123")


if __name__ == "__main__":
    unittest.main()
