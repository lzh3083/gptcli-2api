# -*- coding: utf-8 -*-
"""DataImpulse 动态住宅代理节点提取、出口探测与质量校验适配器。

参考文档: https://documenter.getpostman.com/view/7041120/2sAY4rGRZC (DataImpulse User API)
网关地址: gw.dataimpulse.com:777

核心功能:
  1. 调用 /api/list 接口动态提取指定国家（如 JP）的 Sticky 粘性住宅代理列表。
  2. 支持 HTTP Basic Auth 自动鉴权与 format 格式化。
  3. 探测节点真实出口（IP、归属国、ISP、hosting 机房属性），过滤机房/数据中心脏节点。
  4. 支持一号一独立 IP 调度，防止多次注册复用同一出口 IP 引起风控。
  5. 兼容 /api/stats 余额与用量查询、/api/rotate_ip 强制轮换会话。
"""
from __future__ import annotations

import argparse
import base64
import concurrent.futures
import json
import logging
import os
import re
import subprocess
import sys
import time
from pathlib import Path
from typing import Any, Callable

try:
    import requests
except ImportError:
    requests = None

logger = logging.getLogger(__name__)

DEFAULT_API = "https://gw.dataimpulse.com:777/api/list"
DEFAULT_STATS_API = "https://gw.dataimpulse.com:777/api/stats"
DEFAULT_ROTATE_API = "https://gw.dataimpulse.com:777/api/rotate_ip"

_PROXY_LINE_WITH_AUTH_RE = re.compile(
    r"^(?P<user>[^:]+):(?P<pass>[^@]+)@(?P<host>[A-Za-z0-9._-]+):(?P<port>\d{1,5})$"
)
_PROXY_LINE_HOST_PORT_RE = re.compile(
    r"^(?P<host>[A-Za-z0-9._-]+):(?P<port>\d{1,5})$"
)


class DataImpulseError(RuntimeError):
    """DataImpulse API 或节点提取相关异常。"""
    pass


def _log(message: str) -> None:
    print(f"[DataImpulse] {message}", flush=True)


def _basic_auth_header(user: str, password: str) -> dict[str, str]:
    token = base64.b64encode(f"{user}:{password}".encode("utf-8")).decode("ascii")
    return {"Authorization": f"Basic {token}"}


def parse_proxy_lines(
    text: str,
    default_user: str = "",
    default_password: str = "",
    default_protocol: str = "http",
) -> list[str]:
    """解析 API 返回的纯文本代理行，转换为完整的 URL 列表 (如 http://user:pass@host:port)。"""
    proxies: list[str] = []
    lines = str(text or "").replace("\r", "\n").split("\n")
    for raw in lines:
        line = raw.strip()
        if not line:
            continue
        # 常见错误响应拦截
        if "unauthorized" in line.lower() or "forbidden" in line.lower():
            raise DataImpulseError(f"认证失败，请检查账号密码 (服务端返回: {line})")
        if "error" in line.lower() or "limit" in line.lower():
            raise DataImpulseError(f"接口返回错误: {line}")

        proto = default_protocol.lower()
        if "://" in line:
            proto, line = line.split("://", 1)

        m_auth = _PROXY_LINE_WITH_AUTH_RE.match(line)
        if m_auth:
            entry = f"{proto}://{m_auth.group('user')}:{m_auth.group('pass')}@{m_auth.group('host')}:{m_auth.group('port')}"
            if entry not in proxies:
                proxies.append(entry)
            continue

        m_host = _PROXY_LINE_HOST_PORT_RE.match(line)
        if m_host:
            if default_user and default_password:
                entry = f"{proto}://{default_user}:{default_password}@{m_host.group('host')}:{m_host.group('port')}"
            else:
                entry = f"{proto}://{m_host.group('host')}:{m_host.group('port')}"
            if entry not in proxies:
                proxies.append(entry)
            continue

    return proxies


def build_extract_url(
    api_base: str = DEFAULT_API,
    countries: str = "JP",
    num: int = 5,
    session_ttl: int = 30,
    protocol: str = "http",
    proxy_type: str = "sticky",
    format_str: str = "login:password@hostname:port",
) -> str:
    """构造 /api/list 请求 URL 及 Query 参数。"""
    base = str(api_base or DEFAULT_API).strip() or DEFAULT_API
    params = [
        f"countries={countries.lower() if countries else 'jp'}",
        f"protocol={protocol.lower() if protocol else 'http'}",
        f"type={proxy_type.lower() if proxy_type else 'sticky'}",
        f"session_ttl={int(session_ttl) if session_ttl else 30}",
        f"format={format_str}",
        f"quantity={int(num) if num else 5}",
    ]
    sep = "&" if "?" in base else "?"
    return f"{base}{sep}{'&'.join(params)}"


