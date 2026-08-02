# Tasks

## Web 端 — 容错性增强

- [x] Task 1: 增强 `useCachedFetch` — 自动重试 + stale-while-revalidate
  - [x] 添加重试参数 `maxRetries: number`（默认 3），`retryDelay: number`（默认 1000）
  - [x] 请求失败时指数退避重试（1s → 2s → 4s），最多 3 次
  - [x] 所有重试失败后保留最后成功的缓存数据，设置 `stale: true` 标记
  - [x] 返回结果中增加 `isStale: boolean` 字段，供 UI 层判断是否显示"数据可能不是最新"提示
  - [x] 添加 `onError` 回调参数，供页面层自定义错误处理

- [x] Task 2: 创建 `useOnlineStatus` hook + `OfflineBanner` 组件
  - [x] 创建 `web/src/hooks/useOnlineStatus.ts`，监听 `navigator.onLine` 和 `online`/`offline` 事件
  - [x] 创建 `web/src/components/OfflineBanner.tsx`，红色横幅提示"网络已断开，显示缓存数据"
  - [x] 在 `App.tsx` 中全局集成 `OfflineBanner`
  - [x] 网络恢复后自动隐藏 banner，触发全局数据刷新

- [x] Task 3: 创建统一错误重试组件 `ErrorRetry`
  - [x] 创建 `web/src/components/ErrorRetry.tsx`，包含错误图标、消息文本、重试按钮
  - [x] 支持 `message: string`、`onRetry: () => void`、`fullPage?: boolean` 属性
  - [x] `fullPage=true` 时居中全屏显示，`false` 时内联显示

- [x] Task 4: 创建 `usePrefetch` hook
  - [x] 创建 `web/src/hooks/usePrefetch.ts`
  - [x] 支持 `prefetch(key: string, fetcher: () => Promise<T>)` 方法，将数据写入 sessionStorage
  - [x] 支持 `prefetchOnIdle(key: string, fetcher: () => Promise<T>, ms?: number)` 方法，在浏览器空闲时执行
  - [x] 支持 `prefetchOnVisible(ref: RefObject, key: string, fetcher: () => Promise<T>)` 方法，在元素进入视口时执行

## Web 端 — 页面接入优化

- [x] Task 5: Dashboard 添加预加载逻辑
  - [x] 首次数据加载完成后，空闲时预加载 `home.cameras.list` 和 `home.network.status`
  - [x] 使用 `requestIdleCallback` 或 `setTimeout` 延迟执行，不阻塞主线程

- [x] Task 6: 各页面接入 `ErrorRetry` 组件
  - [x] Cameras.tsx — 替换静态错误文本为 `ErrorRetry` 组件
  - [x] Network.tsx — 替换静态错误文本为 `ErrorRetry` 组件
  - [x] Devices.tsx — 替换静态错误文本为 `ErrorRetry` 组件
  - [x] Users.tsx — 替换静态错误文本为 `ErrorRetry` 组件
  - [x] Profile.tsx — 替换静态错误文本为 `ErrorRetry` 组件
  - [x] 各页面在 `useCachedFetch` 返回 `error` 时显示 `ErrorRetry`，点击重试调用 `refetch`

## Android 端 — 基础设施

- [x] Task 7: 创建 `CacheManager` 统一缓存层
  - [x] 基于 `SharedPreferences` + 内存 LRU 缓存实现
  - [x] 提供 `get<T>(key, maxAgeMs)`、`set<T>(key, data)`、`clear(prefix)`、`invalidateAll()` 接口
  - [x] 使用 kotlinx.serialization 序列化/反序列化缓存数据
  - [x] 支持按前缀批量清除（如 `clear("cameras")` 清除所有 `cameras.*` 缓存）

- [x] Task 8: 创建 `RetryInterceptor` OkHttp 拦截器
  - [x] 仅对 GET 请求自动重试，最多 3 次
  - [x] 指数退避：1s → 2s → 4s
  - [x] 仅重试 5xx 和 IOException（超时、DNS 失败等），不重试 4xx
  - [x] 注册到 `NetworkFactory` 的 OkHttpClient

- [x] Task 9: 创建 `NetworkMonitor` 网络监听组件
  - [x] 基于 `ConnectivityManager.NetworkCallback` 监听网络状态
  - [x] 通过 `StateFlow<Boolean>` 暴露 `isOnline` 状态
  - [x] 在 `HomeCenterApp.onCreate()` 中初始化
  - [x] 提供 `isOnlineNow(): Boolean` 同步检查方法

- [x] Task 10: 创建 `StateLayout` 统一状态 UI 组件
  - [x] 自定义 `FrameLayout` 子类
  - [x] `showLoading()` — 居中显示 Material 进度条
  - [x] `showError(msg, onRetry)` — 显示错误图标 + 消息 + 重试按钮
  - [x] `showEmpty(msg)` — 显示空状态图标 + 消息
  - [x] `showContent()` — 显示实际内容，隐藏状态覆盖层
  - [x] 创建对应的 `layout_state_loading.xml`、`layout_state_error.xml`、`layout_state_empty.xml`

