# 跨页面缓存优化 Spec

## Why
目前摄像头、网络、设备、用户、日志、个人中心等页面在每次导航挂载时都重新请求 API 数据，没有缓存机制。用户切换页面时频繁出现 loading 闪烁，体验不流畅。Dashboard 已使用 `useCachedFetch` 实现缓存，其他页面需要同步接入。

## What Changes
- **所有列表/详情页接入 `useCachedFetch`**：Cameras、Network、Devices、Users、Logs、Profile 页面使用 `useCachedFetch` 缓存 API 响应
- **WebSocket 事件同步缓存**：需要实时更新的页面（Devices、Cameras）通过 WebSocket 事件更新缓存
- **统一缓存 key 命名规范**：`home.{page}.{resource}` 格式（如 `home.cameras.list`、`home.network.status`）
- **合理设置轮询间隔**：根据数据变化频率设置不同的 TTL 和轮询间隔

## Impact
- Affected specs: 页面数据加载、缓存一致性、页面切换流畅度
- Affected code:
  - `web/src/pages/Cameras.tsx` — 摄像头列表缓存
  - `web/src/pages/Network.tsx` — 网络状态缓存
  - `web/src/pages/Devices.tsx` — 设备列表缓存
  - `web/src/pages/Users.tsx` — 用户列表缓存
  - `web/src/pages/Logs.tsx` — 日志列表缓存（分页场景）
  - `web/src/pages/Profile.tsx` — 用户信息缓存
  - `web/src/hooks/useCachedFetch.ts` — 可能需要支持分页缓存（可选）

## ADDED Requirements
### Requirement: 摄像头页面缓存
Cameras 页面 SHALL 使用 `useCachedFetch` 缓存摄像头列表，key 为 `home.cameras.list`，轮询间隔 30 秒。删除摄像头后强制刷新缓存。

#### Scenario: 页面切换回摄像头列表
- **WHEN** 用户从其他页面导航回摄像头页
- **THEN** 立即显示缓存列表，无 loading 闪烁

### Requirement: 网络页面缓存
Network 页面 SHALL 使用 `useCachedFetch` 缓存网络状态，key 为 `home.network.status`，轮询间隔 60 秒（网络状态变化慢）。手动刷新时强制从后端获取。

#### Scenario: 页面切换回网络页
- **WHEN** 用户从 dashboard 导航到网络页
- **THEN** 立即显示缓存的网络状态，无 loading 闪烁

### Requirement: 设备页面缓存
Devices 页面 SHALL 使用 `useCachedFetch` 缓存设备列表，key 为 `home.devices.list`，轮询间隔 30 秒。WebSocket 推送 `device.*` 事件时更新缓存。

#### Scenario: WebSocket 设备状态变更
- **WHEN** WebSocket 推送 `device.online` / `device.offline` 事件
- **THEN** 缓存中的设备状态同步更新，页面无闪烁

### Requirement: 用户页面缓存
Users 页面 SHALL 使用 `useCachedFetch` 缓存用户列表，key 为 `home.users.list`，不轮询（仅手动刷新）。创建/删除用户后强制刷新缓存。

### Requirement: 日志页面缓存
Logs 页面 SHALL 在当前页使用 `useCachedFetch` 缓存当前页的日志数据，key 为 `home.logs.page.{page}`，切换分页时重新请求。不轮询。

### Requirement: 个人中心缓存
Profile 页面 SHALL 使用 `useCachedFetch` 缓存用户信息和设备列表，key 为 `home.profile.info`，轮询间隔 60 秒（JWT 过期倒计时需要更新）。

## MODIFIED Requirements
### Requirement: 页面加载状态
**MODIFIED** — 所有页面从 `useState` + `useEffect` 直接请求改为 `useCachedFetch` 缓存模式，首次加载后切换页面不再显示 loading 动画。

## REMOVED Requirements
无