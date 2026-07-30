# IPv6 全链路测试与开发脚本整合 Spec

## Why

v1.8.7 完成了网络策略审查与前端 IPv6/UX 修复，但本机当时无 IPv6，无法验证 IPv6 直连路径（`http://[<ipv6>]:8088/`）的实际效果。现在本机已支持 IPv6，可以补全此前缺失的测试。同时，项目中有便捷的 PowerShell 脚本散落在子目录（`services/api/scripts/test_ws.ps1`），频繁操作（如 git 提交）缺少一键脚本，导致重复手动步骤。

## What Changes

- **Move** `services/api/scripts/test_ws.ps1` → 项目根目录 `test-ws.ps1`（更新路径引用与文档）
- **Add** `get-token.ps1`（项目根目录）— 一键获取测试 JWT，供 API 调试使用
- **Add** `commit.ps1`（项目根目录）— 交互式 git add → 输入 commit message → commit → push 一键流程
- **Test** IPv6 直连路径（`http://[<nas-ipv6>]:8088/`）的 Dashboard、Network 页面、LiveVideo 传输方式选择器
- **Test** 从中继路径点击"切换到 IPv6 直连"链接跳转到 IPv6 直连的实际效果
- **Update** `PROMPT.md` 增加新脚本的使用说明
- **Update** `docs/ai-context.md` 新增 Phase 13 记录
- **Update** `README.md` 新增 v1.8.8 更新日志

## Impact

- Affected specs: `network-policy-review`（v1.8.7 修复的 IPv6 直连路径此前未验证，本 spec 补全验证）
- Affected code:
  - `services/api/scripts/test_ws.ps1` → `test-ws.ps1`（移动）
  - `get-token.ps1`（新增）
  - `commit.ps1`（新增）
  - `PROMPT.md`（更新脚本清单）
  - `docs/ai-context.md`（新增 Phase 13）
  - `README.md`（新增 v1.8.8）

## ADDED Requirements

### Requirement: 项目根目录便捷脚本集合

项目根目录 SHALL 提供以下一键脚本，覆盖开发与运维高频操作：

| 脚本 | 用途 | 已有/新增 |
|---|---|---|
| `deploy-nas.ps1` | 部署到 NAS | 已有 |
| `test-ws.ps1` | WebSocket 连接测试 | 移动自 `services/api/scripts/test_ws.ps1` |
| `get-token.ps1` | 获取测试 JWT | 新增 |
| `commit.ps1` | 交互式 git 提交并推送 | 新增 |

#### Scenario: 获取测试 JWT
- **WHEN** 用户在项目根目录运行 `.\get-token.ps1`
- **THEN** 脚本使用内置测试 AccessKey 调用 `/api/v1/auth/bind`，输出 JWT token
- **WHEN** 用户运行 `.\get-token.ps1 -BaseUrl "https://dashboard.feiyemomo.top"`
- **THEN** 脚本从中继路径获取 token

#### Scenario: 交互式 git 提交
- **WHEN** 用户在项目根目录运行 `.\commit.ps1`
- **THEN** 脚本显示 `git status --short`、暂存所有已修改/新增文件、打开编辑器或提示用户输入 commit message、提交并推送到远程
- **WHEN** 用户运行 `.\commit.ps1 -Message "fix: ..."` 
- **THEN** 脚本使用提供的 message 直接提交并推送（非交互模式）
- **WHEN** 用户运行 `.\commit.ps1 -DryRun`
- **THEN** 脚本仅显示将要提交的文件和 message，不实际提交

### Requirement: IPv6 直连路径全链路验证

使用 chrome-devtools MCP 验证 IPv6 直连路径（`http://[<nas-ipv6>]:8088/`）的以下行为：

#### Scenario: Dashboard 通过 IPv6 直连访问
- **WHEN** 浏览器导航到 `http://[<nas-ipv6>]:8088/`
- **THEN** Dashboard 正常加载
- **AND** 网络质量卡片显示"IPv6 直连"（而非"局域网"或"远程"）
- **AND** 质量评分为 5 星

#### Scenario: Network 页面在 IPv6 直连下显示正确
- **WHEN** 在 IPv6 直连路径下访问 Network 页面
- **THEN** 连接模型卡片不显示"切换到 IPv6 直连"链接（因为已在直连路径）
- **AND** IPv6 连通性部分显示服务器 IPv6 可达

#### Scenario: LiveVideo 在 IPv6 直连下默认使用 WebRTC
- **WHEN** 在 IPv6 直连路径下打开摄像头直播
- **THEN** LiveVideo 默认传输方式为"自动"（WebRTC 优先）
- **AND** 不是"HLS"（v1.8.7 修复的核心目标）

#### Scenario: 中继路径切换到 IPv6 直连
- **WHEN** 在中继路径（`dashboard.feiyemomo.top`）的 Network 页面点击"切换到 IPv6 直连 →"链接
- **THEN** 浏览器在新标签页打开 `http://[<nas-ipv6>]:8088/`
- **AND** 新标签页正常加载 Dashboard

## MODIFIED Requirements

### Requirement: PROMPT.md 脚本清单

`PROMPT.md` 的"部署脚本"章节 SHALL 扩展为"便捷脚本"章节，列出项目根目录所有一键脚本及其用法。

