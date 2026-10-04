"""账号降智检测：用推理 token 数判断账号是否被静默降级。

## 背景

xAI 会对部分账号做静默降级：接口照常返回 200，账号看起来完全正常，但模型
不再做逐步推理，直接给答案。这类账号在注册环节查不出任何异常（botFlag 也是
干净的），只有真正发一次需要推理的请求才能暴露。

## 判定依据

`/chat/completions` 的 `usage.completion_tokens_details` 里有官方字段
`reasoning_tokens`：

    健康账号   reasoning_tokens ≈ 3000 ~ 8100（本仓库实测 250 ~ 1000+）
    降智账号   reasoning_tokens ≈ 0

这个字段由服务端直接给出，比自己用 `total - prompt - completion` 去推算可靠
（直连时 completion_tokens 只算可见正文，差值里混着其他项）。

注意：**不能**用「有没有 thinking / reasoning_content 字段」来判断。
`cli-chat-proxy.grok.com` 直连时既不返回 `reasoning_content` 也不返回
`thinking`，但 reasoning_tokens 是有的；只有经 CPA 中转时
`reasoning_content` 才会透出。只看字段存在性会把所有账号误判成降智。

## 判据

    0            → hard    （降智）
    1 ~ 阈值以下  → soft    （可疑，推理量异常少）
    阈值以上      → healthy

默认阈值 50：正常推理动辄几千 token，低于 50 基本可以认定没有在真正思考，
同时留出余量避免把「简单问题少推理」误判。

## 与上游实现的差异

上游（grok-register/quality_probe.py）直接读 CPA 凭据文件并自己刷新 token；
本模块改为接收账号池的 GrokCredentials，复用 `grok2api.pool.auth.upstream_headers`
与 `model_health` 的 httpx 客户端（后者已处理账号级代理绑定），避免再实现一套
鉴权与出口逻辑。
"""
from __future__ import annotations

import json
import time
from typing import Any

from grok2api.config import UPSTREAM_BASE
from grok2api.pool.auth import GrokCredentials, upstream_headers

# 需要逐步推理的问题；答案唯一且可校验（3:27 时较小夹角 = 58.5 ≈ 58）。
DEFAULT_PROMPT = (
    "Think step by step. A clock shows 3:27. What is the smaller angle in "
    "degrees between the hour and minute hands? Reply with only the integer."
)
# 实测 grok-4.5 流式首包与完成仅需 6~15 秒（grok-4.7 需 35~50 秒以上易超时），
# 且通过 stream_options.include_usage 能稳定返回 reasoning_tokens。
DEFAULT_MODEL = "grok-4.5"
CHAT_PATH = "/chat/completions"

# 低于此推理量视为可疑。正常账号实测 250~1000+。
SOFT_REASONING_TOKENS = 50
# reasoning_tokens 为 0 直接判降智。
HARD_REASONING_TOKENS = 0
# 整个探测请求的墙钟预算。推理量本身波动很大（实测有账号 reasoning_tokens
# 接近 1 万、单次耗时 110 秒以上），所以预算必须给得比典型值宽松得多，
# 它的作用是兜住"流一直不停"的病态情况，而不是卡正常账号。
DEFAULT_DEADLINE_SEC = 300.0

VERDICT_HEALTHY = "healthy"
VERDICT_SOFT = "soft"
VERDICT_HARD = "hard"
# 账号本身有问题（token 失效 / 无权限 / 被限流），与降智区分开。
VERDICT_RISK = "risk"
VERDICT_ERROR = "error"

# 账号级问题（而非降智）。chat 端点无权限也归到这里：它同样说明账号不可用。
ACCOUNT_ERROR_MARKERS = (
    "permission-denied",
    "permission denied",
    "forbidden",
    "invalid token",
    "expired",
    "no auth",
    "quota",
    "rate limit",
    "ratelimit",
    "too many requests",
    "unauthorized",
)


def _int_field(payload: Any, *keys: str) -> int:
    if not isinstance(payload, dict):
        return 0
    for key in keys:
        try:
            value = int(payload.get(key) or 0)
        except (TypeError, ValueError):
            continue
        if value:
            return value
    return 0


