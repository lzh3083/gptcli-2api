#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Codex 失效账号 24 小时冷却复活与自动废弃调度器 (Codex Account Revival & Deactivation Pipeline)

核心机制:
  1. 记录与追踪因会话吊销 (token_invalidated / refresh_token_invalidated) 等失效的 Codex 账号;
  2. 强制进入 24 小时沉淀冷却期 (避开官方 24h 离线审计与风控观察窗口);
  3. 冷却期满后，自动调用 run_single_codex.py 使用高质量干净住宅代理尝试重新登录并换取全新 Token;
  4. 若登录或授权返回 account_deactivated / 账号已注销停用，则自动触发彻底物理废弃（清空本地与 CPA 凭证、更新数据库与黑名单）;
  5. 若重新登录成功换取 Token，则自动重新同步到 CPA 生产池。

常用参数:
  --status              查看复活队列中所有账号的状态与到期倒计时
  --add <email>         将指定失效账号加入 24 小时复活冷却队列
  --hours <N>           指定冷却时长（默认 24 小时）
  --run                 检查并执行当前已满 24 小时的到期账号复活
  --force               强制立即执行复活（跳过 24h 冷却期）
  --email <email>       配合 --run 或 --force 仅指定执行某单一账号
  --daemon              以常驻守护模式运行，每隔 10 分钟自动检查一次
