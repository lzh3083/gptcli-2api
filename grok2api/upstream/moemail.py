"""Mail helpers (MoeMail / YYDS / GPTMail) + proxy normalization for protocol registration.

Kept intentionally small: only the pieces used by ``grok_build_adapter``
(and optional admin proxy smoke tests). The legacy full-session
``email_registration`` flow was removed in favor of grok-build-auth.

Providers:
  - moemail  — beilunyang/moemail style API (``/api/emails/...``)
  - yyds     — vip.215.im / maliapi.215.im YYDS Mail (``/v1/accounts`` …)
  - gptmail  — mail.chatgpt.org.uk GPTMail (``/api/generate-email`` …)
  - cfmail   — dreamhunter2333/cloudflare_temp_email (``/api/new_address`` …)
"""
from __future__ import annotations

import email
import os
import random
import re
import time
from email import policy
from typing import Any
from urllib.parse import quote, unquote, urlparse, urlunparse

import httpx

from grok2api.config import (
    MOEMAIL_API_KEY,
    MOEMAIL_BASE_URL,
    MOEMAIL_DOMAIN,
    MOEMAIL_EXPIRY_MS,
    XAI_PROXY,
    XAI_PROXY_PASSWORD,
    XAI_PROXY_USERNAME,
)

# Official YYDS Mail API host (docs: https://vip.215.im/docs).
YYDS_DEFAULT_BASE_URL = "https://maliapi.215.im"
YYDS_DEFAULT_DOMAIN = ""  # must be chosen from GET /v1/domains or admin config

# Official GPTMail host (docs: https://mail.chatgpt.org.uk/zh/api/).
GPTMAIL_DEFAULT_BASE_URL = "https://mail.chatgpt.org.uk"
# Docs: public test key is an sk-… key shown on https://mail.chatgpt.org.uk/zh/api/
# (legacy placeholder gpt-test is no longer accepted by the API).
GPTMAIL_PUBLIC_TEST_KEY = ""

# Cloudflare Temp Email (https://github.com/dreamhunter2333/cloudflare_temp_email)
# Self-hosted Workers URL; demo host only for docs/default placeholder.
CFMAIL_DEFAULT_BASE_URL = "https://temp-email-api.awsl.uk"

# TempMail.lol (https://tempmail.lol/zh/api) — free tier needs no API key.
TEMPMAIL_LOL_DEFAULT_BASE_URL = "https://api.tempmail.lol"

# Cloud Mail (https://github.com/maillab/cloud-mail) — Cloudflare Workers +
# Vue3 self-hosted mailbox service. Open API: /api/public/genToken | /api/public/emailList
# | /api/public/addUser. Admin email + password exchanged for an Authorization token.
CLOUDMAIL_DEFAULT_BASE_URL = ""  # no official demo host (self-host only)


def _headers(api_key: str | None = None) -> dict[str, str]:
    key = api_key or MOEMAIL_API_KEY
    if not key:
        return {}
    return {"X-API-Key": key}


def normalize_mail_provider(provider: str | None, *, base_url: str | None = None) -> str:
    """Return ``moemail`` | ``yyds`` | ``gptmail`` | ``cfmail`` | ``tempmail`` | ``cloudmail``.

    Infer from base_url when provider is empty.
    """
    p = (provider or "").strip().lower()
    if p in {"yyds", "yydsmail", "yyds_mail", "vip215", "215", "maliapi"}:
        return "yyds"
    if p in {
        "gptmail",
        "gpt-mail",
        "gpt_mail",
        "chatgptmail",
        "chatgpt-mail",
        "mail.chatgpt",
        "chatgpt.org.uk",
    }:
        return "gptmail"
    if p in {
        "cfmail",
        "cf-mail",
        "cf_mail",
        "cloudflare",
        "cloudflare_temp_email",
        "cloudflare-temp-email",
        "temp-email",
        "tempmail_cf",
        "awsl",
    }:
        return "cfmail"
    if p in {
        "tempmail",
        "tempmail.lol",
        "tempmaillol",
        "tempmail_lol",
        "lol",
        "tmlol",
    }:
        return "tempmail"
    if p in {
        "cloudmail",
        "cloud-mail",
        "cloud_mail",
        "cloud mail",
        "skymail",
        "maillab",
    }:
        return "cloudmail"
    if p in {"tempmail", "tempmail.lol", "tempmaillol", "tempmail_lol", "lol", "tmlol"}:
        return "tempmail"
    if p in {"moemail", "moe", "moe-mail"}:
        return "moemail"
    base = (base_url or "").strip().lower()
    if any(x in base for x in ("maliapi.215.im", "vip.215.im", "215.im/v1", "yyds")):
        return "yyds"
    if any(
        x in base
        for x in (
            "mail.chatgpt.org.uk",
            "chatgpt.org.uk",
            "gptmail",
        )
    ):
        return "gptmail"
    if any(
        x in base
        for x in (
            "temp-email-api",
            "temp-email",
            "cloudflare_temp_email",
            "awsl.uk",
            "/api/new_address",
            "/open_api/settings",
        )
    ):
        return "cfmail"
    if any(x in base for x in ("tempmail.lol", "api.tempmail.lol")):
        return "tempmail"
    # Cloud Mail self-host detection: presence of /public/genToken is unique.
    if any(
        x in base
        for x in (
            "skymail",
            "cloud-mail",
            "/public/genToken",
            "/public/emailList",
            "/public/addUser",
        )
    ):
        return "cloudmail"
    return "moemail"


def normalize_yyds_base_url(base_url: str | None = None) -> str:
    """Normalize user input (docs URL / trailing /v1) to maliapi origin.

    Always prefer the official YYDS API host. Never fall through to a MoeMail
    default (``moemail.example.com``) when callers pass ``base_url or MOEMAIL_BASE_URL``.
    Docs: https://vip.215.im/docs  API: https://maliapi.215.im/v1
    """
    raw = (base_url or "").strip()
    if not raw:
        return YYDS_DEFAULT_BASE_URL
    lower = raw.lower()
    # Docs portal, bare 215.im, or accidental MoeMail defaults → official API.
    if any(
        x in lower
        for x in (
            "vip.215.im",
            "maliapi.215.im",
            "215.im",
            "moemail.example.com",
            "moemail.521884.xyz",
            "example.com",
        )
    ):
        # Only keep custom origin if it's already maliapi.
        if "maliapi.215.im" in lower:
            return YYDS_DEFAULT_BASE_URL
        if "vip.215.im" in lower or "215.im" in lower:
            return YYDS_DEFAULT_BASE_URL
        if "moemail" in lower or "example.com" in lower:
            return YYDS_DEFAULT_BASE_URL
    parsed = urlparse(raw if "://" in raw else f"https://{raw}")
    origin = f"{parsed.scheme or 'https'}://{parsed.netloc}".rstrip("/")
    if not parsed.netloc:
        return YYDS_DEFAULT_BASE_URL
    # Unknown custom host (self-proxy) — allow; otherwise pin official.
    if "215.im" in (parsed.netloc or "").lower():
        return YYDS_DEFAULT_BASE_URL
    return origin or YYDS_DEFAULT_BASE_URL


def normalize_gptmail_base_url(base_url: str | None = None) -> str:
    """Normalize docs / language path pastes to GPTMail origin.

    Docs: https://mail.chatgpt.org.uk/zh/api/
    Always pin to https://mail.chatgpt.org.uk — never fall through to MoeMail defaults.
    """
    raw = (base_url or "").strip()
    if not raw:
        return GPTMAIL_DEFAULT_BASE_URL
    lower = raw.lower()
    if any(
        x in lower
        for x in (
            "chatgpt.org.uk",
            "gptmail",
            "moemail.example.com",
            "moemail.521884.xyz",
            "maliapi.215.im",
            "vip.215.im",
            "example.com",
        )
    ):
        return GPTMAIL_DEFAULT_BASE_URL
    parsed = urlparse(raw if "://" in raw else f"https://{raw}")
    origin = f"{parsed.scheme or 'https'}://{parsed.netloc}".rstrip("/")
    if not parsed.netloc:
        return GPTMAIL_DEFAULT_BASE_URL
    return origin or GPTMAIL_DEFAULT_BASE_URL



def parse_domain_list(text: str | None) -> list[str]:
    """Split multi-domain config into unique hostnames.

    Accepts newlines / commas / semicolons / spaces. Strips ``@`` and leading dots.
    """
    if text is None:
        return []
    raw = str(text).replace("\r\n", "\n").replace("\r", "\n")
    parts: list[str] = []
    for chunk in raw.replace(";", "\n").replace(",", "\n").split("\n"):
        for token in chunk.split():
            d = token.strip().lstrip("@").strip().strip(".").lower()
            if not d or d.startswith("#"):
                continue
            # basic host sanity: no spaces, has a dot or is short label
            if "://" in d:
                # allow pasting https://x.com → x.com
                try:
                    from urllib.parse import urlparse

                    host = urlparse(d if "://" in d else "https://" + d).hostname or ""
                    d = host.strip(".").lower()
                except Exception:
                    continue
            if not d or "/" in d or " " in d:
                continue
            if d not in parts:
                parts.append(d)
    return parts


def pick_domain_from_list(
    text: str | None,
    *,
    index: int | None = None,
    strategy: str = "round_robin",
) -> str:
    """Pick one domain from a multi-domain config string.

    - empty list → ""
    - single → that domain
    - multi + index → round-robin by index (batch registration)
    - multi + no index → random
    """
    domains = parse_domain_list(text)
    if not domains:
        return ""
    if len(domains) == 1:
        return domains[0]
    strat = (strategy or "round_robin").strip().lower()
    if index is not None:
        try:
            i = int(index)
        except (TypeError, ValueError):
            i = 0
        if i < 0:
            i = 0
        return domains[i % len(domains)]
    if strat in {"random", "rand"}:
        return random.choice(domains)
    # default round_robin without index: random is fine for single-shot
    return random.choice(domains)



