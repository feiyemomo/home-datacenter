# 家庭数据中心 Dashboard - 液态玻璃风格视觉优化 PRD

## Overview
- **Summary**: 对家庭数据中心 Web 控制面板的 Dashboard 及全局 App Shell 进行视觉升级，打造更精致的液态玻璃（Liquid Glass）风格。在保留全部通信逻辑（WebSocket、API 调用、认证、路由）的前提下，优化色彩体系、玻璃质感、排版间距、交互动效和信息层级，使整体感觉更温馨、简洁、有呼吸感。
- **Purpose**: 当前界面已有基础毛玻璃效果，但存在以下问题：(1) 卡片渐变和光晕叠加过多导致视觉噪点；(2) 间距偏紧，缺乏呼吸感；(3) 液态玻璃的"折射感"和"流动性"不足，更像普通毛玻璃；(4) 暗色主题偏冷，不够温馨；(5) 部分交互反馈生硬。
- **Target Users**: 家庭数据中心的管理员和家庭成员用户，日常监控系统状态、查看摄像头、管理设备。

## Goals
- 提升全局液态玻璃质感：更真实的玻璃折射、高光边缘、内部光影流转
- 优化色彩体系：暖色主调，降低饱和度冲突，提升暗色模式的温馨感
- 精简视觉层次：减少过度装饰（过多渐变叠加、冗余光晕），让信息更清晰
- 改善排版与间距：更大的留白、更清晰的层级、更舒适的阅读节奏
- 优化交互动效：更流畅的微交互，hover/active 状态更自然如液态流动
- 优化 Dashboard 核心区域：页头、统计卡片、天气卡片、网络质量卡片、报警列表
- 统一组件设计语言：按钮、Badge、输入框、卡片组件风格一致

## Non-Goals (Out of Scope)
- 不修改任何后端 API、WebSocket 通信逻辑、认证流程
- 不改变页面路由结构和导航项
- 不新增功能模块或页面
- 不修改视频流播放（LiveVideo/WebRTC/HLS）的核心逻辑
- 不改变数据获取策略（useCachedFetch、useWebSocket 等 hooks 逻辑不变）
- 不做移动端专属适配之外的响应式重构
- 不引入新的第三方 UI 库或动画库

## Background & Context
- 技术栈：React 18 + TypeScript + Tailwind CSS + Vite + React Router
- 现有设计系统位于 `web/src/index.css`，使用 CSS 变量 + Tailwind 自定义颜色
- 组件库位于 `web/src/components/ui/`（card, button, badge, input）
- 已有 `glass`、`glass-subtle`、`glass-strong`、`glass-glow`、`card-lift` 等工具类
- 暗色/亮色双主题通过 `data-theme` 属性切换
- 使用 lucide-react 图标库
- 部署在 NAS (fnOS) 的 Docker 容器中，通过 Cloudflare Tunnel 对外暴露

## Functional Requirements
- **FR-1**: 全局背景环境升级——优化 ambient orbs 的色彩、大小、模糊度，营造更柔和的暖色氛围光
- **FR-2**: 玻璃质感升级——改进 `.glass` 系列类的 backdrop-filter、渐变、边框、阴影，使其呈现真实液态玻璃的折射感（顶部高光、边缘柔光、内部微渐变）
- **FR-3**: 侧边栏优化——更精致的导航项激活状态、品牌区重设计、用户信息区更温馨
- **FR-4**: 顶部 Header 优化——更简洁的工具栏、更好的主题切换器视觉、移除冗余装饰
- **FR-5**: Dashboard 页头优化——更简洁的页头卡片，减少装饰元素，突出标题和状态
- **FR-6**: 统计卡片（StatCard）重设计——更大的数字展示、更精致的图标容器、去除过度渐变，改为微妙的玻璃内发光
- **FR-7**: 天气卡片（WeatherCard）优化——温度数字更突出，辅助信息更紧凑，图标与温度的视觉关系更和谐
- **FR-8**: 网络质量卡片优化——简化星级展示，质量指示器更精致，延迟信息更清晰
- **FR-9**: 系统日志卡片优化——日志条目更简洁，Badge 大小统一，减少视觉噪音
- **FR-10**: 检测报警卡片优化——AlertItem 列表项间距优化，空状态更友好
- **FR-11**: 按钮组件优化——primary 按钮改为更温暖的渐变，hover 态更柔和，尺寸和圆角微调
- **FR-12**: Badge 组件优化——更精致的胶囊形状、更细腻的背景色
- **FR-13**: 登录页优化——保持玻璃卡片但微调间距和品牌区，使首次印象更温馨
- **FR-14**: 交互动效优化——hover 缓动曲线更自然，stagger 动画更顺滑，减少突兀的缩放
- **FR-15**: 暗色主题色彩微调——背景色从冷紫调改为暖深蓝灰，accent 色增加暖琥珀调

## Non-Functional Requirements
- **NFR-1**: 所有改动不破坏现有功能——API 调用、WebSocket、路由、认证完全正常
- **NFR-2**: 性能不退化——backdrop-filter 使用合理，动画使用 GPU 加速属性（transform, opacity）
- **NFR-3**: 亮色/暗色主题均正常工作，切换流畅
- **NFR-4**: 响应式布局保持——移动端（<768px）正常显示
- **NFR-5**: TypeScript 编译无错误
- **NFR-6**: 构建产物正常，`npm run build` 通过
- **NFR-7**: `prefers-reduced-motion` 用户的动效减弱

