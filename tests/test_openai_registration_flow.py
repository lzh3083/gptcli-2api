# -*- coding: utf-8 -*-
"""
端到端测试：验证 ChatGPT 注册流程各个核心环节的正确性
包括：
1. 验证码抽取 (otp_utils.extract_otp)
2. 密码生成与资料随机 (profile_utils / openai_auth)
3. CPA 凭证规范与导出逻辑
4. 注册调度与状态回调流
"""
import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "grok-build-auth"))
sys.path.insert(0, str(ROOT / "grok-build-auth" / "openai_client"))

from openai_client.core.otp_utils import extract_otp, looks_like_openai_email
from openai_client.core.profile_utils import generate_random_birthday
from openai_client.core.name_samples import random_display_name
from openai_client.core.openai_auth import generate_registration_password
from openai_client.core.codex_oauth import build_codex_storage, save_codex_credential
import grok2api.upstream.grok_build_adapter as adapter


def test_otp_extraction():
    # 测试各种格式的 OpenAI 验证码邮件
    mail_en = {
        "from": "support@openai.com",
        "subject": "Your OpenAI verification code",
        "text": "Your verification code is 849201. Please enter this code within 10 minutes.",
    }
    assert looks_like_openai_email(mail_en), "未能识别 OpenAI 英文邮件"
    code = extract_otp(mail_en)
    assert code == "849201", f"提取验证码错误: {code}"

    mail_subject_code = {
        "from": "noreply@tm.openai.com",
        "subject": "Your OpenAI code is 319502",
        "text": "Thank you for registering.",
    }
    assert extract_otp(mail_subject_code) == "319502", "未能从主题中提取验证码"

    mail_zh = {
        "from": "service@openai.com",
        "subject": "OpenAI 帐户验证码",
        "html": "<p>您的验证代码为：<strong>684120</strong>，请勿泄露。</p>",
    }
    assert looks_like_openai_email(mail_zh), "未能识别 OpenAI 中文邮件"
    assert extract_otp(mail_zh) == "684120", "未能从 HTML 提取中文验证码"
    print("test_otp_extraction: PASS")


def test_profile_and_password():
    pwd = generate_registration_password()
    assert len(pwd) >= 12, f"密码长度不足: {pwd}"
    bday = generate_random_birthday()
    assert len(bday.split("-")) == 3, f"生日格式错误: {bday}"
    name = random_display_name()
    assert " " in name, f"姓名格式错误: {name}"
    print("test_profile_and_password: PASS")


def test_cpa_file_export():
    token_resp = {
        "access_token": "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.dummy_access",
        "refresh_token": "dummy_refresh_token_12345",
        "id_token": "dummy_id_token",
        "expires_in": 3600,
    }
    id_claims = {
        "email": "test-gpt@example.com",
        "account_id": "acc-123456",
        "plan_type": "free",
    }
    storage = build_codex_storage(token_resp, id_claims)
    assert storage.get("type") == "codex"
    assert storage.get("access_token") == token_resp["access_token"]
    assert storage.get("refresh_token") == token_resp["refresh_token"]

    saved_path = save_codex_credential(storage, "test-gpt@example.com", "free")
    assert Path(saved_path).is_file(), f"CPA 凭证未落盘: {saved_path}"
    data = json.loads(Path(saved_path).read_text(encoding="utf-8"))
    assert data["email"] == "test-gpt@example.com"
    assert data["type"] == "codex"
    print("test_cpa_file_export: PASS")


def test_registration_availability():
    avail = adapter.registration_available()
    assert isinstance(avail, dict)
    assert avail.get("ok") is True, f"注册机不可用: {avail}"
    print("test_registration_availability: PASS")


def test_novproxy_and_consistency():
    from grok2api.upstream.browser_register import novproxy
    from grok2api.upstream.browser_register import us_consistency

    # 1. 测试 NovProxy 节点解析
    sample_api_resp = "104.238.12.34:10001\n104.238.12.35:10002\n"
    nodes = novproxy.parse_proxy_lines(sample_api_resp)
    assert len(nodes) == 2, f"解析节点数异常: {nodes}"
    assert nodes[0] == "104.238.12.34:10001"

    # 2. 测试环境一致性描述与默认对齐
    assert us_consistency.enabled() is True
    desc = us_consistency.describe()
    assert "网络环境一致性" in desc

    # 3. 验证 ChatGPT 预检函数存在并可调用
    assert callable(adapter._preflight_chatgpt_path)
    print("test_novproxy_and_consistency: PASS")


if __name__ == "__main__":
    test_otp_extraction()
    test_profile_and_password()
    test_cpa_file_export()
    test_registration_availability()
    test_novproxy_and_consistency()
    print("ALL INTEGRATION TESTS PASSED SUCCESSFULLY!")
