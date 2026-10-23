# Changelog

All notable changes to `gptcli-2api` will be documented in this file.

---

## [v1.3.1] - 2026-10-23

### 🛡️ 手机验证码提交健壮性重构与两级救卡机制 (Phone OTP Robust Submit & Two-Stage Salvage Mechanism)

- **根治 `otp_submit_unresponsive` 导致的接码费用浪费缺陷**：
  - **问题根因解决**：排查并修复了在 OpenAI 手机验证码（`phone-verification`）页面上，已成功收到接码平台短信并填入 OTP，但因前端 React 状态未同步或按钮点击未穿透导致页面卡死、原逻辑干等 60 秒后直接判为超时废弃，而接码平台因已接收短信（HTTP 409 `OTP_RECEIVED`）锁死拒绝退款造成的资金损耗漏洞。
  - **分格 OTP 深度事件派发 (React State Sync)**：
    - 在 6 分格输入框填入单字符时，为每个输入框注入 `dispatchEvent(new Event('input', {bubbles: true}))` 与 `change` 事件；
    - 全部填完后对末尾输入框主动派发 `blur` 事件，彻底同步 React 虚拟 DOM 状态，确保提交按钮解除 `disabled` 状态。
  - **全形态与多语言按钮智能穿透 (Smart Button Penetration)**：
    - 突破仅匹配英文 `Continue`/`Verify` 的局限，支持多语言文本（`Continue`, `Verify`, `Submit`, `Confirm`, `Next`, `続行`, `次へ`, `Continuar`, `Verificar`, `确认`, `继续` 等）及 SVG 图标按钮、React Aria 按钮穿透点击。
  - **主动心跳脉冲补触发机制 (Active Pulse Trigger)**：
    - 在提交后的等待循环中废除单纯的 `time.sleep(1)` 空转，每隔 4 秒若检测到仍停留在验证码页且无报错，自动补发 `ENTER` 键与按钮点击，打破前端动画或焦点丢失引起的偶发卡顿。
  - **首创已收短信两级救卡机制 (Two-Stage Phone OTP Salvage)**：
    - **第 1 级救卡（12秒重注入）**：提交 12 秒仍停留验证码页时，自动清空所有输入框并重新按完整事件流注入手中的 `sms_code` 并重新提交；
    - **第 2 级救卡（28秒整页刷新重填）**：提交 28 秒仍未跳转时，自动执行 `driver.refresh()` 刷新页面，等待页面重新就绪后再次填入手中有效的 `sms_code` 提交；
    - **零弃卡保障**：只要验证码已到手且 OpenAI 未明确报错，流水线全力救活当前卡，彻底避免接码平台扣费而本地弃卡的情况。

- **工程版本治理规范**：
  - 同步推进 `grok2api/app.py`、`internal/buildinfo/buildinfo.go` 版本号至 `v1.3.1`，附带完整发行说明与对应 Git Tag `v1.3.1`。

---

## [v1.3.0] - 2026-10-22

### 🚀 7阶段闭环全自动注册流水线与老挝接码第一优先治理 (7-Stage Closed-Loop Pipeline & Laos SMS Priority)

- **7阶段全链路自动化注册与长效授权闭环 (7-Stage Closed-Loop Architecture)**：
  - **阶段 1：NovProxy 动态住宅 IP 提取与机房过滤**：每轮动态提取独立住宅代理（`socks5h://`），校验 `hosting=False` 排除机房 ASN，并持久化去重出口 IP。
  - **阶段 2：邮箱双源自适应与测活兜底**：严格优先从 Outlook/Hotmail 账号池领号，原子化执行微软 OAuth2 `refresh_token` 测活；空池时自动无缝切换至 Cloudflare Worker 临时邮箱（`EMAIL_SOURCE="outlook,cloudflare"`）。
  - **阶段 3-4：自适应环境对齐与稳定过盾**：按住宅 IP 出口自适应同步浏览器时区与语言指纹，以真实 Chromium 指纹对抗 Turnstile / Sentinel PoW，顺利完成协议注册并自动验证 6 位邮箱 OTP。
  - **阶段 5：Hero-SMS 手机接码与老挝绝对第一优先**：
    - 国家调度池固化为 `16,25,73,117`，注入调度队列强制重排算法，确保**首轮取号绝对优先调度老挝（+856 / 25，成本最优 $0.06）**；
    - 缺号或被拒时依次向后平滑轮退至英国（16）、巴西（73）、葡萄牙（117）；
    - 固化 240 秒深度等待窗口与满 125 秒后台自动退款机制，100% 保护账户资金；
    - 增加 HTTP 409 会话失效与 `invalid_auth_step` 即时熔断机制，杜绝无效消耗号码与余额。
  - **阶段 6：独立 Codex PKCE 授权**：在同一独立住宅代理环境下，直连 ChatGPT 授权端换取 `id_token`、`access_token` 与 `refresh_token` 长效凭证。
  - **阶段 7：多端资产持久化与热同步**：
    - 本地落盘标准 `codex-<email>-free.json`；
    - 自动持久化写入 PostgreSQL `accounts` 表（`cliproxyapi_type='codex'`）；
    - 自动同步至 CPA 凭证目录 `/www/wwwroot/cpa-manager-plus/cliproxyapi/auths/`；
    - 自动调用热同步接口更新至本地 `chatgpt2api`（端口 3000）。

