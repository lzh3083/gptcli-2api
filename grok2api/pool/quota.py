"""Fetch per-account usage / billing quota from cli-chat-proxy.

Upstream endpoints (Grok session token):
  GET /v1/billing  — monthly limit, used, on-demand cap, period, history
  GET /v1/user     — profile, grok code access flags

When quota is exhausted, the account is auto-disabled in the rotation pool
so subsequent requests skip it.
"""

from __future__ import annotations

import re
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

import httpx

from grok2api.pool.auth import GrokCredentials, list_live_credentials, load_credentials_by_id, upstream_headers
from grok2api.config import CLI_VERSION, DEFAULT_MODEL, UPSTREAM_BASE

# Upstream returns amounts as {"val": number}. Unit is USD (often 0 on free/promo).
_QUOTA_TIMEOUT = 20.0


def _compact_http_body(status_code: int, body: str, *, source: str = "upstream") -> str:
    """Return a short operator-safe summary for non-JSON upstream bodies."""
    text = (body or "").strip()
    low = text.lower()
    if low.startswith("<!doctype html") or low.startswith("<html") or "<html" in low[:240]:
        if "cloudflare" in low or "/cdn-cgi/" in low or "cf-error" in low:
            return f"{source} HTTP {status_code}: Cloudflare/HTML challenge; lower concurrency or check outbound proxy"
        return f"{source} HTTP {status_code}: HTML error page"
    return f"{source} HTTP {status_code}: {text[:200]}"


# Hard quota / credit exhaustion signals from upstream error bodies.
# (Pure rate-limit 429 alone is temporary cooldown — not permanent disable.)
_QUOTA_ERROR_RE = re.compile(
    r"("
    r"usage[_ -]?limit[_ -]?reached|"
    r"usage[_ -]?pool[_ -]?exhausted|"
    r"quota[_ -]?exceeded|"
    r"quota\s+exceeded|"
    r"run\s+out\s+of\s+credits|"
    r"out\s+of\s+credits|"
    r"spending[-_ ]?limit|"
    r"personal-team-blocked|"
    r"need\s+a\s+grok\s+subscription|"
    r"monthly\s+limit|"
    r"no\s+credits|"
    r"insufficient\s+credits|"
    r"billing\s+limit|"
    r"usage\s+limit"
    r")",
    re.IGNORECASE,
)


def _headers(token: str) -> dict[str, str]:
    # Reuse CLI client headers; model override not needed for billing/user.
    h = upstream_headers(token, DEFAULT_MODEL)
    h["Accept"] = "application/json"
    return h


def _money(obj: Any) -> float | None:
    if obj is None:
        return None
    if isinstance(obj, (int, float)):
        return float(obj)
    if isinstance(obj, dict) and "val" in obj:
        try:
            return float(obj["val"])
        except (TypeError, ValueError):
            return None
    return None


def _fmt_usd(v: float | None) -> str | None:
    if v is None:
        return None
    if abs(v) < 0.005:
        return "$0.00"
    if abs(v) >= 100:
        return f"${v:,.2f}"
    return f"${v:.2f}"


