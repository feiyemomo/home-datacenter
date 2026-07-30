# Home Datacenter — 可复用提示词

> 每次开始本项目工作时，先读取此文件，再读取 `README.md` 和 `docs/ai-context.md` 了解项目背景。

## 项目概览

家庭数据中心 — Go API + React Web + MQTT + Frigate NVR 的全栈 Docker Compose 项目。
详细文档：
- `README.md` — 部署、配置、运维命令
- `docs/ai-context.md` — 项目架构、Phase 历史、已知陷阱（AI 接手必读）
- `docs/api-documentation.md` — 完整 API 规格

## 生产环境

| 项 | 值 |
|---|---|
| NAS IP | `192.168.31.234` |
| SSH 用户 | `fnos-momo` |
| SSH 密码 | `@Fnos324` |
| SSH 端口 | `22` |
| 远程路径 | `/vol1/docker/home-datacenter` |
| Web UI | `http://192.168.31.234/` (LAN) 或 `https://dashboard.feiyemomo.top/` (Cloudflare Tunnel) |
| API | `http://192.168.31.234:8080/health` |

SSH 连接示例：
```powershell
ssh -p 22 fnos-momo@192.168.31.234
# 密码：@Fnos324
```

## 便捷脚本

项目根目录提供以下一键脚本，覆盖开发与运维高频操作：

| 脚本 | 用途 | 说明 |
|---|---|---|
| `deploy-nas.ps1` | 部署到 NAS | 构建并打包项目，scp 推送至 NAS 并 `docker compose up -d --build` |
| `test-ws.ps1` | WebSocket 连接测试 | 从 `services/api/scripts/` 迁移至根目录，验证 WS 通道是否畅通 |
| `get-token.ps1` | 获取测试 JWT | 调用 `/api/v1/auth/bind` 拿到 token 并写入 `$env:HC_TOKEN`，便于下游 API 调用 |
| `commit.ps1` | 一键 git 提交推送 | 交互式展示 `git status`、提示输入提交信息，提交后自动 `git push` |

### deploy-nas.ps1 — 部署到 NAS

```powershell
# 从项目根目录运行（密码模式，无需 SSH 密钥）
cd D:\Projects\home-datacenter
.\deploy-nas.ps1 -Password '@Fnos324'         # 构建并部署

# 仅更新 compose.yaml/.env（不重新构建镜像）
.\deploy-nas.ps1 -Password '@Fnos324' -NoBuild

# 部署后查看 api 日志
.\deploy-nas.ps1 -Password '@Fnos324' -Logs

# 预览将要打包的文件（不实际部署）
.\deploy-nas.ps1 -DryRun
```

脚本流程：tar 打包（排除 data/.git/node_modules/.env）→ scp 到 NAS → 远程 tar 解压 → 验证 .env 存在 → `docker compose up -d --build` → 打印服务状态。

### test-ws.ps1 — WebSocket 连接测试

```powershell
# 默认测试本地 (localhost:8080)
.\test-ws.ps1 -AccessKey "<key>"

# 测试 NAS 上的 WebSocket
.\test-ws.ps1 -AccessKey "<key>" -BaseUrl "http://192.168.31.234:8080"
```

### get-token.ps1 — 获取测试 JWT

```powershell
# 默认从 LAN 获取
.\get-token.ps1

# 从 Cloudflare 中继域名获取
.\get-token.ps1 -BaseUrl "https://dashboard.feiyemomo.top"

# 获取并复制到剪贴板
.\get-token.ps1 -Copy

# 设置 $env:HC_TOKEN 供后续 API 调用使用（需 dot-source 运行）
. .\get-token.ps1
```

> 注意：脚本会设置 `$env:HC_TOKEN`，但因 PowerShell 作用域限制，需 dot-source（`. .\get-token.ps1`）执行方可在当前 shell 中保留该环境变量。

### commit.ps1 — 一键 git 提交推送

```powershell
# 交互模式：显示 git status，提示输入提交信息，提交后自动 push
.\commit.ps1

# 非交互模式：直接传入提交信息
.\commit.ps1 -Message "fix: ..."

# 预览将要提交的内容（不实际提交）
.\commit.ps1 -DryRun
```

## Dashboard 测试用户

| 项 | 值 |
|---|---|
| User ID | `1` |
| Access Key | `ebc94f7fe99b497a9bdec7bd45add929360e4386be3cad96a8b3922d1d680a05` |

获取 JWT：
```powershell
$body = @{ user_id = 1; access_key = "ebc94f7fe99b497a9bdec7bd45add929360e4386be3cad96a8b3922d1d680a05" } | ConvertTo-Json
$resp = Invoke-RestMethod -Uri "http://192.168.31.234:8080/api/v1/auth/bind" -Method POST -Body $body -ContentType "application/json"
$token = $resp.data.token
```

登录 Dashboard：访问 `http://192.168.31.234/`，在登录页输入 user_id=1 和 access-key。

## 标准工作流

1. **了解项目** — 先读 `README.md` + `docs/ai-context.md`
2. **修改代码** — 在本地编辑，遵循项目已有风格（Go: gofmt；TS: 2-space 缩进，双引号）
3. **部署到 NAS** — `.\deploy-nas.ps1 -Password '@Fnos324'`
4. **验证** — 使用 chrome-devtools MCP 浏览 `http://192.168.31.234/` 或 `https://dashboard.feiyemomo.top/`，检查页面行为和 console
5. **更新文档** — 更新 `docs/ai-context.md` 的 Phase 记录和 `README.md` 的更新日志
6. **Git 提交** — `git add <files>` + `git commit -m "..."` + `git push`

## 常用验证命令

```powershell
# 容器状态
ssh -p 22 fnos-momo@192.168.31.234 "cd /vol1/docker/home-datacenter && docker compose ps"

# API 健康检查
curl -s http://192.168.31.234:8080/health

# 查看 api 日志
ssh -p 22 fnos-momo@192.168.31.234 "cd /vol1/docker/home-datacenter && docker compose logs --tail=50 api"

# 查看 frigate 日志
ssh -p 22 fnos-momo@192.168.31.234 "cd /vol1/docker/home-datacenter && docker compose logs --tail=50 frigate"
```

## 注意事项

- `.env` 文件不入 Git（每个环境独立维护 JWT_SECRET 和 MQTT_PASSWORD）
- `deploy/mosquitto/passwd` 不入 Git（环境特定）
- SSH 密码 `@Fnos324` 含特殊字符，PowerShell 中需用单引号包裹
- 部署脚本会自动排除 `data/`、`.git/`、`node_modules/`、`*.log` 等
- chrome-devtools MCP 可用于自动化浏览器验证（导航、截图、检查 console）
