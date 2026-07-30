# Android 网络策略修正 Spec

## Why

Web 端在 `network-policy-review` (v1.8.7) 中修复了 IPv6 直连分类、质量评分逻辑、初始加载缓存陈旧等问题。Android 端的 `BaseUrlResolver` 已相当完善（三级探测、用户偏好切换、动态 IPv6 获取），但对照 Web 端的修复，仍存在两个对应问题：(1) 硬编码的 `IPV6_DIRECT_URL` 回退常量因 ISP 前缀轮换已陈旧（`37a3:99d0` → `37a4:9141`，compose.yaml 已在 `ipv6-test-and-dev-scripts` spec 中修复，但 Android 代码未同步）；(2) DashboardFragment 首次网络状态获取未传 `refresh=true`，初始显示可能反映最多 60s 的后端缓存陈旧数据，与 Web 端 v1.8.7 修复的问题完全对应。

## What Changes

- **Fix** `BaseUrlResolver.kt` `IPV6_DIRECT_URL` 常量 — 将旧前缀 `2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09` 更新为当前前缀 `2409:8a70:37a4:9141:62be:b4ff:fe08:bd09`，与 `compose.yaml` 的 `NAS_IPV6_ADDRESS` 保持一致。此常量是 `fetchDynamicIpv6Url()` 失败时（未登录、后端不可达）的回退值，必须保持最新。
- **Fix** `DashboardFragment.kt` `loadNetworkStatus()` — 首次获取网络状态时传 `refresh=true`，强制后端执行新鲜检测，避免初始 Dashboard 显示最多 60s 的陈旧质量评分。后续 `onResume` 重新进入页面时使用缓存（后端 60s TTL 已足够新鲜）。实现方式：使用 `@Volatile var firstNetworkFetchDone` 标志位，首次调用后置 true。
- **Bump** `app/build.gradle.kts` 版本号 — versionCode 72 → 73，versionName "1.6.29" → "1.6.30"。
- **Add** `release-notes-v1.6.30.txt` — 记录本次修复内容。
- **Update** `D:\Projects\home-datacenter\docs\ai-context.md` — 新增 Phase 14 记录，文档化 Android 网络策略修正。
- **Update** `D:\Projects\home-datacenter\README.md` — 新增 v1.8.9 更新日志（Android 端修正）。
- **Commit & push** Android 仓库变更，并在 home-datacenter 仓库提交文档更新。

## Impact

- Affected specs: `network-policy-review`（Web 端 v1.8.7 修复的 Android 对应）、`ipv6-test-and-dev-scripts`（compose.yaml NAS_IPV6_ADDRESS 修复的 Android 同步）
- Affected code:
  - `D:\Projects\Android\app\src\main\java\com\homedatacenter\app\util\BaseUrlResolver.kt` — `IPV6_DIRECT_URL` 常量
  - `D:\Projects\Android\app\src\main\java\com\homedatacenter\app\ui\dashboard\DashboardFragment.kt` — `loadNetworkStatus()` 首次刷新标志
  - `D:\Projects\Android\app\build.gradle.kts` — 版本号
  - `D:\Projects\Android\release-notes-v1.6.30.txt` — 新增
  - `D:\Projects\home-datacenter\docs\ai-context.md` — Phase 14
  - `D:\Projects\home-datacenter\README.md` — v1.8.9 日志

## ADDED Requirements

### Requirement: Android Dashboard 首次网络状态新鲜度

DashboardFragment 的首次 `getNetworkStatus` 调用 SHALL 传 `refresh=true`，强制后端执行新鲜网络检测，确保初始显示的质量评分反映当前网络状态而非最多 60s 的后端缓存陈旧数据。

#### Scenario: 应用首次启动后进入 Dashboard
- **WHEN** 用户启动应用并进入 Dashboard，`loadNetworkStatus()` 首次执行
- **THEN** 调用 `getNetworkStatus(token, refresh = true)` 强制后端刷新
- **AND** 后端执行新鲜的 IPv6/NAT/P2P 检测并返回当前状态
- **AND** Dashboard 网络质量卡片显示当前真实评分（非陈旧缓存）

#### Scenario: 后续 onResume 重新进入 Dashboard
- **WHEN** 用户离开 Dashboard 后返回（`onResume` 再次触发 `loadNetworkStatus()`）
- **THEN** 调用 `getNetworkStatus(token, refresh = false)` 使用后端缓存
- **AND** 避免每次页面切换都强制后端检测（STUN 探测有成本）

## MODIFIED Requirements

### Requirement: Android 硬编码 IPv6 回退地址新鲜度

`BaseUrlResolver.kt` 的 `IPV6_DIRECT_URL` 常量 SHALL 与 `compose.yaml` 的 `NAS_IPV6_ADDRESS` 保持一致，作为 `fetchDynamicIpv6Url()` 失败时的回退值。当 ISP 轮换 IPv6 前缀时，两者需同步更新。

#### Scenario: 动态获取失败时使用回退常量
- **WHEN** 用户未登录（`tokenProvider` 返回 null）或后端不可达
- **AND** `fetchDynamicIpv6Url()` 返回 null
- **THEN** `probeSync()` 使用 `IPV6_DIRECT_URL` 常量作为 IPv6 候选
- **AND** 该常量指向当前有效的 NAS IPv6 地址（`2409:8a70:37a4:9141:...`）
- **AND** IPv6 探测能成功连接（而非因陈旧地址失败回退到慢速 Tunnel）
