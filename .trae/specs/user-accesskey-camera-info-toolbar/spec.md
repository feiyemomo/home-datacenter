# 用户创建返回 access-key、摄像头信息整合到工具栏、新增用户按钮移动至右上角 Spec

## Why

1. 创建用户后没有返回 access-key，用户无法获取新设备的凭证，需要抄下来
2. 之前的摄像头信息展示方式不对——信息卡片不应在播放器下方独占区域，应整合到工具栏
3. 新增用户按钮在底部被导航栏遮挡，应像新增摄像头按钮一样放在工具栏右上角

## What Changes

1. **后端：创建用户接口返回 access-key** — `POST /api/v1/user` 响应中增加 `access_key` 字段
2. **Dashboard：创建用户后展示 access-key** — 创建用户弹窗成功后，显示 access-key 供复制
3. **App：创建用户后展示 access-key** — 创建用户对话框成功后，显示 access-key 供复制
4. **App：移除摄像头信息卡片** — 删除 `cardHeader`（播放器下方显示摄像头名称/状态/厂商的卡片），将摄像头名称和状态信息显示在工具栏（`toolbar` 的 `title` 和 `subtitle`）
5. **App：新增用户按钮移至工具栏右上角** — 移除底部 FAB，在 `UsersFragment` 的工具栏右上角添加一个"添加"按钮（图标或文字）

## Impact

- Affected specs: Android app, Web dashboard, Backend API
- Affected code:
  - `d:\Projects\home-datacenter\services\api\internal\handler\user_handler.go`
  - `d:\Projects\home-datacenter\services\api\internal\service\user_service.go`
  - `d:\Projects\home-datacenter\web\src\pages\Users.tsx`
  - `d:\Projects\Android\app\src\main\java\com\homedatacenter\app\ui\admin\UsersFragment.kt`
  - `d:\Projects\Android\app\src\main\java\com\homedatacenter\app\ui\cameras\CameraDetailActivity.kt`
  - `d:\Projects\Android\app\src\main\res\layout\activity_camera_detail.xml`
  - `d:\Projects\Android\app\src\main\res\layout\activity_users.xml`

## ADDED Requirements

### Requirement: 创建用户接口返回 access-key

系统 SHALL 在创建用户成功后返回 `access_key`。

#### Scenario: 成功创建用户
- **WHEN** 管理员调用 `POST /api/v1/user` 创建用户成功
- **THEN** 响应体包含 `access_key` 字段，值为新用户的设备凭证

### Requirement: Dashboard 创建用户后展示 access-key

系统 SHALL 在 Dashboard 创建用户成功后显示 access-key 供复制。

#### Scenario: Dashboard 创建用户
- **WHEN** 管理员在 Dashboard 创建用户成功
- **THEN** 弹窗显示 access-key 和复制按钮，用户可复制凭证

### Requirement: App 创建用户后展示 access-key

系统 SHALL 在 App 创建用户成功后显示 access-key 供复制。

#### Scenario: App 创建用户
- **WHEN** 管理员在 App 创建用户成功
- **THEN** 对话框显示 access-key 和复制按钮，用户可复制凭证

### Requirement: 摄像头信息整合到工具栏

系统 SHALL 将摄像头信息从播放器下方的独立卡片移到工具栏区域。

#### Scenario: 打开摄像头控制页
- **WHEN** 用户打开摄像头控制页
- **THEN** 播放器下方不再显示摄像头信息卡片（`cardHeader`），工具栏标题显示摄像头名称，副标题显示状态等信息

### Requirement: 新增用户按钮移至工具栏右上角

系统 SHALL 将新增用户按钮从底部 FAB 移到工具栏右上角。

#### Scenario: 打开用户管理 tab
- **WHEN** 管理员打开用户管理 tab
- **THEN** 工具栏右上角显示"添加"按钮，点击后弹出创建用户对话框

## MODIFIED Requirements

### Requirement: 摄像头控制页布局
**修改前**: 播放器下方有 `cardHeader` 显示摄像头名称/状态/厂商信息，工具栏标题固定为"摄像头控制"
**修改后**: 移除 `cardHeader`，工具栏标题保持为"摄像头控制"，副标题动态显示摄像头名称、状态和厂商信息

## REMOVED Requirements

### Requirement: 播放器下方的摄像头信息卡片
**Reason**: 信息应整合到工具栏，而非在播放器下方独占区域
**Migration**: 摄像头信息移至工具栏 title/subtitle 显示