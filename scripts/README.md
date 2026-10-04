# scripts/

运维与 Python sidecar 脚本。公开 API / 管理台主路径已迁到 Go。

## 构建 / 运维

| 路径 | 说明 |
|------|------|
| `build_admin_assets.py` | 管理台静态资源打包（`static/js` → `static/dist`） |
| `upgrade_from_file_backend.sh` | file 后端 → hybrid（PG/Redis）升级迁移 |
| `smoke_go_messages.sh` | Go messages 冒烟 |

JSON → PG 迁移请用 Go：

```bash
go run ./cmd/grok2api-migrate
# 或镜像内
/app/bin/grok2api-migrate
```

## Python sidecar（必须保留）

| 路径 | 说明 |
|------|------|
| `registration_service.py` | 注册机 + SSO 内部 HTTP（`127.0.0.1:18070`） |
| `sso_to_auth_json.py` | SSO cookie → token 设备流转换 |

相关实现：

- `grok2api/admin/sso_import.py` — SSO 导入任务
- `grok2api/upstream/grok_build_adapter.py` — 注册编排
- `turnstile-solver/` — 本地过盾
- `grok-build-auth/` — 协议注册引擎

边界说明见 `docs/ARCHITECTURE_GO_PYTHON_BOUNDARY.md` 与 `docs/PYTHON_SIDECAR.md`。

## 包结构约定

Sidecar 代码优先导入 `grok2api.*`。根目录 `sso_to_auth_json.py` 仅为兼容包装。