def extract_reasoning_tokens(usage: Any) -> int:
    """从 usage 里取推理 token 数。

    优先用官方字段；取不到时回退到 total - prompt - completion 的差值
    （部分中转只给 total，差值同样是推理量）。
    """
    if not isinstance(usage, dict):
        return 0
    details = usage.get("completion_tokens_details")
    direct = _int_field(details, "reasoning_tokens", "reasoningTokens")
    if direct:
        return direct
    total = _int_field(usage, "total_tokens", "totalTokens")
    prompt = _int_field(usage, "prompt_tokens", "promptTokens")
    completion = _int_field(usage, "completion_tokens", "completionTokens")
    delta = total - prompt - completion
    return delta if delta > 0 else 0


def classify(reasoning_tokens: Any, soft_threshold: int = SOFT_REASONING_TOKENS) -> str:
    """按推理量给账号定级。"""
    try:
        value = int(reasoning_tokens)
    except (TypeError, ValueError):
        value = 0
    if value <= HARD_REASONING_TOKENS:
        return VERDICT_HARD
    if value < int(soft_threshold):
        return VERDICT_SOFT
    return VERDICT_HEALTHY


def classify_failure(status: int, body: str) -> str:
    """HTTP 失败归类：账号问题（risk） vs 传输/上游问题（error）。"""
    lower = str(body or "").lower()
    if status in (400, 401, 403, 404, 409, 422, 429):
        return VERDICT_RISK
    for marker in ACCOUNT_ERROR_MARKERS:
        if marker in lower:
            return VERDICT_RISK
    return VERDICT_ERROR


def build_payload(
    model: str = DEFAULT_MODEL,
    prompt: str = DEFAULT_PROMPT,
    max_tokens: int = 60,
    stream: bool = True,
) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "model": model,
        "stream": bool(stream),
        "max_tokens": int(max_tokens),
        "messages": [{"role": "user", "content": prompt}],
    }
    if stream:
        # 只有显式要求，服务端才会在末尾 chunk 里带上 usage。
        payload["stream_options"] = {"include_usage": True}
    return payload


def _consume_stream(response: Any, deadline: float | None = None) -> tuple[dict[str, Any], str]:
    """读 SSE 流，返回 (usage, content)。

    ``deadline`` 是墙钟上限（``time.monotonic()`` 基准）。httpx 的 timeout 只管
    单次读取，只要 chunk 持续到达就能一直拖下去，所以这里必须自己设总预算。
    """
    usage: dict[str, Any] = {}
    chunks: list[str] = []
    for line in response.iter_lines():
        if deadline is not None and time.monotonic() > deadline:
            raise TimeoutError(
                f"降智探测超时：已超出墙钟预算 {time.monotonic() - deadline:.0f}s 仍未结束"
            )
        raw = str(line or "").strip()
        if not raw.startswith("data:"):
            continue
        data_str = raw[5:].strip()
        if data_str == "[DONE]":
            break
        try:
            chunk = json.loads(data_str)
        except Exception:
            continue
        if not isinstance(chunk, dict):
            continue
        if isinstance(chunk.get("usage"), dict):
            usage = chunk["usage"]
        for choice in chunk.get("choices") or []:
            if not isinstance(choice, dict):
                continue
            delta = choice.get("delta") or choice.get("message") or {}
            if isinstance(delta, dict) and delta.get("content"):
                chunks.append(str(delta["content"]))
    return usage, "".join(chunks).strip()


def _consume_json(response: Any) -> tuple[dict[str, Any], str]:
    data = response.json()
    if not isinstance(data, dict):
        return {}, ""
    usage = data.get("usage") if isinstance(data.get("usage"), dict) else {}
    chunks: list[str] = []
    for choice in data.get("choices") or []:
        if not isinstance(choice, dict):
            continue
        msg = choice.get("message") or choice.get("delta") or {}
        if isinstance(msg, dict) and msg.get("content"):
            chunks.append(str(msg["content"]))
    return usage or {}, "".join(chunks).strip()


