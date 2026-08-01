# App/Dashboard 主页日志、退出登录按钮与仪表盘布局重构 Spec

## Why

- App 非 admin 用户主页已隐藏日志区域，但网络质量卡片尚可调整大小以更好利用空间
- 退出登录按钮位于设置页底部，不够直观且易误触，需要整合到账户信息区域
- App 日志显示出现异常（ts 字段解析错误导致全部显示 "1970-01-01"）
- Dashboard 仪表盘页面布局不够紧凑，网络质量和系统日志卡片可缩小并增加点击跳转功能

## What Changes

### App 主页（DashboardFragment）
- **非 admin 用户**：保持日志区域隐藏，将网络质量卡片放大（移除隐藏 strategyRow/detailRow 的逻辑，代之以展示一个更紧凑但视觉上更大的卡片）
- **admin 用户**：保持不变

### App 设置页（SettingsFragment）
- 将退出登录按钮从底部独立位置移动到账户信息卡片内"当前用户"后面
- 显示为"账号管理"文字按钮，点击后弹出确认对话框（或显示退出登录按钮）
- 移除原底部 `btnLogout`

### App 日志时间显示修复
- **后端**：检查 `SystemLog` 的 JSON 序列化标签，确保 `ts` 字段的 JSON key 为 `ts`（小写）
- **Android**：检查 `SystemLog` 数据类中 `@SerialName("Ts")` 是否与后端返回的 JSON key 匹配
- **根因**：后端 `json:"ts"` 与 Android 端 `@SerialName("Ts")` 大小写不匹配，导致 ts 始终为 0

### Dashboard 仪表盘（Dashboard.tsx）
- **网络质量卡片**：缩小尺寸，从独占一行改为更紧凑的布局，点击跳转到网络页面 (`/network`)
- **检测报警卡片**：保持不变
- **系统日志卡片**：缩小尺寸，与网络质量卡片共享一行（grid 布局），点击跳转到日志页面
- 仅 admin 用户可见系统日志卡片

## Impact

- Affected specs: 无
- Affected code:
  - `d:\Projects\Android\app\src\main\java\com\homedatacenter\app\ui\dashboard\DashboardFragment.kt`
  - `d:\Projects\Android\app\src\main\res\layout\fragment_dashboard.xml`
  - `d:\Projects\Android\app\src\main\java\com\homedatacenter\app\ui\settings\SettingsFragment.kt`
  - `d:\Projects\Android\app\src\main\res\layout\fragment_settings.xml`
  - `d:\Projects\Android\app\src\main\java\com\homedatacenter\app\data\model\SystemLog.kt`
  - `d:\Projects\home-datacenter\services\api\internal\model\system_log.go`
  - `d:\Projects\home-datacenter\web\src\pages\Dashboard.tsx`

## ADDED Requirements

### Requirement: App 非 admin 用户网络质量卡片放大
**The system SHALL** display a larger/more prominent network quality card for non-admin users in the home tab.

#### Scenario: Non-admin user views home tab
- **WHEN** a non-admin user opens the home tab
- **THEN** the network quality card SHALL occupy more vertical space (show strategy row and detail row that were previously hidden)
- **AND** the recent logs section SHALL remain hidden

### Requirement: App 退出登录按钮整合
**The system SHALL** move the logout button to the account info section.

#### Scenario: User views settings page
- **WHEN** a user opens the settings tab
- **THEN** the account info card SHALL display a "账号管理" option after the "当前用户" name
- **AND** clicking "账号管理" SHALL show a logout confirmation dialog or reveal a logout button
- **AND** the bottom logout button SHALL be removed

### Requirement: App 日志时间显示修复
**The system SHALL** correctly display log timestamps.

#### Scenario: User views logs page
- **WHEN** a user opens the logs tab
- **THEN** each log entry SHALL display its correct timestamp instead of "1970-01-01 08:00:00"

### Requirement: Dashboard 网络质量卡片缩小并可点击跳转
**The system SHALL** make the network quality card more compact and clickable.

#### Scenario: Admin views dashboard
- **WHEN** the dashboard loads
- **THEN** the network quality card SHALL be displayed in a compact layout
- **AND** clicking the card SHALL navigate to `/network`

### Requirement: Dashboard 系统日志卡片缩小并共享一行
**The system SHALL** make the system log card compact and share a row with the network quality card.

#### Scenario: Admin views dashboard
- **WHEN** the dashboard loads
- **THEN** the system log card SHALL be displayed in a compact layout
- **AND** the network quality card and system log card SHALL share one row (grid of 2 columns)
- **AND** clicking the system log card SHALL navigate to the logs page

## MODIFIED Requirements

### Requirement: App 主页非 admin 用户日志隐藏
**The system SHALL** continue hiding the recent logs section for non-admin users.

#### Scenario: Non-admin user views home tab
- **WHEN** a non-admin user opens the home tab
- **THEN** the recent logs section (`tvRecentLogsTitle`, `btnViewAllLogs`, `rvRecentLogs`, `tvRecentLogsEmpty`) SHALL remain hidden

## REMOVED Requirements

### Requirement: App 设置页底部退出登录按钮
**Reason**: 移入账户信息卡片内作为"账号管理"功能
**Migration**: 退出登录功能移至"账号管理"按钮，点击后弹出确认对话框