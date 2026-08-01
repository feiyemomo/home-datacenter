# 工具栏标题、退出登录按钮、日志权限、ID复用、Dashboard日志修复 Spec

## Why

1. App 直播观看页的工具栏 title 目前固定为"摄像头相关信息"，用户希望直接显示摄像头名称，更加直观
2. 设置页的退出登录按钮过于醒目（红色描边、全宽、52dp），容易误触
3. 非 admin 用户的主页 tab（Dashboard）中不应显示日志区域，目前 Web Dashboard 没有 admin 权限检查
4. 已删除用户的 ID 复用功能在前一个 spec（remove-device-page-reuse-ids）中已实现代码逻辑，但用户反馈尚未生效，需要排查验证
5. Dashboard 的日志显示区域存在未明确的问题，需要排查修复

## What Changes

1. **App：摄像头工具栏 title 改为摄像头名称** — 将 `setupHeader()` 中 toolbar 的 title 设置为摄像头名称，subtitle 保持显示状态信息
2. **App：退出登录按钮隐蔽化** — 缩小按钮尺寸、减弱颜色，降低误触概率
3. **Web Dashboard：非 admin 用户隐藏日志区域** — 在 Dashboard.tsx 中添加 admin 权限检查，非 admin 用户不显示系统日志卡片
4. **后端：验证并修复已删除用户 ID 复用** — 排查删除用户时设备记录是否被物理删除，确保 SQLite 自增 ID 可被新记录复用，必要时修复
5. **Dashboard 日志显示问题排查修复** — 排查 Web Dashboard 日志显示问题，修复 API 响应处理或渲染逻辑

## Impact

- Affected specs: Android app UI, Web dashboard, Backend API
- Affected code:
  - `d:\Projects\Android\app\src\main\java\com\homedatacenter\app\ui\cameras\CameraDetailActivity.kt`
  - `d:\Projects\Android\app\src\main\res\layout\fragment_settings.xml`
  - `d:\Projects\Android\app\src\main\java\com\homedatacenter\app\ui\settings\SettingsFragment.kt`
  - `d:\Projects\home-datacenter\web\src\pages\Dashboard.tsx`
  - `d:\Projects\home-datacenter\services\api\internal\service\user_service.go`
  - `d:\Projects\home-datacenter\services\api\internal\repository\device_repository.go`
  - `d:\Projects\home-datacenter\services\api\internal\model\user.go`
  - `d:\Projects\home-datacenter\services\api\internal\model\device.go`
  - 可能涉及的其他文件

## ADDED Requirements

### Requirement: 摄像头工具栏 title 显示摄像头名称

系统 SHALL 将摄像头直播页的工具栏 title 显示为摄像头名称。

#### Scenario: 打开摄像头直播页
- **WHEN** 用户打开摄像头直播页
- **THEN** 工具栏 title 显示摄像头名称，subtitle 显示状态和其他信息

### Requirement: 退出登录按钮隐蔽化

系统 SHALL 将设置页的退出登录按钮设计为不易误触的样式。

#### Scenario: 打开设置页
- **WHEN** 用户打开设置页
- **THEN** 退出登录按钮不再使用醒目的红色描边样式，改为更小的文字按钮或更低调的样式

### Requirement: Web Dashboard 非 admin 隐藏日志

系统 SHALL 在 Web Dashboard 中根据用户权限控制日志区域的可见性。

#### Scenario: 非 admin 用户访问 Dashboard
- **WHEN** 非 admin 用户访问 Web Dashboard
- **THEN** 系统日志卡片不可见

### Requirement: 已删除用户 ID 可复用

系统 SHALL 确保删除用户后，其关联的设备记录被物理删除，释放的 ID 可被新记录复用。

#### Scenario: 删除用户后创建新用户
- **WHEN** 管理员删除一个用户后又创建一个新用户
- **THEN** 新用户及其默认设备可以使用之前被删除用户释放的 ID

### Requirement: Dashboard 日志显示正常

系统 SHALL 确保 Web Dashboard 的系统日志区域能正确显示日志数据。

#### Scenario: 展开 Dashboard 日志区域
- **WHEN** 用户在 Dashboard 展开系统日志区域
- **THEN** 正确显示最近 N 条日志，包括时间、级别、消息

## MODIFIED Requirements

### Requirement: 摄像头控制页布局
**修改前**: 工具栏 title 固定为"摄像头相关信息"，subtitle 显示摄像头名称+状态+厂商信息
**修改后**: 工具栏 title 显示摄像头名称，subtitle 显示状态和其他信息

## REMOVED Requirements

无