def normalize_billing(raw: dict[str, Any] | None) -> dict[str, Any]:
    """Flatten cli-chat-proxy /v1/billing payload into a stable shape."""
    if not isinstance(raw, dict):
        return {"ok": False, "error": "empty billing response"}

    cfg = raw.get("config") if isinstance(raw.get("config"), dict) else raw
    monthly_limit = _money(cfg.get("monthlyLimit") or cfg.get("monthly_limit"))
    used = _money(cfg.get("used"))
    on_demand_cap = _money(cfg.get("onDemandCap") or cfg.get("on_demand_cap"))
    prepaid = _money(cfg.get("prepaidBalance") or cfg.get("prepaid_balance"))
    on_demand_used = _money(cfg.get("onDemandUsed") or cfg.get("on_demand_used"))

    remaining: float | None = None
    if monthly_limit is not None and used is not None:
        remaining = max(0.0, monthly_limit - used)

    usage_pct: float | None = None
    if monthly_limit and monthly_limit > 0 and used is not None:
        usage_pct = round(100.0 * used / monthly_limit, 2)

    history: list[dict[str, Any]] = []
    for item in cfg.get("history") or []:
        if not isinstance(item, dict):
            continue
        cycle = item.get("billingCycle") or item.get("billing_cycle") or {}
        history.append(
            {
                "year": (cycle or {}).get("year"),
                "month": (cycle or {}).get("month"),
                "included_used": _money(item.get("includedUsed") or item.get("included_used")),
                "on_demand_used": _money(item.get("onDemandUsed") or item.get("on_demand_used")),
                "total_used": _money(item.get("totalUsed") or item.get("total_used")),
            }
        )

    unlimited = bool(
        (monthly_limit is None or monthly_limit == 0)
        and (on_demand_cap is None or on_demand_cap == 0)
    )

    exhausted, exhaust_reason = _detect_billing_exhausted(
        monthly_limit=monthly_limit,
        used=used,
        remaining=remaining,
        on_demand_cap=on_demand_cap,
        on_demand_used=on_demand_used,
        unlimited=unlimited,
    )

    return {
        "ok": True,
        "monthly_limit": monthly_limit,
        "used": used,
        "remaining": remaining,
        "on_demand_cap": on_demand_cap,
        "on_demand_used": on_demand_used,
        "prepaid_balance": prepaid,
        "usage_percent": usage_pct,
        "unlimited_or_free": unlimited,
        "exhausted": exhausted,
        "exhaust_reason": exhaust_reason,
        "billing_period_start": cfg.get("billingPeriodStart") or cfg.get("billing_period_start"),
        "billing_period_end": cfg.get("billingPeriodEnd") or cfg.get("billing_period_end"),
        "history": history,
        "display": {
            "monthly_limit": _fmt_usd(monthly_limit),
            "used": _fmt_usd(used),
            "remaining": _fmt_usd(remaining),
            "on_demand_cap": _fmt_usd(on_demand_cap),
            "prepaid_balance": _fmt_usd(prepaid),
            "summary": _summary_text(
                monthly_limit=monthly_limit,
                used=used,
                remaining=remaining,
                unlimited=unlimited,
                exhausted=exhausted,
                usage_pct=usage_pct,
            ),
        },
        "raw": raw,
    }


def _detect_billing_exhausted(
    *,
    monthly_limit: float | None,
    used: float | None,
    remaining: float | None,
    on_demand_cap: float | None,
    on_demand_used: float | None,
    unlimited: bool,
) -> tuple[bool, str | None]:
    """Return (exhausted, reason) from billing numbers."""
    if unlimited:
        return False, None

    # Included monthly budget fully consumed
    if monthly_limit is not None and monthly_limit > 0 and used is not None:
        if used >= monthly_limit or (remaining is not None and remaining <= 0):
            # On-demand may still allow spend
            if on_demand_cap is not None and on_demand_cap > 0:
                od_used = on_demand_used or 0.0
                if od_used >= on_demand_cap:
                    return True, "月限额与按需额度均已用尽"
                # monthly included gone but on-demand remains — not fully exhausted
                return False, None
            return True, f"月限额已用尽（{_fmt_usd(used)} / {_fmt_usd(monthly_limit)}）"

    if on_demand_cap is not None and on_demand_cap > 0 and on_demand_used is not None:
        if on_demand_used >= on_demand_cap and (
            monthly_limit is None or monthly_limit <= 0 or (used is not None and used >= (monthly_limit or 0))
        ):
            return True, f"按需额度已用尽（{_fmt_usd(on_demand_used)} / {_fmt_usd(on_demand_cap)}）"

    return False, None


def is_quota_error_message(error: str | None, status_code: int | None = None) -> bool:
    """True if upstream error indicates hard quota/credit exhaustion.

    Rolling free-usage 429s (subscription:free-usage-exhausted) are temporary —
    they must NOT permanently disable the account. Those are soft-blocked per
    model by model_health instead so sub2api can keep scheduling.
    """
    text = (error or "").strip()
    if not text:
        return False
    low = text.lower()
    # Temporary free-tier windows / pure rate limits — never permanent disable.
    if (
        "free-usage-exhausted" in low
        or "free usage" in low
        or "usage resets over a rolling" in low
        or status_code == 429
    ):
        return False
    if _QUOTA_ERROR_RE.search(text):
        return True
    # 403 + spending/subscription style codes often mean no credits
    if status_code == 403 and any(
        k in low
        for k in ("credit", "subscription", "billing", "spending", "limit", "quota")
    ):
        # "need a grok subscription" / permanent block style only
        if "free-usage" in low or "rate limit" in low:
            return False
        return True
    return False


