## Web 端 — 容错性增强

- [x] `useCachedFetch` 在请求失败时自动重试 3 次（指数退避 1s→2s→4s）
- [x] 所有重试失败后保留最后成功的缓存数据，标记 `isStale: true`
- [x] `useCachedFetch` 返回结果包含 `isStale` 字段
- [x] `useOnlineStatus` hook 正确监听网络状态变化
- [x] `OfflineBanner` 在断网时显示红色横幅提示
- [x] 网络恢复后 OfflineBanner 自动隐藏，触发全局数据刷新
- [x] `ErrorRetry` 组件包含错误图标、消息文本框、重试按钮
- [x] `ErrorRetry` 支持 `fullPage` 和 `inline` 两种模式
- [x] `usePrefetch` hook 支持 `prefetch`、`prefetchOnIdle`、`prefetchOnVisible` 三种模式

## Web 端 — 页面接入优化

- [x] Dashboard 首次加载完成后空闲时预加载 `home.cameras.list` 和 `home.network.status`
- [x] Cameras 页面使用 `ErrorRetry` 替代静态错误文本
- [x] Network 页面使用 `ErrorRetry` 替代静态错误文本
- [x] Devices 页面使用 `ErrorRetry` 替代静态错误文本
- [x] Users 页面使用 `ErrorRetry` 替代静态错误文本
- [x] Profile 页面使用 `ErrorRetry` 替代静态错误文本
- [x] 各页面点击 ErrorRetry 的重试按钮后正确调用 `refetch`

## Android 端 — 基础设施

- [x] `CacheManager` 基于 SharedPreferences + 内存 LRU 缓存实现
- [x] `CacheManager` 提供 `get<T>`、`set<T>`、`clear(prefix)`、`invalidateAll()` 接口
- [x] `CacheManager` 使用 kotlinx.serialization 序列化/反序列化
- [x] `CacheManager` 支持按前缀批量清除缓存
- [x] `RetryInterceptor` 仅对 GET 请求重试，最多 3 次，指数退避
- [x] `RetryInterceptor` 仅重试 5xx 和 IOException，不重试 4xx
- [x] `RetryInterceptor` 已注册到 NetworkFactory 的 OkHttpClient
- [x] `NetworkMonitor` 基于 ConnectivityManager.NetworkCallback 监听网络
- [x] `NetworkMonitor` 通过 StateFlow<Boolean> 暴露 isOnline 状态
- [x] `NetworkMonitor` 在 HomeCenterApp.onCreate() 中初始化
- [x] `StateLayout` 支持 `showLoading`、`showError`、`showEmpty`、`showContent` 四种状态
- [x] `StateLayout` 的 error 状态包含重试按钮
- [x] `layout_state_loading.xml`、`layout_state_error.xml`、`layout_state_empty.xml` 布局文件创建

## Android 端 — WebSocket 优化

- [x] `HomeCenterWebSocket` 维护 `activeSubscriptions: MutableSet<String>`
- [x] `subscribe(topic)` 向集合添加 topic 并发送订阅消息
- [x] `unsubscribe(topic)` 从集合移除 topic
- [x] 连接建立后遍历 `activeSubscriptions` 重新发送订阅消息
- [x] 指数退避重连策略保持不变

## Android 端 — 页面接入

- [x] `PrefetchManager` 在 Application 层初始化
- [x] `PrefetchManager` 提供 `prefetch` 和 `prefetchOnIdle` 方法
- [x] Dashboard 加载完成后预加载 `cameras.list` 和 `devices.list`
- [x] Cameras 加载完成后预加载 ICE config
- [x] Profile 加载完成后预加载 `devices.list`
- [x] DashboardFragment 使用 CacheManager 统一缓存
- [x] DashboardFragment 轮询从 5s 改为 WebSocket 事件驱动 + 30s 兜底
- [x] DashboardFragment 接入 NetworkMonitor，离线时停止轮询
- [x] DashboardFragment 接入 StateLayout
- [x] CamerasFragment 使用 CacheManager 统一缓存
- [x] CamerasFragment 接入 NetworkMonitor
- [x] CamerasFragment 接入 StateLayout (使用自有状态管理作为过渡)
- [x] DevicesFragment 使用 CacheManager 统一缓存
- [x] DevicesFragment 接入 NetworkMonitor
- [x] DevicesFragment 接入 StateLayout (使用自有状态管理作为过渡)
- [x] LogsFragment 使用 CacheManager 缓存日志分页
- [x] LogsFragment 接入 StateLayout (使用自有状态管理作为过渡)

## 验证

- [x] Web `npm run build` 无错误
- [x] Android `gradlew assembleDebug` 通过
- [x] 后端 `go build` 无错误
- [ ] Web: 断开网络 → 显示离线 banner + 缓存数据 → 恢复网络 → 自动刷新
- [ ] Web: 模拟 API 5xx → 自动重试 3 次 → 最终成功显示正确数据
- [ ] Web: 所有 API 失败 → 显示 ErrorRetry → 点击重试 → 恢复正常
- [ ] Web: Dashboard 预加载后 → 切换到 Cameras 页面瞬间显示无 loading
- [ ] Android: 断开网络 → 各页面显示缓存数据 + 离线提示
- [ ] Android: WebSocket 断开 → 重连 → topic 自动恢复订阅
- [ ] Android: 各页面显示统一 loading/error/empty 状态
- [ ] Android: Dashboard 30s 兜底轮询正常，WebSocket 事件驱动更新正常

## 版本

- [ ] Android versionCode 递增，versionName 更新
- [ ] release notes 创建
- [ ] APK 构建并推送到 NAS
- [ ] Git 提交