# 液态玻璃 Dashboard 视觉优化 - 实现计划

## [ ] Task 1: 设计令牌与 CSS 基础层升级
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 调整 CSS 变量色彩体系：暗色背景从冷紫调改为暖深蓝灰，亮色背景微调暖米色
  - 升级 `.glass` / `.glass-subtle` / `.glass-strong` 类的 backdrop-filter、渐变、边框、内阴影，增强真实液态玻璃折射感（顶部高光条、边缘柔光、内部微梯度）
  - 优化 ambient orbs：降低模糊半径、调整色彩为暖琥珀+柔蓝、减少数量但增大尺寸、更柔和的漂浮动画
  - 优化噪点纹理 overlay：降低不透明度
  - 改进滚动条样式：更细更柔和
  - 添加 `prefers-reduced-motion` 媒体查询支持
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-6, AC-10, AC-11
- **Test Requirements**:
  - `programmatic` TR-1.1: 暗色主题背景色为暖深蓝灰（约 RGB 22,20,28），亮色为暖米色
  - `programmatic` TR-1.2: `.glass` 类包含 backdrop-filter blur、saturate、顶部高光伪元素
  - `human-judgement` TR-1.3: 背景 orbs 柔和不刺眼，漂浮缓慢自然
  - `human-judgement` TR-1.4: 玻璃卡片有明显的折射深度感而非简单模糊
- **Notes**: 文件: `web/src/index.css`、`web/tailwind.config.js`

## [ ] Task 2: 基础 UI 组件升级（Button, Badge, Card, Input）
- **Priority**: high
- **Depends On**: Task 1
- **Description**:
  - **Button**: primary 按钮改为暖蓝→暖琥珀的微妙渐变（而非纯蓝），增大圆角到 xl，hover 态更柔和的光晕，移除过于强烈的 shadow；ghost/outline/secondary 按钮统一玻璃质感
  - **Badge**: 统一胶囊形状（rounded-full），背景改为更微妙的半透明色，文字更小更精致，减少硬边框
  - **Card**: 基础 Card 组件改用升级后的 `.glass` 类，padding 微调更透气，圆角统一为 2xl；移除默认的 `glass-glow` 和 `glass-hover-lift`，改为按需添加
  - **Input**: 输入框玻璃质感升级，聚焦态有柔和的暖光晕
- **Acceptance Criteria Addressed**: AC-1, AC-6, AC-11
- **Test Requirements**:
  - `human-judgement` TR-2.1: 按钮 hover 有柔和上浮+微光，无突兀缩放
  - `human-judgement` TR-2.2: Badge 呈精致胶囊状，色彩柔和不刺眼
  - `human-judgement` TR-2.3: 输入框聚焦时有温暖的环形光晕
  - `programmatic` TR-2.4: TypeScript 编译无错误
- **Notes**: 文件: `web/src/components/ui/button.tsx`、`web/src/components/ui/badge.tsx`、`web/src/components/ui/card.tsx`、`web/src/components/ui/input.tsx`

## [ ] Task 3: App Shell 升级（Layout 侧边栏 + Header）
- **Priority**: high
- **Depends On**: Task 1, Task 2
- **Description**:
  - **Sidebar**: 侧边栏背景改为更通透的玻璃（glass-subtle 而非 glass-strong），增加微妙的右侧边缘光；品牌区 logo 改为更温馨的渐变（暖琥珀+柔蓝）；导航项激活态改为左侧柔和发光条+玻璃高亮背景，移除过于厚重的 shadow；用户信息区更紧凑温馨，头像加微妙光环；退出按钮 hover 更柔和
  - **Header**: 顶栏改为更薄更通透的玻璃，减少高度；标题区更简洁，移除冗余 Badge；健康检查链接改为图标按钮；主题切换器 hover 更柔和
  - **主体区域**: 增加页面 padding，让内容更有呼吸感
  - **移动端适配**: 确保侧边栏抽屉和顶栏在小屏幕上正常
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-7, AC-10, AC-11
- **Test Requirements**:
  - `human-judgement` TR-3.1: 侧边栏玻璃通透，导航激活态有柔和发光指示
  - `human-judgement` TR-3.2: Header 简洁不笨重，主题切换器交互流畅
  - `human-judgement` TR-3.3: 移动端汉堡菜单正常，侧边栏滑入流畅
  - `programmatic` TR-3.4: 导航链接正确路由，激活状态正确
- **Notes**: 文件: `web/src/components/Layout.tsx`

## [ ] Task 4: Dashboard 核心区域升级
- **Priority**: high
- **Depends On**: Task 1, Task 2, Task 3
- **Description**:
  - **页头**: 简化页头卡片——移除左侧彩色条和装饰性光晕 blur 圆，改为简洁的玻璃卡片+左侧细线；状态 Badge 更精致
  - **StatCard 统计卡片**: 重设计为更简洁的液态玻璃卡片——数字增大（text-4xl），font-weight 改为 tracking-tight；图标容器改为更小更精致的圆形玻璃按钮（而非大方形）；移除左侧 hover bar 和多重渐变，改为微妙的角向渐变背景+hover 时柔和的内发光；hint 文字更精致；卡片间距调大到 gap-5
  - **天气卡片**: 温度数字放大（text-5xl），天气图标更融入背景；辅助信息（体感/湿度/风速）改为更紧凑的横向排列；移除装饰性 blur 圆和过多渐变；分隔线更 subtle
  - **网络质量卡片**: 简化星级为更精致的圆点指示器；仪表盘图标更小更精致；连接状态 Badge 更简洁；移除渐变覆盖层
  - **系统日志卡片**: 日志条目改为无背景的简洁列表（去掉 glass-subtle 背景），hover 时才出现微妙高亮；Badge 更小巧
  - **检测报警卡片**: 移除顶部渐变线，列表项间距优化，空状态更友好
