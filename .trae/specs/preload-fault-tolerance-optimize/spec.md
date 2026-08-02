# 预加载与容错性全面优化 Spec

## Why

经过前几轮缓存优化，Web 端所有页面已接入 `useCachedFetch` 实现基础缓存，Android 端也实现了基础 WebSocket 重连和状态轮询。但仍存在以下问题：

1. **容错性不足**：API 请求失败时，Web 页面仅显示静态错误文本，无重试按钮；Android 端仅 try-catch 静默失败，用户无感知
2. **无预加载机制**：页面间切换时，目标页面才开始请求数据，没有"提前加载"策略
3. **无离线检测**：网络断开时页面不感知，用户看到的是陈旧数据或无任何提示
4. **Android 无统一缓存层**：各 Fragment 自行管理缓存，策略不一致
5. **WebSocket 重连后未恢复订阅**：Android HomeCenterWebSocket 重连后未重新订阅 topic
6. **无 stale-while-revalidate 模式**：API 失败后缓存数据无法继续使用，直接显示错误

## What Changes

### Web 端
- **`useCachedFetch` 增加容错能力**：添加自动重试（3 次指数退避）、stale-while-revalidate 模式（API 失败时保留缓存继续展示）
- **添加离线检测**：创建 `useOnlineStatus` hook，检测网络状态变化，断网时显示 banner 提示
- **添加预加载路由**：创建 `usePrefetch` hook，在页面进入视口或 hover 链接时提前加载数据
- **优化错误 UI**：所有页面添加"重试"按钮，错误状态更友好
- **Dashboard 预加载其他页面数据**：在 Dashboard 空闲时预加载 Cameras、Network 等页面数据

### Android 端
- **创建统一缓存层**：基于 `SharedPreferences` + 内存 LRU 缓存的统一数据缓存管理器
- **添加 API 自动重试**：创建 `RetryInterceptor` OkHttp 拦截器，自动重试失败的请求（3 次指数退避）
- **添加离线检测**：创建 `NetworkMonitor` 组件，监听网络状态变化
- **优化 WebSocket 重连**：重连后自动恢复所有已订阅的 topic
- **添加预加载管理器**：创建 `PrefetchManager`，在 Fragment 空闲时预加载相邻页面数据
- **统一错误/加载/空状态 UI**：创建 `StateLayout` 组件，统一管理 loading/error/empty 三种状态显示
- **优化 Dashboard 数据加载**：由 5s 轮询改为 WebSocket 事件驱动 + 兜底 30s 轮询

## Impact
- Affected specs: 页面数据加载、容错性、预加载策略、离线体验
- Affected code:
  - `web/src/hooks/useCachedFetch.ts` — 增强容错和 stale-while-revalidate
  - `web/src/hooks/useOnlineStatus.ts` — 新建，离线检测
  - `web/src/hooks/usePrefetch.ts` — 新建，预加载
  - `web/src/pages/Dashboard.tsx` — 添加预加载逻辑
  - `web/src/pages/Cameras.tsx` — 优化错误 UI
  - `web/src/pages/Network.tsx` — 优化错误 UI
  - `web/src/pages/Devices.tsx` — 优化错误 UI
  - `web/src/pages/Users.tsx` — 优化错误 UI
  - `web/src/pages/Logs.tsx` — 优化错误 UI
  - `web/src/pages/Profile.tsx` — 优化错误 UI
  - `web/src/components/OfflineBanner.tsx` — 新建，离线提示条
  - `web/src/components/ErrorRetry.tsx` — 新建，通用错误重试组件
  - Android 各 Fragment 文件 — 接入统一缓存和错误 UI
  - Android 新建 `CacheManager.kt`, `NetworkMonitor.kt`, `PrefetchManager.kt`, `StateLayout.kt`, `RetryInterceptor.kt`

## ADDED Requirements

### Requirement: useCachedFetch 增强 — 自动重试
`useCachedFetch` SHALL 在首次请求失败时自动重试，最多 3 次，使用指数退避（1s → 2s → 4s）。

#### Scenario: 后端临时不可用
- **WHEN** API 请求返回 5xx 错误
- **THEN** hook 自动重试 3 次，每次间隔递增；若最终成功，正常更新数据和缓存；若全部失败，保留最后成功的数据继续展示（stale-while-revalidate）

### Requirement: useCachedFetch 增强 — stale-while-revalidate
`useCachedFetch` SHALL 在 API 请求失败时，保留 sessionStorage 中的缓存数据继续展示，仅在状态中标记 `stale: true`。

#### Scenario: 网络抖动导致 API 超时
- **WHEN** 轮询请求超时失败
- **THEN** 页面继续显示上次缓存的正常数据，不显示错误状态；在数据卡片上显示一个微弱的"数据可能不是最新"提示

### Requirement: 离线检测
创建一个 `useOnlineStatus` hook，监听 `navigator.onLine` 和 `online`/`offline` 事件。

#### Scenario: 网络断开
- **WHEN** 用户网络断开
- **THEN** 页面顶部显示一个红色离线提示条"网络已断开，显示缓存数据"，所有页面继续使用缓存数据
- **WHEN** 网络恢复
- **THEN** 离线提示条消失，所有页面自动触发一次刷新