## Android 端 — WebSocket 优化

- [x] Task 11: 优化 `HomeCenterWebSocket` 重连恢复订阅
  - [x] 添加 `activeSubscriptions: MutableSet<String>` 属性
  - [x] `subscribe(topic)` 方法向集合添加 topic 并发送订阅消息
  - [x] `unsubscribe(topic)` 方法从集合移除 topic
  - [x] 连接建立后（`onOpen`），遍历 `activeSubscriptions` 重新发送订阅消息
  - [x] 保持指数退避重连策略不变

## Android 端 — 页面接入

- [x] Task 12: 创建 `PrefetchManager` 预加载管理器
  - [x] 在 Application 层初始化
  - [x] 提供 `prefetch(key, fetcher)` 方法，结果写入 `CacheManager`
  - [x] 提供 `prefetchOnIdle(key, fetcher)` 方法，在 `Handler` 空闲时执行
  - [x] Dashboard 加载完成后预加载 `cameras.list` 和 `devices.list`
  - [x] Cameras 页面加载完成后预加载 ICE config
  - [x] Profile 页面加载完成后预加载 `devices.list`

- [x] Task 13: DashboardFragment 接入优化
  - [x] 使用 `CacheManager` 替代本地缓存管理
  - [x] 轮询策略改为 WebSocket 事件驱动 + 30s 兜底轮询
  - [x] 接入 `NetworkMonitor`，离线时停止轮询，显示缓存数据
  - [x] 接入 `StateLayout` 管理 loading/error/empty 状态
  - [x] 接入 `PrefetchManager`，加载完成后预加载 Cameras 和 Devices

- [x] Task 14: CamerasFragment 接入优化
  - [x] 使用 `CacheManager` 替代本地缓存管理
  - [x] 接入 `NetworkMonitor`，离线时显示缓存数据
  - [x] 接入 `RetryInterceptor` 受益于自动重试
  - [x] 接入 `StateLayout` 管理 loading/error/empty 状态（使用自有状态管理作为过渡）
  - [x] 接入 `PrefetchManager`，加载完成后预加载 ICE config

- [x] Task 15: DevicesFragment 接入优化
  - [x] 使用 `CacheManager` 替代本地缓存管理
  - [x] 接入 `NetworkMonitor`，离线时显示缓存数据
  - [x] 接入 `StateLayout` 管理 loading/error/empty 状态（使用自有状态管理作为过渡）

- [x] Task 16: LogsFragment 接入优化
  - [x] 使用 `CacheManager` 缓存日志分页数据
  - [x] 接入 `StateLayout` 管理 loading/error/empty 状态（使用自有状态管理作为过渡）

## 验证与发布

- [x] Task 17: 构建验证
  - [x] Web `npm run build` 无错误
  - [x] Android `gradlew assembleDebug` 通过
  - [x] 后端 `go build` 无错误

- [ ] Task 18: 端到端验证
  - [ ] Web: 断开网络，页面显示离线 banner + 缓存数据，恢复网络后自动刷新
  - [ ] Web: 模拟 API 5xx，页面自动重试后显示正确数据
  - [ ] Web: 所有 API 失败时，页面显示 ErrorRetry 组件，点击重试恢复正常
  - [ ] Web: Dashboard 预加载后，切换到 Cameras 页面瞬间显示无 loading
  - [ ] Android: 断开网络，各页面显示缓存数据 + 离线提示
  - [ ] Android: WebSocket 断开后重连，topic 自动恢复订阅
  - [ ] Android: 各页面显示统一 loading/error/empty 状态
  - [ ] Android: Dashboard 30s 兜底轮询正常，WebSocket 事件驱动更新正常

- [x] Task 19: 版本更新
  - [x] Android versionCode 递增（106），versionName 更新（1.7.12）
  - [x] 创建 release notes
  - [x] 构建 APK 并推送到 NAS
  - [x] 提交 Git

# Task Dependencies
- Task 5 (Dashboard 预加载) 依赖 Task 4 (usePrefetch hook)
- Task 6 (各页面接入 ErrorRetry) 依赖 Task 3 (ErrorRetry 组件)
- Task 13 (DashboardFragment) 依赖 Task 7 (CacheManager), Task 9 (NetworkMonitor), Task 10 (StateLayout), Task 12 (PrefetchManager)
- Task 14 (CamerasFragment) 依赖 Task 7, Task 9, Task 10, Task 12
- Task 15 (DevicesFragment) 依赖 Task 7, Task 9, Task 10
- Task 16 (LogsFragment) 依赖 Task 7, Task 10
- Task 18 (端到端验证) 依赖所有前置任务
- Task 1, 2, 3, 4, 7, 8, 9, 10, 11 可以并行执行