# AGENTS.md — 本仓库的硬性约束

> 任何 agent / 自动化流程进入本仓库前，**必须先读这一页**。
> 违反下面的铁律等于返工。

---

## ⛔ 铁律 1：Codex 账号生产只走一条链路

```text
阶段1 代理  →  阶段2 邮箱  →  阶段3-4 注册  →  阶段5 接码  →  阶段6 换Token  →  阶段7 交付
  ↓            ↓              ↓                ↓              ↓               ↓
used_ips    email_pool    chatgpt-*.json   activation_id   access_token    PG+CPA+2api
```

**唯一入口：**

```bash
python3 -u grok-build-auth/openai_client/run_batch_hotmail_pipeline.py 10
```

容器内启动：

```bash
sudo -n docker exec -d gptcli-2api bash -c \
  "cd /app/grok-build-auth/openai_client && python3 -u run_batch_hotmail_pipeline.py 10 \
   > /app/data/batch_hotmail_10.log 2>&1"
```

## ⛔ 铁律 2：彻底废除二阶段注册，禁止生产单独 ChatGPT 账号

二阶段存量补跑脚本 `run_batch_existing_codex.py` 及其在流水线内的所有二阶段冗余代码**已全部删除**。
只做阶段 5-7 的存量转化既耗费接码费又产出低质半成品。
用户铁令：**"记住不要单独的ChatGPT账号，要接手机号码的codex账号！把二阶段的注册任务，相关代码全部删了，不需要二阶段注册。"**

| 模块 / 链路 | 状态 | 判定 |
| :--- | :--- | :--- |
| `run_batch_hotmail_pipeline.py` | 端到端一体化：从住宅代理、邮箱注册到手机接码、Codex 授权同浏览器一次性完成 | ✅ **唯一生产链路** |
| 二阶段补跑脚本 `run_batch_existing_codex.py` | 源码已物理删除，禁止任何存量补跑 | ❌ **已彻底删除** |
| 单独 ChatGPT 账号生产 | 未绑手机号不视为成功，不落盘单独 ChatGPT 凭证 | ❌ **严格禁止** |

---

## 完整机制文档

- [`docs/CODEX_REGISTRATION_FLOW.md`](docs/CODEX_REGISTRATION_FLOW.md) — 7 阶段端到端机制
- [`docs/MEMORY_INDEX.md`](docs/MEMORY_INDEX.md) — 速查、配置、已知坑、诊断命令

---

## 其他硬性要求

1. **质量优先于速度** — 账号存活率比注册数量重要，宁慢勿废。
2. **接码资金零浪费** — 任何未收到短信的号码必须触发退款，不得沉淀。
3. **改动必须双端同步** — 面板（服务器 `/www/wwwroot/gptcli-2api`）与 GitHub 必须一致。