def normalize_cfmail_base_url(base_url: str | None = None) -> str:
    """Normalize Cloudflare Temp Email Workers URL to API origin.

    Accepts worker host or accidental ``/api`` / ``/admin`` / docs suffixes.
    Never falls through to MoeMail defaults. Demo host is only a last-resort
    fallback when empty. Deploy your own worker for production.
    Repo: https://github.com/dreamhunter2333/cloudflare_temp_email
    """
    raw = (base_url or "").strip()
    if not raw:
        return CFMAIL_DEFAULT_BASE_URL
    lower = raw.lower()
    # Reject accidental MoeMail / YYDS pastes.
    if any(
        x in lower
        for x in (
            "moemail.example.com",
            "moemail.521884.xyz",
            "maliapi.215.im",
            "vip.215.im",
            "chatgpt.org.uk",
        )
    ):
        return CFMAIL_DEFAULT_BASE_URL
    parsed = urlparse(raw if "://" in raw else f"https://{raw}")
    origin = f"{parsed.scheme or 'https'}://{parsed.netloc}".rstrip("/")
    if not parsed.netloc:
        return CFMAIL_DEFAULT_BASE_URL
    return origin or CFMAIL_DEFAULT_BASE_URL


def _cfmail_headers(
    *,
    api_key: str | None = None,
    site_password: str | None = None,
    content_type: bool = False,
    as_admin: bool | None = None,
) -> dict[str, str]:
    """Build CF Temp Email headers (dreamhunter2333/cloudflare_temp_email).

    - Address JWT: ``Authorization: Bearer <jwt>`` (inbox JWT from create)
    - Admin password: ``x-admin-auth`` (ADMIN_PASSWORDS) for ``/admin/*``
    - Site password: ``x-custom-auth`` (PASSWORDS) for private-site ``/open_api/*``
    """
    headers: dict[str, str] = {}
    key = (api_key or "").strip()
    site = (site_password or "").strip()
    if key:
        parts = key.split(".")
        is_jwt = len(parts) == 3 and all(parts) and not key.startswith("http")
        if is_jwt and as_admin is not True:
            headers["Authorization"] = f"Bearer {key}"
        else:
            # Admin password for /admin/new_address etc.
            headers["x-admin-auth"] = key
            # Many private deploys use the same string for site PASSWORDS.
            if not site:
                headers["x-custom-auth"] = key
    if site:
        headers["x-custom-auth"] = site
    if content_type:
        headers["Content-Type"] = "application/json"
    return headers


def normalize_proxy_config(
    proxy: str | None = None,
    *,
    username: str | None = None,
    password: str | None = None,
) -> dict[str, Any] | None:
    """Normalize a proxy URL into curl/httpx-friendly forms."""
    raw = (proxy or XAI_PROXY or "").strip()
    if not raw:
        return None
    # "direct"/"none"/"off"/"0" are sentinels meaning "no proxy", not host names.
    # Without this guard the `f"http://{raw}"` shorthand below produced the
    # literal URL http://direct, which leaked into the outbound pool and made
    # every consumer fail with "Could not resolve proxy: direct" or
    # "Temporary failure in name resolution".
    try:
        from grok2api.upstream.proxy_pool import is_direct_proxy
    except Exception:  # pragma: no cover - proxy_pool is a sibling module
        from proxy_pool import is_direct_proxy  # type: ignore
    if is_direct_proxy(raw):
        return None
    env_user = XAI_PROXY_USERNAME
    env_pass = XAI_PROXY_PASSWORD
    lower = raw.lower()
    if lower.startswith("soket5://"):
        raw = "socks5://" + raw.split("://", 1)[1]
    elif lower.startswith("socket5://"):
        raw = "socks5://" + raw.split("://", 1)[1]
    elif "://" not in raw:
        raw = f"http://{raw}"

    parsed = urlparse(raw)
    if parsed.scheme not in {"http", "https", "socks5", "socks5h"}:
        raise ValueError("proxy scheme must be http, https, socks5, or socks5h")
    if not parsed.netloc or not parsed.hostname:
        raise ValueError("proxy must include host and port")
    try:
        port = parsed.port
    except ValueError as e:
        raise ValueError("proxy port is invalid") from e
    proxy_user = (username if username is not None else "").strip()
    proxy_pass = (password if password is not None else "").strip()
    if not proxy_user and username is None:
        proxy_user = env_user
    if not proxy_pass and password is None:
        proxy_pass = env_pass
    if not proxy_user and parsed.username:
        proxy_user = unquote(parsed.username)
    if not proxy_pass and parsed.password:
        proxy_pass = unquote(parsed.password)

    if proxy_pass and not proxy_user:
        raise ValueError("proxy username is required when proxy password is set")

    host = parsed.hostname or ""
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    if port is not None:
        host = f"{host}:{port}"
    proxy_no_auth = urlunparse(
        (
            parsed.scheme,
            host,
            parsed.path or "",
            parsed.params or "",
            parsed.query or "",
            parsed.fragment or "",
        )
    )
    proxy_auth = (proxy_user, proxy_pass) if proxy_user else None
    proxy_with_auth = proxy_no_auth
    if proxy_user:
        auth = quote(proxy_user, safe="")
        if proxy_pass:
            auth = f"{auth}:{quote(proxy_pass, safe='')}"
        proxy_with_auth = urlunparse(
            (
                parsed.scheme,
                f"{auth}@{host}",
                parsed.path or "",
                parsed.params or "",
                parsed.query or "",
                parsed.fragment or "",
            )
        )
    return {
        "proxy": proxy_with_auth,
        "curl_proxy": proxy_no_auth,
        "proxy_auth": proxy_auth,
    }


# Back-compat alias used by older adapter code paths.
_normalize_proxy_config = normalize_proxy_config


def _extract_codes_and_links(text: str) -> dict[str, list[str]]:
    codes = sorted(set(re.findall(r"(?<!\d)\d{6,8}(?!\d)", text or "")))
    links = sorted(set(re.findall(r"https?://[^\s\"'<>)]+", text or "")))
    return {"codes": codes, "links": links}


def _moemail_infer_domain(
    client: httpx.Client,
    base: str,
    *,
    api_key: str | None = None,
) -> str | None:
    try:
        resp = client.get(f"{base}/api/emails", headers=_headers(api_key))
        if resp.status_code >= 400:
            return None
        data = resp.json()
    except Exception:
        return None
    emails = data.get("emails") if isinstance(data, dict) else None
    if not isinstance(emails, list):
        return None
    for item in emails:
        if not isinstance(item, dict):
            continue
        address = item.get("email") or item.get("address")
        if isinstance(address, str) and "@" in address:
            return address.rsplit("@", 1)[1].strip() or None
    return None


def moemail_create_mailbox(
    *,
    name: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,
    api_key: str | None = None,
    base_url: str | None = None,
    proxy: str | None = None,  # accepted for API compat; unused by httpx path
    proxy_username: str | None = None,
    proxy_password: str | None = None,
) -> dict[str, Any]:
    if not (api_key or MOEMAIL_API_KEY):
        raise ValueError(
            "MoeMail API key missing. Set GROK2API_MOEMAIL_API_KEY or pass api_key."
        )

    base = (base_url or MOEMAIL_BASE_URL).rstrip("/")
    # MoeMail only accepts official presets: 3600000 / 86400000 / 259200000 / 0.
    # Do not use `expiry_ms or default` — permanent is 0 and must be preserved.
    _OFFICIAL = {3_600_000, 86_400_000, 259_200_000, 0}
    if expiry_ms is None:
        chosen = int(MOEMAIL_EXPIRY_MS)
    else:
        chosen = int(expiry_ms)
    if chosen not in _OFFICIAL:
        # snap to nearest timed preset (never invent permanent from bad input)
        timed = (3_600_000, 86_400_000, 259_200_000)
        chosen = min(timed, key=lambda p: abs(p - chosen))
    payload: dict[str, Any] = {
        "expiryTime": chosen,
        "domain": (pick_domain_from_list(domain) if domain else "") or domain or MOEMAIL_DOMAIN,
    }
    if name:
        payload["name"] = name

    # Bulk registration fans out mailbox creates; MoeMail occasionally returns
    # 502/503/429 under load. Retry transient failures instead of failing the job.
    try:
        max_attempts = max(
            1,
            min(8, int(os.environ.get("GROK2API_MOEMAIL_CREATE_RETRIES", "4") or 4)),
        )
    except (TypeError, ValueError):
        max_attempts = 4
    last_err = ""
    data: dict[str, Any] | None = None
    with httpx.Client(timeout=30.0) as client:
        headers = {**_headers(api_key), "Content-Type": "application/json"}
        for attempt in range(1, max_attempts + 1):
            try:
                resp = client.post(
                    f"{base}/api/emails/generate",
                    json=payload,
                    headers=headers,
                )
                if resp.status_code == 400 and "域名" in resp.text and not domain:
                    inferred = _moemail_infer_domain(client, base, api_key=api_key)
                    if inferred and inferred != payload.get("domain"):
                        payload["domain"] = inferred
                        resp = client.post(
                            f"{base}/api/emails/generate",
                            json=payload,
                            headers=headers,
                        )
                if resp.status_code >= 400:
                    last_err = (
                        f"MoeMail create failed {resp.status_code}: {resp.text[:500]}"
                    )
                    transient = resp.status_code in {408, 425, 429, 500, 502, 503, 504}
                    if transient and attempt < max_attempts:
                        time.sleep(min(12.0, 0.8 * attempt + random.uniform(0.1, 0.6)))
                        continue
                    raise RuntimeError(last_err)
                data = resp.json()
                break
            except RuntimeError:
                raise
            except Exception as e:  # noqa: BLE001
                last_err = f"MoeMail create network error: {e}"
                if attempt < max_attempts:
                    time.sleep(min(12.0, 0.8 * attempt + random.uniform(0.1, 0.6)))
                    continue
                raise RuntimeError(last_err) from e
    if not isinstance(data, dict):
        raise RuntimeError(last_err or "MoeMail create failed")

    email_id = data.get("id") or data.get("emailId")
    address = data.get("email") or data.get("address")
    if not email_id or not address:
        raise RuntimeError(f"Unexpected MoeMail create response: {data}")
    return {"id": str(email_id), "email": str(address), "raw": data}