def fetch_nodes(
    user: str,
    password: str,
    countries: str = "JP",
    num: int = 5,
    session_ttl: int = 30,
    protocol: str = "http",
    proxy_type: str = "sticky",
    api_base: str = DEFAULT_API,
    timeout: float = 25.0,
    attempts: int = 3,
    delay: float = 3.0,
    log: Callable[[str], None] = _log,
) -> list[str]:
    """通过 Basic Auth 从 DataImpulse API 提取指定国家的代理节点列表。"""
    user = str(user or "").strip()
    password = str(password or "").strip()
    if not user or not password:
        raise DataImpulseError("缺少 DataImpulse 账号密码 (user/password 必填)")

    url = build_extract_url(
        api_base=api_base,
        countries=countries,
        num=num,
        session_ttl=session_ttl,
        protocol=protocol,
        proxy_type=proxy_type,
    )
    headers = _basic_auth_header(user, password)
    headers["User-Agent"] = "DataImpulseClient/1.0"

    last_error: str | None = None
    for attempt in range(1, max(1, attempts) + 1):
        try:
            if requests is not None:
                resp = requests.get(url, headers=headers, timeout=timeout)
                if resp.status_code == 401 or resp.status_code == 403:
                    raise DataImpulseError(f"HTTP {resp.status_code} 认证失败，请检查 DataImpulse 账密")
                if resp.status_code != 200:
                    raise DataImpulseError(f"HTTP {resp.status_code}: {resp.text[:120]}")
                raw_text = resp.text
            else:
                # 兼容未安装 requests 时使用 curl
                auth_str = f"{user}:{password}"
                cmd = ["curl", "-s", "-u", auth_str, "-m", str(int(timeout)), url]
                out = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout + 5)
                raw_text = out.stdout or ""

            nodes = parse_proxy_lines(
                raw_text,
                default_user=user,
                default_password=password,
                default_protocol=protocol,
            )
            if nodes:
                log(f"第 {attempt} 次提取成功: 获取到 {len(nodes)} 个节点 ({countries}, type={proxy_type})")
                return nodes
            last_error = f"接口未返回可用节点内容: {raw_text[:120]}"
        except DataImpulseError:
            raise
        except Exception as exc:
            last_error = str(exc)[:120]

        if attempt < attempts:
            log(f"第 {attempt} 次提取失败 ({last_error})，{delay:.1f} 秒后重试...")
            time.sleep(delay)

    raise DataImpulseError(f"DataImpulse 节点提取失败: {last_error or '未知错误'}")


def probe_node(
    node: str,
    expect_country: str = "JP",
    timeout: float = 20.0,
) -> dict[str, Any]:
    """探测 DataImpulse 代理节点的真实出口 IP、归属地及机房属性。

    返回 dict:
      {
        "node": "http://user:pass@host:port",
        "ok": bool,
        "exit_ip": str,
        "country": str,
        "city": str,
        "isp": str,
        "hosting": bool, # 机房 IP 标识（OpenAI 封锁机房，住宅宽带必须为 False）
        "reason": str (若失败)
      }
    """
    node = str(node or "").strip()
    if not node.startswith("http://") and not node.startswith("socks5://") and not node.startswith("socks5h://"):
        node = f"http://{node}"

    target_url = (
        "http://ip-api.com/json/?fields=status,query,country,countryCode,"
        "regionName,city,isp,org,as,hosting,proxy,mobile"
    )

    resp_data: dict[str, Any] = {}
    try:
        if requests is not None:
            proxies = {"http": node, "https": node}
            r = requests.get(target_url, proxies=proxies, timeout=timeout)
            if r.status_code == 200:
                resp_data = r.json()
        else:
            cmd = ["curl", "-s", "-m", str(int(timeout)), "-x", node, target_url]
            out = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout + 5)
            if out.stdout:
                resp_data = json.loads(out.stdout)
    except Exception as exc:
        return {"node": node, "ok": False, "reason": f"连接/探测超时异常: {str(exc)[:60]}"}

    if not resp_data or resp_data.get("status") != "success":
        return {"node": node, "ok": False, "reason": "ip-api 探测无响应或状态非 success"}

    code = str(resp_data.get("countryCode") or "").upper()
    is_hosting = bool(resp_data.get("hosting"))
    exit_ip = str(resp_data.get("query") or "").strip()

    result: dict[str, Any] = {
        "node": node,
        "ok": True,
        "exit_ip": exit_ip,
        "country": code,
        "city": str(resp_data.get("city") or ""),
        "region": str(resp_data.get("regionName") or ""),
        "isp": str(resp_data.get("isp") or ""),
        "hosting": is_hosting,
        "mobile": bool(resp_data.get("mobile")),
    }

    expect = str(expect_country or "").strip().upper()
    if expect and expect not in ("AUTO", "*", "ANY") and code != expect:
        result["ok"] = False
        result["reason"] = f"国家不符: 期望 {expect} 实际 {code}"
    elif is_hosting:
        result["ok"] = False
        result["reason"] = f"机房/数据中心出口 (hosting=True, ISP={result['isp']})"

    return result


