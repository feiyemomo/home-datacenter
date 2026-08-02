# Dashboard 性能优化 Spec

## Why
Dashboard 经过上一轮缓存优化后，仍存在以下性能问题：
1. 整个 Dashboard 组件在每次 state 变化时全部重新渲染，StatCard、WeatherCard 等子组件未使用 memo 隔离
2. WebSocket 事件监听器每次 `lastMessage` 变化都重新创建 effect，造成不必要的逻辑执行
3. 系统状态+网络状态 5 秒轮询过于频繁，增加服务器负载和客户端渲染压力
4. 检测报警列表未做虚拟化，大量报警条目时 DOM 节点过多
5. CSS 毛玻璃动画在低端设备上可能造成卡顿

## What Changes
- **React.memo 隔离子组件**：StatCard、WeatherCard、AlertItem 使用 React.memo 减少无效重渲染
- **优化轮询间隔**：系统状态轮询从 5s 调整为 10s，与系统日志对齐
- **提取组件到独立文件**：WeatherCard、StatCard、AlertItem、LiveAlertBanner 拆分为独立文件，便于优化和代码分割
- **WebSocket 监听优化**：使用 ref 追踪最新事件，避免 effect 重复创建
- **CSS 动画优化**：添加 `will-change` 和 `transform` 优化，减少重排

## Impact
- Affected specs: 仪表盘渲染性能、组件架构
- Affected code:
  - `web/src/pages/Dashboard.tsx` — 大幅精简，提取子组件
  - `web/src/components/dashboard/` — 新建目录，存放提取的组件
  - `web/src/hooks/useWebSocket.ts` — 可选优化

## ADDED Requirements
### Requirement: 子组件使用 React.memo
StatCard、WeatherCard、AlertItem 等子组件 SHALL 使用 `React.memo` 包裹，配合 `useMemo` 减少无效重渲染。

#### Scenario: 检测报警 WebSocket 事件到达
- **WHEN** WebSocket 推送新的 `camera.motion` 事件
- **THEN** 只有 LiveAlertBanner 重新渲染，StatCard 和 WeatherCard 不重渲染

### Requirement: 提取组件到独立文件
Dashboard 中的子组件 SHALL 提取到 `web/src/components/dashboard/` 目录下，每个文件一个组件。

#### 组件列表
- `StatCard.tsx` — 统计卡片（在线设备、MQTT、WS 客户端、运行时长）
- `WeatherCard.tsx` — 天气卡片
- `LiveAlertBanner.tsx` — 实时检测报警横幅
- `AlertItem.tsx` — 报警列表项
- `AlertSnapshotModal.tsx` — 报警截图弹窗
- `SystemSnapshot.tsx` — 系统快照折叠面板

### Requirement: 优化轮询间隔
系统状态和网络状态轮询 SHALL 从 5s 调整为 10s。

## MODIFIED Requirements
### Requirement: WebSocket 监听
**MODIFIED** — 使用 `useRef` 追踪最新 WebSocket 事件，避免每次 `lastMessage` 变化都重新创建 effect。

## REMOVED Requirements
无