- **工程版本治理规范践行**：
  - 遵循 SemVer 规范，同步推进 `grok2api/app.py`、`internal/buildinfo/buildinfo.go` 版本号至 `v1.3.0`，附带完整发行说明与对应 Git Tag `v1.3.0`。

---

## [v1.2.2] - 2026-10-17

### 🛡️ 手机验证异常状态自愈与网络抖动容灾 (Phone Flow Resilience & Network Fault Recovery)

- **修复网络抖动（ERR_SSL_PROTOCOL_ERROR）导致的虚假退出缺陷**：
  - 排查并修复了在代理网络瞬时抖动、浏览器页面进入 `chrome-error://chromewebdata/` 异常状态时，代码因找不到手机输入表单误判为“已离开手机验证页”而提前退出，导致在后续阶段白白等待 180 秒超时的严重逻辑漏洞；
  - 增加对异常页面 URL（`chrome-error`、`about:blank`）与真实后续授权页（`workspace`、`consent`、`callback`）的严格状态判别矩阵；
  - 遇到页面异常时自动强制重新导航至 `https://auth.openai.com/add-phone` 并无缝继续执行换号重试，保证多轮接码闭环平滑走完。

- **工程版本治理规范持续践行**：
  - 严格履行代码提交 GitHub 前必须同步 SemVer 版本号递增（v1.2.2）、CHANGELOG 详尽记录与对应版本 Git Tag 的发布铁律。

---

## [v1.2.1] - 2026-10-17

### 📱 接码策略重构与工程版本治理规范 (SMS Country Strategy & Version Governance)

- **接码国家轮询池精准优化（英国第一优先，巴西第二备选）**：
  - 彻底剔除接码成功率极低的菲律宾（`4`）与印尼（`6`）虚拟卡号段，以及欧洲葡萄牙（`117`）；
  - 国家轮换策略确立为 `SMS_COUNTRY="16,73"`：首选英国（`16`），巴西（`73`）主力实体卡号段高可用轮换；
  - 彻底解决 OpenAI 后端短信网关（Twilio/Carrier Lookup）对廉价虚拟号静默弃包（Silent Drop）导致的无限死等问题。

- **短信路由与重试时间窗口标准化**：
  - 单号等待时间严格设定为 `SMS_CODE_WAIT=120` 秒（保障国际跨网短信充分路由下发）；
  - 单账号换号重试上限设为 `SMS_MAX_RETRIES=10` 次；
  - 轮询间隔保持 `SMS_POLL_INTERVAL=5` 秒，兼顾实时性与平台配额安全。

- **浏览器自动化与表单提交流程鲁棒性增强**：
  - 优化 React-Aria 电话输入框国旗选择与掩码适配，支持 E.164 规范多位数精准回填；
  - 增加候选 Continue 提交按钮评分机制与 `form.requestSubmit()` 自动回退，消除前端点击假死；
  - 优化 NovProxy 住宅节点分配策略与一号一 IP 释放机制。

- **工程标准与 Git 版本发布治理铁律**：
  - 建立“每次代码变更推送到 GitHub 必须同步执行 SemVer 版本号递增、CHANGELOG 详细版本说明与 Git Tag 标记”的工程规范；
  - 保持 `internal/buildinfo.Version`、`grok2api/app.py APP_VERSION` 与 `git tag` 三方严格同频一致。

---

## [v1.2.0] - 2026-10-17

