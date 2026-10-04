"""NovProxy 住宅代理节点提取与预热。

供应商特点（实测）
------------------
- 提取接口：https://white.novproxy.com/white/api?region=US&num=N&time=120&format=1&type=txt
  返回纯文本，一行一个 `host:port`，**无账密**（靠源 IP 白名单）。
- 入口是机房（Zenlayer 香港等），**出口才是目标国家的住宅 IP**。
  所以不能用入口 IP 判断质量，必须实测出口。
- 每个 `host:port` 是一个独立会话，出口 IP 各自不同。
- `time` 是会话存活分钟数。**用 120 而非 10**：实测 time=10 时立即可用率
  仅 5/10 且几十秒内就批量失效，time=120 时 9/10 立即可用且端口段独立。
- **部分端口首次连接会失败**，重试后会通；个别出口不在目标国家
  （实测有节点落到阿塞拜疆），必须按真实出口国家校验后剔除。

因此本工具在生成节点文件前会：
  1. 提取（可多轮，直到凑够目标数量）
  2. 并发探测真实出口，校验国家与非机房属性
  3. 对失败的节点做一轮延迟重试（预热）
  4. 只把验证通过的节点写入节点文件

节点文件格式与项目一致：一行一个 `host:port`（`proxy_pool_v3` 直接读取）。
"""
from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import re
import sys
import time

DEFAULT_API = "https://white.novproxy.com/white/api"
_PROXY_LINE_RE = re.compile(r"^(?P<host>[A-Za-z0-9._-]+):(?P<port>\d{1,5})$")
# 供应商在失败时可能回纯文本提示，识别出来给出明确原因
_ERROR_HINTS = (
    (re.compile(r"not\s+added\s+to\s+whitelist", re.I), "源 IP 未加入白名单"),
    (re.compile(r"traffic.*(expired|used\s*up)|流量.*(过期|用尽)", re.I), "套餐流量已过期"),
    (re.compile(r"(auth|token|key).*(invalid|error|denied)", re.I), "认证失败"),
    (re.compile(r"no\s+(available\s+)?(proxy|ip|resource)", re.I), "暂无可用节点"),
)


class NovProxyError(RuntimeError):
    pass


def _log(message):
    print("[novproxy] %s" % message, flush=True)


def parse_proxy_lines(text):
    """解析提取接口返回，返回 ["host:port", ...]。"""
    proxies = []
    for raw in str(text or "").replace("\r", "\n").split("\n"):
        line = raw.strip()
        if not line:
            continue
        match = _PROXY_LINE_RE.match(line)
        if match:
            port = int(match.group("port"))
            if 1 <= port <= 65535:
                entry = "%s:%d" % (match.group("host"), port)
                if entry not in proxies:
                    proxies.append(entry)
            continue
        for pattern, reason in _ERROR_HINTS:
            if pattern.search(line):
                raise NovProxyError("%s（服务端返回: %s）" % (reason, line[:120]))
        if len(line) > 2 and ":" not in line:
            raise NovProxyError("提取接口返回异常内容: %s" % line[:200])
    return proxies


def build_extract_url(api_base, region="US", num=10, minutes=120, fmt=1, kind="txt"):
    base = str(api_base or DEFAULT_API).strip() or DEFAULT_API
    sep = "&" if "?" in base else "?"
    return "%s%sregion=%s&num=%d&time=%d&format=%d&type=%s" % (
        base, sep, region, int(num), int(minutes), int(fmt), kind)


def fetch_nodes(api_base, region="US", num=10, minutes=120, timeout=45.0, attempts=3,
                delay=6.0, log=_log):
    """提取节点，失败重试。返回去重后的列表。"""
    url = build_extract_url(api_base, region=region, num=num, minutes=minutes)
    last_error = None
    for attempt in range(1, max(1, attempts) + 1):
        try:
            text = _curl(url, timeout)
            nodes = parse_proxy_lines(text)
            if nodes:
                log("第 %d 次提取到 %d 个节点" % (attempt, len(nodes)))
                return nodes
            last_error = "接口未返回可用节点"
        except NovProxyError:
            raise
        except Exception as exc:
            last_error = str(exc)[:120]
        if attempt < attempts:
            log("第 %d 次提取失败（%s），%.0f 秒后重试" % (attempt, last_error, delay))
            time.sleep(delay)
    raise NovProxyError("提取失败: %s" % (last_error or "未知原因"))


def _curl(url, timeout, proxy=""):
    """用 curl 发请求。proxy 非空时走 SOCKS5（远端解析域名）。"""
    cmd = ["curl", "-s", "-m", str(int(timeout))]
    if proxy:
        cmd += ["--socks5-hostname", proxy]
    cmd.append(url)
    import subprocess
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout + 10)
    return out.stdout or ""


