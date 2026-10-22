# Codex 账号注册全流程（7 阶段闭环）

> ⛔ **唯一正确入口：`run_batch_hotmail_pipeline.py`（完整 6 阶段链路）。**
> **禁止启动 `run_batch_existing_codex.py`（只跑阶段 5-7 的存量转化，产出不符合要求，白耗接码费）。**

> 本文档记录从零生成一个可用 Codex 账号的端到端机制。
> 每个账号在完全隔离的环境下独立流转，任一阶段失败即熔断并释放资源。

## 全景架构

```text
[阶段 1: 住宅 IP 分配] ──> [阶段 2: 邮箱凭证领取] ──> [阶段 3-4: 浏览器注册]
                                                              │
        ┌─────────────────────────────────────────────────────┘
        ▼
[阶段 5: 手机接码与防风控] ──> [阶段 6: Codex PKCE 授权] ──> [阶段 7: 双端交付热同步]
```

入口脚本：`grok-build-auth/openai_client/run_batch_hotmail_pipeline.py`

```bash
python3 -u run_batch_hotmail_pipeline.py 10    # 目标 10 个账号，注册最多 26 轮
```

---

## 阶段 1：独立住宅 IP 调度（一号一 IP 原则）

**核心逻辑**：严格杜绝机房 IP，避免触发 OpenAI 首页的 Cloudflare 盾或批量封禁。

| 项 | 说明 |
| :--- | :--- |
| 实现 | `register.py` → `_resolve_novproxy_one_ip()` |
| 上游 | NovProxy 动态住宅 API `https://white.novproxy.com/white/api`，region=SG |
| 校验 | 逐个 probe，`hosting=False` 确认民用宽带；剔除已用过的 exit_ip |
| 生命周期 | 该 IP 绑定当前账号全流程（注册 → 接码 → 授权）复用同 IP |
| 落盘 | `/app/data/used_novproxy_ips.json`、`novproxy_nodes.txt` |

---

## 阶段 2：微软邮箱池分配与存活预检

**核心逻辑**：领取具备直接收信权限的 Hotmail / Outlook 账号。

| 项 | 说明 |
| :--- | :--- |
| 实现 | `config/email.py`，`EMAIL_SOURCE=outlook,cloudflare` |
| 池文件 | `用于注册的邮箱.txt` |
| 双通道 | Microsoft Graph API + 底层 IMAP 直连（`core/outlook_client.py`） |
| 预检 | 校验 refresh_token 有效性，确保后续秒级收信 |
| 取码 | IMAP(New) 直连优先，Graph 兜底，settle 5s 防旧码误取 |

---

## 阶段 3 & 4：指纹浏览器对齐与基础账号注册

**核心逻辑**：CloakBrowser 模拟真人环境完成 ChatGPT 账号注册。

| 项 | 说明 |
| :--- | :--- |
| 启动参数 | `headless=True humanize=True geoip=True persistent=False` |
| 指纹 | 时区跟随出口 IP（如 `Asia/Singapore`）、语言 `en-SG`、WebRTC/Canvas 对齐 |
| 注册页 | `https://auth.openai.com/` |
| 密码 | 自动生成 14 位高强度随机密码 |
| 资料 | 自动填姓名与生日（`about_you`） |
| 风控 | Sentinel 令牌获取（`sentinel` 节点） |
| 产物 | `chatgpt-<email>.json` |

### 注册状态机（`register.py`，17 个节点）

```text
starting → email_acquired → preflight → bootstrap → auth_init
→ signin → authorize → password_page → sentinel → register_user
→ otp_send → waiting_otp → validating_otp → oauth_callback
→ about_you → codex_oauth → success
```

---

## 阶段 5：手机短信验证与资金安全保障（核心安全层）

**核心逻辑**：开通 Codex 必需的手机验证，兼顾成功率与资金零浪费。

| 项 | 说明 |
| :--- | :--- |
| 实现 | `core/roxy_codex_oauth.py` → `_do_phone_verification_if_present()` |
| 上游 | Hero-SMS（smsbower 适配），`https://hero-sms.com/stubs/handler_api.php`，service `dr` |
| 国家池 | `SMS_COUNTRY=16,73,117,25` |
| 轮转 | `target_country = countries[(attempt-1) % len(countries)]` |
| 提交 | 填号 → 选 SMS 通道 → 点击 Continue → 兜底 JS `form.requestSubmit()` |
| 等待 | `SMS_CODE_WAIT=150`，5 秒间隔轮询 |
| 填码 | 收到 6 位 OTP → 填入 → ENTER + 匹配提交按钮 + `requestSubmit` |
| 退款 | 超时/号段拦截 → 自动 `setStatus=8` 取消，平台 100% 全额原路退款 |

### 接码国家梯队

