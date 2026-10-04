"""Push local Grok accounts into a CLIProxyAPI (CPA) instance.

CPA management API (router-for-me/CLIProxyAPI):

1. Auth: ``Authorization: Bearer <management-key>`` or ``X-Management-Key``
2. List: ``GET  /v0/management/auth-files``
3. Upload one file:
   - multipart: ``POST /v0/management/auth-files``  (any form file field, ``*.json``)
   - or raw body: ``POST /v0/management/auth-files?name=xai-email.json``

Config is stored under settings key ``cliproxyapi_config``.
"""

from __future__ import annotations

import json
import re
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

import grok2api.pool.accounts as accounts

_DEFAULT_TIMEOUT = 45.0
_USER_AGENT = "grokcli-2api-cliproxyapi-push/1.0"


def _default_config() -> dict[str, Any]:
    return {
        "enabled": False,
        "base_url": "",
        # Management key (plaintext stored in DB settings; redacted in public API)
        "management_key": "",
        # How many accounts to push in parallel
        "concurrency": 4,
        # After protocol registration, auto-push to CPA
        "auto_push_on_register": False,
        # Prefer type=xai for Grok Build / cli-chat-proxy tokens
        "auth_type": "xai",
        "base_upstream": "https://cli-chat-proxy.grok.com/v1",
        "notes_prefix": "grokcli-2api",
    }


def _normalize_config(raw: Any, *, include_secrets: bool = True) -> dict[str, Any]:
    base = _default_config()
    if not isinstance(raw, dict):
        out = dict(base)
        if not include_secrets:
            out["has_management_key"] = False
            out.pop("management_key", None)
        return out
    out = dict(base)
    out["enabled"] = bool(raw.get("enabled", False))
    out["base_url"] = str(
        raw.get("base_url") or raw.get("url") or ""
    ).strip().rstrip("/")
    key = raw.get("management_key")
    if key is None:
        key = raw.get("secret_key") or raw.get("api_key") or raw.get("password")
    if include_secrets:
        out["management_key"] = "" if key is None else str(key)
    else:
        out["management_key"] = ""
        out["has_management_key"] = bool(str(key or "").strip())
    try:
        conc = int(raw.get("concurrency") or 4)
    except (TypeError, ValueError):
        conc = 4
    out["concurrency"] = max(1, min(16, conc))
    auto_push = raw.get("auto_push_on_register")
    if auto_push is None:
        auto_push = raw.get("auto_import_on_register")
    out["auto_push_on_register"] = bool(auto_push)
    auth_type = str(raw.get("auth_type") or "xai").strip().lower() or "xai"
    if auth_type in ("x-ai", "x.ai", "grok"):
        auth_type = "xai"
    out["auth_type"] = auth_type
    out["base_upstream"] = str(
        raw.get("base_upstream")
        or raw.get("upstream_base_url")
        or "https://cli-chat-proxy.grok.com/v1"
    ).strip() or "https://cli-chat-proxy.grok.com/v1"
    out["notes_prefix"] = (
        str(raw.get("notes_prefix") or "grokcli-2api").strip() or "grokcli-2api"
    )
    return out


def get_cliproxyapi_config(*, include_secrets: bool = True) -> dict[str, Any]:
    try:
        from grok2api.admin.settings_store import _get_setting_value  # type: ignore

        raw = _get_setting_value("cliproxyapi_config", None)
    except Exception:
        raw = None
    return _normalize_config(raw, include_secrets=include_secrets)


def set_cliproxyapi_config(
    patch: dict[str, Any] | None, *, replace: bool = False
) -> dict[str, Any]:
    """Merge or replace cliproxyapi_config. Empty management_key keeps previous."""
    if patch is None:
        patch = {}
    if not isinstance(patch, dict):
        raise ValueError("cliproxyapi_config must be an object")
    current = get_cliproxyapi_config(include_secrets=True)
    if replace:
        merged = _normalize_config(patch, include_secrets=True)
        if not str(merged.get("management_key") or "").strip() and current.get(
            "management_key"
        ):
            merged["management_key"] = current["management_key"]
    else:
        merged = dict(current)
        for k, v in patch.items():
            if k in ("management_key", "secret_key", "password", "api_key") and (
                v is None or str(v).strip() == ""
            ):
                continue
            if k in ("secret_key", "password", "api_key"):
                merged["management_key"] = v
            else:
                merged[k] = v
        merged = _normalize_config(merged, include_secrets=True)
    try:
        from grok2api.admin.settings_store import _set_setting_value  # type: ignore

        _set_setting_value("cliproxyapi_config", merged)
    except Exception as e:  # noqa: BLE001
        raise RuntimeError(f"failed to persist cliproxyapi_config: {e}") from e
    return get_cliproxyapi_config(include_secrets=True)


