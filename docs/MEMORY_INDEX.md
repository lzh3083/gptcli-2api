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

## 铁律 2：彻底废除二阶段注册与存量脚本

二阶段存量补跑脚本 `run_batch_existing_codex.py` 及流水线中的二阶段冗余逻辑**已彻底删除**。
严禁生产没有接码绑定手机号的单独 ChatGPT 账号。
用户铁令：**"记住不要单独的ChatGPT账号，要接手机号码的codex账号！把二阶段的注册任务，相关代码全部删了，不需要二阶段注册。"**

| 模块 | 判定 |
| :--- | :--- |
| `run_batch_hotmail_pipeline.py` | ✅ **唯一正确生产链路**（注册+接码一体化） |
| `run_batch_existing_codex.py` | ❌ **已彻底删除废除** |

## 铁律 3：账号交付标准

只有完成手机号验证码验证并生成 `codex-*-free.json` 的 Codex 账号才算交付成功，绝不保留无手机号的单独 ChatGPT 半成品账号。

---

# 核心流程文档

**7 阶段闭环注册流程完整机制** → [`docs/CODEX_REGISTRATION_FLOW.md`](CODEX_REGISTRATION_FLOW.md)

---

# 速查

## 关键配置

```bash
SMS_COUNTRY=4,6,7,2,16,73   # 菲律宾→印尼→马来西亚→哈萨克→英→巴 (亚太/新加坡高亲和接码)
SMS_CODE_WAIT=180
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
| 2 邮箱 | 🟡 **状态卡死，非枯竭** | `available=0` 是调度 bug：54 个 cooling 冷冻期已过 44h 却无自动恢复；实测 **55 个干净可用** |
| 3-4 注册 | ✅ 通 | 能正常产出 `chatgpt-*.json` |
| 5 接码 | 🔴 **根本瓶颈** | 能进验证码页，短信到达率极低 |
| 6 换 Token | ✅ 通 | 存量 28 个证明可用 |
| 7 交付 | ✅ 通 | chatgpt2api 37 个在线 |

---

# 邮箱池诊断结论（2026-10-22 23:45 实测）

## 结论：不是枯竭，是状态卡死

`pick_account()` 报 `available=0` 是**调度 bug**，不是没邮箱。

| 状态 | 数量 | 实测结果 |
| :--- | :--- | :--- |
| cooling | 54 | 冷冻期标注「24h 后启用」，实际已过 44h，**无自动恢复逻辑** → 永久卡住 |
| failed | 123 | 混杂多种原因，见下表 |
| used | 27 | 已消耗 |

## failed 原因分布

| 原因 | 数量 | 可否复用 |
| :--- | :--- | :--- |
| 二手脏邮箱 `recycled_dirty_mailbox` | 39 | ❌ 永久废弃 |
| `audit_target_registered` 第三方客户端授权 | 28 | ❌ 已注册过 |
| `audit_blacklisted_recycled` 改别名翻新 | 20 | ❌ 黑名单 |
| `invalid_refresh_token` | 14 | ❌ 失效 |
| `audit_target_registered` 已存在 OpenAI 账号 | 10 | ❌ 已注册 |
| 注册流程失败（找不到 OTP 输入框） | 8 | ✅ 可重试 |
| Cloak 注册失败（Locator / session） | 2 | ✅ 可重试 |

## 关键实测数据

对 64 个候选（54 cooling + 8 可重试 + 2 Cloak失败）跑 `_check_mailbox_clean_for_openai`：

```text
干净可用 55 | 不可用 9
```

**55 个邮箱收件箱零消息（clean_msgs=0）、OAuth 有效，完全可用。**

之前抽检 4 个 cooling 全部 `AADSTS70000`，是运气问题（抽到了失效的那批）。

## 恢复方法

把「冷冻期已过且邮箱干净」的 cooling 记录改回 `available`：

```sql
UPDATE email_pool SET status='available', updated_at=datetime('now')
WHERE status='cooling' AND created_at <= datetime('now','-24 hours');
```

建议先用 `_check_mailbox_clean_for_openai` 过滤，只放行 clean_msgs=0 的。

## 教训

**报 `available=0` 时不要直接判定枯竭。** 先跑三项检测：
1. OAuth 有效性（`_ms_access_token`）
2. 邮箱干净度（`_check_mailbox_clean_for_openai`）
3. 状态机是否卡住（cooling 有无恢复逻辑）

---

# 已踩过的坑

1. **`SMS_CODE_WAIT` 被硬编码覆盖** — `sms_provider.py` 曾有 `if wait_sec < 240: wait_sec = 240`，把配置强制拉回 240s。已修（`8e1ee71`）。
2. **重启总是卡在同一批坏号** — 无失败持久化导致每次从队首重跑。已加 `codex_failed_accounts.json`（`e8da6e1`）。
3. **非 Hotmail 域名取不到 OTP** — `@jgxjs.com` 等自定义域名无邮件上下文。候选限定 hotmail/outlook（`d431569`）。
4. **Hero-SMS 伪装限流** — 短时间连续取号返回 `NO_NUMBERS`，并非真缺货。失败后仅等 10s 重试会持续撞限流。
5. **chatgpt2api 查询字段** — `/api/accounts` 返回 `{"items": [...]}`，不是 `accounts`。
6. **老挝 25 全虚拟号** — `physicalCount=0`，验证后易触发 `account_deactivated`，仅作末位兜底。
7. **误跑二阶段脚本** — 白天改的是 `run_batch_hotmail_pipeline.py`，却启动了 `run_batch_existing_codex.py`，白跑 10 分钟。见铁律 2。
8. **误判邮箱池枯竭** — `available=0` 实为 cooling 状态无自动恢复，实际 55 个可用。抽检 4 个全失效导致误判。

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