def _summary_text(
    *,
    monthly_limit: float | None,
    used: float | None,
    remaining: float | None,
    unlimited: bool,
    exhausted: bool,
    usage_pct: float | None,
) -> str:
    if exhausted:
        base = "额度已耗尽"
        if used is not None and monthly_limit is not None:
            return f"{base}（{_fmt_usd(used)} / {_fmt_usd(monthly_limit)}）"
        return base
    if unlimited:
        return "免费/促销（未设月限额）"
    parts = []
    if used is not None and monthly_limit is not None:
        parts.append(f"已用 {_fmt_usd(used)} / {_fmt_usd(monthly_limit)}")
    elif used is not None:
        parts.append(f"已用 {_fmt_usd(used)}")
    if remaining is not None and monthly_limit and monthly_limit > 0:
        parts.append(f"剩余 {_fmt_usd(remaining)}")
    if usage_pct is not None:
        parts.append(f"{usage_pct}%")
    return " · ".join(parts) if parts else "—"


def apply_exhaustion_to_pool(
    account_id: str | None,
    *,
    reason: str,
    source: str = "billing",
) -> dict[str, Any] | None:
    """Disable account in rotation pool when quota is gone."""
    if not account_id:
        return None
    try:
        import grok2api.pool.account_pool as account_pool

        return account_pool.disable_for_quota(account_id, reason=reason, source=source)
    except Exception as e:  # noqa: BLE001
        return {"id": account_id, "error": str(e)}


def maybe_disable_from_quota_result(result: dict[str, Any]) -> dict[str, Any]:
    """If quota result says exhausted, disable the account and annotate result.

    Always persist last_quota into DB/settings for admin UI cache.
    When billing shows healthy again, re-enable accounts previously removed for quota.
    """
    if not result.get("ok"):
        # still cache failed query timestamp/error so UI can show "上次失败"
        account_id = result.get("account_id")
        if account_id:
            try:
                import grok2api.pool.account_pool as account_pool
                account_pool.save_quota_snapshot(account_id, result)
            except Exception:
                pass
        return result
    account_id = result.get("account_id")
    if result.get("exhausted"):
        reason = result.get("exhaust_reason") or "额度已耗尽"
        disabled = apply_exhaustion_to_pool(
            account_id, reason=reason, source="billing"
        )
        result["auto_disabled"] = True
        result["auto_reenabled"] = False
        result["disabled_record"] = disabled
        result["display"] = dict(result.get("display") or {})
        result["display"]["summary"] = f"额度耗尽 · 已移出轮询（{reason}）"
        # disable_for_quota already writes last_quota; keep explicit snapshot too
        if account_id:
            try:
                import grok2api.pool.account_pool as account_pool
                account_pool.save_quota_snapshot(account_id, result)
            except Exception:
                pass
    else:
        result["auto_disabled"] = False
        reenabled = None
        if account_id:
            try:
                import grok2api.pool.account_pool as account_pool

                # save_quota_snapshot re-enters pool when healthy; also explicit path
                # for older rows that only had enabled=false without last_quota.
                account_pool.save_quota_snapshot(account_id, result)
                meta = None
                try:
                    meta = account_pool.get_account_pool_meta(account_id)
                except Exception:
                    meta = None
                if isinstance(meta, dict) and (
                    meta.get("disabled_for_quota") or meta.get("enabled") is False
                ):
                    src = str(meta.get("quota_source") or meta.get("disabled_source") or "")
                    if src in ("", "billing", "upstream_error", "model_health", "quota") or bool(
                        meta.get("disabled_for_quota")
                    ):
                        reenabled = account_pool.reenable_for_quota(
                            account_id,
                            reason="billing 查询显示额度可用",
                            source="billing",
                        )
            except Exception:
                reenabled = None
        result["auto_reenabled"] = bool(reenabled)
        result["reenabled_record"] = reenabled
        if reenabled:
            result["display"] = dict(result.get("display") or {})
            summary = (result.get("display") or {}).get("summary") or "额度可用"
            result["display"]["summary"] = f"{summary} · 已重新入池"
    return result


def handle_upstream_error_for_quota(
    account_id: str | None,
    *,
    error: str = "",
    status_code: int | None = None,
) -> dict[str, Any] | None:
    """
    On upstream failure: if message indicates quota exhaustion,
    permanently disable the account from rotation (not just cooldown).
    """
    if not account_id or not is_quota_error_message(error, status_code):
        return None
    reason = f"上游额度错误 (HTTP {status_code}): {(error or '')[:120]}"
    return apply_exhaustion_to_pool(account_id, reason=reason, source="upstream_error")