### Requirement: 预加载机制
创建一个 `usePrefetch` hook，在组件进入视口或指定条件触发时提前加载数据。

#### Scenario: 用户停留在 Dashboard
- **WHEN** Dashboard 首次数据加载完成后，空闲时
- **THEN** 自动调用 `prefetch` 预加载 Cameras、Network 页面的 `useCachedFetch` 数据
- **WHEN** 用户导航到 Cameras 页面
- **THEN** 数据已缓存在 sessionStorage 中，页面瞬间显示，无 loading

### Requirement: 错误 UI 统一化
所有页面 SHALL 在 API 请求失败时显示统一的错误重试组件，包含错误说明和"重试"按钮。

#### Scenario: 服务器宕机
- **WHEN** 所有 API 请求均失败
- **THEN** 页面显示错误提示"无法连接到服务器"和一个"重试"按钮，点击后重新发起所有请求

### Requirement: Android 统一缓存层
创建一个 `CacheManager` 单例，使用 `SharedPreferences` + 内存 LRU 缓存，提供统一的缓存读写接口。

#### 接口定义
- `fun <T> get(key: String, maxAgeMs: Long): T?` — 读取缓存，超时返回 null
- `fun <T> set(key: String, data: T)` — 写入缓存
- `fun clear(prefix: String)` — 按前缀清除缓存
- `fun invalidateAll()` — 清除所有缓存

#### 缓存 Key 规范
- `dashboard.status` — 系统状态
- `dashboard.weather` — 天气
- `cameras.list` — 摄像头列表
- `devices.list` — 设备列表
- `logs.page.{page}` — 日志分页
- `network.status` — 网络状态
- `profile.info` — 个人信息

### Requirement: Android API 自动重试
创建 `RetryInterceptor` OkHttp 拦截器，对 GET 请求自动重试（最多 3 次，指数退避 1s→2s→4s），对 POST/PUT/DELETE 不重试。

#### Scenario: 后端临时不可用
- **WHEN** GET 请求返回 5xx 或超时
- **THEN** 拦截器自动重试，最多 3 次；若最终成功，正常返回数据；若全部失败，抛出异常由上层处理

### Requirement: Android 网络监听
创建 `NetworkMonitor` 组件，基于 `ConnectivityManager` 监听网络状态变化，通过 `StateFlow<Boolean>` 暴露当前在线状态。

#### Scenario: 网络断开
- **WHEN** 网络断开
- **THEN** `NetworkMonitor.isOnline` 变为 `false`，所有 Fragment 停止发起新请求，显示缓存数据 + 离线提示
- **WHEN** 网络恢复
- **THEN** `NetworkMonitor.isOnline` 变为 `true`，所有 Fragment 自动重新加载数据

### Requirement: Android WebSocket 重连恢复订阅
`HomeCenterWebSocket` SHALL 在重连成功后，自动恢复所有之前已订阅的 topic。

#### 实现方式
- 维护一个 `subscribedTopics: MutableSet<String>` 记录所有已订阅的 topic
- 连接建立后，遍历 `subscribedTopics` 重新发送 `subscribe` 消息
- `subscribe()` 方法同时向 `subscribedTopics` 添加 topic 并发送订阅消息
- `unsubscribe()` 方法从 `subscribedTopics` 移除 topic

### Requirement: Android 预加载管理
创建 `PrefetchManager`，在 Fragment 空闲时预加载相邻页面数据。

#### 预加载策略
- Dashboard 加载完成后，预加载 Cameras 和 Devices 列表
- Cameras 页面加载完成后，预加载 ICE config
- Profile 页面加载完成后，预加载 Devices 列表

### Requirement: Android 统一状态 UI
创建 `StateLayout` 自定义 ViewGroup，统一管理 loading/error/empty 三种状态显示。

#### 状态切换
- `showLoading()` — 显示居中加载动画
- `showError(message: String, onRetry: () -> Unit)` — 显示错误图标 + 消息 + 重试按钮
- `showEmpty(message: String)` — 显示空状态图标 + 消息
- `showContent()` — 显示实际内容

### Requirement: Android Dashboard 优化轮询策略
Dashboard 数据刷新由 5s 固定轮询改为 WebSocket 事件驱动 + 兜底 30s 轮询。

#### 实现方式
- WebSocket 事件到达时立即更新对应数据
- 兜底轮询间隔从 5s 延长到 30s
- 事件驱动更新后重置兜底计时器

## MODIFIED Requirements

### Requirement: Web 页面错误处理
**MODIFIED** — 所有页面从显示静态错误文本升级为显示 `ErrorRetry` 组件，包含错误说明和重试按钮。

### Requirement: Android DashboardFragment 数据加载
**MODIFIED** — 使用 `CacheManager` 统一管理缓存，API 请求失败时优先展示缓存数据。

### Requirement: Android 各 Fragment 接入 CacheManager
**MODIFIED** — CamerasFragment、DevicesFragment、LogsFragment、DashboardFragment 从各自管理缓存改为使用 `CacheManager` 统一缓存。

## REMOVED Requirements
无