"""
from __future__ import annotations

import argparse
import json
import logging
import os
import shutil
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s"
)
logger = logging.getLogger("CodexRevival")


def find_data_root() -> Path:
    """自动探测数据持久化目录。"""
    candidates = [
        Path("/app/data"),
        Path(__file__).resolve().parents[1] / "data",
        Path("/www/wwwroot/gptcli-2api/data"),
    ]
    for c in candidates:
        if c.is_dir():
            return c
    p = Path(__file__).resolve().parents[1] / "data"
    p.mkdir(parents=True, exist_ok=True)
    return p


def get_queue_file() -> Path:
    return find_data_root() / "codex_revival_queue.json"


def load_queue() -> dict[str, dict]:
    q_file = get_queue_file()
    if not q_file.exists():
        return {}
    try:
        data = json.loads(q_file.read_text(encoding="utf-8"))
        return data if isinstance(data, dict) else {}
    except Exception as e:
        logger.warning(f"读取复活队列失败: {e}")
        return {}


def save_queue(q: dict[str, dict]) -> None:
    q_file = get_queue_file()
    try:
        q_file.write_text(json.dumps(q, indent=2, ensure_ascii=False), encoding="utf-8")
    except Exception as e:
        logger.error(f"写入复活队列失败: {e}")


def add_to_queue(email: str, reason: str = "token_invalidated", hours: float = 24.0) -> dict:
    """将失效账号加入 24 小时复活队列。"""
    email_clean = email.strip()
    key = email_clean.lower()
    q = load_queue()
    now_ts = int(time.time())
    cooling_until_ts = int(now_ts + hours * 3600)
    now_str = time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(now_ts))
    cooling_str = time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(cooling_until_ts))

    item = q.get(key, {})
    item.update({
        "email": email_clean,
        "invalidated_at": now_str,
        "cooling_hours": hours,
        "cooling_until": cooling_until_ts,
        "cooling_until_str": cooling_str,
        "status": "cooling",  # cooling | revived | deactivated | failed
        "attempts": item.get("attempts", 0),
        "last_attempt_at": item.get("last_attempt_at"),
        "last_error": reason,
    })
    q[key] = item
    save_queue(q)
    logger.info(f"★ 已将账号加入复活队列: {email_clean} | 冷却时长: {hours}h | 预计解冻复活时间: {cooling_str} | 原因: {reason}")
    return item


def print_status() -> None:
    """输出当前复活队列所有账号的详细状态。"""
    q = load_queue()
    if not q:
        logger.info("当前复活队列为空。")
        return

    now_ts = time.time()
    logger.info("=" * 80)
    logger.info(f"📋 Codex 账号 24 小时冷却复活队列 (共 {len(q)} 个账号)")
    logger.info("=" * 80)

    for key, info in sorted(q.items(), key=lambda x: x[1].get("cooling_until", 0)):
        email = info.get("email", key)
        status = info.get("status", "unknown")
        cooling_until = info.get("cooling_until", 0)
        cooling_until_str = info.get("cooling_until_str", "-")
        invalidated_at = info.get("invalidated_at", "-")
        reason = info.get("last_error", "-")
        attempts = info.get("attempts", 0)

        rem_sec = max(0, int(cooling_until - now_ts))
        rem_hours = rem_sec / 3600.0

        if status == "cooling":
            if rem_sec > 0:
                state_desc = f"⏳ 冷却中 (剩余 {rem_hours:.1f}h，解冻于 {cooling_until_str})"
            else:
                state_desc = "🟢 已成熟，待调度复活"
        elif status == "revived":
            state_desc = "🎉 已成功复活重新投产"
        elif status == "deactivated":
            state_desc = "🚫 官方注销已彻底废弃 (account_deactivated)"
        else:
            state_desc = f"✕ 异常/失败 ({status})"

        logger.info(f"- {email:<35} | 状态: {state_desc} | 失效时间: {invalidated_at} | 尝试次数: {attempts} | 原因: {reason}")
    logger.info("=" * 80)


def execute_single_revival(email: str) -> tuple[int, str]:
    """执行单个账号的重新登录授权换 Token 流程。"""
    in_docker = os.path.exists("/.dockerenv") or os.path.exists("/app/grok-build-auth")
    
    if in_docker:
        script_path = "/app/grok-build-auth/openai_client/run_single_codex.py"
        cmd = [sys.executable, script_path, email]
    else:
        # 在宿主机运行，优先调用 docker exec
        check_docker = subprocess.run(["docker", "ps", "--filter", "name=gptcli-2api", "-q"], capture_output=True, text=True)
        if check_docker.stdout.strip():
            cmd = ["sudo", "-n", "docker", "exec", "gptcli-2api", "python3", "/app/grok-build-auth/openai_client/run_single_codex.py", email]
        else:
            script_path = str(Path(__file__).resolve().parents[1] / "grok-build-auth" / "openai_client" / "run_single_codex.py")
            cmd = [sys.executable, script_path, email]

    logger.info(f"🚀 开始执行账号复活调用: {' '.join(cmd)}")
    try:
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=300)
        stdout = proc.stdout or ""
        stderr = proc.stderr or ""
        full_out = stdout + "\n" + stderr
        return proc.returncode, full_out
    except subprocess.TimeoutExpired:
        logger.error(f"⏱ 账号复活执行超时 (300s): {email}")
        return 124, "timeout > 300s"
    except Exception as e:
        logger.error(f"⚠ 启动复活脚本异常: {e}")
        return 1, str(e)


def process_queue(force: bool = False, target_email: str | None = None) -> None:
    """检查并执行符合条件的账号复活。"""
    q = load_queue()
    if not q:
        logger.info("当前复活队列为空，无须处理。")
        return

    now_ts = time.time()
    candidates = []

    for key, info in q.items():
        email = info.get("email", key)
        if target_email and email.lower() != target_email.lower():
            continue

        status = info.get("status")
        cooling_until = info.get("cooling_until", 0)

        # 仅处理 cooling 或明确指定的账号
        if status == "deactivated" and not force:
            continue
        if status == "revived" and not force:
            continue

        if force or (status == "cooling" and now_ts >= cooling_until):
            candidates.append(email)

    if not candidates:
        logger.info("未发现已达到 24 小时解冻标准的待复活账号（可使用 --status 查看剩余时间或 --force 强制执行）。")
        return

    logger.info(f"★ 发现 {len(candidates)} 个满足复活条件的账号，开始逐个通过住宅代理尝试重新授权...")

    for email in candidates:
        key = email.lower()
        item = q.get(key, {})
        item["status"] = "processing"
        item["attempts"] = item.get("attempts", 0) + 1
        item["last_attempt_at"] = time.strftime("%Y-%m-%d %H:%M:%S")
        save_queue(q)

        code, output = execute_single_revival(email)

        if code == 0:
            logger.info(f"🎉 [复活成功] 账号 {email} 已重新获取全新 Token，并已落盘同步至 CPA 生产池！")
            item["status"] = "revived"
            item["last_error"] = None
        elif code == 2 or "account_deactivated" in output.lower():
            logger.warning(f"🚫 [账号注销彻底废弃] 账号 {email} 检测到官方返回 account_deactivated，已执行全链路清理废弃！")
            item["status"] = "deactivated"
            item["last_error"] = "account_deactivated"
        else:
            logger.warning(f"✕ [复活失败] 账号 {email} 本轮复活未成功 (exit_code={code})")
            item["status"] = "failed"
            # 若尝试小于 3 次，设置退避重试（再冷冻 2 小时）
            if item["attempts"] < 3:
                retry_hours = 2.0
                item["status"] = "cooling"
                item["cooling_until"] = int(time.time() + retry_hours * 3600)
                item["cooling_until_str"] = time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(item["cooling_until"]))
                logger.info(f"  > 将在 {retry_hours}h 后重试第 {item['attempts'] + 1} 次 (预计: {item['cooling_until_str']})")
            item["last_error"] = output[-300:].replace("\n", " ")

        save_queue(q)
        time.sleep(3)


def run_daemon(interval_sec: int = 600) -> None:
    """常驻后台循环。"""
    logger.info(f"🔄 启动 Codex 账号复活常驻守护调度器，每 {interval_sec} 秒检查一次队列...")
    while True:
        try:
            process_queue(force=False)
        except Exception as e:
            logger.error(f"守护调度异常: {e}")
        time.sleep(interval_sec)


def main():
    parser = argparse.ArgumentParser(description="Codex 失效账号 24 小时冷却复活与自动废弃调度器")
    parser.add_argument("--status", action="store_true", help="查看复活队列中所有账号的状态与到期倒计时")
    parser.add_argument("--add", type=str, metavar="EMAIL", help="将指定失效账号加入 24 小时复活冷却队列")
    parser.add_argument("--hours", type=float, default=24.0, help="指定冷却时长，默认 24 小时")
    parser.add_argument("--reason", type=str, default="token_invalidated", help="失效原因标注")
    parser.add_argument("--run", action="store_true", help="检查并执行当前已满 24 小时的到期账号复活")
    parser.add_argument("--force", action="store_true", help="强制立即执行复活（跳过 24h 冷却期）")
    parser.add_argument("--email", type=str, metavar="EMAIL", help="指定只处理某个特定账号")
    parser.add_argument("--daemon", action="store_true", help="以常驻守护模式运行")
    parser.add_argument("--interval", type=int, default=600, help="常驻模式轮询间隔秒数（默认 600 秒）")

    args = parser.parse_args()

    if args.add:
        add_to_queue(args.add, reason=args.reason, hours=args.hours)
        print_status()
        return

    if args.status:
        print_status()
        return

    if args.daemon:
        run_daemon(interval_sec=args.interval)
        return

    if args.run or args.force:
        process_queue(force=args.force, target_email=args.email)
        return

    # 默认无参数时展示状态并执行常规检测
    print_status()
    process_queue(force=False, target_email=args.email)


if __name__ == "__main__":
    main()