- **Acceptance Criteria Addressed**: AC-1, AC-3, AC-4, AC-5, AC-6, AC-11
- **Test Requirements**:
  - `human-judgement` TR-4.1: 统计卡片数字醒目、图标精致、hover 柔和流畅
  - `human-judgement` TR-4.2: 天气卡片温度突出，辅助信息不喧宾夺主
  - `human-judgement` TR-4.3: 网络质量指示器清晰美观
  - `human-judgement` TR-4.4: 日志和报警列表简洁不杂乱
  - `programmatic` TR-4.5: 所有数据正常加载和刷新，WebSocket 实时更新正常
  - `programmatic` TR-4.6: TypeScript 编译无错误
- **Notes**: 文件: `web/src/pages/Dashboard.tsx`、`web/src/components/dashboard/StatCard.tsx`、`web/src/components/dashboard/WeatherCard.tsx`

## [ ] Task 5: 登录页 + 其他页面视觉一致性优化
- **Priority**: medium
- **Depends On**: Task 1, Task 2
- **Description**:
  - **登录页**: 品牌图标改为更温馨的渐变；输入框与全局 Input 组件风格统一；登录按钮使用升级后的暖色调；卡片 padding 微调；背景 orbs 与全局一致
  - **摄像头页面**: 页头简化（与 Dashboard 页头一致的风格）；摄像头卡片 header 区域更简洁，减少边框分割；卡片圆角统一
  - **确保其他页面**（Devices、Network、Logs、Users、Profile）的卡片和元素自动继承升级后的基础组件样式
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-6, AC-12
- **Test Requirements**:
  - `human-judgement` TR-5.1: 登录页温馨友好，品牌感强
  - `human-judgement` TR-5.2: 摄像头卡片风格与 Dashboard 一致
  - `human-judgement` TR-5.3: 其他页面基础元素（按钮、卡片、Badge）风格统一
  - `programmatic` TR-5.4: 登录流程正常，摄像头列表正常加载
- **Notes**: 文件: `web/src/pages/Login.tsx`、`web/src/pages/Cameras.tsx`、可能涉及其他页面小调整

## [ ] Task 6: 动效与微交互精细化
- **Priority**: medium
- **Depends On**: Task 1-5
- **Description**:
  - 统一所有过渡动画使用 `cubic-bezier(0.32, 0.72, 0, 1)` 缓动曲线
  - 优化 stagger-children 动画：更短的延迟间隔（30ms vs 50ms），更柔和的进入
  - 优化 card-lift hover：减小 translateY 距离（-2px vs -3px），scale 更微妙（1.002 vs 1.005），shadow 更柔和
  - 为数字变化添加更平滑的过渡（StatCard 的 number-pop）
  - 为导航项切换添加微妙的滑动指示动画
  - 添加 `prefers-reduced-motion` 支持
- **Acceptance Criteria Addressed**: AC-6
- **Test Requirements**:
  - `human-judgement` TR-6.1: 所有 hover/active 动效流畅自然，无突兀感
  - `human-judgement` TR-6.2: 页面加载 stagger 动画顺滑不拖沓
  - `programmatic` TR-6.3: `prefers-reduced-motion` 下动画被禁用
- **Notes**: 文件: `web/src/index.css`、各组件文件中的 transition 调整

## [ ] Task 7: 构建验证与浏览器预览测试
- **Priority**: high
- **Depends On**: Task 1-6
- **Description**:
  - 在 web 目录运行 `npm run build`，确保 TypeScript 编译和 Vite 构建通过
  - 在开发模式下启动 dev server，使用浏览器逐个页面截图验证
  - 测试亮色/暗色主题切换
  - 测试响应式布局（桌面/平板/移动端）
  - 测试所有交互：按钮点击、导航切换、hover 效果、WebSocket 实时更新
  - 部署到 NAS 验证生产环境正常
- **Acceptance Criteria Addressed**: AC-8, AC-9, AC-10
- **Test Requirements**:
  - `programmatic` TR-7.1: `npm run build` 成功，无错误无警告
  - `programmatic` TR-7.2: dev server 启动正常，各页面可访问
  - `human-judgement` TR-7.3: 亮色/暗色主题切换流畅，色彩正确
  - `human-judgement` TR-7.4: 移动端布局正常无溢出
  - `human-judgement` TR-7.5: 所有交互反馈流畅自然
  - `programmatic` TR-7.6: 部署到 NAS 后可正常访问和使用
- **Notes**: 部署命令: `deploy-nas.ps1 -Password '@Fnos324'`
