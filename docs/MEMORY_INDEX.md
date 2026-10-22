# 记忆索引 — Codex 账号自动化系统

## 核心流程文档

**7 阶段闭环注册流程完整机制** → [`docs/CODEX_REGISTRATION_FLOW.md`](docs/CODEX_REGISTRATION_FLOW.md)

覆盖：住宅 IP 分配 → 邮箱凭证领取 → 指纹浏览器注册 → 手机接码防风控 → PKCE 授权换 Token → 双端交付热同步。

---

## 速查

### 入口脚本

| 脚本 | 用途 |
| :--- | :--- |
| `run_batch_hotmail_pipeline.py <N>` | 完整闭环（注册 + 接码授权 + 落盘） |
| `run_batch_existing_codex.py <N>` | 存量转化（跳过注册，只补 Codex 授权） |
| `deliver_codex.py <email>` | 单独补交付 |

启动方式：

```bash
sudo -n docker exec -d gptcli-2api bash -c \
  "cd /app/grok-build-auth/openai_client && python3 -u run_batch_hotmail_pipeline.py 10 \
   > /app/data/batch_hotmail_10.log 2>&1"
```

### 关键配置

```bash
SMS_COUNTRY=16,73,117,25   # 英→巴→葡→老挝
SMS_CODE_WAIT=150
SMS_MAX_RETRIES=10
EMAIL_SOURCE=outlook,cloudflare
```

### 目标主机

| 项 | 值 |
| :--- | :--- |
| VPS | `oracle-singapore` / `134.185.81.98` |
| 容器 | `gptcli-2api`（supervisor: registration-sidecar / turnstile-solver / gptcli-2api） |
| API 网关 | `http://134.185.81.98:3000` |
| 凭据目录 | `/www/wwwroot/gptcli-2api/data/cpa_auth_files/` |
| 失败黑名单 | `/www/wwwroot/gptcli-2api/data/codex_failed_accounts.json` |

### 已踩过的坑

1. **`SMS_CODE_WAIT` 被硬编码覆盖** — `sms_provider.py` 曾有 `if wait_sec < 240: wait_sec = 240`，把配置强制拉回 240s。已修（`8e1ee71`）。
2. **重启总是卡在同一批坏号** — 无失败持久化导致每次从队首重跑。已加 `codex_failed_accounts.json`（`e8da6e1`）。
3. **非 Hotmail 域名取不到 OTP** — `@jgxjs.com` 等自定义域名无邮件上下文。已限定候选为 hotmail/outlook（`d431569`）。
4. **Hero-SMS 伪装限流** — 短时间连续取号返回 `NO_NUMBERS`，并非真缺货。失败后仅等 10s 重试会持续撞限流。
5. **chatgpt2api 查询字段** — `/api/accounts` 返回 `{"items": [...]}`，不是 `accounts`。
6. **老挝 25 全虚拟号** — `physicalCount=0`，验证后易触发 `account_deactivated`，仅作末位兜底。