def probe_credentials(
    creds: GrokCredentials,
    *,
    model: str = DEFAULT_MODEL,
    prompt: str = DEFAULT_PROMPT,
    soft_threshold: int = SOFT_REASONING_TOKENS,
    timeout: float = 50.0,
    deadline_sec: float = DEFAULT_DEADLINE_SEC,
    max_tokens: int = 60,
    stream: bool = True,
    client: Any = None,
) -> dict[str, Any]:
    """探测单个账号的降智情况。永不抛异常。

    默认流式 + stream_options.include_usage。``timeout`` 是单次读写的超时，
    ``deadline_sec`` 是整个请求的墙钟预算 —— 两者都需要：只有前者时，服务端
    只要持续吐 chunk 就能把探测拖到任意长。
    """
    result: dict[str, Any] = {
        "account_id": getattr(creds, "auth_key", None),
        "email": getattr(creds, "email", None),
        "verdict": VERDICT_ERROR,
        "reasoning_tokens": 0,
        "completion_tokens": 0,
        "total_tokens": 0,
        "content": "",
        "status": 0,
        "error": "",
        "model": model,
        "duration_sec": 0.0,
    }
    token = str(getattr(creds, "token", "") or "").strip()
    if not token:
        result["verdict"] = VERDICT_RISK
        result["error"] = "凭据缺少 access_token"
        return result

    owns_client = False
    if client is None:
        try:
            from grok2api.pool.model_health import _probe_http_client

            client = _probe_http_client(getattr(creds, "auth_key", None))
        except Exception:
            import httpx

            client = httpx.Client(timeout=timeout, follow_redirects=True)
            owns_client = True

    url = f"{UPSTREAM_BASE}{CHAT_PATH}"
    headers = upstream_headers(token, model)
    headers["Accept"] = "text/event-stream" if stream else "application/json"
    payload = build_payload(
        model=model, prompt=prompt, max_tokens=max_tokens, stream=stream
    )

    started = time.time()
    deadline = time.monotonic() + max(1.0, float(deadline_sec or 0))
    try:
        if stream:
            with client.stream(
                "POST", url, headers=headers, json=payload, timeout=timeout
            ) as response:
                result["status"] = int(response.status_code)
                if response.status_code >= 400:
                    body = ""
                    try:
                        body = response.read().decode("utf-8", "replace")[:200]
                    except Exception:
                        pass
                    result["verdict"] = classify_failure(result["status"], body)
                    result["error"] = f"HTTP {result['status']}: {body or '无响应体'}"
                    result["duration_sec"] = round(time.time() - started, 2)
                    return result
                usage, content = _consume_stream(response, deadline=deadline)
        else:
            response = client.post(
                url, headers=headers, json=payload, timeout=timeout
            )
            result["status"] = int(response.status_code)
            if response.status_code >= 400:
                body = str(getattr(response, "text", "") or "")[:200]
                result["verdict"] = classify_failure(result["status"], body)
                result["error"] = f"HTTP {result['status']}: {body or '无响应体'}"
                result["duration_sec"] = round(time.time() - started, 2)
                return result
            usage, content = _consume_json(response)
    except Exception as exc:
        result["error"] = f"{type(exc).__name__}: {str(exc)[:200]}"
        result["duration_sec"] = round(time.time() - started, 2)
        return result
    finally:
        if owns_client:
            try:
                client.close()
            except Exception:
                pass

    reasoning = extract_reasoning_tokens(usage)
    result["reasoning_tokens"] = reasoning
    result["completion_tokens"] = _int_field(usage, "completion_tokens", "completionTokens")
    result["total_tokens"] = _int_field(usage, "total_tokens", "totalTokens")
    result["content"] = content[:200]
    result["verdict"] = classify(reasoning, soft_threshold=soft_threshold)
    result["duration_sec"] = round(time.time() - started, 2)
    return result


def summarize(results: list[dict[str, Any]]) -> dict[str, int]:
    """按判定汇总数量。"""
    summary = {
        VERDICT_HEALTHY: 0,
        VERDICT_SOFT: 0,
        VERDICT_HARD: 0,
        VERDICT_RISK: 0,
        VERDICT_ERROR: 0,
    }
    for item in results or []:
        verdict = str((item or {}).get("verdict") or VERDICT_ERROR)
        if verdict not in summary:
            verdict = VERDICT_ERROR
        summary[verdict] += 1
    return summary


def format_line(item: dict[str, Any]) -> str:
    """单行可读摘要，用于日志。"""
    email = str((item or {}).get("email") or "?")
    verdict = str((item or {}).get("verdict") or "?")
    mark = {
        VERDICT_HEALTHY: "✅",
        VERDICT_SOFT: "⚠️",
        VERDICT_HARD: "🧠",
        VERDICT_RISK: "🚫",
    }.get(verdict, "❓")
    detail = f"推理 {item.get('reasoning_tokens', 0)} tok"
    if item.get("error"):
        detail = str(item["error"])[:70]
    return f"{mark} {email[:26]:<26} {verdict:<8} {detail}"
