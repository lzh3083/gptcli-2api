# Codex 账号注册全流程（7 阶段闭环）

> ⛔ **唯一正确入口：`run_batch_hotmail_pipeline.py`（完整 7 阶段端到端链路）。**
> **彻底废除并删除二阶段注册与存量补跑脚本 `run_batch_existing_codex.py`。严禁生产无手机号的单独 ChatGPT 账号。**

> 本文档记录从零生成一个可用 Codex 账号的端到端机制。
> 每个账号在完全隔离的环境下独立流转，任一阶段失败即熔断并释放资源。

## 全景架构

```text
[阶段 1: 住宅 IP 分配] ──> [阶段 2: 邮箱凭证领取] ──> [阶段 3-4: 浏览器注册]
                                                              │
        ┌─────────────────────────────────────────────────────┘
        ▼
[阶段 5: 手机接码与防风控] ──> [阶段 6: Codex PKCE 授权] ──> [阶段 7: 凭据落盘与 CPA 专用同步]
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
| 实现 | `register.py` → `_resolve_proxy_one_ip()`（或 `_resolve_novproxy_one_ip` 兼容接口） |
| 上游 | 动态住宅代理网络（自适应支持各主流动态家宽：DataImpulse、NovProxy 或自有民用代理池，支持配置国家代码如 SG / US / JP） |
| 校验 | 逐个 probe，`hosting=False` 确认民用宽带；剔除已用过的 exit_ip |
| 生命周期 | 该 IP 绑定当前账号全流程（注册 → 接码 → 授权）复用同 IP |
| 落盘 | `/app/data/used_novproxy_ips.json`、`novproxy_nodes.txt` / 节点历史（记录已用 IP，杜绝同 IP 关联） |

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
| 国家池 | `SMS_COUNTRY=4,6,66,2,16,73` (禁用马来西亚7，单价≤$0.075) |
| 最高价格 | `SMS_MAX_PRICE=0.075` |
| 轮转 | `target_country = countries[(attempt-1) % len(countries)]` |
| 提交 | 填号 → 选 SMS 通道 → 点击 Continue → 兜底 JS `form.requestSubmit()` |
| 等待 | `SMS_CODE_WAIT=180`，5 秒间隔轮询 |
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

## 阶段 7：凭据落盘与 CPA 专用同步

**核心逻辑**：成果仅同步至 CPA（CPA Manager Plus / CLI Proxy API）与数据库存档，严禁推送到任何其他 API 网关（如 chatgpt2api、sub2api 等）。

| 步骤 | 动作 |
| :--- | :--- |
| ① | 解析 `id_token` 取 `user_id` |
| ② | upsert PostgreSQL 数据库存档 |
| ③ | 写 `/app/data/cpa_auth_files/codex-<email>-free.json` |
| ④ | 写 `cpa_sync_manifest.txt` 同步清单 |
| ⑤ | 执行 `/app/scripts/sync_codex_to_cpa.sh` 自动同步至 CPA 生产池 `/www/wwwroot/cpa-manager-plus/cliproxyapi/auths/` |

CPA Manager 自动检测并热加载 `auths` 目录凭据，无需向外部 API 端口进行二次热同步。

---

## 运行形态

| 脚本 | 用途 | 判定 |
| :--- | :--- | :--- |
| `run_batch_hotmail_pipeline.py <N>` | **完整闭环**：阶段 1-4 注册 + 阶段 5-6 接码授权 + 阶段 7 落盘 CPA（同浏览器一次性完成） | ✅ **唯一正确入口** |
| `run_batch_existing_codex.py` | 历史二阶段存量补跑脚本 | ❌ **已彻底删除废除** |
| `deliver_codex.py <email>` | 单独补交付（PostgreSQL + CPA 凭据） | ✅ 辅助 |
| `run_single_codex.py <email>` | 单账号手动触发重新授权换 Token，支持 `account_deactivated` 自动清理废弃 | ✅ 辅助 |
| `auto_revive_codex.py` | **24 小时冷却复活调度器**：追踪会话吊销账号，冷冻 24h 后通过干净住宅代理重新授权换 Token；若 `account_deactivated` 则彻底物理废弃 | ✅ 自动化自愈 |

### 生产质量与自愈保障

1. **接码质量**：流水线全程严格执行手机接码绑定，未成功接码的账号不落盘凭证，直接标记失败并释放资源，从根源杜绝未绑手机号的单独 ChatGPT 账号。
2. **离线风控 24h 冷却复活与废弃**：针对新号可能遭遇的官方离线 session invalidation，自动加入 24 小时复活队列（`codex_revival_queue.json`）。满 24 小时后通过干净住宅代理调用 `run_single_codex.py` 重新换发 Token；若提示已注销（`account_deactivated`）则彻底废弃并清除本地与生产池凭证。

---

## 关键环境变量

```bash
SMS_COUNTRY=4,6,66,2,16,73
SMS_MAX_PRICE=0.075
SMS_CODE_WAIT=180
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