def moemail_fetch_messages(
    email_id: str,
    *,
    api_key: str | None = None,
    base_url: str | None = None,
    include_details: bool = True,
) -> list[dict[str, Any]]:
    if not email_id:
        return []
    if not (api_key or MOEMAIL_API_KEY):
        return []

    base = (base_url or MOEMAIL_BASE_URL).rstrip("/")
    with httpx.Client(timeout=30.0) as client:
        resp = client.get(f"{base}/api/emails/{email_id}", headers=_headers(api_key))
        if resp.status_code >= 400:
            raise RuntimeError(
                f"MoeMail list failed {resp.status_code}: {resp.text[:500]}"
            )
        data = resp.json()
        messages = data.get("messages") if isinstance(data, dict) else None
        if not isinstance(messages, list):
            return []

        out: list[dict[str, Any]] = []
        for raw in messages[:20]:
            if not isinstance(raw, dict):
                continue
            item = dict(raw)
            msg_id = item.get("id") or item.get("messageId")
            if include_details and msg_id:
                detail = client.get(
                    f"{base}/api/emails/{email_id}/{msg_id}",
                    headers=_headers(api_key),
                )
                if detail.status_code == 200:
                    d = detail.json()
                    msg = d.get("message") if isinstance(d, dict) else None
                    if isinstance(msg, dict):
                        item.update(msg)
            text = "\n".join(
                str(item.get(k) or "")
                for k in ("subject", "content", "html", "from_address", "from")
            )
            item["extracted"] = _extract_codes_and_links(text)
            out.append(item)
        return out


# Private aliases matching historical names used by grok_build_adapter.
_moemail_create_mailbox = moemail_create_mailbox
_moemail_fetch_messages = moemail_fetch_messages


def yyds_create_mailbox(
    *,
    name: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,  # accepted for API compat; YYDS temp mail is ~24h
    api_key: str | None = None,
    base_url: str | None = None,
    proxy: str | None = None,
    proxy_username: str | None = None,
    proxy_password: str | None = None,
) -> dict[str, Any]:
    """Create a temporary inbox on YYDS Mail (https://vip.215.im/docs)."""
    key = (api_key or MOEMAIL_API_KEY or "").strip()
    if not key:
        raise ValueError(
            "YYDS Mail API key missing. Save AC-… key in 协议注册 → YYDS panel "
            "(X-API-Key). Docs: https://vip.215.im/docs"
        )
    base = normalize_yyds_base_url(base_url or YYDS_DEFAULT_BASE_URL)
    # Never fall back to MOEMAIL_DOMAIN (MoeMail default / example.com). Empty
    # means auto: randomly pick a healthy public domain from GET /v1/domains.
    # Multi-domain config (newlines/commas) → pick one (random when no index).
    dom = pick_domain_from_list(domain) if domain else ""
    if not dom:
        dom = (domain or "").strip().lstrip("@").strip(".")
    if not dom:
        dom = yyds_pick_domain(api_key=key, base_url=base) or ""
    if not dom:
        raise ValueError(
            "YYDS Mail domain auto-fetch failed. Leave domain empty for random "
            "public domain, or set an explicit domain from GET /v1/domains."
        )
    local = (name or "").strip().lower() or None
    payload: dict[str, Any] = {"domain": dom}
    if local:
        payload["localPart"] = local

    with httpx.Client(timeout=30.0) as client:
        headers = {**_headers(key), "Content-Type": "application/json"}
        resp = client.post(f"{base}/v1/accounts", json=payload, headers=headers)
        if resp.status_code >= 400:
            raise RuntimeError(
                f"YYDS create failed {resp.status_code}: {resp.text[:500]}"
            )
        data = resp.json()

    # Envelope: { success, data: { id, address, token, ... } }
    body = data.get("data") if isinstance(data, dict) and "data" in data else data
    if not isinstance(body, dict):
        raise RuntimeError(f"Unexpected YYDS create response: {data}")
    email_id = body.get("id") or body.get("inboxId") or body.get("accountId")
    address = body.get("address") or body.get("email")
    token = body.get("token") or body.get("tempToken") or ""
    if not email_id or not address:
        raise RuntimeError(f"Unexpected YYDS create response: {data}")
    return {
        "id": str(email_id),
        "email": str(address),
        "token": str(token or ""),
        "provider": "yyds",
        "raw": data,
        # Keep expiry_ms for logging only (service is ~24h temp).
        "expiry_ms": 86_400_000 if expiry_ms is None else int(expiry_ms),
    }


def yyds_list_domains(
    *,
    api_key: str | None = None,
    base_url: str | None = None,
    public_only: bool = True,
    ready_only: bool = True,
) -> list[str]:
    """List usable domains from YYDS catalog (``GET /v1/domains``)."""
    key = (api_key or MOEMAIL_API_KEY or "").strip()
    base = normalize_yyds_base_url(base_url or YYDS_DEFAULT_BASE_URL)
    try:
        with httpx.Client(timeout=20.0) as client:
            resp = client.get(f"{base}/v1/domains", headers=_headers(key) if key else {})
            if resp.status_code >= 400:
                return []
            data = resp.json()
    except Exception:
        return []
    items = data
    if isinstance(data, dict):
        items = data.get("data") or data.get("domains") or data.get("items") or []
    if not isinstance(items, list):
        return []
    preferred: list[str] = []
    fallback: list[str] = []
    seen: set[str] = set()
    for item in items:
        if not isinstance(item, dict):
            continue
        name = item.get("domain") or item.get("name") or item.get("host")
        if not isinstance(name, str) or not name.strip():
            continue
        name = name.strip().lstrip("@").strip(".")
        if not name or name in seen:
            continue
        if public_only and item.get("isPublic") is False:
            continue
        # Docs catalog uses isMxValid / isVerified; receivingReady is optional.
        if ready_only:
            if item.get("isMxValid") is False:
                continue
            if item.get("isVerified") is False:
                continue
            if item.get("receivingReady") is False:
                continue
        seen.add(name)
        if item.get("wildcardMxValid") is True or item.get("wildcard_mx_valid") is True:
            preferred.append(name)
        else:
            fallback.append(name)
    # Prefer wildcard-MX domains first so random pick weights healthier ones.
    return preferred + fallback


def yyds_pick_domain(
    *,
    api_key: str | None = None,
    base_url: str | None = None,
) -> str | None:
    """Randomly pick a healthy public domain from YYDS catalog.

    Catalog order is preferred (wildcard MX) then fallback. Randomize across
    the full usable set so batch registration rotates domains.
    Empty admin domain => call this.
    """
    domains = yyds_list_domains(api_key=api_key, base_url=base_url)
    if not domains:
        return None
    return random.choice(domains)


def yyds_fetch_messages(
    email_id: str,
    *,
    api_key: str | None = None,
    base_url: str | None = None,
    include_details: bool = True,
    address: str | None = None,
    token: str | None = None,
) -> list[dict[str, Any]]:
    """List (+ optionally detail) messages for a YYDS inbox."""
    if not email_id and not address:
        return []
    key = (api_key or MOEMAIL_API_KEY or "").strip()
    base = normalize_yyds_base_url(base_url or YYDS_DEFAULT_BASE_URL)
    headers = _headers(key) if key else {}
    if token and not key:
        headers = {"Authorization": f"Bearer {token}"}
    # When both present, X-API-Key is enough per docs; token reserved for temp-only flows.

    with httpx.Client(timeout=30.0) as client:
        # Prefer canonical inbox path when id is known; fall back to address query.
        messages: list[Any] = []
        if email_id:
            resp = client.get(
                f"{base}/v1/inboxes/{email_id}/messages",
                headers=headers,
                params={"limit": 20},
            )
            if resp.status_code >= 400 and address:
                resp = client.get(
                    f"{base}/v1/messages",
                    headers=headers,
                    params={"address": address, "limit": 20},
                )
            elif resp.status_code >= 400:
                raise RuntimeError(
                    f"YYDS list failed {resp.status_code}: {resp.text[:500]}"
                )
        else:
            resp = client.get(
                f"{base}/v1/messages",
                headers=headers,
                params={"address": address, "limit": 20},
            )
            if resp.status_code >= 400:
                raise RuntimeError(
                    f"YYDS list failed {resp.status_code}: {resp.text[:500]}"
                )

        data = resp.json() if resp.content else {}
        body = data.get("data") if isinstance(data, dict) and "data" in data else data
        if isinstance(body, dict):
            messages = body.get("messages") or body.get("items") or []
        elif isinstance(body, list):
            messages = body
        if not isinstance(messages, list):
            return []

        out: list[dict[str, Any]] = []
        for raw in messages[:20]:
            if not isinstance(raw, dict):
                continue
            item = dict(raw)
            msg_id = item.get("id") or item.get("messageId")
            if include_details and msg_id:
                params = {"address": address} if address else None
                detail = client.get(
                    f"{base}/v1/messages/{msg_id}",
                    headers=headers,
                    params=params,
                )
                if detail.status_code == 200:
                    d = detail.json()
                    msg = d.get("data") if isinstance(d, dict) and "data" in d else d
                    if isinstance(msg, dict):
                        # Some envelopes nest { message: {...} }
                        if isinstance(msg.get("message"), dict):
                            item.update(msg["message"])
                        else:
                            item.update(msg)
            # Flatten from.address for code extractors used by the adapter.
            from_obj = item.get("from")
            if isinstance(from_obj, dict):
                item.setdefault("from_address", from_obj.get("address") or "")
                item.setdefault("from", from_obj.get("address") or from_obj.get("name") or "")
            text = "\n".join(
                str(item.get(k) or "")
                for k in (
                    "subject",
                    "content",
                    "text",
                    "textBody",
                    "html",
                    "htmlBody",
                    "body",
                    "from_address",
                    "from",
                    "verificationCode",
                )
            )
            item["extracted"] = _extract_codes_and_links(text)
            # Surface server-side OTP when present.
            vc = item.get("verificationCode")
            if vc and isinstance(item.get("extracted"), dict):
                codes = list(item["extracted"].get("codes") or [])
                s = str(vc).strip()
                if s and s not in codes:
                    codes.insert(0, s)
                    item["extracted"]["codes"] = codes
            out.append(item)
        return out



