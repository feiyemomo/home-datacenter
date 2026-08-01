# Tasks

- [x] Task 1: 删除设置中的管理员功能区域
  - 移除 SettingsFragment.kt 中管理员功能相关 UI 和逻辑（用户管理入口等）
  - 移除 fragment_settings.xml 中管理员区域布局

- [x] Task 2: 修复用户管理 tab FAB 位置
  - 调整 activity_users.xml 中 FAB 的 marginBottom 从 20dp 改为 76dp，避免被底部导航栏遮挡

- [x] Task 3: 摄像头控制页隐藏 PTZ/预设位
  - 在 CameraDetailActivity.kt setupPtz() 中根据 camera.hasPtz 控制 PTZ 标题、PTZ 面板、预设位标题、预设位容器的可见性
  - 为 activity_camera_detail.xml 中的 PTZ 标题、预设位标题、预设位容器添加 ID

- [x] Task 4: 合并摄像头信息到控制页顶部
  - 将 strings.xml 中 camera_detail_title 从"摄像头控制"改为"摄像头相关信息"
  - 摄像头信息已在 cardHeader 中展示，无需独立标签页

# Task Dependencies

- 无依赖关系，Task 1-4 可并行执行