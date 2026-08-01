# Tasks

- [ ] Task 1: 修复 App 日志时间显示 "1970-01-01" 问题
  - 检查后端 `SystemLog` 的 JSON 序列化标签与 Android 端 `@SerialName` 是否匹配
  - 修复后端 `system_log.go` 中 `json:"ts"` → 确认正确为 `json:"ts"`
  - 修复 Android `SystemLog.kt` 中 `@SerialName("Ts") val ts` → 改为 `@SerialName("ts") val ts` 以匹配后端 JSON key
  - 验证：日志页面正常显示正确时间戳

- [x] Task 2: App 设置页退出登录按钮移动到账户信息区域
  - 修改 `fragment_settings.xml`：在 `tvUserName` 后面添加"账号管理"按钮，移除底部 `btnLogout`
  - 修改 `SettingsFragment.kt`：将退出登录逻辑绑定到"账号管理"按钮，点击后弹出确认对话框

- [ ] Task 3: App 主页非 admin 用户网络质量卡片放大
  - 修改 `DashboardFragment.kt`：非 admin 用户不再隐藏 `networkStrategyRow` 和 `networkDetailRow`，保持日志区域隐藏
  - 修改 `fragment_dashboard.xml`：非 admin 用户视角下网络质量卡片可适当增加内边距或高度

- [x] Task 4: Dashboard 仪表盘布局优化（网络质量 + 系统日志卡片同行）
  - 修改 `Dashboard.tsx`：
    - 网络质量卡片缩小（移除详细路径描述、能力指示器，保留标题 + stars + 路径标签）
    - 系统日志卡片缩小（移除展开/折叠功能，仅显示最近 3 条日志摘要）
    - 两个卡片用 `grid grid-cols-2` 共享一行
    - 网络质量卡片点击跳转 → `/network`
    - 系统日志卡片点击跳转 → 日志页面
  - 检测报警卡片保持不变

- [x] Task 5: 构建部署验证
  - 构建后端镜像并部署到 NAS
  - 构建 Web 前端并部署
  - 构建 Android APK 并推送到 NAS
  - 验证所有功能正常工作
  - 提交 Git

# Task Dependencies

- Task 1、2、3 可并行执行（互不依赖）
- Task 4 可独立并行执行
- Task 5 依赖 Task 1、2、3、4 完成