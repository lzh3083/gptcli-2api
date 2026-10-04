"""Proxy pool helpers for protocol registration / outbound HTTP.

Supports multi-line proxy lists (one proxy per line) with shared optional
username/password, plus simple rotation strategies for batch jobs.

Accepted line formats:
  - http://host:port
  - http://user:pass@host:port
  - socks5://host:port
  - host:port
  - host:port:user:pass
  - scheme://host:port:user:pass  (common residential-provider style)

Legacy single-proxy config continues to work unchanged.
"""

from __future__ import annotations

import os
import random
import threading
from typing import Any, Iterable
from urllib.parse import quote, unquote, urlparse, urlunparse

_lock = threading.Lock()
_rr_index = 0
_outbound_proxy_cache_key: tuple[Any, ...] | None = None
_outbound_proxy_cache_value: dict[str, Any] | None = None


def _copy_outbound_proxy_source(src: dict[str, Any]) -> dict[str, Any]:
    out = dict(src)
    out["pool"] = list(src.get("pool") or [])
    return out


def invalidate_outbound_proxy_cache() -> None:
    """Drop cached outbound proxy source / parsed pool after config changes."""
    global _outbound_proxy_cache_key, _outbound_proxy_cache_value
    with _lock:
        _outbound_proxy_cache_key = None
        _outbound_proxy_cache_value = None


def _env_proxy_text() -> str:
    # Prefer dedicated pool env, then the classic single-proxy vars.
    for key in (
        "GROK2API_XAI_PROXY_POOL",
        "GROK2API_PROXY_POOL",
        "GROK2API_XAI_PROXY",
        "GROK2API_PROXY",
        "GROK_CLI_PROXY",
    ):
        val = (os.getenv(key) or "").strip()
        if val:
            return val
    return ""


def _env_proxy_user() -> str:
    return (
        os.getenv("GROK2API_XAI_PROXY_USERNAME")
        or os.getenv("GROK2API_PROXY_USERNAME")
        or ""
    ).strip()


def _env_proxy_pass() -> str:
    return (
        os.getenv("GROK2API_XAI_PROXY_PASSWORD")
        or os.getenv("GROK2API_PROXY_PASSWORD")
        or ""
    ).strip()


def split_proxy_text(text: str | None) -> list[str]:
    """Split multi-proxy text into raw lines (comma / newline / semicolon)."""
    raw = (text or "").strip()
    if not raw:
        return []
    # Normalize common separators while preserving URL schemes (://).
    # First split on newlines / semicolons; then on commas only when the token
    # does not look like a single URL with query string.
    chunks: list[str] = []
    for part in raw.replace("\r\n", "\n").replace("\r", "\n").split("\n"):
        part = part.strip()
        if not part:
            continue
        if ";" in part:
            for sub in part.split(";"):
                sub = sub.strip()
                if sub:
                    chunks.append(sub)
            continue
        # Comma-separated lists: "a,b,c" — but not "http://x?a=1,b=2" (rare).
        if "," in part and "://" not in part.split(",", 1)[0]:
            for sub in part.split(","):
                sub = sub.strip()
                if sub:
                    chunks.append(sub)
            continue
        # Also allow "url1,url2" when each segment has a scheme.
        if "," in part:
            maybe = [s.strip() for s in part.split(",") if s.strip()]
            if maybe and all("://" in s or s.count(":") >= 1 for s in maybe):
                chunks.extend(maybe)
                continue
        chunks.append(part)
    # Drop comments / empty.
    out: list[str] = []
    seen: set[str] = set()
    for c in chunks:
        line = c.strip()
        if not line or line.startswith("#"):
            continue
        if line not in seen:
            seen.add(line)
            out.append(line)
    return out


def _normalize_line_scheme(raw: str) -> str:
    s = (raw or "").strip()
    if not s:
        return ""
    lower = s.lower()
    if lower.startswith("soket5://"):
        return "socks5://" + s.split("://", 1)[1]
    if lower.startswith("socket5://"):
        return "socks5://" + s.split("://", 1)[1]
    return s


