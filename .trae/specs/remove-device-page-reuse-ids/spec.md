# 取消设备页、用户在线状态与废弃ID复用 Spec

## Why

当前系统以设备为核心进行展示和管理，但用户实际关心的是"用户"层面的操作。设备页功能冗余，新增用户时创建设备的选项增加了复杂度，设备级的在线状态对用户来说不够直观。同时用户删除后设备 ID 应该可以被复用，避免 ID 资源浪费。

## What Changes

1. **取消 web 设备页** — 移除 `/devices` 路由、页面组件和侧边栏导航项
2. **取消 app 设备页** — 移除设备 Fragment 和底部导航栏的设备 tab
3. **取消新增用户时的创建设备选项** — 移除创建用户表单中的设备名称输入框和后端 `initial_device_name` 逻辑
4. **用户在线状态** — 后端新增 `online` 字段到用户列表响应，判断依据是该用户是否有任意设备在线
5. **废弃 ID 复用保障** — 确保用户删除时设备记录被物理删除，释放设备 ID 供新设备使用
6. **部署测试推送** — 构建并部署到 NAS，验证功能，提交 Git

## Impact

- Affected specs: 设备管理、用户管理、认证系统
- Affected code:
  - Web: `Layout.tsx` (侧边栏), `App.tsx` (路由), `Devices.tsx` (删除), `Users.tsx` (创建表单)
  - App: `DevicesFragment.kt`, `DeviceAdapter.kt`, `MainActivity.kt` (底部导航), 相关布局文件
  - Backend: `user_handler.go` (响应增加 online 字段), `user_service.go` (查询用户在线状态), `device_handler.go` (保留内部 API 但移除设备页), `user_handler.go` (移除 `initial_device_name`)
  - 后端 device API 路由保留（供 JWT 认证和内部使用），但不再通过 UI 暴露管理功能

## ADDED Requirements

### Requirement: 用户在线状态
The system SHALL provide user-level online status in the user list API.

#### Scenario: 用户列表包含在线状态
- **WHEN** 管理员调用 `GET /api/v1/user`
- **THEN** 响应中每个用户对象包含 `online: true/false` 字段
- **AND** `online=true` 当且仅当该用户有至少一个设备在 deviceManager 中标记为在线

### Requirement: 废弃设备 ID 复用
The system SHALL clean up device records when a user is deleted.

#### Scenario: 删除用户后设备 ID 可复用
- **WHEN** 管理员删除一个用户
- **THEN** 该用户的所有设备记录被物理删除（非软删除）
- **AND** 后续新创建的设备可以使用被释放的 ID

## MODIFIED Requirements

### Requirement: 用户创建
**Reason**: 移除创建用户时附带创建设备的选项，简化流程

- **WHEN** 管理员调用 `POST /api/v1/user`
- **THEN** 请求体不再包含 `initial_device_name` 字段
- **AND** 响应不再包含 `device` 和 `access_key` 字段
- **AND** 用户创建后，管理员可通过其他方式（如设备绑定流程）为用户分配设备

### Requirement: 设备管理 UI
#### Scenario: Web 设备页
- **WHEN** 用户访问 web 界面
- **THEN** 侧边栏不再显示"设备"导航项
- **AND** `/devices` 路由不再可用

#### Scenario: App 设备页
- **WHEN** 用户打开 Android 应用
- **THEN** 底部导航栏不再显示设备 tab
- **AND** 设备相关的 Fragment 不再加载

## REMOVED Requirements

### Requirement: 设备创建页 Web UI
**Reason**: 设备管理功能从 UI 中移除，改为通过设备绑定流程管理
**Migration**: 仍可通过后端 API 和设备绑定流程管理设备

### Requirement: 新用户创建设备选项
**Reason**: 简化用户创建流程，设备管理改为独立流程
**Migration**: 创建用户后，用户可通过设备绑定流程自行绑定设备