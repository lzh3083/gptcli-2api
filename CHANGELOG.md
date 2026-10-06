# Changelog

All notable changes to `gptcli-2api` will be documented in this file.

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