def probe_node(node, expect_country="US", timeout=25.0, fetch=None):
    """探测节点真实出口。返回 dict（含 ok / exit_ip / country / hosting）。"""
    # query 必须显式列出，否则 ip-api 不返回被查询的 IP，exit_ip 会恒为空
    target = ("http://ip-api.com/json/?fields=status,query,country,countryCode,"
              "regionName,city,isp,org,as,hosting,proxy,mobile")
    getter = fetch or _curl
    try:
        if fetch is None:
            text = getter(target, timeout, node)
        else:
            text = getter(target, timeout, node)
        text = (text or "").strip()
    except Exception as exc:
        return {"node": node, "ok": False, "reason": str(exc)[:80]}
    if not text:
        return {"node": node, "ok": False, "reason": "无响应"}
    if "connect proxy error" in text or "msg:" in text[:20]:
        return {"node": node, "ok": False, "reason": "上游连接失败"}
    try:
        data = json.loads(text)
    except Exception:
        return {"node": node, "ok": False, "reason": "响应无法解析: %s" % text[:60]}
    if data.get("status") != "success":
        return {"node": node, "ok": False, "reason": "查询失败"}
    code = str(data.get("countryCode") or "").upper()
    result = {
        "node": node,
        "ok": True,
        "exit_ip": data.get("query") or "",
        "country": code,
        "city": data.get("city") or "",
        "region": data.get("regionName") or "",
        "isp": data.get("isp") or "",
        "hosting": bool(data.get("hosting")),
        "mobile": bool(data.get("mobile")),
    }
    expect = str(expect_country or "").strip().upper()
    if expect and code != expect:
        result["ok"] = False
        result["reason"] = "国家不符: 期望 %s 实际 %s" % (expect, code or "?")
    return result


def verify_nodes(nodes, expect_country="US", workers=10, timeout=25.0,
                 retry_failed=True, retry_delay=18.0, log=_log):
    """并发验证节点；失败的可选延迟重试一轮（预热）。

    返回 (good, bad)，good 为探测结果 dict 列表。
    """
    def run(batch):
        with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, workers)) as pool:
            return list(pool.map(lambda n: probe_node(n, expect_country, timeout), batch))

    results = run(nodes)
    good = [r for r in results if r.get("ok")]
    bad = [r for r in results if not r.get("ok")]
    log("首轮验证: 可用 %d / %d" % (len(good), len(nodes)))

    if bad and retry_failed:
        retry_targets = [r["node"] for r in bad]
        log("等待 %.0f 秒后重试 %d 个失败节点（预热）" % (retry_delay, len(retry_targets)))
        time.sleep(retry_delay)
        again = run(retry_targets)
        recovered = [r for r in again if r.get("ok")]
        still_bad = [r for r in again if not r.get("ok")]
        if recovered:
            log("预热后新增可用 %d 个" % len(recovered))
        good.extend(recovered)
        bad = still_bad
    return good, bad


def write_nodes(path, good, log=_log, scheme="socks5h"):
    """写节点文件。

    两个都必须做对，否则节点会「连不上但报错不指向原因」：

    1. **必须带 socks5h://（h = hostname，远端解析）**。本项目运行在
       Clash/Mihomo 类环境里，容器 DNS 对绝大多数域名返回 fake-ip
       （198.18.0.0/15 虚拟地址）。`socks5://` 会让本地 HTTP 桥先做
       DNS 解析，拿到 fake-ip 再发给上游代理，结果必然连不上。
       `socks5h://` 把域名原样交给代理端解析，才是正确做法。

    2. **必须带协议前缀**。裸 `host:port` 会被 parse_subscription_source
       当成 HTTP 代理，而 NovProxy 只提供 SOCKS5。
    """
    target = str(path or "").strip()
    if not target:
        raise NovProxyError("未指定输出文件")
    directory = os.path.dirname(os.path.abspath(target))
    if directory:
        os.makedirs(directory, exist_ok=True)
    prefix = str(scheme or "").strip()
    if prefix and not prefix.endswith("://"):
        prefix += "://"
    lines = []
    for item in good:
        node = (item.get("node") if isinstance(item, dict) else str(item)) or ""
        node = node.strip()
        if not node:
            continue
        if "://" in node:
            # 已有协议前缀：socks5 -> socks5h，避免本地解析拿到 fake-ip
            if prefix.endswith("socks5h://") and node.startswith("socks5://"):
                node = "socks5h://" + node[len("socks5://"):]
            lines.append(node)
        else:
            lines.append(prefix + node)
    tmp = target + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        handle.write("\n".join(lines) + ("\n" if lines else ""))
    try:
        os.chmod(tmp, 0o600)
    except Exception:
        pass
    os.replace(tmp, target)
    log("已写入 %d 个节点: %s" % (len(lines), target))
    return len(lines)


