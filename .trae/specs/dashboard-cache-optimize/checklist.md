# Checklist

- [x] 系统日志使用 `useCachedFetch` 缓存加载，key 为 `home.dashboard.logs`
- [x] 系统日志缓存刷新间隔为 10 秒
- [x] 移除独立的 `_logsLoading` 状态和 `useEffect` 加载逻辑
- [x] WS `system.log` 事件同时更新 React state 和 sessionStorage 缓存
- [x] 页面切换回 dashboard 时系统日志卡片无 loading 闪烁