def public_cliproxyapi_config() -> dict[str, Any]:
    cfg = get_cliproxyapi_config(include_secrets=True)
    return _normalize_config(cfg, include_secrets=False)


def _urljoin(base: str, path: str) -> str:
    base = (base or "").rstrip("/")
    if not path.startswith("/"):
        path = "/" + path
    return base + path


def _http(
    method: str,
    url: str,
    *,
    headers: dict[str, str] | None = None,
    data: bytes | None = None,
    timeout: float = _DEFAULT_TIMEOUT,
) -> tuple[int, bytes, dict[str, str]]:
    req = urllib.request.Request(url, data=data, method=method.upper())
    req.add_header("User-Agent", _USER_AGENT)
    for k, v in (headers or {}).items():
        if v is not None and str(v) != "":
            req.add_header(k, str(v))
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
            hdrs = {k.lower(): v for k, v in resp.headers.items()}
            return int(resp.status), body, hdrs
    except urllib.error.HTTPError as e:
        body = e.read() if hasattr(e, "read") else b""
        hdrs = {k.lower(): v for k, v in (e.headers or {}).items()}
        return int(e.code), body, hdrs
    except Exception as e:  # noqa: BLE001
        raise RuntimeError(f"CLIProxyAPI request failed: {e}") from e


def _auth_headers(cfg: dict[str, Any]) -> dict[str, str]:
    key = str(cfg.get("management_key") or "").strip()
    if not key:
        raise ValueError("未配置 CLIProxyAPI management key")
    return {
        "Authorization": f"Bearer {key}",
        "X-Management-Key": key,
    }


def _safe_email_filename(email: str) -> str:
    raw = (email or "").strip().lower()
    if not raw:
        return "unknown"
    safe = re.sub(r"[^a-z0-9@._+-]+", "_", raw)
    safe = safe.strip("._") or "unknown"
    return safe[:180]


def _record_filename(record: dict[str, Any]) -> str:
    email = str(record.get("email") or "").strip()
    safe = _safe_email_filename(email)
    lower = safe.lower()
    if lower.startswith("xai-") or lower.startswith("xai_") or lower.startswith("xai"):
        fname = safe
    else:
        # CPA convention from save_cliproxyapi_auth_record
        t = str(record.get("type") or "xai").strip().lower() or "xai"
        if t in ("xai", "grok", "x-ai", "x.ai"):
            fname = f"xai-{safe}"
        else:
            fname = f"{t}-{safe}"
    if not fname.endswith(".json"):
        fname = f"{fname}.json"
    return fname


def test_connection(cfg: dict[str, Any] | None = None) -> dict[str, Any]:
    """Smoke-test CPA management API: list auth-files."""
    cfg = _normalize_config(cfg or get_cliproxyapi_config(include_secrets=True))
    base = cfg.get("base_url") or ""
    if not base:
        return {"ok": False, "error": "请先填写 CLIProxyAPI URL（如 http://127.0.0.1:8317）"}
    try:
        headers = _auth_headers(cfg)
    except ValueError as e:
        return {"ok": False, "error": str(e)}
    url = _urljoin(base, "/v0/management/auth-files")
    try:
        code, body, _ = _http("GET", url, headers=headers, timeout=15.0)
    except Exception as e:  # noqa: BLE001
        return {"ok": False, "error": str(e), "url": url}
    text = body.decode("utf-8", errors="replace") if body else ""
    if code >= 400:
        return {
            "ok": False,
            "error": f"HTTP {code}: {text[:300]}",
            "status_code": code,
            "url": url,
        }
    try:
        data = json.loads(text) if text else {}
    except json.JSONDecodeError:
        data = {"raw": text[:500]}
    files = []
    if isinstance(data, dict):
        files = data.get("files") or data.get("auths") or []
    n = len(files) if isinstance(files, list) else 0
    return {
        "ok": True,
        "status_code": code,
        "url": url,
        "auth_files": n,
        "sample": (files[:5] if isinstance(files, list) else []),
        "message": f"连接成功，CPA 当前约 {n} 个 auth 文件",
    }