## Constraints
- **Technical**: 必须使用现有技术栈（React + Tailwind CSS），不引入新依赖
- **Business**: 保持通信逻辑不变，不允许 break 现有功能
- **Dependencies**: lucide-react 图标、现有 hooks 和 API 层

## Assumptions
- 用户希望保持中文界面文案不变
- "液态玻璃"指 Apple 风格的 frosted glass 效果：半透明、模糊背景、微妙边框、高光边缘、柔和阴影
- "温馨"指暖色调（琥珀、暖橙、柔粉），低对比度的舒适感，避免冷蓝/冷紫
- "简洁"指去除视觉噪音，留白充足，信息层级清晰，不过度装饰

## Acceptance Criteria

### AC-1: 全局玻璃质感提升
- **Given**: 用户打开任意页面
- **When**: 页面加载完成
- **Then**: 卡片、侧边栏、Header 呈现真实液态玻璃效果——有明显的顶部高光边缘、柔和的半透明背景、微妙的内阴影、自然的边框光
- **Verification**: `human-judgment`
- **Notes**: 对比原有效果，玻璃应更"通透"且"有深度"，而非简单的模糊+半透明

### AC-2: 暖色温馨氛围
- **Given**: 用户在暗色或亮色主题下
- **When**: 浏览任意页面
- **Then**: 整体色调偏暖——暗色模式背景为暖深蓝灰而非冷紫，亮色模式背景为暖米色；accent 色以暖琥珀/暖蓝为主，无冷紫色主导
- **Verification**: `human-judgment`

### AC-3: Dashboard 页头简洁清晰
- **Given**: 用户在 Dashboard 页面
- **When**: 页面加载完成
- **Then**: 页头区域标题醒目、副标题清晰、状态 Badge 精致，装饰性光晕和渐变最少化
- **Verification**: `human-judgment`

### AC-4: 统计卡片视觉精致
- **Given**: 用户查看 Dashboard 的 4 个统计卡片
- **When**: 卡片渲染完成
- **Then**: 数字大而清晰、图标容器有微妙玻璃质感、hover 有流畅的上浮和微光效果，卡片之间间距舒适
- **Verification**: `human-judgment`

### AC-5: 天气卡片信息层级清晰
- **Given**: 用户查看天气卡片
- **When**: 天气数据加载完成
- **Then**: 温度数字最突出，天气图标与温度和谐排列，湿度/风速/体感信息紧凑且不喧宾夺主
- **Verification**: `human-judgment`

### AC-6: 交互反馈流畅自然
- **Given**: 用户与按钮、卡片、导航项交互
- **When**: hover 或 click 元素
- **Then**: 过渡动画使用自然缓动曲线（cubic-bezier(0.32, 0.72, 0, 1)），动效时长 200-400ms，无突兀跳变
- **Verification**: `human-judgment`

### AC-7: 侧边栏导航精致
- **Given**: 用户使用侧边栏导航
- **When**: 切换页面或 hover 导航项
- **Then**: 激活项有柔和的发光指示条+玻璃高亮背景，hover 项有微妙的背景变化，品牌区视觉协调
- **Verification**: `human-judgment`

### AC-8: 功能完整性
- **Given**: 用户进行正常操作
- **When**: 登录、切换页面、查看摄像头、切换主题、刷新数据
- **Then**: 所有功能正常工作，无 JavaScript 错误，API 调用正常，WebSocket 连接正常
- **Verification**: `programmatic`

### AC-9: TypeScript 编译通过
- **Given**: 代码修改完成
- **When**: 运行 `npm run build`（在 web 目录）
- **Then**: 编译成功，无类型错误
- **Verification**: `programmatic`

### AC-10: 暗色/亮色主题切换流畅
- **Given**: 用户在任意页面
- **When**: 点击主题切换按钮切换亮色/暗色/跟随系统
- **Then**: 主题切换有平滑过渡动画（0.5s 左右），所有元素颜色正确切换，无闪烁
- **Verification**: `human-judgment`

### AC-11: 视觉简洁无冗余
- **Given**: 用户浏览任意页面
- **When**: 审视页面整体
- **Then**: 无过多渐变叠加、无冗余光晕效果、信息层级分明、留白充足、无视觉疲劳感
- **Verification**: `human-judgment`

### AC-12: 登录页温馨友好
- **Given**: 用户访问登录页
- **When**: 页面加载完成
- **Then**: 登录卡片居中、品牌图标温馨、输入框风格统一、按钮有暖色调、整体氛围亲切
- **Verification**: `human-judgment`

## Open Questions
- [ ] 暗色主题的主色调偏好：暖深蓝灰 vs 暖深棕灰？（默认采用暖深蓝灰）
- [ ] 侧边栏是否需要从玻璃实色改为更透明的全玻璃效果？（默认改为更透明）
- [ ] 统计卡片的 accent 色彩是否保留 4 种不同颜色，还是统一为一种温馨色？（默认保留但降低饱和度）