def verify_nodes(
    nodes: list[str],
    expect_country: str = "JP",
    workers: int = 5,
    timeout: float = 20.0,
    log: Callable[[str], None] = _log,
) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """并发校验一批节点，返回 (good_list, bad_list)。"""
    with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, workers)) as pool:
        probes = list(pool.map(lambda n: probe_node(n, expect_country=expect_country, timeout=timeout), nodes))

    good = [p for p in probes if p.get("ok")]
    bad = [p for p in probes if not p.get("ok")]
    log(f"并发探测完成: 可用 {len(good)} / 总计 {len(nodes)}")
    return good, bad


def get_plan_stats(
    user: str,
    password: str,
    api_url: str = DEFAULT_STATS_API,
    timeout: float = 15.0,
) -> dict[str, Any]:
    """查询 DataImpulse 账号流量统计 (/api/stats)。"""
    headers = _basic_auth_header(user, password)
    if requests is not None:
        r = requests.get(api_url, headers=headers, timeout=timeout)
        r.raise_for_status()
        return r.json()
    cmd = ["curl", "-s", "-u", f"{user}:{password}", "-m", str(int(timeout)), api_url]
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout + 5)
    return json.loads(out.stdout)


def rotate_ip(
    user: str,
    password: str,
    port: int | None = None,
    sessid: str | None = None,
    api_url: str = DEFAULT_ROTATE_API,
    timeout: float = 15.0,
) -> dict[str, Any]:
    """重置粘性会话并切换出口 IP (/api/rotate_ip)。"""
    headers = _basic_auth_header(user, password)
    params = {}
    if port:
        params["port"] = port
    if sessid:
        params["sessid"] = sessid
    if not params:
        raise DataImpulseError("rotate_ip 需提供 port 或 sessid 参数")

    if requests is not None:
        r = requests.get(api_url, headers=headers, params=params, timeout=timeout)
        r.raise_for_status()
        return r.json()

    query_str = "&".join(f"{k}={v}" for k, v in params.items())
    full_url = f"{api_url}?{query_str}"
    cmd = ["curl", "-s", "-u", f"{user}:{password}", "-m", str(int(timeout)), full_url]
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout + 5)
    return json.loads(out.stdout)


_USED_DATAIMPULSE_EXIT_IPS: set[str] = set()
_USED_DATAIMPULSE_NODES: set[str] = set()