def list_auth_files(cfg: dict[str, Any] | None = None) -> dict[str, Any]:
    cfg = _normalize_config(cfg or get_cliproxyapi_config(include_secrets=True))
    base = cfg.get("base_url") or ""
    if not base:
        raise ValueError("未配置 CLIProxyAPI URL")
    headers = _auth_headers(cfg)
    url = _urljoin(base, "/v0/management/auth-files")
    code, body, _ = _http("GET", url, headers=headers, timeout=20.0)
    text = body.decode("utf-8", errors="replace") if body else ""
    if code >= 400:
        raise RuntimeError(f"list auth-files failed HTTP {code}: {text[:300]}")
    data = json.loads(text) if text else {}
    return data if isinstance(data, dict) else {"files": data}


def _upload_one(
    cfg: dict[str, Any],
    *,
    filename: str,
    record: dict[str, Any],
) -> dict[str, Any]:
    base = cfg.get("base_url") or ""
    headers = _auth_headers(cfg)
    raw = json.dumps(record, ensure_ascii=False).encode("utf-8")
    # Prefer raw body + name query (no multipart dependency issues)
    q = urllib.parse.urlencode({"name": filename})
    url = _urljoin(base, f"/v0/management/auth-files?{q}")
    hdrs = dict(headers)
    hdrs["Content-Type"] = "application/json"
    code, body, _ = _http("POST", url, headers=hdrs, data=raw, timeout=_DEFAULT_TIMEOUT)
    text = body.decode("utf-8", errors="replace") if body else ""
    if code >= 400:
        # Fallback multipart with field name "file"
        boundary = f"----g2a{int(time.time()*1000)}"
        parts: list[bytes] = []
        parts.append(
            (
                f"--{boundary}\r\n"
                f'Content-Disposition: form-data; name="file"; filename="{filename}"\r\n'
                f"Content-Type: application/json\r\n\r\n"
            ).encode("utf-8")
            + raw
            + b"\r\n"
        )
        parts.append(f"--{boundary}--\r\n".encode("utf-8"))
        mbody = b"".join(parts)
        murl = _urljoin(base, "/v0/management/auth-files")
        mhdrs = dict(headers)
        mhdrs["Content-Type"] = f"multipart/form-data; boundary={boundary}"
        code2, body2, _ = _http(
            "POST", murl, headers=mhdrs, data=mbody, timeout=_DEFAULT_TIMEOUT
        )
        text2 = body2.decode("utf-8", errors="replace") if body2 else ""
        if code2 >= 400:
            return {
                "ok": False,
                "filename": filename,
                "error": f"HTTP {code}: {text[:200]} | multipart HTTP {code2}: {text2[:200]}",
                "status_code": code2,
            }
        return {
            "ok": True,
            "filename": filename,
            "status_code": code2,
            "via": "multipart",
        }
    return {"ok": True, "filename": filename, "status_code": code, "via": "json-body"}


def _entry_to_cpa_record(
    entry: dict[str, Any],
    *,
    aid: str,
    cfg: dict[str, Any],
) -> dict[str, Any] | None:
    rec = accounts.build_cliproxyapi_export_record(entry, aid=aid)
    if not rec:
        return None
    # Force preferred type / base_url from settings for Grok Build channel
    pref = str(cfg.get("auth_type") or "xai").strip().lower() or "xai"
    if pref in ("xai", "grok", "x-ai", "x.ai"):
        rec["type"] = "xai"
    if cfg.get("base_upstream"):
        rec["base_url"] = str(cfg.get("base_upstream"))
    note_prefix = str(cfg.get("notes_prefix") or "grokcli-2api")
    rec["note"] = f"{note_prefix}:{aid}"
    return rec


