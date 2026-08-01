# Tasks

- [x] Task 1: 后端 — 移除创建用户时的 `initial_device_name` 选项
  - 修改 `user_handler.go` 中的 `createUserRequest` 结构体，移除 `InitialDeviceName` 字段
  - 修改 `user_handler.go` 中的 `Create` 方法，不再处理 `initial_device_name`
  - 修改 `user_service.go` 中的 `Create` 方法，移除 `initialDeviceName` 参数和关联的设备创建逻辑
  - 修改 `user_service.go` 中的 `CreateResult` 结构体，移除 `Device` 和 `AccessKey` 字段

- [x] Task 2: 后端 — 用户列表添加在线状态
  - 修改 `user_handler.go` 中的 `List` 方法，对每个用户查询 deviceManager 判断是否有设备在线
  - 响应中每个用户对象增加 `online: true/false` 字段
  - 需要将 `device.Manager` 注入到 `UserHandler` 中

- [x] Task 3: 后端 — 确保废弃设备 ID 可复用
  - 验证 `user_service.Delete` 中 `deviceRepo.DeleteByUser` 执行的是物理删除
  - Device 模型没有 `gorm.DeletedAt` 字段，确认 `Delete` 是物理删除

- [x] Task 4: Web — 取消设备页
  - 移除 `App.tsx` 中的 `/devices` 路由
  - 移除 `Layout.tsx` 侧边栏中的"设备"导航项
  - 删除 `Devices.tsx` 页面组件（或保留文件但不再引用）
  - 清理 `api/device.ts` 中不再使用的 API 调用（可选保留供内部使用）

- [x] Task 5: Web — 取消新增用户时的创建设备选项
  - 移除 `Users.tsx` 中的 `newDeviceName` 状态和输入框
  - 移除 `handleCreate` 中的 `initial_device_name` 传参
  - 移除创建成功后的 `access_key` 显示和复制功能

- [x] Task 6: App — 取消设备页
  - 移除底部导航栏中的设备 tab（`MainActivity.kt` 中的 navigation 配置）
  - 移除 `DevicesFragment.kt` 和 `DeviceAdapter.kt` 文件（或标记为不再使用）
  - 移除 `item_device_binding.xml` 等布局文件（如果不再被引用）

- [x] Task 7: 部署测试
  - 构建后端镜像并部署到 NAS
  - 构建 web 前端并部署
  - 构建 Android APK 并推送到 NAS
  - 验证：用户列表显示在线状态
  - 验证：创建用户不再有设备选项
  - 验证：设备页不再可访问
  - 提交 Git

# Task Dependencies
- [Task 1] 和 [Task 2] 可以并行执行（后端修改）
- [Task 4] 依赖 [Task 1]（Web 后端 API 先改好）
- [Task 5] 依赖 [Task 1]（后端去掉了 `initial_device_name`）
- [Task 6] 独立执行
- [Task 7] 依赖所有前置任务完成