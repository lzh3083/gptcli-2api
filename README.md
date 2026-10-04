# gptcli-2api

**ChatGPT 自动化协议注册、Codex OAuth 授权与多账号 OpenAI / Anthropic 兼容 API 网关平台**

把 **ChatGPT / Codex 账号登录态** 转成 **OpenAI / Anthropic 兼容 API**，并内置全自动注册机与现代化 Web 管理台：多 API Key、多账号轮询负载均衡、Codex OAuth 自动授权、CPA 授权文件导出、Sentinel VM 挑战破解、邮箱 OTP 自动接收。

**当前版本：v1.0** · 协议注册引擎 · Codex OAuth · CPA 凭证导出 · 端口防冲突 · Go 主进程网关

[![Release](https://img.shields.io/github/v/release/lzh3083/gptcli-2api?display_name=tag)](https://github.com/lzh3083/gptcli-2api/releases)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

---

## 核心特性

- **ChatGPT 全自动协议注册**：移植自 `turb-gpt-free-register`，内置 13 步高仿真注册协议流，集成真实 Node.js Sentinel VM 挑战计算、`curl_cffi` Chrome146 TLS 指纹伪装。
- **Codex OAuth 自动授权**：注册完成后无缝触发 Codex OAuth 授权提取，自动生成包含 `refresh_token` / `id_token` / `account_id` 的完整凭证。
- **CPA 授权文件格式导出**：凭证直接落盘到 `data/cpa_auth_files/`，生成符合 CLIProxyAPI (CPA) 标准的 `codex-{email}.json` 授权文件，支持管理台一键导出与集成。
- **NovProxy 动态住宅家宽接入**：支持白名单 API 动态提取与 UserPass 账密模式，自动组装 `socks5h` 协议节点，天然实现“一号一独立住宅物理 IP”。
- **全链路前置一致性配合 (Preflight & Consistency)**：
  - **注册路径预检**：非破坏性探测 `chatgpt.com` 与 `auth.openai.com` 连通性与 Cloudflare 状态，阻断即熔断换号，杜绝损耗；
  - **环境自适应对齐**：按代理出口地理位置自动对齐真实时区、系统 Locale 与 `Accept-Language`，杜绝指纹矛盾；
  - **求解器出站同步**：注册节点实时同步至本地 Turnstile Solver，保证打码过盾出口与业务请求出口 IP 100% 一致。
- **端口彻底防冲突**：
  - **Go 主网关 / Web 管理台**：默认 **`8081`**（避免与 grokcli 的 `3000` 冲突）
  - **Python 注册 / SSO Sidecar**：默认 **`18080`**（避免与 grokcli 的 `18070` 冲突）
  - **Turnstile 验证码解题服务**：默认 **`5073`**（避免与 grokcli 的 `5072` 冲突）
- **双协议高性能 API 网关**：Go 编写的高并发网关，原生兼容 OpenAI（`/v1/chat/completions`、`/v1/models`）与 Anthropic（`/v1/messages`）格式。
- **大账号池负载均衡与冷却治理**：支持 `round_robin` / `least_used` / `random` 轮询策略；异常与限流自动踢出冷却；会话粘性（Prompt Cache 亲和）。
- **PostgreSQL + Redis 生产级存储**：账号池状态、用量统计、任务日志落盘存储，多 Worker 共享热状态。
- **全功能 Web 管理控制台**：直观的账号池视图、实时批量注册进度仪表盘、API Key 权限管理、流式调用耗时与用量明细统计。

---

## 架构

```
客户端 (OpenAI SDK · Anthropic SDK · NextChat · LobeChat · new-api · sub2api · Claude Code)
        │  /v1/chat/completions  ·  /v1/models  ·  /v1/messages
        ▼
   gptcli-2api  (Go 主进程 · 端口 8081 · multi-worker)
        │
        ├─ Web 管理后台 (/admin)
        ├─ 账号池轮询 · 额度与状态监测 · 自动冷却 · 会话粘性
        ├─ PostgreSQL (账号数据 / Key / 任务日志 / 系统设置)
        ├─ Redis (分布式锁 / 计数 / 会话 / 注册状态)
        │
        ├─ Python Sidecar (端口 18080 loopback)
        │     ├─ ChatGPT 协议注册机 (openai_client/register.py)
        │     ├─ Node.js Sentinel Runner (sentinel-runner.js + sdk.js)
        │     ├─ Codex OAuth 授权提取 (core/codex_oauth.py)
        │     ├─ 邮箱接收器 (MoeMail / GPTMail / CF Temp Email / IMAP / Outlook)
        │     └─ CPA Auth-Files 凭证落盘 (data/cpa_auth_files/)
        │
        └─ Turnstile Solver (端口 5073 loopback)
              └─ 验证码过盾求解器 (Camoufox / YesCaptcha)
```

---

## 端口配置映射（与 grokcli 并存设计）

| 服务组件 | 默认端口 | 容器内绑定 | 环境变量 | 说明 |
| :--- | :--- | :--- | :--- | :--- |
| **API 网关 / Admin 控制台** | **`8081`** | `0.0.0.0:8081` | `GROK2API_PORT=8081` | 对外服务端口，可自由配置 |
| **注册 / SSO Sidecar** | **`18080`** | `127.0.0.1:18080` | `GROK2API_REGISTRATION_PORT=18080` | 本地 loopback，不对外暴露 |
| **Turnstile Solver** | **`5073`** | `127.0.0.1:5073` | `TURNSTILE_PORT=5073` | 本地 loopback，不对外暴露 |

---

## 快速开始

### 方式 A：Docker Compose 部署（推荐）

```bash
# 1. 克隆代码仓库
git clone https://github.com/lzh3083/gptcli-2api.git
cd gptcli-2api

# 2. 复制并编辑环境变量
cp .env.example .env
# 建议修改 GROK2API_ADMIN_PASSWORD（管理台密码）

# 3. 启动全套服务
docker compose up -d --build

# 4. 检查服务健康状态
curl -fsS http://127.0.0.1:8081/health
```

- **Web 管理控制台**：打开浏览器访问 `http://127.0.0.1:8081/admin`
- **默认管理密码**：`.env` 中的 `GROK2API_ADMIN_PASSWORD`

### 方式 B：本地环境直接运行

#### 环境要求
- Go `>= 1.22`
- Python `>= 3.10`（带虚拟环境）
- Node.js `>= 18`
- PostgreSQL & Redis

```bash
cd gptcli-2api

# 1. 编译 Go 二进制程序
mkdir -p bin
go build -o bin/grok2api ./cmd/grok2api
go build -o bin/grok2api-migrate ./cmd/grok2api-migrate

# 2. 配置 Python 依赖
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt

# 3. 运行启动脚本
./start.sh
```

---

## ChatGPT 协议注册与 CPA 凭证

### 1. 注册协议执行流
1. **网络与环境预检**：初始化 TLS 模拟与浏览器环境。
2. **首页匿名预热**：建立合法 Session Cookie 依赖。
3. **认证前置链**：获取 Provider 列表与 CSRF 防伪凭证。
4. **发起 OAuth Signin**：获取 OpenAI Authorize 链接。
5. **Sentinel 挑战求解**：通过内联 Node.js VM 挑战计算并构建 Sentinel 请求头。
6. **提交用户注册**：提交账号邮箱与生成的安全强密码。
7. **邮箱验证码提取**：支持 MoeMail、GPTMail、Cloudflare 临时邮箱、IMAP 等多种渠道自动拉取 6 位数字 OTP。
8. **完善个人资料**：随机提交合规英文姓名与成年出生日期。
9. **建立登录态**：跟随回调提取 `accessToken`。
10. **触发 Codex OAuth**：自动走 Codex 授权流程，换取完整 OAuth Token。

### 2. CPA Auth-Files 凭证结构
导出的凭证文件保存在 `data/cpa_auth_files/`，格式如下：

```json
{
  "id_token": "eyJhbGciOiJSUzI1NiIs...",
  "access_token": "eyJhbGciOiJSUzI1NiIs...",
  "refresh_token": "rt-xxxxxxxxxxxx",
  "account_id": "user-xxxxxxxxxxxx",
  "last_refresh": "2026-10-04T16:38:51Z",
  "email": "user@example.com",
  "type": "codex",
  "expired": "2026-10-04T17:38:51Z"
}
```

- 该文件可直接放入 **CLIProxyAPI (CPA)** 的 `auth-files` 目录供 CPA 作为上游使用。
- 也可以直接在 Web 管理后台「系统设置 / 账号导出」中一键打包下载。

---

## 客户端接入示例

### OpenAI SDK (Python)
```python
from openai import OpenAI

client = OpenAI(
    api_key="your-api-key",  # 在 Web 控制台「API Keys」页面生成的 Key
    base_url="http://127.0.0.1:8081/v1",
)

response = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "你好，请做个自我介绍"}],
)
print(response.choices[0].message.content)
```

### cURL 请求
```bash
curl http://127.0.0.1:8081/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

---

## 支持的模型列表

网关内置支持以下 OpenAI 官方最新主力模型路由与智能别名映射：
- **GPT-4o 旗舰与全模态**：`gpt-4o`（默认模型，128k 上下文）、`gpt-4o-mini`（高性价比小型模型）、`chatgpt-4o-latest`（动态跟踪 ChatGPT 官方最新版本）
- **o 系列深度推理主力**：`o3-mini`（新一代高效推理旗舰，支持 reasoning_effort 思考强度）、`o1`（旗舰深度推理模型）、`o1-mini`（快速逻辑与代码推理）、`o1-preview`（早期推理预览版）
- **前沿研究与经典旗舰**：`gpt-4.5-preview`（超大研究级前沿模型）、`gpt-4-turbo`（128k 经典旗舰）、`gpt-4`（8k 经典模型）、`gpt-3.5-turbo`（经典轻量对话）
- **多模态生图与向量嵌入**：`dall-e-3` / `dall-e-2`（官方高分辨率画图）、`text-embedding-3-small` / `text-embedding-3-large` / `text-embedding-ada-002`（文本向量嵌入）

---

## 开源协议

本项目采用 [Apache License 2.0](LICENSE) 开源协议。
仅供技术研究与学习测试使用，请严格遵守 OpenAI 服务条款与当地相关法律法规。