def _hostport_userpass(raw: str) -> str | None:
    """Parse host:port:user:pass (or scheme://host:port:user:pass) → URL."""
    s = _normalize_line_scheme(raw)
    if not s:
        return None
    scheme = "http"
    rest = s
    if "://" in s:
        scheme, rest = s.split("://", 1)
        scheme = (scheme or "http").strip().lower() or "http"
        if scheme in {"soket5", "socket5"}:
            scheme = "socks5"
    # Already a normal URL with optional userinfo.
    if "@" in rest or rest.count(":") <= 1:
        if "://" not in s:
            return f"{scheme}://{rest}"
        return f"{scheme}://{rest}" if not s.startswith(f"{scheme}://") else s

    # host:port:user:pass  (user/pass may contain ':')
    # IPv6 is not supported in this shorthand (use full URL).
    parts = rest.split(":")
    if len(parts) < 4:
        if "://" not in s:
            return f"{scheme}://{rest}"
        return s
    host = parts[0].strip()
    port = parts[1].strip()
    user = parts[2]
    password = ":".join(parts[3:])
    if not host or not port:
        return None
    try:
        int(port)
    except ValueError:
        return None
    auth = quote(user, safe="")
    if password != "":
        auth = f"{auth}:{quote(password, safe='')}"
    return f"{scheme}://{auth}@{host}:{port}"


# Values the UI/API use to say "explicitly no proxy". They are sentinels, not
# host names: without this list, normalize_proxy_config's `f"http://{raw}"`
# shorthand turned "direct" into the literal proxy URL http://direct, which then
# entered the outbound pool and made curl/httpx fail with
# "Could not resolve proxy: direct" / "Temporary failure in name resolution".
DIRECT_PROXY_SENTINELS = frozenset(
    {"direct", "none", "off", "no", "false", "nil", "null", "0"}
)

_PROXY_SCHEME_PREFIXES = (
    "http://",
    "https://",
    "socks5://",
    "socks5h://",
    "socks4://",
    "socks4a://",
)


def is_direct_proxy(value: str | None) -> bool:
    """True when ``value`` explicitly means "no proxy".

    Accepts the bare sentinel ("direct") as well as a scheme-wrapped spelling
    ("http://direct") so already-persisted bad config is also recognised.
    An empty value is *not* direct: empty means "unset, use the default".
    """
    text = str(value or "").strip().lower()
    if not text:
        return False
    for prefix in _PROXY_SCHEME_PREFIXES:
        if text.startswith(prefix):
            text = text[len(prefix):]
            break
    return text.rstrip("/") in DIRECT_PROXY_SENTINELS


def canonicalize_proxy_line(
    raw: str,
    *,
    username: str | None = None,
    password: str | None = None,
) -> str:
    """Return a single proxy URL with optional shared auth applied.

    Raises ValueError when the line is not a usable proxy.
    """
    from grok2api.upstream.moemail import normalize_proxy_config

    line = (raw or "").strip()
    if not line:
        raise ValueError("empty proxy line")
    # Expand host:port:user:pass shorthand first.
    expanded = _hostport_userpass(line) or line
    cfg = normalize_proxy_config(
        expanded,
        username=username,
        password=password,
    )
    if not cfg or not cfg.get("proxy"):
        raise ValueError("invalid proxy")
    return str(cfg["proxy"])


def parse_proxy_pool(
    text: str | None = None,
    *,
    username: str | None = None,
    password: str | None = None,
    fallback_env: bool = True,
) -> list[str]:
    """Parse proxy pool text into a de-duplicated list of full proxy URLs.

    Invalid lines are skipped (not raised) so a large paste still yields the
    usable subset. Callers that need strict validation should use
    ``validate_proxy_pool``.
    """
    raw = (text if text is not None else "").strip()
    if not raw and fallback_env:
        raw = _env_proxy_text()
    lines = split_proxy_text(raw)
    if not lines:
        return []

    user = username
    pwd = password
    if user is None and fallback_env:
        user = _env_proxy_user() or None
    if pwd is None and fallback_env:
        pwd = _env_proxy_pass() or None
    # Empty string means "explicitly none"; None means "use default/env".
    user_s = None if user is None else str(user).strip()
    pass_s = None if pwd is None else str(pwd).strip()

    out: list[str] = []
    seen: set[str] = set()
    for line in lines:
        try:
            url = canonicalize_proxy_line(line, username=user_s, password=pass_s)
        except Exception:
            continue
        if url and url not in seen:
            seen.add(url)
            out.append(url)
    return out