| 顺位 | 国家 | 单价 | 备注 |
| :--- | :--- | :--- | :--- |
| 1 | 16 英国 | $0.045 | 最低价，欧洲高权重 |
| 2 | 73 巴西 | $0.054 | 库存最大（百万级） |
| 3 | 117 葡萄牙 | $0.060 | 欧盟洁净号段 |
| 4 | 25 老挝 | $0.060 | 备选；**physicalCount=0，全虚拟号** |

**注意**：`sms_provider.py` 曾有 `if wait_sec < 240: wait_sec = 240` 硬编码下限，会覆盖 `SMS_CODE_WAIT` 配置。已修为仅防御 60s 以下极端值（commit `8e1ee71`）。

**老挝号段风险**：国家 25 的 `physicalCount` 长期为 0（纯虚拟号）。历史上用老挝号完成验证的账号大量出现 `403 account_deactivated`，故仅在英/巴/葡全部取号失败时作为末位兜底。

---

## 阶段 6：Codex PKCE 原生 OAuth 授权换 Token

**核心逻辑**：用官方 Codex 授权端点换取长效凭据。

```text
client_id     = app_EMoamEEZ73f0CkXaXp7hrann
redirect_uri  = http://localhost:1455/auth/callback
flow          = codex_cli_simplified_flow=true
PKCE          = code_challenge_method=S256
```

| 项 | 说明 |
| :--- | :--- |
| 构造 | 本地生成 `code_verifier` / `code_challenge` / `state` |
| 拦截 | 从 performance 记录与窗口导航捕获 callback `code` |
| 换 Token | 浏览器原生上下文 POST `/oauth/token` |
| 产物 | `access_token`、`refresh_token`（数月有效）、`id_token` |
| 落盘 | `codex-<email>-free.json` |

### 授权子流程

```text
打开 authorize → 填邮箱 → 填密码 → 邮箱 OTP 轮询
→ 填 OTP → 检测 add-phone
   ├─ 403 account_deactivated → 立刻判死跳过
   └─ accepted → 阶段 5 接码
→ 填短信码 → 等 localhost:1455 回调 → code 换 token
```

---

## 阶段 7：双端落盘交付与 chatgpt2api 热同步

**核心逻辑**：成果即时入库投产，无需人工干预。

| 步骤 | 动作 |
| :--- | :--- |
| ① | 解析 `id_token` 取 `user_id` |
| ② | upsert PostgreSQL |
| ③ | 写 `/app/data/cpa_auth_files/codex-<email>-free.json` |
| ④ | 写 `cpa_sync_manifest.txt` |
| ⑤ | `POST http://172.17.0.1:3000/api/accounts`（Bearer auth） |

网关无需重启，实时加载新账号并刷新配额，直接对外提供 API 转发。

查询接口返回字段为 `items`（不是 `accounts`）：

```bash
curl -H "Authorization: Bearer <AUTH_KEY>" http://134.185.81.98:3000/api/accounts
```

---

## 运行形态

| 脚本 | 用途 | 判定 |
| :--- | :--- | :--- |
| `run_batch_hotmail_pipeline.py <N>` | **完整闭环**：阶段 1-4 注册 + 阶段 5-6 接码授权 + 阶段 7 落盘 | ✅ **唯一正确入口** |
| `run_batch_existing_codex.py <N>` | 跳过注册，只对已有 `chatgpt-*.json` 补 Codex 授权 | ⛔ **禁止启动（废的）** |
| `deliver_codex.py <email>` | 单独补交付（PG + CPA + chatgpt2api） | ✅ 辅助 |
| `run_single_codex.py` | 单账号手动触发 | ✅ 辅助 |

### 失败黑名单

`run_batch_existing_codex.py` 维护 `/app/data/codex_failed_accounts.json`：
同一账号累计失败 ≥2 次（`CODEX_FAIL_SKIP=2`）永久跳过，避免重启后反复卡在同一批坏号上。

---

## 关键环境变量

```bash
SMS_COUNTRY=16,73,117,25
SMS_CODE_WAIT=150
SMS_MAX_RETRIES=10
EMAIL_SOURCE=outlook,cloudflare
```

---

## 已知瓶颈

| 环节 | 状态 |
| :--- | :--- |
| 代理分配 | 稳定（一号一IP，机房 IP 自动剔除） |
| 邮箱登录 | 稳定（IMAP/Graph 秒级取码） |
| Sentinel / Turnstile | 已过 |
| **手机短信** | **主要瓶颈：能进验证码页，但短信到达率不稳** |
| Token 交换 | 稳定 |
| 双端落盘 | 稳定 |

Hero-SMS 对同一 API Key 短时间连续取号存在限流，表现为伪装的 `NO_NUMBERS`。取号失败后仅等 10~13 秒即重试容易持续撞限流，必要时需加大退避。