def push_accounts(
    account_ids: list[str] | None = None,
    *,
    concurrency: int | None = None,
) -> dict[str, Any]:
    """Push local accounts into CPA auth dir via management API.

    account_ids=None → all accounts with tokens.
    """
    cfg = get_cliproxyapi_config(include_secrets=True)
    if not cfg.get("base_url"):
        raise ValueError("请先在设置页填写 CLIProxyAPI URL 与 management key")
    if not str(cfg.get("management_key") or "").strip():
        raise ValueError("请先在设置页填写 CLIProxyAPI management key")

    data = accounts.read_auth_map() or {}
    if account_ids is None:
        items = [(k, v) for k, v in data.items() if isinstance(v, dict)]
    else:
        wanted = {str(x).strip() for x in account_ids if str(x).strip()}
        items = []
        for k, v in data.items():
            if not isinstance(v, dict):
                continue
            if k in wanted or str(v.get("email") or "") in wanted:
                items.append((k, v))

    jobs: list[tuple[str, str, dict[str, Any]]] = []
    skipped = 0
    for aid, entry in items:
        rec = _entry_to_cpa_record(entry, aid=str(aid), cfg=cfg)
        if not rec:
            skipped += 1
            continue
        fname = _record_filename(rec)
        jobs.append((str(aid), fname, rec))

    if not jobs:
        return {
            "ok": True,
            "total": 0,
            "success": 0,
            "failed": 0,
            "skipped_no_token": skipped,
            "results": [],
            "message": "没有可推送的账号（缺少 access token）",
        }

    try:
        workers = int(concurrency if concurrency is not None else cfg.get("concurrency") or 4)
    except (TypeError, ValueError):
        workers = 4
    workers = max(1, min(16, workers, len(jobs)))

    results: list[dict[str, Any]] = []
    ok_n = 0
    fail_n = 0

    def _one(job: tuple[str, str, dict[str, Any]]) -> dict[str, Any]:
        aid, fname, rec = job
        try:
            r = _upload_one(cfg, filename=fname, record=rec)
            r["account_id"] = aid
            r["email"] = rec.get("email")
            return r
        except Exception as e:  # noqa: BLE001
            return {
                "ok": False,
                "account_id": aid,
                "filename": fname,
                "email": rec.get("email"),
                "error": str(e)[:300],
            }

    with ThreadPoolExecutor(max_workers=workers) as ex:
        futs = [ex.submit(_one, j) for j in jobs]
        for fut in as_completed(futs):
            r = fut.result()
            results.append(r)
            if r.get("ok"):
                ok_n += 1
            else:
                fail_n += 1

    return {
        "ok": fail_n == 0,
        "total": len(jobs),
        "success": ok_n,
        "failed": fail_n,
        "skipped_no_token": skipped,
        "concurrency": workers,
        "results": results,
        "message": f"CLIProxyAPI 导入完成：成功 {ok_n} / 失败 {fail_n} / 共 {len(jobs)}"
        + (f"（跳过无 token {skipped}）" if skipped else ""),
    }


def push_account_ids(account_ids: list[str], **kwargs: Any) -> dict[str, Any]:
    return push_accounts(account_ids, **kwargs)


def maybe_auto_push_registered_accounts(
    account_ids: list[str] | None,
    *,
    source: str = "register",
    cfg: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Push freshly registered local accounts into CPA when configured.

    Safe no-op when disabled / missing URL or key. Never raises.
    """
    ids = [str(x).strip() for x in (account_ids or []) if str(x).strip()]
    if not ids:
        return {"ok": True, "skipped": True, "reason": "no_accounts", "results": []}
    try:
        live = cfg or get_cliproxyapi_config(include_secrets=True)
    except Exception as e:  # noqa: BLE001
        return {
            "ok": False,
            "skipped": True,
            "reason": f"config_error: {e}",
            "results": [],
        }
    if not bool(live.get("auto_push_on_register")):
        return {
            "ok": True,
            "skipped": True,
            "reason": "auto_push_on_register_disabled",
            "results": [],
        }
    if not bool(live.get("enabled")):
        return {
            "ok": False,
            "skipped": True,
            "reason": "cliproxyapi_disabled",
            "results": [],
        }
    if not str(live.get("base_url") or "").strip():
        return {
            "ok": False,
            "skipped": True,
            "reason": "missing_base_url",
            "results": [],
        }
    if not str(live.get("management_key") or "").strip():
        return {
            "ok": False,
            "skipped": True,
            "reason": "missing_management_key",
            "results": [],
        }
    try:
        result = push_accounts(ids)
        result["source"] = source
        result["skipped"] = False
        try:
            print(
                f"[cliproxyapi] auto_push_on_register source={source} "
                f"total={result.get('total')} ok={result.get('success')} "
                f"fail={result.get('failed')}"
            )
        except Exception:
            pass
        return result
    except Exception as e:  # noqa: BLE001
        return {
            "ok": False,
            "skipped": False,
            "source": source,
            "error": str(e)[:300],
            "results": [],
        }