def validate_proxy_pool(
    text: str | None = None,
    *,
    username: str | None = None,
    password: str | None = None,
    fallback_env: bool = False,
) -> dict[str, Any]:
    """Validate every non-empty line; return ok/errors/proxies summary."""
    lines = split_proxy_text(text or "")
    if not lines and fallback_env:
        lines = split_proxy_text(_env_proxy_text())
    user = username
    pwd = password
    if user is None and fallback_env:
        user = _env_proxy_user() or None
    if pwd is None and fallback_env:
        pwd = _env_proxy_pass() or None
    user_s = None if user is None else str(user).strip()
    pass_s = None if pwd is None else str(pwd).strip()

    proxies: list[str] = []
    errors: list[dict[str, str]] = []
    for i, line in enumerate(lines, start=1):
        try:
            url = canonicalize_proxy_line(line, username=user_s, password=pass_s)
            proxies.append(url)
        except Exception as e:  # noqa: BLE001
            errors.append({"line": i, "raw": line[:200], "error": str(e)[:200]})
    return {
        "ok": not errors and bool(proxies),
        "count": len(proxies),
        "proxies": proxies,
        "errors": errors,
        "empty": not lines,
    }


def normalize_proxy_strategy(value: str | None) -> str:
    s = (value or "round_robin").strip().lower().replace("-", "_")
    if s in {"rr", "round", "roundrobin", "round_robin"}:
        return "round_robin"
    if s in {"rand", "random"}:
        return "random"
    if s in {"sticky", "first", "fixed"}:
        return "sticky"
    return "round_robin"


def pick_proxy(
    proxies: Iterable[str] | None,
    *,
    strategy: str | None = "round_robin",
    index: int | None = None,
) -> str | None:
    """Pick one proxy URL from a pool.

    - round_robin: global counter (thread-safe), or ``index`` when provided
    - random: uniform random
    - sticky: always first
    """
    pool = [str(p).strip() for p in (proxies or []) if str(p).strip()]
    if not pool:
        return None
    mode = normalize_proxy_strategy(strategy)
    if mode == "sticky" or len(pool) == 1:
        return pool[0]
    if mode == "random":
        return random.choice(pool)
    # round_robin
    if index is not None:
        return pool[int(index) % len(pool)]
    global _rr_index
    with _lock:
        i = _rr_index
        _rr_index = (i + 1) % (10**9)
    return pool[i % len(pool)]


def resolve_proxy_for_request(
    *,
    proxy: str | None = None,
    proxy_username: str | None = None,
    proxy_password: str | None = None,
    strategy: str | None = None,
    index: int | None = None,
    fallback_env: bool = True,
) -> str | None:
    """High-level: parse pool text + pick one URL for this job/request."""
    pool = parse_proxy_pool(
        proxy,
        username=proxy_username,
        password=proxy_password,
        fallback_env=fallback_env,
    )
    if not pool and fallback_env:
        # Prefer admin outbound / auto-discovered peer proxies.
        try:
            src = get_outbound_proxy_source() or {}
            if src.get("enabled", True):
                pool = list(src.get("pool") or [])
                if not strategy:
                    strategy = src.get("proxy_strategy") or strategy
        except Exception:
            pool = []
    if not pool:
        return None
    strat = strategy
    if strat is None:
        strat = (
            os.getenv("GROK2API_PROXY_STRATEGY")
            or os.getenv("GROK2API_XAI_PROXY_STRATEGY")
            or "round_robin"
        )
    return pick_proxy(pool, strategy=strat, index=index)


def pool_summary(
    text: str | None = None,
    *,
    username: str | None = None,
    password: str | None = None,
    strategy: str | None = None,
    fallback_env: bool = True,
) -> dict[str, Any]:
    pool = parse_proxy_pool(
        text,
        username=username,
        password=password,
        fallback_env=fallback_env,
    )
    return {
        "enabled": bool(pool),
        "count": len(pool),
        "strategy": normalize_proxy_strategy(strategy),
        # Mask credentials in previews.
        "preview": [_mask_proxy_url(p) for p in pool[:8]],
    }


