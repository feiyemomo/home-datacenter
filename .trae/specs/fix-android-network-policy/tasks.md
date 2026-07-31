# Tasks

- [x] Task 1: 更新 `BaseUrlResolver.kt` 的 `IPV6_DIRECT_URL` 常量
  - [x] SubTask 1.1: 将 `2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09` 更新为 `2409:8a70:37a4:9141:62be:b4ff:fe08:bd09`，与 compose.yaml 的 `NAS_IPV6_ADDRESS` 一致
  - [x] SubTask 1.2: 更新常量上方注释中的旧地址引用（注释无旧地址引用，无需修改）

- [x] Task 2: 修复 `DashboardFragment.kt` 首次网络状态获取的新鲜度
  - [x] SubTask 2.1: 在 `DashboardFragment` 中添加 `@Volatile private var firstNetworkFetchDone = false` 标志位
  - [x] SubTask 2.2: 修改 `loadNetworkStatus()` 的网络调用，首次调用传 `refresh = !firstNetworkFetchDone`，调用后置 `firstNetworkFetchDone = true`
  - [x] SubTask 2.3: 在 `onDestroyView()` 中重置 `firstNetworkFetchDone = false`（Fragment 重建时重新强制刷新）

- [x] Task 3: 更新版本号
  - [x] SubTask 3.1: `app/build.gradle.kts` versionCode 72 → 73，versionName "1.6.29" → "1.6.30"

- [x] Task 4: 新增 `release-notes-v1.6.30.txt`
  - [x] SubTask 4.1: 记录 IPv6 回退地址更新 + Dashboard 首次网络状态强制刷新

- [x] Task 5: 更新 home-datacenter 文档
  - [x] SubTask 5.1: `docs/ai-context.md` 新增 Phase 14 记录（Android 网络策略修正）
  - [x] SubTask 5.2: `README.md` 新增 v1.8.9 更新日志

- [x] Task 6: Git 提交并推送
  - [x] SubTask 6.1: Android 仓库提交并推送（commit 1f242ce，push 成功）
  - [x] SubTask 6.2: home-datacenter 仓库提交文档更新并推送（commit 52f42f0，push 成功）

# Task Dependencies

- [Task 3] depends on [Task 1], [Task 2]（版本号在代码修复后递增）
- [Task 4] depends on [Task 1], [Task 2], [Task 3]
- [Task 5] depends on [Task 1], [Task 2]（文档记录实际修复内容）
- [Task 6] depends on [Task 4], [Task 5]
- [Task 1] 和 [Task 2] 相互独立，可并行执行