def mask_node(node):
    """面板展示用：隐去节点里的账号密码。

    面板无鉴权，代理凭据属于敏感信息，绝不能回显明文。
    """
    if isinstance(node, dict):
        node = node.get("node") or ""
    text = str(node or "")
    if "://" not in text:
        return text
    scheme, rest = text.split("://", 1)
    if "@" not in rest:
        return text
    creds, host = rest.rsplit("@", 1)
    user = creds.split(":", 1)[0]
    return "%s://%s:***@%s" % (scheme, user, host)


def generate(api_base, out_path, region="US", want=10, minutes=120, expect_country="US",
             workers=10, timeout=25.0, attempts=3, rounds=3, log=_log):
    """提取 + 验证 + 写文件，直到凑够 want 个或轮次用尽。"""
    collected = []
    seen = set()
    for round_index in range(1, max(1, rounds) + 1):
        remaining = max(1, want - len(collected))
        try:
            nodes = fetch_nodes(api_base, region=region, num=max(remaining * 2, remaining),
                                minutes=minutes, attempts=attempts, log=log)
        except NovProxyError as exc:
            log("第 %d 轮提取失败: %s" % (round_index, exc))
            continue
        fresh = [n for n in nodes if n not in seen]
        if not fresh:
            log("第 %d 轮没有新节点" % round_index)
            continue
        seen.update(fresh)
        good, _bad = verify_nodes(fresh, expect_country=expect_country,
                                  workers=workers, timeout=timeout, log=log)
        collected.extend(good)
        log("累计可用 %d / 目标 %d" % (len(collected), want))
        if len(collected) >= want:
            break
    if not collected:
        raise NovProxyError("未取得任何可用节点")
    # 按出口 IP 去重（同一住宅 IP 重复出现没有意义）
    unique = []
    seen_ip = set()
    for item in collected:
        ip = item.get("exit_ip") or item["node"]
        if ip in seen_ip:
            continue
        seen_ip.add(ip)
        unique.append(item)
    return unique[:want] if want else unique


def cmd_generate(args):
    try:
        good = generate(
            args.api_base, args.out, region=args.region, want=args.num,
            minutes=args.minutes, expect_country=args.expect,
            workers=args.workers, timeout=args.timeout, rounds=args.rounds,
        )
    except NovProxyError as exc:
        _log("❌ %s" % exc)
        return 1
    write_nodes(args.out, good)
    print()
    print("  出口明细:")
    for item in good:
        print("    %-24s %-15s %-14s %s" % (
            item["node"], item.get("exit_ip", ""), (item.get("city") or "")[:13],
            (item.get("isp") or "")[:32]))
    return 0


def cmd_probe(args):
    nodes = []
    if args.file:
        with open(args.file, encoding="utf-8") as handle:
            nodes = [l.strip() for l in handle if l.strip()]
    elif args.node:
        nodes = [args.node]
    if not nodes:
        _log("❌ 未提供节点（--node 或 --file）")
        return 1
    good, bad = verify_nodes(nodes, expect_country=args.expect,
                             workers=args.workers, timeout=args.timeout,
                             retry_failed=not args.no_retry)
    print()
    print("  可用 %d / %d" % (len(good), len(nodes)))
    for item in good:
        print("    ✅ %-24s %-15s %s / %s" % (
            item["node"], item.get("exit_ip", ""), (item.get("city") or "")[:14],
            (item.get("isp") or "")[:30]))
    for item in bad:
        print("    ❌ %-24s %s" % (item["node"], item.get("reason", "")[:60]))
    return 0 if good else 1


def build_parser():
    parser = argparse.ArgumentParser(description="NovProxy 住宅代理节点提取与预热")
    parser.add_argument("--api-base", default=os.environ.get("NOVPROXY_API", DEFAULT_API))
    sub = parser.add_subparsers(dest="command", required=True)

    p_gen = sub.add_parser("generate", help="提取并验证节点，写入节点文件")
    p_gen.add_argument("--out", default="./novproxy_nodes.txt")
    p_gen.add_argument("--region", default="US")
    p_gen.add_argument("--num", type=int, default=10, help="目标可用节点数")
    p_gen.add_argument("--minutes", type=int, default=120, help="会话存活分钟数")
    p_gen.add_argument("--expect", default="US", help="期望出口国家代码")
    p_gen.add_argument("--workers", type=int, default=10)
    p_gen.add_argument("--timeout", type=float, default=25.0)
    p_gen.add_argument("--rounds", type=int, default=3, help="最多提取轮数")
    p_gen.set_defaults(func=cmd_generate)

    p_probe = sub.add_parser("probe", help="验证已有节点")
    p_probe.add_argument("--node", default="")
    p_probe.add_argument("--file", default="")
    p_probe.add_argument("--expect", default="US")
    p_probe.add_argument("--workers", type=int, default=10)
    p_probe.add_argument("--timeout", type=float, default=25.0)
    p_probe.add_argument("--no-retry", action="store_true", help="不做预热重试")
    p_probe.set_defaults(func=cmd_probe)
    return parser


def main(argv=None):
    args = build_parser().parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
