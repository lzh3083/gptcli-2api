#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
冷冻沉淀池到期流转调度器 (Cooling-to-CPA Pipeline)
功能:
  1. 扫描 cpa_cooling/ 沉淀目录下的所有 codex-*.json 凭证;
  2. 检查冷却时长 (默认 3 小时，可通过 CODEX_COOLING_HOURS 配置);
  3. 对已达到沉淀时长的成熟账号，自动流转到 cpa_auth_files/，并同步至 CPA 生产池;
  4. 支持参数:
     --force: 强制立即流转所有冷却中账号 (跳过沉淀时间检查)
     --status: 仅查看当前沉淀池状态，不执行流转
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

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger("CoolingSync")

def find_data_dirs() -> tuple[Path, Path, Path]:
    candidates = [
        Path("/app/data"),
        Path(__file__).resolve().parents[1] / "data",
        Path("/www/wwwroot/gptcli-2api/data"),
    ]
    data_root = None
    for c in candidates:
        if c.is_dir():
            data_root = c
            break
    if not data_root:
        data_root = Path(__file__).resolve().parents[1] / "data"
        data_root.mkdir(parents=True, exist_ok=True)

    cooling_dir = data_root / "cpa_cooling"
    prod_dir = data_root / "cpa_auth_files"
    cpa_host_dir = Path("/www/wwwroot/cpa-manager-plus/cliproxyapi/auths")

    cooling_dir.mkdir(parents=True, exist_ok=True)
    prod_dir.mkdir(parents=True, exist_ok=True)
    return cooling_dir, prod_dir, cpa_host_dir


def check_and_promote(force: bool = False, status_only: bool = False) -> None:
    cooling_dir, prod_dir, cpa_host_dir = find_data_dirs()
    cooling_hours = float(os.environ.get("CODEX_COOLING_HOURS", "3"))
    cooling_seconds = cooling_hours * 3600.0

    files = sorted(cooling_dir.glob("codex-*.json"))
    now = time.time()

    logger.info("=" * 60)
    logger.info("=== 扫描 Codex 沉淀静默池 ===")
    logger.info(f"沉淀池目录: {cooling_dir} | 标准冷静期: {cooling_hours} 小时")
    logger.info(f"待检账号总数: {len(files)} 个")
    logger.info("=" * 60)

    if not files:
        logger.info("沉淀池为空，当前无待处理账号。")
        return

    ready_files: list[Path] = []
    cooling_files: list[tuple[Path, float]] = []

    for f in files:
        try:
            data = json.loads(f.read_text(encoding="utf-8"))
        except Exception:
            data = {}
        
        reg_ts = float(data.get("registered_at") or 0.0)
        cool_until = float(data.get("cooling_until") or 0.0)
        if not reg_ts:
            reg_ts = f.stat().st_mtime
        if not cool_until:
            cool_until = reg_ts + cooling_seconds

        rem = cool_until - now
        if force or rem <= 0:
            ready_files.append(f)
        else:
            cooling_files.append((f, rem))

    # 输出仍在沉淀期的账号
    for f, rem in cooling_files:
        rem_min = max(1, int(rem // 60))
        rem_h = rem / 3600.0
        logger.info(f"⏳ [沉淀中] {f.name} 剩余冷却时间: {rem_h:.1f}h ({rem_min} 分钟)")

    if status_only:
        logger.info(f"当前状态统计: 仍在冷却 {len(cooling_files)} 个 | 已成熟待流转 {len(ready_files)} 个")
        return

    if not ready_files:
        logger.info("当前暂无达到冷静期的成熟账号。")
        return

    logger.info("-" * 60)
    logger.info(f"🚀 开始安全流转 {len(ready_files)} 个已成熟账号至 CPA 生产池...")

    promoted_emails: list[str] = []
    for f in ready_files:
        dest_prod = prod_dir / f.name
        try:
            data = json.loads(f.read_text(encoding="utf-8"))
            data["status"] = "active"
            data["promoted_at"] = int(time.time())
            f.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
        except Exception:
            pass

        shutil.copy2(f, dest_prod)
        f.unlink(missing_ok=True)
        email = f.stem.replace("codex-", "").replace("-free", "")
        promoted_emails.append(email)
        logger.info(f"✅ [成熟流转] {email} 成功从沉淀池移动至生产凭证目录: {dest_prod.name}")

    sync_script_candidates = [
        Path("/app/scripts/sync_codex_to_cpa.sh"),
        Path("/www/wwwroot/gptcli-2api/scripts/sync_codex_to_cpa.sh"),
    ]
    for sc in sync_script_candidates:
        if sc.is_file():
            try:
                subprocess.run(["bash", str(sc)], capture_output=True, timeout=60, check=True)
                logger.info("  > [CPA 同步] 已触发 sync_codex_to_cpa.sh，生产目录已热加载最新凭证")
            except Exception as se:
                logger.warning("  > [CPA 同步] 执行同步脚本异常: %s", se)
            break

    if cpa_host_dir.is_dir():
        for email in promoted_emails:
            target_f = prod_dir / f"codex-{email}-free.json"
            if target_f.is_file():
                try:
                    shutil.copy2(target_f, cpa_host_dir / target_f.name)
                except Exception:
                    pass

    logger.info("=" * 60)
    logger.info(f"🎉 批量流转完成！共激活交付 {len(promoted_emails)} 个成熟 Codex 账号。")
    logger.info("=" * 60)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Codex 冷却沉淀池自动流转调度器")
    parser.add_argument("--force", action="store_true", help="强制立即流转全部冷却中凭证")
    parser.add_argument("--status", action="store_true", help="仅查看当前状态，不执行流转")
    args = parser.parse_args()

    check_and_promote(force=args.force, status_only=args.status)