def resolve_one_ip(
    user: str,
    password: str,
    country: str = "JP",
    session_ttl: int = 30,
    num: int = 5,
    api_base: str = DEFAULT_API,
    data_dir: str | Path | None = None,
    step_cb: Callable[[str, str], None] | None = None,
) -> str:
    """高层一号一IP分配入口：从 DataImpulse 拉取节点、验证出口、去重并返回可用的代理 URL。"""
    user = str(user or "").strip()
    password = str(password or "").strip()
    country = str(country or "JP").strip().upper()

    if not user or not password:
        raise DataImpulseError("未配置 DATAIMPULSE_USER 或 DATAIMPULSE_PASS，无法提取代理")

    if step_cb:
        step_cb("proxy_extract", f"正在从 DataImpulse 提取独立住宅代理 ({country}, ttl={session_ttl}min)...")

    # 持久化已用 IP 记录
    target_data_dir = Path(data_dir or os.environ.get("GROK2API_DATA_DIR") or "/app/data")
    if not target_data_dir.is_dir():
        target_data_dir = Path(__file__).resolve().parents[3] / "data"
    used_ip_file = target_data_dir / "used_dataimpulse_ips.json"

    try:
        if used_ip_file.is_file():
            persisted = json.loads(used_ip_file.read_text(encoding="utf-8") or "{}")
            for ip_item in persisted.get("exit_ips") or []:
                if ip_item:
                    _USED_DATAIMPULSE_EXIT_IPS.add(str(ip_item).strip())
            for nd_item in persisted.get("nodes") or []:
                if nd_item:
                    _USED_DATAIMPULSE_NODES.add(str(nd_item).strip())
    except Exception:
        pass

    last_err = ""
    fallback_candidate: tuple[str, str, dict[str, Any]] | None = None

    for round_idx in range(1, 4):
        try:
            nodes = fetch_nodes(
                user=user,
                password=password,
                countries=country,
                num=num,
                session_ttl=session_ttl,
                api_base=api_base,
                attempts=2,
                timeout=18.0,
                log=lambda m: logger.info(f"[DataImpulse] {m}"),
            )
            for node in nodes:
                probe = probe_node(node, expect_country=country, timeout=18.0)
                if not probe.get("ok"):
                    last_err = str(probe.get("reason") or "probe_failed")
                    continue
                if probe.get("hosting"):
                    last_err = f"机房 IP 属性 ({probe.get('exit_ip')})"
                    continue

                exit_ip = str(probe.get("exit_ip") or "").strip()
                if fallback_candidate is None:
                    fallback_candidate = (node, exit_ip, probe)

                if node in _USED_DATAIMPULSE_NODES:
                    last_err = f"节点端口/会话已使用 ({node})"
                    continue
                if exit_ip and exit_ip in _USED_DATAIMPULSE_EXIT_IPS:
                    last_err = f"出口 IP {exit_ip} 历史已使用"
                    continue

                # 命中全新独立住宅 IP
                _USED_DATAIMPULSE_NODES.add(node)
                if exit_ip:
                    _USED_DATAIMPULSE_EXIT_IPS.add(exit_ip)

                try:
                    used_ip_file.parent.mkdir(parents=True, exist_ok=True)
                    used_ip_file.write_text(
                        json.dumps(
                            {
                                "exit_ips": list(_USED_DATAIMPULSE_EXIT_IPS)[-200:],
                                "nodes": list(_USED_DATAIMPULSE_NODES)[-200:],
                            },
                            ensure_ascii=False,
                            indent=2,
                        )
                        + "\n",
                        encoding="utf-8",
                    )
                except Exception:
                    pass

                msg = (
                    f"已分配 DataImpulse 住宅节点: {node} "
                    f"(出口IP={exit_ip}, 国家={probe.get('country')}, ISP={probe.get('isp')})"
                )
                if step_cb:
                    step_cb("proxy_assigned", msg)
                else:
                    logger.info(f"[DataImpulse] {msg}")
                return node
        except Exception as exc:
            last_err = str(exc)
            logger.warning(f"[DataImpulse] 第 {round_idx} 轮提取异常: {exc}")
        time.sleep(1.5)

    if fallback_candidate is not None:
        node, exit_ip, probe = fallback_candidate
        _USED_DATAIMPULSE_NODES.add(node)
        msg = (
            f"已分配 DataImpulse 住宅节点(池内复用): {node} "
            f"(出口IP={exit_ip}, 国家={probe.get('country')}, ISP={probe.get('isp')})"
        )
        if step_cb:
            step_cb("proxy_assigned", msg)
        else:
            logger.info(f"[DataImpulse] {msg}")
        return node

    raise RuntimeError(f"无法从 DataImpulse 获取可用的独立住宅代理节点 (country={country}): {last_err}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="DataImpulse 住宅代理命令行调试工具")
    parser.add_argument("--user", default=os.environ.get("DATAIMPULSE_USER", ""), help="DataImpulse 用户名")
    parser.add_argument("--password", default=os.environ.get("DATAIMPULSE_PASS", ""), help="DataImpulse 密码")
    parser.add_argument("--country", default="JP", help="指定国家代码 (默认 JP)")
    parser.add_argument("--num", type=int, default=3, help="提取数量")
    parser.add_argument("--ttl", type=int, default=30, help="会话粘性分钟数 (默认 30)")
    parser.add_argument("--stats", action="store_true", help="查询流量统计")
    parser.add_argument("--probe", action="store_true", help="提取并测试真实出口 IP")
    args = parser.parse_args()

    if not args.user or not args.password:
        print("错误: 请通过 --user / --password 或环境变量 DATAIMPULSE_USER / DATAIMPULSE_PASS 提供账密")
        sys.exit(1)

    if args.stats:
        print(">>> 正在查询流量统计...")
        stats = get_plan_stats(args.user, args.password)
        print(json.dumps(stats, indent=2, ensure_ascii=False))
        sys.exit(0)

    print(f">>> 正在从 DataImpulse 提取代理 (country={args.country}, num={args.num})...")
    nodes_list = fetch_nodes(
        user=args.user,
        password=args.password,
        countries=args.country,
        num=args.num,
        session_ttl=args.ttl,
    )
    for idx, nd in enumerate(nodes_list, 1):
        print(f"[{idx}] {nd}")

    if args.probe:
        print("\n>>> 开始并发验证节点出口质量...")
        good_nodes, bad_nodes = verify_nodes(nodes_list, expect_country=args.country)
        print(f"有效可用住宅节点 ({len(good_nodes)}):")
        for g in good_nodes:
            print(f"  - {g['node']} -> 出口={g['exit_ip']} 国家={g['country']} ISP={g['isp']} hosting={g['hosting']}")
        if bad_nodes:
            print(f"异常/不可用节点 ({len(bad_nodes)}):")
            for b in bad_nodes:
                print(f"  - {b['node']} -> 原因={b.get('reason')}")
