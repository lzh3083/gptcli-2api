#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
一键将购买的 Hotmail/Outlook OAuth2 卡密（email----password----client_id----refresh_token）
导入到注册机 SQLite 邮箱池中。

用法：
  1. 传入卡密文件：
     python3 scripts/import_hotmail.py hotmail.txt
  2. 管道/标准输入导入：
     cat hotmail.txt | python3 scripts/import_hotmail.py
"""
from __future__ import annotations

import sys
from pathlib import Path

ROOT_DIR = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT_DIR / "grok-build-auth"))
sys.path.insert(0, str(ROOT_DIR / "grok-build-auth" / "openai_client"))

from openai_client.core.outlook_client import import_outlook_from_text, OUTLOOK_ACCOUNTS_FILE
from openai_client.core.db import outlook_pool_summary


def main() -> int:
    text = ""
    if len(sys.argv) > 1:
        src_file = Path(sys.argv[1]).expanduser().resolve()
        if not src_file.is_file():
            print(f"[错误] 找不到文件: {src_file}", file=sys.stderr)
            return 1
        text = src_file.read_text(encoding="utf-8")
    else:
        default_file = ROOT_DIR / "grok-build-auth" / "openai_client" / OUTLOOK_ACCOUNTS_FILE
        if not sys.stdin.isatty():
            text = sys.stdin.read()
        if not text.strip() and default_file.is_file():
            text = default_file.read_text(encoding="utf-8")
        elif not text.strip():
            print("用法: python3 scripts/import_hotmail.py <卡密文件路径>", file=sys.stderr)
            return 1

    text = text.strip()
    if not text:
        print("[错误] 输入内容为空", file=sys.stderr)
        return 1

    # 同步追加/保存到本地默认文件（已被 .gitignore 忽略，不会上传 GitHub）
    target_txt = ROOT_DIR / "grok-build-auth" / "openai_client" / OUTLOOK_ACCOUNTS_FILE
    existing = target_txt.read_text(encoding="utf-8") if target_txt.is_file() else ""
    merged_lines = list(dict.fromkeys(
        [ln.strip() for ln in (existing + "\n" + text).splitlines() if ln.strip()]
    ))
    target_txt.write_text("\n".join(merged_lines) + "\n", encoding="utf-8")

    inserted, skipped = import_outlook_from_text(text)
    summary = outlook_pool_summary()
    print(f"[成功] 导入完成: 新增={inserted}, 已存在跳过={skipped}")
    print(f"[邮箱池状态] {summary}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
