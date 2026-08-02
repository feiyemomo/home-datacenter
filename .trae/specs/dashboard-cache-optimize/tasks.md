# Tasks

- [x] Task 1: 系统日志接入 useCachedFetch 缓存
  - [x] SubTask 1.1: 将 `listSystemLogs` 从 `useEffect` 改为 `useCachedFetch`，key 为 `home.dashboard.logs`，refetchMs 10s
  - [x] SubTask 1.2: 移除独立的 `_logsLoading` 状态和 `useEffect` 加载逻辑
  - [x] SubTask 1.3: 日志卡片使用缓存数据渲染，空状态和加载状态与缓存一致

- [x] Task 2: WebSocket 日志事件同步写入缓存
  - [x] SubTask 2.1: WS `system.log` 处理中将新日志 prepend 后同步写入 sessionStorage
  - [x] SubTask 2.2: 确保缓存更新后 React state 也同步更新

# Task Dependencies
- [Task 2] depends on [Task 1]