def yyds_wait_next_message(
    *,
    address: str,
    api_key: str | None = None,
    base_url: str | None = None,
    token: str | None = None,
    wait: int = 15,
    email_id: str | None = None,
) -> dict[str, Any] | None:
    """Poll ``GET /v1/messages/next`` (OTP-friendly, marks seen, extracts verificationCode).

    Docs: https://vip.215.im/docs — wait 0–30s long-poll; 204 when empty.
    """
    addr = (address or "").strip()
    if not addr:
        return None
    key = (api_key or MOEMAIL_API_KEY or "").strip()
    base = normalize_yyds_base_url(base_url or YYDS_DEFAULT_BASE_URL)
    headers: dict[str, str] = {}
    if key:
        headers = _headers(key)
    elif token:
        headers = {"Authorization": f"Bearer {token}"}
    else:
        return None
    wait_s = max(0, min(int(wait or 0), 30))
    params: dict[str, Any] = {"address": addr, "wait": wait_s}
    try:
        with httpx.Client(timeout=float(wait_s) + 20.0) as client:
            resp = client.get(f"{base}/v1/messages/next", headers=headers, params=params)
            if resp.status_code == 204 or not resp.content:
                return None
            if resp.status_code >= 400:
                # Fallback: list + detail once.
                return None
            data = resp.json()
    except Exception:
        return None
    body = data.get("data") if isinstance(data, dict) and "data" in data else data
    if not isinstance(body, dict):
        return None
    msg = body.get("message") if isinstance(body.get("message"), dict) else body
    if not isinstance(msg, dict):
        return None
    # Flatten for extractors.
    from_obj = msg.get("from")
    if isinstance(from_obj, dict):
        msg.setdefault("from_address", from_obj.get("address") or "")
        msg.setdefault("from", from_obj.get("address") or from_obj.get("name") or "")
    text_blob = "\n".join(
        str(msg.get(k) or "")
        for k in (
            "subject",
            "content",
            "text",
            "textBody",
            "html",
            "htmlBody",
            "body",
            "from_address",
            "from",
            "verificationCode",
        )
    )
    msg["extracted"] = _extract_codes_and_links(text_blob)
    vc = msg.get("verificationCode")
    if vc and isinstance(msg.get("extracted"), dict):
        codes = list(msg["extracted"].get("codes") or [])
        s = str(vc).strip()
        if s and s not in codes:
            codes.insert(0, s)
            msg["extracted"]["codes"] = codes
    msg.setdefault("inboxAddress", body.get("inboxAddress") or addr)
    return msg



def gptmail_create_mailbox(
    *,
    name: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,  # accepted for API compat; GPTMail retains ~24h
    api_key: str | None = None,
    base_url: str | None = None,
    proxy: str | None = None,
    proxy_username: str | None = None,
    proxy_password: str | None = None,
) -> dict[str, Any]:
    """Create a temporary inbox on GPTMail.

    Docs: https://mail.chatgpt.org.uk/zh/api/
    Auth: ``X-API-Key: sk-…`` (public test key is shown on the docs page; copy it
    into 协议注册 → GPTMail). Legacy ``gpt-test`` is no longer valid.

    Endpoints:
      GET/POST /api/generate-email  — random or {prefix, domain}
      GET /api/emails?email=…       — list mails
      GET /api/email/{id}           — mail detail
    """
    key = (api_key or MOEMAIL_API_KEY or "").strip() or (GPTMAIL_PUBLIC_TEST_KEY or "").strip()
    if not key or key in {"gpt-test", "PUBLIC_API_KEY", "public"}:
        raise ValueError(
            "GPTMail API Key missing. Open https://mail.chatgpt.org.uk/zh/api/ "
            "copy the public sk-… test key (or your own key from shop.chatgpt.org.uk) "
            "into 协议注册 → GPTMail API Key."
        )
    if key.startswith("mk_") or key.startswith("AC-"):
        raise ValueError(
            "GPTMail API Key looks like MoeMail/YYDS. Use an sk-… key from "
            "https://mail.chatgpt.org.uk/zh/api/"
        )
    base = normalize_gptmail_base_url(base_url or GPTMAIL_DEFAULT_BASE_URL)
    # Multi-domain config → pick one; empty domain → let server choose via generate,
    # or pick from /api/domains/public for local compose fallback.
    dom = pick_domain_from_list(domain) if domain else ""
    if not dom:
        dom = (domain or "").strip().lstrip("@").strip(".")
    pre = (name or "").strip().lower() or None
    if pre:
        pre = re.sub(r"[^a-z0-9._+-]", "", pre) or None

    headers = {**_headers(key), "Content-Type": "application/json", "Accept": "application/json"}

    def _parse_email_payload(resp: "httpx.Response") -> str:
        if resp.status_code >= 400:
            raise RuntimeError(
                f"GPTMail create failed {resp.status_code}: {resp.text[:500]}"
            )
        data = resp.json() if resp.content else {}
        if isinstance(data, dict) and data.get("success") is False:
            raise RuntimeError(
                f"GPTMail create failed: {data.get('error') or data}"
            )
        body = data.get("data") if isinstance(data, dict) and "data" in data else data
        if not isinstance(body, dict):
            raise RuntimeError(f"Unexpected GPTMail create response: {data}")
        address = body.get("email") or body.get("address")
        if not address or "@" not in str(address):
            raise RuntimeError(f"Unexpected GPTMail create response: {data}")
        return str(address).strip()

    with httpx.Client(timeout=30.0) as client:
        address = ""
        raw: dict[str, Any] = {}
        try:
            if pre or dom:
                payload: dict[str, Any] = {}
                if pre:
                    payload["prefix"] = pre
                if dom:
                    payload["domain"] = dom
                resp = client.post(
                    f"{base}/api/generate-email",
                    json=payload,
                    headers=headers,
                )
            else:
                resp = client.get(f"{base}/api/generate-email", headers=headers)
            address = _parse_email_payload(resp)
            try:
                raw = resp.json() if resp.content else {}
            except Exception:
                raw = {}
        except Exception as first_err:
            # Docs: if you already know a live public domain, compose prefix@domain
            # locally and only use /api/emails (saves one generate call).
            picked = dom or gptmail_pick_domain(api_key=key, base_url=base) or ""
            if pre and picked:
                address = f"{pre}@{picked}"
                raw = {
                    "composed": True,
                    "domain": picked,
                    "generate_error": str(first_err)[:300],
                }
            else:
                raise

    return {
        "id": address,
        "email": address,
        "token": "",
        "provider": "gptmail",
        "raw": raw,
        "expiry_ms": 86_400_000 if expiry_ms is None else int(expiry_ms),
    }



def gptmail_list_domains(
    *,
    api_key: str | None = None,
    base_url: str | None = None,
) -> list[str]:
    """List active public domains from GPTMail catalog (``GET /api/domains/public``)."""
    base = normalize_gptmail_base_url(base_url or GPTMAIL_DEFAULT_BASE_URL)
    key = (api_key or MOEMAIL_API_KEY or "").strip()
    try:
        with httpx.Client(timeout=20.0) as client:
            # Public domain list does not require a key.
            resp = client.get(
                f"{base}/api/domains/public",
                headers=_headers(key) if key else {},
            )
            if resp.status_code >= 400:
                return []
            data = resp.json()
    except Exception:
        return []
    body = data.get("data") if isinstance(data, dict) and "data" in data else data
    items = body.get("domains") if isinstance(body, dict) else body
    if not isinstance(items, list):
        return []
    out: list[str] = []
    seen: set[str] = set()
    for item in items:
        if not isinstance(item, dict):
            continue
        name = item.get("domain_name") or item.get("domain") or item.get("name")
        if not isinstance(name, str) or not name.strip():
            continue
        if item.get("is_active") in (0, False, "0", "false"):
            continue
        name = name.strip().lstrip("@").strip(".")
        if not name or name in seen:
            continue
        seen.add(name)
        out.append(name)
    return out


def gptmail_pick_domain(
    *,
    api_key: str | None = None,
    base_url: str | None = None,
) -> str | None:
    """Pick an active public domain from GPTMail catalog."""
    domains = gptmail_list_domains(api_key=api_key, base_url=base_url)
    if not domains:
        return None
    return random.choice(domains)


def gptmail_fetch_messages(
    email_id: str,
    *,
    api_key: str | None = None,
    base_url: str | None = None,
    include_details: bool = True,
    address: str | None = None,
    token: str | None = None,
) -> list[dict[str, Any]]:
    """List messages for a GPTMail inbox.

    GPTMail keys mailboxes by the full email address (``?email=``).
    ``email_id`` may be either the address or a message id when fetching detail.
    """
    addr = (address or email_id or "").strip()
    if not addr or "@" not in addr:
        # If only a message id was passed, we cannot list; need address.
        if address and "@" in address:
            addr = address.strip()
        else:
            return []
    key = (api_key or MOEMAIL_API_KEY or "").strip() or (GPTMAIL_PUBLIC_TEST_KEY or "").strip()
    if not key or key in {"gpt-test", "PUBLIC_API_KEY"}:
        raise RuntimeError(
            "GPTMail API Key missing for inbox poll. Set sk-… in 协议注册 → GPTMail "
            "(https://mail.chatgpt.org.uk/zh/api/)."
        )
    base = normalize_gptmail_base_url(base_url or GPTMAIL_DEFAULT_BASE_URL)
    headers = {**_headers(key), "Accept": "application/json"}

    with httpx.Client(timeout=30.0) as client:
        resp = client.get(
            f"{base}/api/emails",
            headers=headers,
            params={"email": addr},
        )
        if resp.status_code >= 400:
            raise RuntimeError(
                f"GPTMail list failed {resp.status_code} for {addr}: {resp.text[:500]}. "
                "Check X-API-Key (sk-…) and email query param."
            )
        data = resp.json() if resp.content else {}
        body = data.get("data") if isinstance(data, dict) and "data" in data else data
        messages: list[Any] = []
        if isinstance(body, dict):
            messages = body.get("emails") or body.get("messages") or body.get("items") or []
        elif isinstance(body, list):
            messages = body
        if not isinstance(messages, list):
            return []

        out: list[dict[str, Any]] = []
        for raw in messages[:20]:
            if not isinstance(raw, dict):
                continue
            item = dict(raw)
            msg_id = item.get("id") or item.get("messageId") or item.get("email_id")
            # List payload often already includes content; detail is optional.
            if include_details and msg_id and not (
                item.get("content") or item.get("html_content") or item.get("html")
            ):
                detail = client.get(
                    f"{base}/api/email/{msg_id}",
                    headers=headers,
                )
                if detail.status_code == 200:
                    d = detail.json() if detail.content else {}
                    msg = d.get("data") if isinstance(d, dict) and "data" in d else d
                    if isinstance(msg, dict):
                        if isinstance(msg.get("email"), dict):
                            item.update(msg["email"])
                        elif isinstance(msg.get("message"), dict):
                            item.update(msg["message"])
                        else:
                            item.update(msg)
            # Normalize field names for shared code extractors.
            if item.get("html_content") and not item.get("html"):
                item["html"] = item.get("html_content")
            if item.get("content") and not item.get("text"):
                item["text"] = item.get("content")
            if item.get("from_address") and not item.get("from"):
                item["from"] = item.get("from_address")
            text = "\n".join(
                str(item.get(k) or "")
                for k in (
                    "subject",
                    "content",
                    "text",
                    "html",
                    "html_content",
                    "from_address",
                    "from",
                )
            )
            item["extracted"] = _extract_codes_and_links(text)
            out.append(item)
        return out


