# Changelog

All notable changes to `gptcli-2api` will be documented in this file.

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
