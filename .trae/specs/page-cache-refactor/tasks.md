# Tasks

- [x] Task 1: 摄像头页面接入缓存
  - [x] SubTask 1.1: 将 `listCameras` 请求改为 `useCachedFetch`，key 为 `home.cameras.list`，轮询间隔 30s
  - [x] SubTask 1.2: 删除摄像头后调用 `mutate` 强制刷新缓存
  - [x] SubTask 1.3: 移除独立的 `loading` 状态，使用 `useCachedFetch` 返回的 `loading`

- [x] Task 2: 网络页面接入缓存
  - [x] SubTask 2.1: 将 `getNetworkStatus` + `checkClientIPv6` 改为 `useCachedFetch`，key 为 `home.network.status`，轮询间隔 60s
  - [x] SubTask 2.2: 手动刷新时传递 `force=true` 参数
  - [x] SubTask 2.3: 移除独立的 `loading`/`refreshing` 状态

- [x] Task 3: 设备页面接入缓存
  - [x] SubTask 3.1: 将 `listDevices` + `getSystemStatus` 改为 `useCachedFetch`，key 为 `home.devices.list`，轮询间隔 30s
  - [x] SubTask 3.2: WebSocket `device.*` 事件到达时更新缓存

- [x] Task 4: 用户页面接入缓存
  - [x] SubTask 4.1: 将 `listUsers` 改为 `useCachedFetch`，key 为 `home.users.list`，不轮询
  - [x] SubTask 4.2: 创建/删除用户后调用 `mutate` 或 `refetch` 刷新缓存

- [x] Task 5: 日志页面接入缓存
  - [x] SubTask 5.1: 当前页日志使用 `useCachedFetch` 缓存，key 为 `home.logs.page.{page}`
  - [x] SubTask 5.2: 切换分页时重新请求

- [x] Task 6: 个人中心接入缓存
  - [x] SubTask 6.1: 将 `getCurrentUser` + `listDevices` 改为 `useCachedFetch`，key 为 `home.profile.info`，轮询间隔 60s

- [x] Task 7: 构建并部署
  - [x] SubTask 7.1: 运行 `npm run build` 确认无编译错误
  - [x] SubTask 7.2: 部署到生产环境
  - [x] SubTask 7.3: 使用 Chrome DevTools 验证各页面缓存生效

# Task Dependencies
- [Task 7] depends on [Task 1], [Task 2], [Task 3], [Task 4], [Task 5], [Task 6]