def cfmail_list_domains(
    *,
    api_key: str | None = None,
    base_url: str | None = None,
    site_password: str | None = None,
) -> list[str]:
    """List domains from CF Temp Email public settings (``GET /open_api/settings``)."""
    base = normalize_cfmail_base_url(base_url or CFMAIL_DEFAULT_BASE_URL)
    # Prefer site password / admin password as x-custom-auth for private sites.
    headers = _cfmail_headers(
        api_key=None,
        site_password=site_password or api_key,
        content_type=False,
    )
    try:
        with httpx.Client(timeout=20.0) as client:
            resp = client.get(f"{base}/open_api/settings", headers=headers)
            if resp.status_code >= 400:
                return []
            data = resp.json() if resp.content else {}
    except Exception:
        return []
    # open_api/settings returns a flat object (not {data: ...}).
    body = data if isinstance(data, dict) else {}
    if isinstance(data, dict) and isinstance(data.get("data"), dict):
        body = {**data, **data["data"]}
    if not isinstance(body, dict):
        return []
    out: list[str] = []
    seen: set[str] = set()
    for key in (
        "defaultDomains",
        "default_domains",
        "domains",
        "randomSubdomainDomains",
        "random_subdomain_domains",
    ):
        items = body.get(key)
        if isinstance(items, str):
            items = [x.strip() for x in items.split(",") if x.strip()]
        if not isinstance(items, list):
            continue
        for item in items:
            if isinstance(item, dict):
                name = item.get("domain") or item.get("name") or item.get("value")
            else:
                name = item
            if not isinstance(name, str) or not name.strip():
                continue
            name = name.strip().lstrip("@").strip(".")
            if not name or name in seen:
                continue
            seen.add(name)
            out.append(name)
    return out


def cfmail_pick_domain(
    *,
    api_key: str | None = None,
    base_url: str | None = None,
    site_password: str | None = None,
) -> str | None:
    """Randomly pick a domain from CF Temp Email public settings."""
    domains = cfmail_list_domains(
        api_key=api_key, base_url=base_url, site_password=site_password
    )
    if not domains:
        return None
    return random.choice(domains)


def _cfmail_parse_raw_rfc822(raw: str) -> dict[str, Any]:
    """Best-effort RFC822 parse for CF Temp Email raw mail bodies."""
    out: dict[str, Any] = {}
    text = (raw or "").strip()
    if not text:
        return out
    try:
        msg = email.message_from_string(text, policy=policy.default)
    except Exception:
        out["text"] = text[:8000]
        return out
    out["subject"] = str(msg.get("subject") or "")
    out["from"] = str(msg.get("from") or "")
    out["to"] = str(msg.get("to") or "")
    texts: list[str] = []
    htmls: list[str] = []
    if msg.is_multipart():
        for part in msg.walk():
            ctype = (part.get_content_type() or "").lower()
            disp = str(part.get_content_disposition() or "").lower()
            if disp == "attachment":
                continue
            try:
                payload = part.get_content()
            except Exception:
                try:
                    payload = part.get_payload(decode=True)
                    if isinstance(payload, bytes):
                        payload = payload.decode(
                            part.get_content_charset() or "utf-8",
                            errors="replace",
                        )
                except Exception:
                    payload = None
            if not isinstance(payload, str):
                continue
            if ctype == "text/html":
                htmls.append(payload)
            elif ctype.startswith("text/"):
                texts.append(payload)
    else:
        try:
            payload = msg.get_content()
        except Exception:
            payload = msg.get_payload(decode=True)
            if isinstance(payload, bytes):
                payload = payload.decode(
                    msg.get_content_charset() or "utf-8", errors="replace"
                )
        if isinstance(payload, str):
            if (msg.get_content_type() or "").lower() == "text/html":
                htmls.append(payload)
            else:
                texts.append(payload)
    if texts:
        out["text"] = "\n".join(texts)
    if htmls:
        out["html"] = "\n".join(htmls)
    if not texts and not htmls:
        out["text"] = text[:8000]
    return out


def cfmail_create_mailbox(
    *,
    name: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,  # accepted for API compat; CF address is durable
    api_key: str | None = None,
    base_url: str | None = None,
    site_password: str | None = None,
    proxy: str | None = None,
    proxy_username: str | None = None,
    proxy_password: str | None = None,
) -> dict[str, Any]:
    """Create an address on Cloudflare Temp Email.

    Preferred path (automation): ``POST /admin/new_address`` with admin password
    in ``x-admin-auth`` (pass as api_key).

    Fallback: ``POST /api/new_address`` (may require Turnstile / open create).

    Docs: https://github.com/dreamhunter2333/cloudflare_temp_email
    """
    key = (api_key or MOEMAIL_API_KEY or "").strip()
    base = normalize_cfmail_base_url(base_url or CFMAIL_DEFAULT_BASE_URL)
    if not key:
        raise ValueError(
            "Cloudflare Temp Email admin password missing. Set 协议注册 → CF "
            "Admin 密码 (x-admin-auth / ADMIN_PASSWORDS). "
            "Repo: https://github.com/dreamhunter2333/cloudflare_temp_email"
        )
    # Never bleed MoeMail default domain into CF.
    dom = pick_domain_from_list(domain) if domain else ""
    if not dom:
        dom = (domain or "").strip().lstrip("@").strip(".")
    if not dom:
        dom = cfmail_pick_domain(
            api_key=key, base_url=base, site_password=site_password
        ) or ""
    if not dom:
        raise ValueError(
            "Cloudflare Temp Email domain missing. Fill CF 域名 in 协议注册, "
            "or ensure GET /open_api/settings returns domains "
            f"(base={base})."
        )
    local = (name or "").strip().lower()
    if not local:
        local = secrets_token_hex_local()
    # Strip chars CF rejects (worker uses address name regex).
    local = re.sub(r"[^a-z0-9._+-]", "", local) or secrets_token_hex_local()

    # Admin create: name required; enablePrefix optional (worker PREFIX).
    # Public create may need Turnstile — automation must use admin password.
    payload_admin: dict[str, Any] = {
        "name": local,
        "domain": dom,
        "enablePrefix": False,
        "enableRandomSubdomain": False,
    }
    payload_public: dict[str, Any] = {
        "name": local,
        "domain": dom,
        "enableRandomSubdomain": False,
    }
    headers = _cfmail_headers(
        api_key=key, site_password=site_password, content_type=True, as_admin=True
    )
    use_admin = "x-admin-auth" in headers

    last_err = ""
    with httpx.Client(timeout=30.0) as client:
        resp = None
        if use_admin:
            resp = client.post(
                f"{base}/admin/new_address", json=payload_admin, headers=headers
            )
            if resp.status_code >= 400:
                last_err = f"admin/new_address {resp.status_code}: {resp.text[:300]}"
                # Some deploys use site password only — try public path too.
                pub_headers = _cfmail_headers(
                    api_key=None,
                    site_password=site_password or key,
                    content_type=True,
                )
                resp = client.post(
                    f"{base}/api/new_address",
                    json=payload_public,
                    headers=pub_headers,
                )
        else:
            resp = client.post(
                f"{base}/api/new_address", json=payload_public, headers=headers
            )
        if resp is None or resp.status_code >= 400:
            detail = (resp.text[:500] if resp is not None else last_err)
            raise RuntimeError(
                f"CF Temp Email create failed ({base}): {detail or last_err}. "
                "Use Workers API origin (not Pages UI), ADMIN_PASSWORDS in "
                "x-admin-auth, and a domain from /open_api/settings."
            )
        # Response may be JSON object or plain text error already handled.
        try:
            data = resp.json() if resp.content else {}
        except Exception as e:
            raise RuntimeError(
                f"CF Temp Email create returned non-JSON: {resp.text[:300]}"
            ) from e

    body = data.get("data") if isinstance(data, dict) and "data" in data else data
    if not isinstance(body, dict):
        raise RuntimeError(f"Unexpected CF Temp Email create response: {data}")
    address = (
        body.get("address")
        or body.get("email")
        or body.get("mail")
        or body.get("name")
    )
    jwt = (
        body.get("jwt")
        or body.get("token")
        or body.get("credential")
        or body.get("address_jwt")
        or ""
    )
    address_id = (
        body.get("address_id")
        or body.get("id")
        or body.get("addressId")
        or address
    )
    if not address or "@" not in str(address):
        # Some responses only return jwt + partial; try settings with jwt.
        if jwt:
            try:
                with httpx.Client(timeout=20.0) as client:
                    sresp = client.get(
                        f"{base}/api/settings",
                        headers=_cfmail_headers(api_key=str(jwt)),
                    )
                    if sresp.status_code < 400:
                        sdata = sresp.json() if sresp.content else {}
                        sbody = (
                            sdata.get("data")
                            if isinstance(sdata, dict) and "data" in sdata
                            else sdata
                        )
                        if isinstance(sbody, dict):
                            address = sbody.get("address") or address
            except Exception:
                pass
    if not address or "@" not in str(address):
        raise RuntimeError(f"Unexpected CF Temp Email create response: {data}")
    if not jwt:
        # Without address JWT we cannot poll inbox.
        raise RuntimeError(
            "CF Temp Email create returned no address JWT. "
            "Use admin password (x-admin-auth) via api_key, or enable open create."
        )
    return {
        "id": str(address_id or address),
        "email": str(address).strip(),
        "token": str(jwt),
        "provider": "cfmail",
        "raw": data,
        "expiry_ms": 86_400_000 if expiry_ms is None else int(expiry_ms),
    }


def secrets_token_hex_local() -> str:
    """Local-part generator without importing secrets at module top for clarity."""
    import secrets as _secrets

    return _secrets.token_hex(5).lower()


