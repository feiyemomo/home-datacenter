# Dashboard 缓存优化 Spec

## Why
Dashboard 当前存在以下流畅度问题：
1. 系统日志 (`listSystemLogs`) 每次挂载都重新请求，无缓存，切换页面回来时闪白
2. 实时 WebSocket 日志事件直接操作 DOM，与缓存数据流不一致
3. 页面切换回 dashboard 时，部分卡片有短暂 loading 闪烁

## What Changes
- **系统日志接入 useCachedFetch**：将系统日志的加载改为与天气、状态、报警一致的缓存模式
- **WebSocket 日志事件写入缓存**：实时日志事件同时更新 sessionStorage 缓存，保持数据一致性
- **优化 loading 状态**：首次加载完成后切换页面不再显示 loading 动画

## Impact
- Affected specs: 仪表盘数据加载、缓存一致性
- Affected code:
  - `web/src/pages/Dashboard.tsx` — 系统日志加载逻辑
  - `web/src/hooks/useCachedFetch.ts` — 缓存写入能力增强（可选）

## ADDED Requirements
### Requirement: 系统日志缓存
The system SHALL use `useCachedFetch` 缓存系统日志数据，key 为 `home.dashboard.logs`，刷新间隔 10 秒。

#### Scenario: 页面切换回 dashboard
- **WHEN** 用户从其他页面导航回 dashboard
- **THEN** 系统日志卡片立即显示上次缓存的数据，无 loading 闪烁

### Requirement: WebSocket 日志同步
WS `system.log` 事件到达时，SHALL 同时更新 React state 和 sessionStorage 缓存。

#### Scenario: 实时日志推送
- **WHEN** WebSocket 推送新的 `system.log` 事件
- **THEN** 日志列表实时更新，且缓存同步写入

## MODIFIED Requirements
### Requirement: 系统日志加载
**MODIFIED** — 从 `useEffect` 直接请求改为 `useCachedFetch` 缓存模式，移除独立的 `_logsLoading` 状态。

## REMOVED Requirements
无