def normalize_user(raw: dict[str, Any] | None) -> dict[str, Any]:
    if not isinstance(raw, dict):
        return {}
    return {
        "user_id": raw.get("userId") or raw.get("principalId") or raw.get("user_id"),
        "email": raw.get("email"),
        "first_name": raw.get("firstName") or raw.get("first_name"),
        "last_name": raw.get("lastName") or raw.get("last_name"),
        "has_grok_code_access": raw.get("hasGrokCodeAccess"),
        "user_blocked_reason": raw.get("userBlockedReason"),
        "team_id": raw.get("teamId"),
        "team_name": raw.get("teamName"),
        "organization_id": raw.get("organizationId"),
        "organization_name": raw.get("organizationName"),
        "principal_type": raw.get("principalType"),
    }


def fetch_quota_for_creds(creds: GrokCredentials) -> dict[str, Any]:
    """Synchronous quota fetch for one account."""
    base = {
        "account_id": creds.auth_key,
        "email": creds.email,
        "user_id": creds.user_id,
        "fetched_at": time.time(),
    }
    headers = _headers(creds.token)
    billing_url = f"{UPSTREAM_BASE}/billing"
    user_url = f"{UPSTREAM_BASE}/user"
    try:
        with httpx.Client(timeout=_QUOTA_TIMEOUT) as client:
            br = client.get(billing_url, headers=headers)
            ur = client.get(user_url, headers=headers)
    except httpx.HTTPError as e:
        return {**base, "ok": False, "error": f"network: {e}"}

    billing_raw = None
    user_raw = None
    try:
        if br.status_code == 200:
            billing_raw = br.json()
        else:
            return {
                **base,
                "ok": False,
                "error": _compact_http_body(br.status_code, br.text or "", source="billing"),
                "status_code": br.status_code,
            }
    except Exception as e:  # noqa: BLE001
        return {**base, "ok": False, "error": f"billing parse: {e}"}

    try:
        if ur.status_code == 200:
            user_raw = ur.json()
    except Exception:
        user_raw = None

    bill = normalize_billing(billing_raw if isinstance(billing_raw, dict) else None)
    user = normalize_user(user_raw if isinstance(user_raw, dict) else None)
    result = {
        **base,
        **bill,
        "user": user,
        "cli_version": CLI_VERSION,
        "upstream": UPSTREAM_BASE,
    }
    return maybe_disable_from_quota_result(result)


async def fetch_quota_for_creds_async(creds: GrokCredentials) -> dict[str, Any]:
    base = {
        "account_id": creds.auth_key,
        "email": creds.email,
        "user_id": creds.user_id,
        "fetched_at": time.time(),
    }
    headers = _headers(creds.token)
    billing_url = f"{UPSTREAM_BASE}/billing"
    user_url = f"{UPSTREAM_BASE}/user"
    try:
        async with httpx.AsyncClient(timeout=_QUOTA_TIMEOUT) as client:
            br = await client.get(billing_url, headers=headers)
            ur = await client.get(user_url, headers=headers)
    except httpx.HTTPError as e:
        return {**base, "ok": False, "error": f"network: {e}"}

    try:
        if br.status_code != 200:
            return {
                **base,
                "ok": False,
                "error": _compact_http_body(br.status_code, br.text or "", source="billing"),
                "status_code": br.status_code,
            }
        billing_raw = br.json()
    except Exception as e:  # noqa: BLE001
        return {**base, "ok": False, "error": f"billing parse: {e}"}

    user_raw = None
    try:
        if ur.status_code == 200:
            user_raw = ur.json()
    except Exception:
        user_raw = None

    bill = normalize_billing(billing_raw if isinstance(billing_raw, dict) else None)
    user = normalize_user(user_raw if isinstance(user_raw, dict) else None)
    result = {
        **base,
        **bill,
        "user": user,
        "cli_version": CLI_VERSION,
        "upstream": UPSTREAM_BASE,
    }
    return maybe_disable_from_quota_result(result)


def fetch_quota_by_account_id(account_id: str) -> dict[str, Any]:
    creds = load_credentials_by_id(account_id)
    return fetch_quota_for_creds(creds)


