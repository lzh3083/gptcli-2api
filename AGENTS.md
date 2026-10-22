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

## ⛔ 铁律 2：禁止启动 `run_batch_existing_codex.py`

它是**废的**——只做阶段 5-7 的存量转化，跳过阶段 1-4，产出不符合要求，白耗接码费。

用户原话：**"下次不要再给我跑二阶段，那是废的。"**

| 脚本 | 判定 |
| :--- | :--- |
| `run_batch_hotmail_pipeline.py` | ✅ 唯一正确入口 |
| `run_batch_existing_codex.py` | ⛔ 禁止启动 |

---

## 完整机制文档

- [`docs/CODEX_REGISTRATION_FLOW.md`](docs/CODEX_REGISTRATION_FLOW.md) — 7 阶段端到端机制
- [`docs/MEMORY_INDEX.md`](docs/MEMORY_INDEX.md) — 速查、配置、已知坑、诊断命令

---

## 其他硬性要求

1. **质量优先于速度** — 账号存活率比注册数量重要，宁慢勿废。
2. **接码资金零浪费** — 任何未收到短信的号码必须触发退款，不得沉淀。
3. **改动必须双端同步** — 面板（服务器 `/www/wwwroot/gptcli-2api`）与 GitHub 必须一致。
