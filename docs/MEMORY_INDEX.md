# 记忆索引 — Codex 账号自动化系统

> ⛔ **开工前必读「铁律」区块。违反即返工。**

---

# ⛔ 铁律（写死，不得违反）

## 铁律 1：唯一正确入口是完整 6 阶段链路

```text
阶段1 代理  →  阶段2 邮箱  →  阶段3-4 注册  →  阶段5 接码  →  阶段6 换Token  →  阶段7 交付
  ↓            ↓              ↓                ↓              ↓               ↓
used_ips    email_pool    chatgpt-*.json   activation_id   access_token    PG+CPA+2api
```

**唯一入口脚本：**

```bash
python3 -u run_batch_hotmail_pipeline.py 10
```

启动方式：

```bash
sudo -n docker exec -d gptcli-2api bash -c \
  "cd /app/grok-build-auth/openai_client && python3 -u run_batch_hotmail_pipeline.py 10 \
   > /app/data/batch_hotmail_10.log 2>&1"
```

## 铁律 2：禁止跑 `run_batch_existing_codex.py`

**这是废的。** 它只做阶段 5-7 的存量转化，跳过阶段 1-4，产出不符合要求。

| 脚本 | 判定 |
| :--- | :--- |
| `run_batch_hotmail_pipeline.py` | ✅ **唯一正确** |
| `run_batch_existing_codex.py` | ⛔ **禁止启动**，白耗接码费 |

用户已明确：**"下次不要再给我跑二阶段，那是废的。"**

## 铁律 3：阶段编号对应关系（别搞混）

| 用户口径 | 含义 | 脚本 |
| :--- | :--- | :--- |
| 完整链路 | 1→2→3-4→5→6→7 全跑 | `run_batch_hotmail_pipeline.py` |
| 「二阶段」 | 只跑 5-7 的存量转化，**废的** | ~~`run_batch_existing_codex.py`~~ |

---

# 核心流程文档

**7 阶段闭环注册流程完整机制** → [`docs/CODEX_REGISTRATION_FLOW.md`](CODEX_REGISTRATION_FLOW.md)

---

# 速查

## 关键配置

```bash
SMS_COUNTRY=16,73,117,25   # 英→巴→葡→老挝
SMS_CODE_WAIT=150
SMS_MAX_RETRIES=10
EMAIL_SOURCE=outlook,cloudflare
```

## 目标主机

| 项 | 值 |
| :--- | :--- |
| VPS | `oracle-singapore` / `134.185.81.98` |
| 容器 | `gptcli-2api`（supervisor: registration-sidecar / turnstile-solver / gptcli-2api） |
| API 网关 | `http://134.185.81.98:3000` |
| 凭据目录 | `/www/wwwroot/gptcli-2api/data/cpa_auth_files/` |
| 邮箱池 DB | `/app/grok-build-auth/openai_client/turb.sqlite3` 表 `email_pool` |
| 失败黑名单 | `/app/data/codex_failed_accounts.json` |

## 各阶段健康度（2026-10-22 23:30 快照）

| 阶段 | 状态 | 说明 |
| :--- | :--- | :--- |
| 1 代理 | ⚠️ 不稳 | NovProxy 偶发 `国家不符: 期望 SG 实际 SA` |
| 2 邮箱 | 🔴 枯竭 | `available=0`；cooling 54 个 token 已 AADSTS70000 失效 |
| 3-4 注册 | ✅ 通 | 能正常产出 `chatgpt-*.json` |
| 5 接码 | 🔴 **根本瓶颈** | 能进验证码页，短信到达率极低 |
| 6 换 Token | ✅ 通 | 存量 28 个证明可用 |
| 7 交付 | ✅ 通 | chatgpt2api 37 个在线 |

---

# 已踩过的坑

1. **`SMS_CODE_WAIT` 被硬编码覆盖** — `sms_provider.py` 曾有 `if wait_sec < 240: wait_sec = 240`，把配置强制拉回 240s。已修（`8e1ee71`）。
2. **重启总是卡在同一批坏号** — 无失败持久化导致每次从队首重跑。已加 `codex_failed_accounts.json`（`e8da6e1`）。
3. **非 Hotmail 域名取不到 OTP** — `@jgxjs.com` 等自定义域名无邮件上下文。候选限定 hotmail/outlook（`d431569`）。
4. **Hero-SMS 伪装限流** — 短时间连续取号返回 `NO_NUMBERS`，并非真缺货。失败后仅等 10s 重试会持续撞限流。
5. **chatgpt2api 查询字段** — `/api/accounts` 返回 `{"items": [...]}`，不是 `accounts`。
6. **老挝 25 全虚拟号** — `physicalCount=0`，验证后易触发 `account_deactivated`，仅作末位兜底。
7. **误跑二阶段脚本** — 白天改的是 `run_batch_hotmail_pipeline.py`，却启动了 `run_batch_existing_codex.py`，白跑 10 分钟。见铁律 2。

---

# 诊断命令

```bash
# 看流水线进度
sudo -n tail -f /www/wwwroot/gptcli-2api/data/batch_hotmail_10.log

# 邮箱池状态
sudo -n python3 -c "
import sqlite3
c = sqlite3.connect('/www/wwwroot/gptcli-2api/grok-build-auth/openai_client/turb.sqlite3')
print(c.execute('select status, count(*) from email_pool group by status').fetchall())
"

# 资产盘点
ls -1 /www/wwwroot/gptcli-2api/data/cpa_auth_files/chatgpt-*.json | wc -l   # 基础账号
ls -1 /www/wwwroot/gptcli-2api/data/cpa_auth_files/codex-*.json | wc -l     # 已授权

# Hero-SMS 余额
curl -s "https://hero-sms.com/stubs/handler_api.php?action=getBalance&api_key=65f98d0330618120582d49f3eff7923f"
```
