# Tasks

- [x] Task 1: 提取子组件到独立文件并添加 React.memo
  - [x] SubTask 1.1: 创建 `web/src/components/dashboard/` 目录
  - [x] SubTask 1.2: 提取 `StatCard` 到 `StatCard.tsx`，使用 `React.memo`
  - [x] SubTask 1.3: 提取 `WeatherCard` 到 `WeatherCard.tsx`，使用 `React.memo`
  - [x] SubTask 1.4: 提取 `LiveAlertBanner` 到 `LiveAlertBanner.tsx`
  - [x] SubTask 1.5: 提取报警列表项为 `AlertItem.tsx`，使用 `React.memo`
  - [x] SubTask 1.6: 提取 `AlertSnapshotModal` 到 `AlertSnapshotModal.tsx`
  - [x] SubTask 1.7: 提取 `SystemSnapshot` 到 `SystemSnapshot.tsx`

- [x] Task 2: 优化 Dashboard.tsx 主组件
  - [x] SubTask 2.1: 将轮询间隔从 5s 调整为 10s（`refetchMs: 5000` → `refetchMs: 10000`）
  - [x] SubTask 2.2: 使用 `useRef` 优化 WebSocket 事件监听，避免 effect 重复创建
  - [x] SubTask 2.3: 精简 Dashboard.tsx，导入提取的子组件替代内联定义

- [x] Task 3: 构建并部署
  - [x] SubTask 3.1: 运行 `npm run build` 确认无编译错误
  - [x] SubTask 3.2: 部署到生产环境

# Task Dependencies
- [Task 2] depends on [Task 1]
- [Task 3] depends on [Task 2]