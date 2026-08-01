# 清理设置与摄像头控制页 UI Spec

## Why

App 设置中的管理员功能区域与独立用户管理 tab 功能重复，需要清理；摄像头控制页对不支持云台的摄像头仍显示 PTZ/预设位控件，且摄像头信息独占一个标签页，布局不够紧凑。

## What Changes

1. **App：删除设置中的管理员功能** — 移除 SettingsFragment 中的管理员功能区域（用户管理入口等），因为已有独立的管理员用户管理 tab。
2. **App：修复用户管理 tab FAB 位置** — 将添加按钮往上移，避免被底部导航栏遮挡。
3. **App：摄像头控制页隐藏 PTZ/预设位** — 进入摄像头控制页时，如果摄像头不支持云台（ptz == false），隐藏 PTZ 控制面板和预设位相关 UI。
4. **App：合并摄像头信息到控制页顶部** — 去掉独立的摄像头信息标签页，将"摄像头控制"标签改为"摄像头相关信息"，在控制页顶部展示摄像头基本信息。

## Impact

- Affected specs: Android app UI
- Affected code: `d:\Projects\Android\app\src\main\java\com\homedatacenter\app\ui\settings\SettingsFragment.kt`, `d:\Projects\Android\app\src\main\java\com\homedatacenter\app\ui\admin\UsersFragment.kt`, `d:\Projects\Android\app\src\main\java\com\homedatacenter\app\ui\cameras\CameraDetailActivity.kt`, 及相关布局文件

## ADDED Requirements

### Requirement: 设置中移除管理员功能

系统 SHALL 从 app 设置界面移除管理员功能区域。

#### Scenario: 管理员进入设置
- **WHEN** 管理员用户打开设置页
- **THEN** 不再显示管理员功能区域（用户管理入口等）

### Requirement: 用户管理 FAB 位置调整

系统 SHALL 将用户管理 tab 的添加按钮位置调整，使其不被底部导航栏遮挡。

#### Scenario: 打开用户管理 tab
- **WHEN** 管理员打开用户管理 tab
- **THEN** 添加按钮（FAB）位置在导航栏上方，完全可见且可点击

### Requirement: 不支持云台时隐藏 PTZ/预设位

系统 SHALL 在摄像头不支持云台时隐藏 PTZ 控制面板和预设位相关 UI。

#### Scenario: 进入不支持云台的摄像头控制页
- **WHEN** 用户进入 ptz == false 的摄像头控制页
- **THEN** PTZ 控制面板、预设位相关按钮和控件均不可见

### Requirement: 合并摄像头信息到控制页顶部

系统 SHALL 将摄像头信息合并到控制页顶部，移除独立的摄像头信息标签页。

#### Scenario: 进入摄像头控制页
- **WHEN** 用户进入摄像头控制页
- **THEN** 顶部标签显示"摄像头相关信息"而非"摄像头控制"，并在该页面顶部展示摄像头基本信息（名称、状态、IP 等）

## MODIFIED Requirements

无

## REMOVED Requirements

### Requirement: 设置中的管理员功能区域
**Reason**: 功能重复，已有独立的管理员用户管理 tab
**Migration**: 管理员通过底部导航栏的用户管理 tab 进行用户管理

### Requirement: 摄像头信息独立标签页
**Reason**: 布局不够紧凑，信息量少不值得独占一个标签
**Migration**: 信息合并到控制页顶部展示