def _mask_proxy_url(url: str) -> str:
    try:
        p = urlparse(url)
        if not p.hostname:
            return url[:48]
        host = p.hostname
        if ":" in host and not host.startswith("["):
            host = f"[{host}]"
        port = f":{p.port}" if p.port else ""
        user = unquote(p.username) if p.username else ""
        if user:
            return f"{p.scheme}://{user}:***@{host}{port}"
        return f"{p.scheme}://{host}{port}"
    except Exception:
        return (url or "")[:48]


def httpx_proxy_arg(proxy_url: str | None) -> str | None:
    """httpx Client(proxy=...) expects a single URL string (or None)."""
    s = (proxy_url or "").strip()
    return s or None


def curl_proxies_arg(proxy_url: str | None) -> dict[str, str] | None:
    """curl_cffi / requests style proxies dict."""
    s = (proxy_url or "").strip()
    if not s:
        return None
    return {"http": s, "https": s}


# ── Outbound (account pool) proxy selection ─────────────────────────────────




def _auto_proxy_candidates() -> list[str]:
    """Best-effort proxies reachable from dockerized app.

    Order:
      1) env GROK2API_AUTO_PROXY / HTTP(S)_PROXY
      2) compose-peer service names (privoxy/warp-proxy)
      3) host-gateway common ports (when extra_hosts host.docker.internal is set)
    """
    out: list[str] = []
    for key in (
        "GROK2API_AUTO_PROXY",
        "HTTPS_PROXY",
        "HTTP_PROXY",
        "https_proxy",
        "http_proxy",
        "ALL_PROXY",
        "all_proxy",
    ):
        v = (os.getenv(key) or "").strip()
        if v:
            out.append(v)
    # Peer containers on shared docker networks.
    out.extend(
        [
            "http://privoxy:8118",
            "http://warp-proxy:1080",
            "socks5://warp-proxy:1080",
            "http://host.docker.internal:40080",
            "http://host.docker.internal:7890",
            "http://host.docker.internal:8118",
        ]
    )
    # de-dupe preserve order
    seen: set[str] = set()
    uniq: list[str] = []
    for u in out:
        if u and u not in seen:
            seen.add(u)
            uniq.append(u)
    return uniq


def first_working_proxy(candidates: list[str] | None = None, *, timeout: float = 1.2) -> str | None:
    """Return first candidate that accepts TCP connect (not full HTTP check)."""
    import socket
    from urllib.parse import urlparse

    for raw in candidates or _auto_proxy_candidates():
        try:
            url = canonicalize_proxy_line(raw)
        except Exception:
            url = (raw or "").strip()
        if not url:
            continue
        try:
            p = urlparse(url if "://" in url else ("http://" + url))
            host = p.hostname
            port = p.port or (1080 if (p.scheme or "").startswith("socks") else 8080)
            if not host:
                continue
            with socket.create_connection((host, int(port)), timeout=timeout):
                return url
        except Exception:
            continue
    return None