def cfmail_fetch_messages(
    email_id: str,
    *,
    api_key: str | None = None,
    base_url: str | None = None,
    include_details: bool = True,
    address: str | None = None,
    token: str | None = None,
    site_password: str | None = None,
) -> list[dict[str, Any]]:
    """List messages for a CF Temp Email address JWT.

    Prefers parsed endpoints; falls back to raw RFC822 list/detail.
    ``token`` (address JWT) is required for inbox access. ``api_key`` may also
    be the JWT when the admin key is not needed.
    """
    # Inbox access requires the address JWT returned at create time.
    jwt = (token or "").strip()
    if not jwt:
        # Only fall back to api_key if it is a JWT (3 segments), never admin password.
        cand = (api_key or MOEMAIL_API_KEY or "").strip()
        parts = cand.split(".")
        if len(parts) == 3 and all(parts):
            jwt = cand
    if not jwt:
        # 尝试通过管理员密码从 /admin/mails 获取邮件（支持已有老账号）
        admin_key = (api_key or MOEMAIL_API_KEY or "").strip()
        if admin_key:
            target_addr = (address or email_id or "").strip()
            base = normalize_cfmail_base_url(base_url or CFMAIL_DEFAULT_BASE_URL)
            admin_headers = _cfmail_headers(api_key=admin_key, as_admin=True)
            try:
                with httpx.Client(timeout=30.0) as client:
                    resp = client.get(
                        f"{base}/admin/mails",
                        headers=admin_headers,
                        params={"limit": 20, "offset": 0, "address": target_addr} if target_addr else {"limit": 20, "offset": 0},
                    )
                    if resp.status_code < 400:
                        data = resp.json() if resp.content else {}
                        body = data.get("data") if isinstance(data, dict) and "data" in data else data
                        items = []
                        if isinstance(body, dict):
                            items = body.get("results") or body.get("mails") or body.get("items") or []
                        elif isinstance(body, list):
                            items = body
                        out = []
                        for raw_item in items:
                            if isinstance(raw_item, dict):
                                raw_rfc = str(raw_item.get("raw") or "")
                                if raw_rfc:
                                    parsed = _cfmail_parse_raw_rfc822(raw_rfc)
                                    parsed["id"] = raw_item.get("id") or parsed.get("id")
                                    parsed["created_at"] = raw_item.get("created_at") or parsed.get("created_at")
                                    out.append(parsed)
                                else:
                                    out.append(raw_item)
                        return out
            except Exception:
                pass
        return []
    base = normalize_cfmail_base_url(base_url or CFMAIL_DEFAULT_BASE_URL)
    headers = _cfmail_headers(api_key=jwt, site_password=site_password)

    with httpx.Client(timeout=30.0) as client:
        # 1) Parsed list (newer deploys)
        items: list[Any] = []
        used_parsed = False
        resp = client.get(
            f"{base}/api/parsed_mails",
            headers=headers,
            params={"limit": 20, "offset": 0},
        )
        if resp.status_code < 400:
            data = resp.json() if resp.content else {}
            body = data.get("data") if isinstance(data, dict) and "data" in data else data
            if isinstance(body, dict):
                items = body.get("results") or body.get("mails") or body.get("items") or []
            elif isinstance(body, list):
                items = body
            used_parsed = True
        else:
            # 2) Raw list fallback
            resp = client.get(
                f"{base}/api/mails",
                headers=headers,
                params={"limit": 20, "offset": 0},
            )
            if resp.status_code >= 400:
                raise RuntimeError(
                    f"CF Temp Email list failed {resp.status_code}: {resp.text[:500]}"
                )
            data = resp.json() if resp.content else {}
            body = data.get("data") if isinstance(data, dict) and "data" in data else data
            if isinstance(body, dict):
                items = body.get("results") or body.get("mails") or body.get("items") or []
            elif isinstance(body, list):
                items = body

        if not isinstance(items, list):
            return []

        out: list[dict[str, Any]] = []
        for raw in items[:20]:
            if not isinstance(raw, dict):
                continue
            item = dict(raw)
            msg_id = item.get("id") or item.get("mail_id") or item.get("message_id")
            if include_details and msg_id and not used_parsed:
                detail = client.get(
                    f"{base}/api/mail/{msg_id}",
                    headers=headers,
                )
                if detail.status_code == 200:
                    d = detail.json() if detail.content else {}
                    msg = d.get("data") if isinstance(d, dict) and "data" in d else d
                    if isinstance(msg, dict):
                        item.update(msg)
            # Normalize CF shapes → shared extractor fields.
            if not item.get("text") and not item.get("html"):
                raw_rfc = (
                    item.get("raw")
                    or item.get("source")
                    or item.get("message")
                    or item.get("content")
                    or ""
                )
                if isinstance(raw_rfc, str) and ("\n" in raw_rfc or "From:" in raw_rfc):
                    parsed = _cfmail_parse_raw_rfc822(raw_rfc)
                    for k, v in parsed.items():
                        item.setdefault(k, v)
            if item.get("sender") and not item.get("from"):
                item["from"] = item.get("sender")
            if item.get("source") and not item.get("from"):
                # Some rows store envelope sender in source.
                src = item.get("source")
                if isinstance(src, str) and "@" in src and "\n" not in src:
                    item["from"] = src
            text = "\n".join(
                str(item.get(k) or "")
                for k in (
                    "subject",
                    "text",
                    "html",
                    "content",
                    "from",
                    "sender",
                )
            )
            item["extracted"] = _extract_codes_and_links(text)
            if msg_id is not None:
                item["id"] = str(msg_id)
            out.append(item)
        return out



def normalize_tempmail_base_url(base_url: str | None = None) -> str:
    """TempMail.lol API origin (https://tempmail.lol/zh/api)."""
    raw = (base_url or "").strip()
    if not raw:
        return TEMPMAIL_LOL_DEFAULT_BASE_URL
    lower = raw.lower()
    if "tempmail.lol" in lower or "api.tempmail" in lower:
        return TEMPMAIL_LOL_DEFAULT_BASE_URL
    # reject other providers
    if any(
        x in lower
        for x in (
            "moemail",
            "maliapi",
            "chatgpt.org.uk",
            "workers.dev",
            "example.com",
        )
    ):
        return TEMPMAIL_LOL_DEFAULT_BASE_URL
    parsed = urlparse(raw if "://" in raw else f"https://{raw}")
    origin = f"{parsed.scheme or 'https'}://{parsed.netloc}".rstrip("/")
    if not parsed.netloc:
        return TEMPMAIL_LOL_DEFAULT_BASE_URL
    return origin or TEMPMAIL_LOL_DEFAULT_BASE_URL


def tempmail_create_mailbox(
    *,
    name: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,
    api_key: str | None = None,
    base_url: str | None = None,
    proxy: str | None = None,
    proxy_username: str | None = None,
    proxy_password: str | None = None,
) -> dict[str, Any]:
    """Create a free TempMail.lol inbox (no API key required).

    Docs: https://tempmail.lol/zh/api
    - POST https://api.tempmail.lol/v2/inbox/create
    - Optional JSON: prefix, domain (custom/paid), community
    - Free tier: omit Authorization; Plus/Ultra: Authorization: Bearer <api_key>
    """
    base = normalize_tempmail_base_url(base_url)
    key = (api_key or "").strip()
    pre = (name or "").strip().lower() or None
    if pre:
        pre = re.sub(r"[^a-z0-9._+-]", "", pre) or None
    dom = pick_domain_from_list(domain) if domain else ""
    if not dom:
        dom = (domain or "").strip().lstrip("@").strip(".") or None

    payload: dict[str, Any] = {}
    if pre:
        payload["prefix"] = pre
    if dom:
        # Custom domains typically need Plus/Ultra; free will 400 Invalid domain.
        payload["domain"] = dom

    headers: dict[str, str] = {"Accept": "application/json", "Content-Type": "application/json"}
    if key:
        # Paid tiers (Plus / Ultra)
        headers["Authorization"] = f"Bearer {key}"

    with httpx.Client(timeout=30.0) as client:
        resp = client.post(f"{base}/v2/inbox/create", json=payload or {}, headers=headers)
        if resp.status_code >= 400 and dom and "Invalid domain" in (resp.text or ""):
            # Free tier: drop custom domain and retry random.
            payload.pop("domain", None)
            resp = client.post(f"{base}/v2/inbox/create", json=payload or {}, headers=headers)
        if resp.status_code >= 400:
            raise RuntimeError(
                f"TempMail.lol create failed {resp.status_code}: {resp.text[:500]}"
            )
        data = resp.json() if resp.content else {}

    address = data.get("address") or data.get("email")
    token = data.get("token") or ""
    if not address or "@" not in str(address):
        raise RuntimeError(f"Unexpected TempMail.lol create response: {data}")
    if not token:
        raise RuntimeError(
            f"TempMail.lol create returned no token (needed for /v2/inbox): {data}"
        )
    return {
        "id": str(address).strip(),  # list is token-keyed; id kept as address
        "email": str(address).strip(),
        "token": str(token),
        "provider": "tempmail",
        "raw": data,
        "expiry_ms": 86_400_000 if expiry_ms is None else int(expiry_ms),
    }


def tempmail_fetch_messages(
    email_id: str,
    *,
    api_key: str | None = None,
    base_url: str | None = None,
    include_details: bool = True,
    address: str | None = None,
    token: str | None = None,
) -> list[dict[str, Any]]:
    """List messages for a TempMail.lol inbox via ``GET /v2/inbox?token=…``.

    Free tier: only the inbox token from create is required (no API key).
    Response: ``{emails:[{from,to,subject,body,html,date}], expired:bool}``.
    """
    tok = (token or "").strip()
    if not tok:
        # Mis-wired callers sometimes put token in email_id; don't treat address as token.
        return []
    base = normalize_tempmail_base_url(base_url)
    headers: dict[str, str] = {"Accept": "application/json"}
    key = (api_key or "").strip()
    if key:
        headers["Authorization"] = f"Bearer {key}"

    with httpx.Client(timeout=30.0) as client:
        resp = client.get(
            f"{base}/v2/inbox",
            headers=headers,
            params={"token": tok},
        )
        if resp.status_code >= 400:
            raise RuntimeError(
                f"TempMail.lol inbox failed {resp.status_code}: {resp.text[:500]}"
            )
        data = resp.json() if resp.content else {}

    if isinstance(data, dict) and data.get("expired") is True:
        return []
    messages = []
    if isinstance(data, dict):
        messages = data.get("emails") or data.get("messages") or data.get("items") or []
    elif isinstance(data, list):
        messages = data
    if not isinstance(messages, list):
        return []

    out: list[dict[str, Any]] = []
    for raw in messages[:30]:
        if not isinstance(raw, dict):
            continue
        item = dict(raw)
        # Normalize for shared extractors / wait_for_code.
        if item.get("body") and not item.get("text"):
            item["text"] = item.get("body")
        if item.get("body") and not item.get("content"):
            item["content"] = item.get("body")
        if item.get("from") and not item.get("from_address"):
            item["from_address"] = item.get("from")
        text_blob = "\n".join(
            str(item.get(k) or "")
            for k in ("subject", "body", "text", "content", "html", "from", "from_address")
        )
        item["extracted"] = _extract_codes_and_links(text_blob)
        # Stable id for detail-less list
        if not item.get("id"):
            item["id"] = str(
                item.get("date")
                or hash(text_blob) & 0xFFFFFFFF
            )
        out.append(item)
    return out