async def fetch_all_quotas(
    *,
    include_expired: bool = False,
    max_workers: int | None = None,
) -> dict[str, Any]:
    """Query quota for every live account concurrently; auto-disable exhausted ones."""
    try:
        from grok2api.config import QUOTA_WORKERS
    except Exception:
        QUOTA_WORKERS = 4
    if max_workers is None:
        max_workers = QUOTA_WORKERS

    # auto_refresh=False: avoid OIDC fan-out while also hitting billing endpoints
    accounts = list_live_credentials(include_expired=include_expired, auto_refresh=False)
    # de-dupe by user_id
    seen: set[str] = set()
    unique: list[GrokCredentials] = []
    for c in accounts:
        uid = c.user_id or c.auth_key or ""
        if uid in seen:
            continue
        seen.add(uid)
        unique.append(c)

    results: list[dict[str, Any]] = []

    def _fetch_one(creds: GrokCredentials) -> dict[str, Any]:
        return fetch_quota_for_creds(creds)

    workers = min(int(max_workers), max(1, len(unique))) if unique else 1
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="quota-") as ex:
        for fut in as_completed(ex.submit(_fetch_one, c) for c in unique):
            try:
                results.append(fut.result())
            except Exception as e:  # noqa: BLE001
                results.append({
                    "ok": False,
                    "error": str(e)[:300],
                    "fetched_at": time.time(),
                })

    # Mark results that belong to disabled rotation accounts so UI/stats can
    # exclude them from available-quota aggregates.
    disabled_ids: set[str] = set()
    try:
        import grok2api.pool.account_pool as account_pool

        for a in account_pool.list_pool_accounts():
            if a.get("enabled") is False or a.get("disabled_for_quota"):
                if a.get("id"):
                    disabled_ids.add(str(a["id"]))
    except Exception:
        disabled_ids = set()

    for r in results:
        aid = r.get("account_id")
        r["pool_disabled"] = bool(aid and str(aid) in disabled_ids)

    ok_count = sum(1 for r in results if r.get("ok"))
    exhausted_count = sum(1 for r in results if r.get("exhausted"))
    auto_disabled = sum(1 for r in results if r.get("auto_disabled"))
    pool_disabled_count = sum(1 for r in results if r.get("pool_disabled"))
    # Available totals exclude manually/quota-disabled accounts.
    active_ok = [
        r
        for r in results
        if r.get("ok") and not r.get("pool_disabled") and not r.get("exhausted")
    ]
    total_used = sum(
        float(r["used"]) for r in active_ok if r.get("used") is not None
    )
    total_limit = sum(
        float(r["monthly_limit"])
        for r in active_ok
        if r.get("monthly_limit") is not None
    )
    total_remaining = sum(
        float(r["remaining"])
        for r in active_ok
        if r.get("remaining") is not None
    )
    return {
        "ok": True,
        "fetched_at": time.time(),
        "count": len(results),
        "ok_count": ok_count,
        "exhausted_count": exhausted_count,
        "auto_disabled_count": auto_disabled,
        "pool_disabled_count": pool_disabled_count,
        "active_ok_count": len(active_ok),
        "total_used": total_used,
        "total_monthly_limit": total_limit,
        "total_remaining": total_remaining,
        "accounts": results,
    }


def list_cached_quotas(*, include_expired: bool = True) -> dict[str, Any]:
    """Return last_quota snapshots from DB/settings without upstream calls."""
    try:
        import grok2api.pool.account_pool as account_pool
        rows = account_pool.list_pool_accounts()
    except Exception as e:  # noqa: BLE001
        return {"ok": False, "error": str(e), "results": [], "count": 0}

    results: list[dict[str, Any]] = []
    for a in rows:
        if not include_expired and a.get("expired"):
            continue
        q = a.get("last_quota")
        if not isinstance(q, dict) or not q:
            continue
        item = dict(q)
        item.setdefault("account_id", a.get("id"))
        item.setdefault("email", a.get("email"))
        item.setdefault("user_id", a.get("user_id"))
        item.setdefault("ok", True if item.get("error") in (None, "") else False)
        item["cached"] = True
        item["pool_disabled"] = bool(a.get("disabled_for_quota") or a.get("enabled") is False)
        # normalize display
        if not item.get("display"):
            summary = item.get("summary")
            if summary:
                item["display"] = {"summary": summary}
        results.append(item)

    exhausted = sum(1 for r in results if r.get("exhausted") or r.get("auto_disabled"))
    ok_n = sum(1 for r in results if r.get("ok") and not r.get("exhausted"))
    return {
        "ok": True,
        "cached": True,
        "count": len(results),
        "ok_count": ok_n,
        "exhausted_count": exhausted,
        "results": results,
    }