### 🛡️ 纯注册机深度加固、高可用守护与流程重构 (Pure Registration Hardening & Process Supervisor)

- **项目定位厘清与原生 GPTCLI_* 命名规范**：
  - 彻底厘清与 `grokcli-2api` 的定位差异，确立纯 ChatGPT 自动化注册与 CPA 凭证导出专有定位；
  - 引入 `GPTCLI_*` 环境变量规范，并自动双轨兼容 `GROK2API_*`；
  - 默认开启 `GPTCLI_MODE=register-only` 纯注册机模式，跳过所有未使用的 LLM API 网关路由，实现零攻击面与零冗余资源占用。

- **多进程自治守护与容灾自愈 (supervisord Integration)**：
  - 引入 `/app/deploy/supervisord.conf` 进程看门狗，彻底终结旧版 entrypoint 裸后台进程在求解器或侧车崩溃时无人拉起、导致批量注册静默中断的问题；
  - 将 `turnstile-solver`、`registration-sidecar` 与 `gptcli-2api` 主服务统一纳入进程监管与优雅回收。

- **全链路注册可观测性与转化漏斗 (Registration Funnel & Observability)**：
  - 新增 `GET /admin/api/accounts/register-email/funnel` 接口；
  - 将散落日志提炼为 7 大标准错误分类（打码失败、Sentinel PoW 挑战未过、邮箱取信超时、接码缺号、二手邮箱、风控阻断、代理网络故障）；
  - 提供多阶段漏斗转化率实时统计，直接为住宅代理与接码号段优化提供量化支撑。

- **Python 策略模式重构与上游语法错误修复**：
  - 在 `email_provider.py` 引入统一的 `_EmailStrategy` 策略注册表，解耦 9 家邮箱服务商同构逻辑，收敛 300+ 行重复代码；
  - 提取 `BaseRegistrationFlow` 流程抽象与 `RegistrationResult` 标准结构；
  - 排查并彻底修复上游在 `model_health.py` 与 `token_maintainer.py` 桩实现中残留的缩进语法破坏（`IndentationError`），全库通过 `python -m compileall` 校验。

- **全量安全加固 (P0–P2)**：
  - `clientIP()` 校验可信代理网段（`GROK2API_TRUSTED_PROXIES`），彻底封堵伪造 XFF 冒充 loopback 夺取管理员特权的攻击链；
  - 管理员初始化接口引入本地一次性安全令牌 `<dataDir>/admin-setup.token`；
  - Turnstile Solver 默认绑定 127.0.0.1、强制密钥验证并拦截私网 SSRF；
  - 37 处请求解码强制 16MiB 长度上限；
  - Config 热更新采用原子快照消除读写竞态；
  - TOTP 密钥、OAuth 授权码与 Token 全面指纹化脱敏；
  - 默认关闭容器内 Docker 热更新并开启 `no-new-privileges:true`；
  - 管理台登录增加爆破限流（5 次失败锁 10 分钟）；恢复强制 TLS 校验。

- **工程治理与 CI 门禁升级**：
  - 在 GitHub Actions 新增 `python-gate`（全量 compileall 语法拦截、单元与协议契约测试）与 `docker-smoke` 容器构建校验；
  - 锁定 Python 运行依赖于 `requirements.lock`。

---

## [v1.1.0] - 2026-10-06

### 🎯 纯注册机模式架构重构与 CPA 凭证独占保障 (Pure Registration & CPA Sovereignty)

- **纯注册机定位彻底固化 (Pure Registration Engine Transition)**：
  - 将项目运行定位彻底重构为**纯粹的账号注册机**，完全剥离与关闭账号的定时探活、模型可用性巡检与自动续期任务；
  - 彻底禁用 Go 内核常驻 `Maintainer` 协程与 `ModelHealth` 探活组件（`GROK2API_GO_MAINTAINER=0`、`GROK2API_TOKEN_MAINTAIN=0`、`GROK2API_MODEL_HEALTH=0`）；
  - 全面熔断管理后台的“刷新选中账号”、“全量刷新”、“模型探测”等 API 路由，即便在 Web UI 手动点击或脚本触发也会在接口层直接拦截，避免误触发外部刷新操作。