def get_outbound_proxy_source() -> dict[str, Any]:
    """Load effective outbound proxy pool text/auth/strategy.

    Preference order:
      1) settings_store.outbound_proxy_config (admin UI)
      2) env GROK2API_XAI_PROXY_POOL / GROK2API_XAI_PROXY
      3) registration_config.proxy (shared pool fallback)
    """
    global _outbound_proxy_cache_key, _outbound_proxy_cache_value
    text = ""
    user = ""
    password = ""
    strategy = "round_robin"
    enabled = True
    source = "none"

    try:
        from grok2api.admin.settings_store import get_outbound_proxy_config

        cfg = get_outbound_proxy_config(include_secrets=True) or {}
        if isinstance(cfg, dict):
            enabled = bool(cfg.get("enabled", True))
            text = str(cfg.get("proxy") or "").strip()
            user = str(cfg.get("proxy_username") or "").strip()
            password = str(cfg.get("proxy_password") or "").strip()
            strategy = normalize_proxy_strategy(
                str(cfg.get("proxy_strategy") or "round_robin")
            )
            if text:
                source = "settings"
    except Exception:
        pass

    if not text:
        env_text = _env_proxy_text()
        if env_text:
            text = env_text
            user = user or _env_proxy_user()
            password = password or _env_proxy_pass()
            strategy = normalize_proxy_strategy(
                os.getenv("GROK2API_XAI_PROXY_STRATEGY")
                or os.getenv("GROK2API_PROXY_STRATEGY")
                or strategy
            )
            source = "env"

    if not text:
        try:
            from grok2api.admin.settings_store import get_registration_config

            reg = get_registration_config(include_secrets=True) or {}
            if isinstance(reg, dict) and str(reg.get("proxy") or "").strip():
                text = str(reg.get("proxy") or "").strip()
                user = user or str(reg.get("proxy_username") or "").strip()
                password = password or str(reg.get("proxy_password") or "").strip()
                strategy = normalize_proxy_strategy(
                    str(reg.get("proxy_strategy") or strategy)
                )
                source = "registration"
        except Exception:
            pass

    cache_key = (bool(enabled), source, text, user, password, strategy)
    with _lock:
        if (
            _outbound_proxy_cache_key == cache_key
            and _outbound_proxy_cache_value is not None
        ):
            return _copy_outbound_proxy_source(_outbound_proxy_cache_value)

    if not enabled:
        out = {
            "enabled": False,
            "proxy": "",
            "proxy_username": user,
            "proxy_password": password,
            "proxy_strategy": strategy,
            "source": source,
            "pool": [],
        }
    else:
        pool = parse_proxy_pool(
            text,
            username=user or None,
            password=password or None,
            fallback_env=False,
        )
        out = {
            "enabled": bool(pool),
            "proxy": text,
            "proxy_username": user,
            "proxy_password": password,
            "proxy_strategy": strategy,
            "source": source if pool else "none",
            "pool": pool,
            "text": text,
        }
    if not out.get("pool"):
        # Auto-discover reachable local/peer proxies so registration/SSO
        # don't silently go direct when UI pool text is empty.
        auto = first_working_proxy()
        if auto:
            out["text"] = auto
            out["proxy"] = auto
            out["pool"] = [auto]
            out["source"] = "auto"
            out["enabled"] = True
            # Re-key cache so empty settings don't stick forever without auto.
            cache_key = (True, "auto", auto, user, password, strategy)
    with _lock:
        _outbound_proxy_cache_key = cache_key
        _outbound_proxy_cache_value = _copy_outbound_proxy_source(out)
    return _copy_outbound_proxy_source(out)


def pick_proxy_for_account(
    account_id: str | None = None,
    *,
    strategy: str | None = None,
    pool: list[str] | None = None,
) -> str | None:
    """Pick a proxy for an account-pool outbound request.

    Account traffic defaults to **stable sticky-by-account** so multi-turn
    affinity keeps the same egress IP. Explicit strategies:
      - sticky: always first proxy
      - random: random each call
      - round_robin: stable hash(account_id) when account_id given, else global RR
    """
    if pool is None:
        src = get_outbound_proxy_source()
        if not src.get("enabled"):
            return None
        pool = list(src.get("pool") or [])
        if strategy is None:
            strategy = str(src.get("proxy_strategy") or "round_robin")
    pool = [str(p).strip() for p in (pool or []) if str(p).strip()]
    if not pool:
        return None
    mode = normalize_proxy_strategy(strategy)
    if mode == "sticky" or len(pool) == 1:
        return pool[0]
    if mode == "random":
        return random.choice(pool)
    # round_robin / default: pin by account id when available
    aid = str(account_id or "").strip()
    if aid:
        # FNV-1a 32-bit — fast stable hash, no crypto dependency.
        h = 2166136261
        for ch in aid.encode("utf-8", errors="ignore"):
            h ^= ch
            h = (h * 16777619) & 0xFFFFFFFF
        return pool[h % len(pool)]
    return pick_proxy(pool, strategy="round_robin")


def outbound_pool_public_summary() -> dict[str, Any]:
    src = get_outbound_proxy_source()
    pool = list(src.get("pool") or [])
    return {
        "enabled": bool(src.get("enabled") and pool),
        "count": len(pool),
        "strategy": normalize_proxy_strategy(
            str(src.get("proxy_strategy") or "round_robin")
        ),
        "source": src.get("source") or "none",
        "preview": [_mask_proxy_url(p) for p in pool[:8]],
    }
