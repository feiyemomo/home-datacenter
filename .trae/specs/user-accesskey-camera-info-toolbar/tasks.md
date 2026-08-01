# Tasks

- [x] Task 1: 后端创建用户接口返回 access-key
  - 修改 `POST /api/v1/user` handler，在成功创建用户后返回 `access_key` 字段
  - 确认 service 层返回 access-key

- [x] Task 2: Dashboard 创建用户后展示 access-key
  - 修改 `Users.tsx` 的创建用户回调，成功后在弹窗中显示 access-key 和复制按钮

- [x] Task 3: App 创建用户后展示 access-key
  - 修改 `UsersFragment.kt` 的 `showCreateUserDialog()`，成功创建后显示 access-key 对话框

- [x] Task 4: App 摄像头信息整合到工具栏
  - 移除 `activity_camera_detail.xml` 中的 `cardHeader`（播放器下方的信息卡片）
  - 修改 `CameraDetailActivity.kt` 的 `setupHeader()`，工具栏 title 保持为"摄像头控制"，subtitle 动态显示摄像头名称、状态和厂商信息

- [x] Task 5: App 新增用户按钮移至工具栏右上角
  - 移除 `activity_users.xml` 中的 `fabAddUser` FAB
  - 在 `UsersFragment.kt` 的 toolbar 添加菜单项或导航按钮，点击后弹出创建用户对话框

# Task Dependencies

- Task 1 是 Task 2 和 Task 3 的前置依赖（后端接口改了，前端才能展示）
- Task 4 和 Task 5 可并行执行，与 Task 1-3 也无依赖