- **CPA 凭证生命周期独占保障 (Codex OAuth Refresh Sovereignty)**：
  - 彻底规避 OpenAI OAuth Refresh Token 单次消耗轮换规则（Single-Use Token Rotation）导致的并发竞争作废问题；
  - 账号注册并完成 Codex PKCE 授权后，凭证文件一次性出厂落盘至 `data/cpa_auth_files/codex-{email}.json`，本地杜绝任何后续二次请求，凭证的刷新与健康维护完全由下游新加坡 CPA 独占接管。

- **优质接码国家轮换池优化 (England / Brazil / Portugal Rotation Pool)**：
  - 优化短信服务商（SMSBower / Hero-SMS）的号码轮换配置，默认按英格兰 (`16`)、巴西 (`73`)、葡萄牙 (`117`) 等高通过率号段智能轮询；
  - 在 `add-phone/send` 与 `phone-otp/validate` 环节全面注入真实 Sentinel PoW 挑战防欺诈 Token，有效绕过官方 `fraud_guard` 400 校验。

- **Cloudflare 临时邮箱全局拉取增强 (CF Mail Admin Fallback)**：
  - 增强 Cloudflare Temp Mail 邮件拉取逻辑，支持在缺少独立 Session JWT 的场景下自动使用管理员秘钥全局回溯 `/admin/mails` 邮件流，并在应用层精准过滤目标地址，彻底解决登录验证码拉取超时。

---

## [v1.0] - 2026-10-04

### 🚀 核心架构与里程碑发布 (Initial Release)

- **ChatGPT 自动化协议注册引擎 (Headless Protocol Engine)**：
  - 完整解耦并移植 `turb-gpt-free-register` 核心 13 步高仿真注册协议流；
  - 集成真实 Node.js Sentinel VM 运行时 (`sentinel-runner.js` + `sdk.js`)，自动完成 OpenAI 防护挑战计算；
  - 基于 `curl_cffi` 模拟 Chrome 146 级 TLS / JA3 / HTTP2 指纹，规避反爬阻断；
  - 支持多渠道（MoeMail、GPTMail、Cloudflare 临时邮箱、IMAP 等）自动接收并精准抽取 6 位数字 OTP；
  - 自动生成符合 OpenAI 校验规则的高强度密码与合规成年人身份信息。

- **Codex OAuth 自动授权与 CPA 凭证导出 (Codex OAuth & CPA Files)**：
  - 注册成功后自动触发 Codex OAuth 授权交互，提取完整 Token 凭证（`access_token`、`refresh_token`、`id_token`、`account_id`）；
  - 自动按 CLIProxyAPI (CPA) 规范落盘凭证文件至 `data/cpa_auth_files/codex-{email}.json`；
  - 支持在 Web 管理后台一键批量下载或推送到第三方平台。

- **NovProxy 动态住宅家宽与前置一致性配合 (NovProxy & Preflight Consistency)**：
  - 支持 NovProxy API 白名单提取与 UserPass 账密生成模式，自动拼装 `socks5h` 格式，保障一号一独立住宅 IP；
  - 集成 ChatGPT / OpenAI 专用非破坏性路径预检（`_preflight_chatgpt_path`），提前拦截 Cloudflare 阻断风险；
  - 自动探测出口 IP 地理位置，自适应对齐时区、Locale 与 Accept-Language；
  - 毫秒级同步代理至本地 Turnstile Solver 求解器，保证求解端与业务请求端出口 IP 100% 一致。

- **多服务端口隔离防冲突设计 (Independent Ports Architecture)**：
  - 全面调整服务监听端口，支持与现存 `grokcli-2api` 无缝共存：
    - **Go API 网关 / Web 管理台**：默认 **`8081`**
    - **Python 注册 / SSO Sidecar**：默认 **`18080`**
    - **Turnstile 验证码解题服务**：默认 **`5073`**

- **Go 高性能双协议 API 网关**：
  - 原生提供标准 OpenAI 接口（`/v1/chat/completions`、`/v1/models`）与 Anthropic 接口（`/v1/messages`）；
  - 默认映射与支持 OpenAI 主力模型矩阵：`gpt-4o`、`gpt-4o-mini`、`chatgpt-4o-latest`、`o1`、`o1-mini`、`o3-mini`、`text-embedding-3-small`；
  - 账号池支持 `https://auth.openai.com::{user_id}` 规范命名，具备 `round_robin` / `least_used` / `random` 轮询与异常冷却机制。

- **Web 管理控制台与任务运维**：
  - 提供可视化的账号池状态、实时注册任务流、API Key 生成与管理、用量统计及模型探测面板。
