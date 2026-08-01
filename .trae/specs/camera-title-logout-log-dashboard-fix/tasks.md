# Tasks

- [x] Task 1: App 摄像头工具栏 title 改为摄像头名称
  - 修改 `CameraDetailActivity.kt` 的 `setupHeader()` 方法，将 `binding.toolbar.title` 设置为摄像头名称，subtitle 保持显示状态信息
  - 不再需要修改 `strings.xml` 中的 `camera_detail_title`，因为 title 将在运行时动态设置

- [x] Task 2: App 退出登录按钮隐蔽化
  - 修改 `fragment_settings.xml` 中 `btnLogout` 的样式：缩小尺寸（如高度改为 40dp）、去掉红色描边、改为次要颜色或灰色文字
  - 可选：改为 TextButton 样式，放置在一个更不显眼的位置

- [x] Task 3: Web Dashboard 非 admin 用户隐藏日志区域
  - 在 `Dashboard.tsx` 中读取当前用户权限（通过 `useAuth` 或类似机制）
  - 非 admin 用户不显示系统日志卡片（`Card` 组件从 `系统日志` 开始到 `系统快照` 之前）

- [ ] Task 4: 排查并修复已删除用户 ID 复用问题
  - 检查 `user_service.go` 的 `Delete` 方法确保级联删除设备时使用物理删除
  - 检查 `device_repository.go` 的 `DeleteByUser` 方法确认是物理删除
  - 验证 User 和 Device 模型无 `DeletedAt` 字段
  - 验证 SQLite 自增 ID 机制在新记录插入时是否确实复用已删除的 ID
  - 如果存在 `gorm.DeletedAt` 软删除，改为物理删除
  - 部署后通过测试验证 ID 复用

- [ ] Task 5: 排查 Dashboard 日志显示问题
  - 检查 `listSystemLogs` API 调用是否正确返回数据
  - 检查 `SystemLog` 类型定义与后端返回字段是否匹配
  - 检查渲染逻辑中是否有字段名不匹配或其他 bug
  - 检查后端 API 路径、权限、数据源等问题
  - 使用 Chrome DevTools 验证修复后的效果

- [x] Task 6: 构建部署验证
  - 构建后端镜像并部署到 NAS
  - 构建 Web 前端并部署
  - 构建 Android APK 并推送到 NAS
  - 验证所有功能正常工作
  - 提交 Git

# Task Dependencies

- Task 4 是 Task 6 的前置依赖（后端修复后需要部署）
- Task 1、2、3、5 可并行执行
- Task 6 依赖所有前置任务完成