# Tasks

- [x] Task 1: 移动 `test_ws.ps1` 到项目根目录
  - [x] SubTask 1.1: 将 `services/api/scripts/test_ws.ps1` 移动到项目根目录 `test-ws.ps1`（使用 git mv 保留历史）
  - [x] SubTask 1.2: 更新脚本头部注释，反映新路径和用法
  - [x] SubTask 1.3: 检查并更新文档中对旧路径的引用

- [x] Task 2: 创建 `get-token.ps1`（项目根目录）
  - [x] SubTask 2.1: 编写脚本，使用内置测试 AccessKey（`ebc94f7fe99b497a9bdec7bd45add929360e4386be3cad96a8b3922d1d680a05`，user_id=1）调用 `/api/v1/auth/bind`
  - [x] SubTask 2.2: 支持 `-BaseUrl` 参数（默认 `http://192.168.31.234:8080`），支持 LAN/中继/IPv6 三种路径
  - [x] SubTask 2.3: 输出 JWT token 到 stdout，同时设置 `$env:HC_TOKEN` 方便后续 API 调用
  - [x] SubTask 2.4: 包含 `-Copy` 选项，自动将 token 复制到剪贴板

- [x] Task 3: 创建 `commit.ps1`（项目根目录）
  - [x] SubTask 3.1: 编写交互式脚本：显示 `git status --short` → `git add -A` → 提示用户输入 commit message → `git commit` → `git push`
  - [x] SubTask 3.2: 支持 `-Message "..."` 参数（非交互模式，直接使用提供的 message）
  - [x] SubTask 3.3: 支持 `-DryRun` 选项（仅显示将要提交的文件和 message，不实际操作）
  - [x] SubTask 3.4: 在提交前显示 diff 统计（`git diff --stat HEAD`）供用户确认
  - [x] SubTask 3.5: 推送后显示远程提交链接或确认信息

- [x] Task 4: IPv6 直连路径全链路测试（chrome-devtools MCP）
  - [x] SubTask 4.1: 获取 NAS 当前公网 IPv6 地址（通过 `get-network-status` API 或 SSH 查询）
  - [x] SubTask 4.2: 使用 chrome-devtools 导航到 `http://[<nas-ipv6>]:8088/`，登录，验证 Dashboard 网络卡片显示"IPv6 直连" + 5 星
  - [x] SubTask 4.3: 在 IPv6 直连路径下访问 Network 页面，验证不显示"切换到 IPv6 直连"链接
  - [x] SubTask 4.4: 在 IPv6 直连路径下打开摄像头直播，验证 LiveVideo 默认传输方式为"自动"（WebRTC 优先），非 HLS
  - [x] SubTask 4.5: 在中继路径（`dashboard.feiyemomo.top`）的 Network 页面，验证"切换到 IPv6 直连 →"链接的 href 指向正确的 IPv6 地址
  - [x] SubTask 4.6: 检查浏览器控制台无应用错误
  - [x] SubTask 4.7: 发现并修复 NAS_IPV6_ADDRESS 过时问题（ISP 前缀轮换 37a3→37a4，compose.yaml 默认值已更新）

- [ ] Task 5: 更新文档
  - [ ] SubTask 5.1: 更新 `PROMPT.md` — 将"部署脚本"章节扩展为"便捷脚本"章节，列出 `deploy-nas.ps1`、`test-ws.ps1`、`get-token.ps1`、`commit.ps1` 的用法
  - [ ] SubTask 5.2: 更新 `docs/ai-context.md` — 新增 Phase 13 记录（脚本整合 + IPv6 全链路验证结果）
  - [ ] SubTask 5.3: 更新 `README.md` — 新增 v1.8.8 更新日志

- [ ] Task 6: Git 提交并推送
  - [ ] SubTask 6.1: 使用新建的 `commit.ps1` 脚本提交所有变更
  - [ ] SubTask 6.2: 验证推送成功

# Task Dependencies

- [Task 4] depends on [Task 1], [Task 2], [Task 3]（脚本应先就位，测试中可使用 get-token.ps1 获取 token）
- [Task 5] depends on [Task 1], [Task 2], [Task 3], [Task 4]
- [Task 6] depends on [Task 5]
- [Tasks 1, 2, 3] 相互独立，可并行执行