def tempmail_list_domains(
    *,
    api_key: str | None = None,
    base_url: str | None = None,
) -> list[str]:
    """Free TempMail.lol assigns random domains; no public catalog API.

    Optional domain/prefix is only for paid custom domains. Return empty so UI
    shows auto-assign.
    """
    return []


# ── Cloud Mail (maillab/cloud-mail) ────────────────────────────────────────
# Cloud Mail is a user-account mailbox service (not a temp inbox). Admin email
# + password authenticate via POST /api/public/genToken → KV-cached UUID token.
# The token is then sent as `Authorization: <token>` header on subsequent
# /api/public/* calls. Mailboxes are created via /api/public/addUser with a list of
# {email, password} and read via /api/public/emailList filtering by toEmail.
#
# The adapter api_key slot stores `admin_email:admin_password` (plain). We
# parse it here so users only manage a single field in the admin UI.

# Process-local token cache: (token, expires_at). Token lives in Cloudflare KV
# with no documented TTL, but re-running genToken invalidates the previous one.
# We cache for 25 minutes and refresh on 401.
_CLOUDMAIL_TOKEN_CACHE: dict[str, tuple[str, float]] = {}
_CLOUDMAIL_TOKEN_TTL_SEC = 1500.0  # 25 min


def normalize_cloudmail_base_url(base_url: str | None = None) -> str:
    """Normalize Cloud Mail Worker origin. Self-hosted only — no default host.

    Accepts ``https://skymail.example.com`` or ``skymail.example.com`` and
    strips trailing ``/``, ``/api``, ``/#/`` paths pasted from the browser.
    """
    raw = (base_url or "").strip()
    if not raw:
        return ""
    lower = raw.lower()
    # Reject accidental other-provider pastes.
    if any(
        x in lower
        for x in (
            "moemail.example.com",
            "moemail.521884.xyz",
            "maliapi.215.im",
            "vip.215.im",
            "chatgpt.org.uk",
            "temp-email-api.awsl.uk",
            "api.tempmail.lol",
        )
    ):
        return ""
    parsed = urlparse(raw if "://" in raw else f"https://{raw}")
    if not parsed.netloc:
        return ""
    origin = f"{parsed.scheme or 'https'}://{parsed.netloc}".rstrip("/")
    return origin


def _cloudmail_parse_credential(api_key: str | None) -> tuple[str, str]:
    """Split ``admin_email:password`` into (email, password).

    Email is the part before the last ``:`` so passwords containing ``:`` work.
    """
    raw = (api_key or "").strip()
    if not raw:
        return "", ""
    # base64 fallback: if no ':' present, try decoding base64(email:password).
    if ":" not in raw:
        import base64 as _b64

        try:
            decoded = _b64.urlsafe_b64decode(raw + "=" * (-len(raw) % 4)).decode(
                "utf-8", errors="replace"
            )
            if ":" in decoded:
                raw = decoded
        except Exception:
            pass
    if ":" not in raw:
        return "", ""
    email, _, password = raw.rpartition(":")
    return email.strip(), password.strip()


def _cloudmail_get_token(
    *,
    base: str,
    admin_email: str,
    admin_password: str,
) -> str:
    """Obtain (and cache) a Cloud Mail public API token via /public/genToken."""
    cache_key = base
    cached = _CLOUDMAIL_TOKEN_CACHE.get(cache_key)
    now = time.time()
    if cached and cached[1] > now:
        return cached[0]
    with httpx.Client(timeout=30.0) as client:
        resp = client.post(
            f"{base}/api/public/genToken",
            json={"email": admin_email, "password": admin_password},
            headers={"Content-Type": "application/json"},
        )
        if resp.status_code >= 400:
            raise RuntimeError(
                f"Cloud Mail genToken failed {resp.status_code}: {resp.text[:500]}"
            )
        data = resp.json() if resp.content else {}
    # Envelope: { code: 200, message: "success", data: { token: "uuid" } }
    body = data.get("data") if isinstance(data, dict) else None
    token = ""
    if isinstance(body, dict):
        token = str(body.get("token") or "").strip()
    if not token:
        raise RuntimeError(f"Cloud Mail genToken returned no token: {data}")
    _CLOUDMAIL_TOKEN_CACHE[cache_key] = (token, now + _CLOUDMAIL_TOKEN_TTL_SEC)
    return token


def _cloudmail_invalidate_token(base: str) -> None:
    _CLOUDMAIL_TOKEN_CACHE.pop(base, None)


def cloudmail_list_domains(
    *,
    api_key: str | None = None,
    base_url: str | None = None,
) -> list[str]:
    """Fetch domain list from Cloud Mail public website config.

    GET /api/setting/websiteConfig is excluded from auth and returns site config
    including the configured ``domain`` array. Empty list when unreachable.
    """
    base = normalize_cloudmail_base_url(base_url)
    if not base:
        return []
    try:
        with httpx.Client(timeout=20.0) as client:
            resp = client.get(f"{base}/api/setting/websiteConfig")
            if resp.status_code >= 400:
                return []
            data = resp.json() if resp.content else {}
    except Exception:
        return []
    body = data.get("data") if isinstance(data, dict) and "data" in data else data
    if not isinstance(body, dict):
        return []
    raw_domains = body.get("domainList") or body.get("domain") or body.get("domains") or []
    if isinstance(raw_domains, str):
        raw_domains = [x.strip() for x in raw_domains.split(",") if x.strip()]
    if not isinstance(raw_domains, list):
        return []
    out: list[str] = []
    seen: set[str] = set()
    for item in raw_domains:
        if isinstance(item, dict):
            name = item.get("domain") or item.get("name") or item.get("value")
        else:
            name = item
        if not isinstance(name, str) or not name.strip():
            continue
        name = name.strip().lstrip("@").strip(".").lower()
        if not name or name in seen:
            continue
        seen.add(name)
        out.append(name)
    return out


def cloudmail_pick_domain(
    *,
    api_key: str | None = None,
    base_url: str | None = None,
) -> str | None:
    """Randomly pick a domain from Cloud Mail website config."""
    domains = cloudmail_list_domains(api_key=api_key, base_url=base_url)
    if not domains:
        return None
    return random.choice(domains)


def cloudmail_create_mailbox(
    *,
    name: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,  # accepted for API compat; Cloud Mail accounts are durable
    api_key: str | None = None,
    base_url: str | None = None,
    proxy: str | None = None,
    proxy_username: str | None = None,
    proxy_password: str | None = None,
) -> dict[str, Any]:
    """Create a mailbox user on Cloud Mail (maillab/cloud-mail).

    Requires admin credentials in ``api_key`` as ``admin_email:password`` and
    the self-hosted Worker origin in ``base_url``. Calls:
      1. POST /api/public/genToken  → Authorization token
      2. POST /api/public/addUser   → create the user/mailbox
    The new account's password is randomly generated and returned for callers
    that may need to log in to the mailbox UI.
    """
    admin_email, admin_password = _cloudmail_parse_credential(api_key)
    if not admin_email or not admin_password:
        raise ValueError(
            "Cloud Mail admin credentials missing. Fill 协议注册 → Cloud Mail "
            "API Key as `admin_email:password`. Repo: "
            "https://github.com/maillab/cloud-mail"
        )
    base = normalize_cloudmail_base_url(base_url)
    if not base:
        raise ValueError(
            "Cloud Mail Base URL missing. Fill 协议注册 → Cloud Mail Base URL "
            "(self-hosted Worker origin, e.g. https://skymail.example.com)."
        )

    # Resolve target domain: explicit > config list > /setting/websiteConfig.
    dom = pick_domain_from_list(domain) if domain else ""
    if not dom:
        dom = (domain or "").strip().lstrip("@").strip(".")
    if not dom:
        dom = cloudmail_pick_domain(api_key=api_key, base_url=base) or ""
    if not dom:
        raise ValueError(
            "Cloud Mail domain missing. Fill Cloud Mail 域名 in 协议注册, or "
            f"ensure GET {base}/api/setting/websiteConfig returns a domain list."
        )

    # Local-part: explicit name > random hex. Cloud Mail accepts standard
    # email name chars; strip anything else.
    local = (name or "").strip().lower()
    if not local:
        import secrets as _s

        local = _s.token_hex(5)
    local = re.sub(r"[^a-z0-9._+-]", "", local) or secrets_token_hex_local()
    address = f"{local}@{dom}"

    # Random password for the new mailbox account (returned to caller).
    import secrets as _s

    mailbox_password = _s.token_urlsafe(12)

    token = _cloudmail_get_token(
        base=base, admin_email=admin_email, admin_password=admin_password
    )
    payload = {
        "list": [
            {"email": address, "password": mailbox_password},
        ]
    }
    with httpx.Client(timeout=30.0) as client:
        resp = client.post(
            f"{base}/api/public/addUser",
            json=payload,
            headers={
                "Authorization": token,
                "Content-Type": "application/json",
            },
        )
        # 401 → token expired/invalidated; refresh once and retry.
        if resp.status_code == 401:
            _cloudmail_invalidate_token(base)
            token = _cloudmail_get_token(
                base=base, admin_email=admin_email, admin_password=admin_password
            )
            resp = client.post(
                f"{base}/api/public/addUser",
                json=payload,
                headers={
                    "Authorization": token,
                    "Content-Type": "application/json",
                },
            )
        if resp.status_code >= 400:
            raise RuntimeError(
                f"Cloud Mail addUser failed {resp.status_code}: {resp.text[:500]}"
            )
        data = resp.json() if resp.content else {}

    # Cloud Mail addUser returns {code:200, message:"success", data:null} on OK.
    code = data.get("code") if isinstance(data, dict) else None
    if isinstance(code, int) and code != 200:
        raise RuntimeError(
            f"Cloud Mail addUser failed: {data.get('message') or data}"
        )

    return {
        # Cloud Mail has no per-mailbox numeric id; the address is the identity.
        "id": address,
        "email": address,
        # Reuse `token` slot to carry admin token so callers can poll without
        # re-running genToken (fetch_messages will refresh on 401 anyway).
        "token": token,
        "password": mailbox_password,
        "provider": "cloudmail",
        "raw": data,
        "expiry_ms": 86_400_000 if expiry_ms is None else int(expiry_ms),
    }


def cloudmail_fetch_messages(
    email_id: str,
    *,
    api_key: str | None = None,
    base_url: str | None = None,
    include_details: bool = True,
    address: str | None = None,
    token: str | None = None,
) -> list[dict[str, Any]]:
    """List messages for a Cloud Mail inbox via POST /public/emailList.

    ``email_id`` / ``address`` is the full mailbox address (Cloud Mail has no
    numeric mailbox id). Filtering uses ``toEmail`` LIKE match (server-side).
    """
    addr = (address or email_id or "").strip()
    if not addr or "@" not in addr:
        return []
    admin_email, admin_password = _cloudmail_parse_credential(api_key)
    if not admin_email or not admin_password:
        return []
    base = normalize_cloudmail_base_url(base_url)
    if not base:
        return []

    # Reuse cached admin token when caller did not pass one.
    tok = (token or "").strip()
    if not tok:
        try:
            tok = _cloudmail_get_token(
                base=base, admin_email=admin_email, admin_password=admin_password
            )
        except Exception:
            return []

    payload: dict[str, Any] = {
        "toEmail": addr,
        "num": 1,
        "size": 20,
        "timeSort": "desc",
        "isDel": 0,
    }
    with httpx.Client(timeout=30.0) as client:
        resp = client.post(
            f"{base}/api/public/emailList",
            json=payload,
            headers={
                "Authorization": tok,
                "Content-Type": "application/json",
            },
        )
        if resp.status_code == 401:
            _cloudmail_invalidate_token(base)
            try:
                tok = _cloudmail_get_token(
                    base=base, admin_email=admin_email, admin_password=admin_password
                )
            except Exception:
                return []
            resp = client.post(
                f"{base}/api/public/emailList",
                json=payload,
                headers={
                    "Authorization": tok,
                    "Content-Type": "application/json",
                },
            )
        if resp.status_code >= 400:
            raise RuntimeError(
                f"Cloud Mail emailList failed {resp.status_code}: {resp.text[:500]}"
            )
        data = resp.json() if resp.content else {}

    # Envelope: { code:200, data: [{...email rows...}] }
    body = data.get("data") if isinstance(data, dict) and "data" in data else data
    messages: list[Any] = []
    if isinstance(body, list):
        messages = body
    elif isinstance(body, dict):
        messages = body.get("list") or body.get("emails") or body.get("items") or []
    if not isinstance(messages, list):
        return []

    out: list[dict[str, Any]] = []
    for raw in messages[:20]:
        if not isinstance(raw, dict):
            continue
        item = dict(raw)
        # Normalize field names so shared extractors work.
        if item.get("sendEmail") and not item.get("from"):
            item["from"] = item.get("sendEmail")
        if item.get("sendEmail") and not item.get("from_address"):
            item["from_address"] = item.get("sendEmail")
        if item.get("sendName") and not item.get("from_name"):
            item["from_name"] = item.get("sendName")
        if item.get("createTime") and not item.get("date"):
            item["date"] = item.get("createTime")
        if item.get("emailId") and not item.get("id"):
            item["id"] = str(item.get("emailId"))
        # Cloud Mail already stores content + text on each row, so detail
        # fetch is unnecessary. include_details kept for API compat only.
        text_blob = "\n".join(
            str(item.get(k) or "")
            for k in (
                "subject",
                "content",
                "text",
                "html",
                "from",
                "from_address",
                "sendEmail",
                "sendName",
            )
        )
        item["extracted"] = _extract_codes_and_links(text_blob)
        out.append(item)
    return out


def create_mailbox(
    *,
    provider: str | None = None,
    name: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,
    api_key: str | None = None,
    base_url: str | None = None,
    proxy: str | None = None,
    proxy_username: str | None = None,
    proxy_password: str | None = None,
) -> dict[str, Any]:
    """Provider-aware mailbox create (``moemail`` | ``yyds`` | ``gptmail`` | ``cfmail`` | ``tempmail`` | ``cloudmail``)."""
    prov = normalize_mail_provider(provider, base_url=base_url)
    if prov == "cloudmail":
        return cloudmail_create_mailbox(
            name=name,
            domain=domain,
            expiry_ms=expiry_ms,
            api_key=api_key,
            base_url=base_url,
            proxy=proxy,
            proxy_username=proxy_username,
            proxy_password=proxy_password,
        )
    if prov == "yyds":
        return yyds_create_mailbox(
            name=name,
            domain=domain,
            expiry_ms=expiry_ms,
            api_key=api_key,
            base_url=base_url,
            proxy=proxy,
            proxy_username=proxy_username,
            proxy_password=proxy_password,
        )
    if prov == "gptmail":
        return gptmail_create_mailbox(
            name=name,
            domain=domain,
            expiry_ms=expiry_ms,
            api_key=api_key,
            base_url=base_url,
            proxy=proxy,
            proxy_username=proxy_username,
            proxy_password=proxy_password,
        )
    if prov == "cfmail":
        return cfmail_create_mailbox(
            name=name,
            domain=domain,
            expiry_ms=expiry_ms,
            api_key=api_key,
            base_url=base_url,
            proxy=proxy,
            proxy_username=proxy_username,
            proxy_password=proxy_password,
        )
    if prov == "tempmail":
        return tempmail_create_mailbox(
            name=name,
            domain=domain,
            expiry_ms=expiry_ms,
            api_key=api_key,
            base_url=base_url,
            proxy=proxy,
            proxy_username=proxy_username,
            proxy_password=proxy_password,
        )
    box = moemail_create_mailbox(
        name=name,
        domain=domain,
        expiry_ms=expiry_ms,
        api_key=api_key,
        base_url=base_url,
        proxy=proxy,
        proxy_username=proxy_username,
        proxy_password=proxy_password,
    )
    box.setdefault("provider", "moemail")
    box.setdefault("token", "")
    return box


def fetch_messages(
    email_id: str,
    *,
    provider: str | None = None,
    api_key: str | None = None,
    base_url: str | None = None,
    include_details: bool = True,
    address: str | None = None,
    token: str | None = None,
) -> list[dict[str, Any]]:
    """Provider-aware message list."""
    prov = normalize_mail_provider(provider, base_url=base_url)
    if prov == "cloudmail":
        return cloudmail_fetch_messages(
            email_id,
            api_key=api_key,
            base_url=base_url,
            include_details=include_details,
            address=address,
            token=token,
        )
    if prov == "yyds":
        return yyds_fetch_messages(
            email_id,
            api_key=api_key,
            base_url=base_url,
            include_details=include_details,
            address=address,
            token=token,
        )
    if prov == "gptmail":
        return gptmail_fetch_messages(
            email_id,
            api_key=api_key,
            base_url=base_url,
            include_details=include_details,
            address=address or email_id,
            token=token,
        )
    if prov == "cfmail":
        return cfmail_fetch_messages(
            email_id,
            api_key=api_key,
            base_url=base_url,
            include_details=include_details,
            address=address,
            token=token,
        )
    if prov == "tempmail":
        return tempmail_fetch_messages(
            email_id,
            api_key=api_key,
            base_url=base_url,
            include_details=include_details,
            address=address,
            token=token,
        )
    return moemail_fetch_messages(
        email_id,
        api_key=api_key,
        base_url=base_url,
        include_details=include_details,
    )


def test_xai_proxy(
    *,
    proxy: str | None = None,
    proxy_username: str | None = None,
    proxy_password: str | None = None,
) -> dict[str, Any]:
    """Smoke-test whether a proxy can reach accounts.x.ai."""
    try:
        proxy_cfg = normalize_proxy_config(
            proxy,
            username=proxy_username,
            password=proxy_password,
        )
    except ValueError as e:
        return {"ok": False, "error": str(e), "proxy_enabled": False}

    url = "https://accounts.x.ai/sign-up?redirect=grok-com"
    headers = {
        "user-agent": (
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
            "(KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
        ),
        "accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
    }
    try:
        from curl_cffi import requests as curl_requests
    except Exception:
        curl_requests = None

    if curl_requests is not None:
        try:
            kwargs: dict[str, Any] = {
                "headers": headers,
                "timeout": 45,
                "allow_redirects": True,
                "impersonate": "chrome",
            }
            if proxy_cfg:
                kwargs["proxies"] = {
                    "http": proxy_cfg["proxy"],
                    "https": proxy_cfg["proxy"],
                }
            resp = curl_requests.get(url, **kwargs)
            return {
                "ok": 200 <= int(resp.status_code) < 400,
                "status_code": int(resp.status_code),
                "body_preview": (resp.text or "")[:500],
                "transport": "curl_cffi",
                "proxy_enabled": bool(proxy_cfg),
            }
        except Exception as e:  # noqa: BLE001
            return {
                "ok": False,
                "status_code": 0,
                "body_preview": str(e)[:500],
                "transport": "curl_cffi",
                "proxy_enabled": bool(proxy_cfg),
            }

    try:
        with httpx.Client(
            timeout=45.0,
            proxy=proxy_cfg["proxy"] if proxy_cfg else None,
            follow_redirects=True,
        ) as client:
            resp = client.get(url, headers=headers)
            return {
                "ok": 200 <= int(resp.status_code) < 400,
                "status_code": int(resp.status_code),
                "body_preview": (resp.text or "")[:500],
                "transport": "httpx",
                "proxy_enabled": bool(proxy_cfg),
            }
    except Exception as e:  # noqa: BLE001
        return {
            "ok": False,
            "status_code": 0,
            "body_preview": str(e)[:500],
            "transport": "httpx",
            "proxy_enabled": bool(proxy_cfg),
        }
