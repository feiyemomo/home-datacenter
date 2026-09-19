# Home Datacenter — Web 前端

家庭数据中心管理控制台，基于 **React 18 + Vite 5 + Tailwind CSS** 构建的响应式单页面应用（SPA），生产环境由多阶段构建的 **Nginx** 镜像交付。

---

## 技术栈与核心依赖

- **核心框架**：React 18 + TypeScript + Vite 5
- **路由与状态**：React Router v6（嵌套路由 + 常驻 Layout Shell）
- **样式与主题**：Tailwind CSS + 自定义设计系统（支持 Light / Dark 双主题即时切换与持久化）
- **流媒体播放**：
  - **WebRTC**：低延迟直播通道（首选，支持 PTZ 实时协同）；
  - **HLS.js**：HEVC/H.264 兼容直播回退通道；
  - **MSE (fMP4)**：渐进式时间轴录像回放引擎，支持精准拖拽与秒开。
- **实时通信**：原生 WebSocket（带应用层心跳、指数退避重连、主题持久化订阅与排队重发）。
- **网络与请求**：Axios（统一拦截器、自动重试） + 自研 `useCachedFetch`（带并发竞态消除与离线缓存）。

---

## 功能页面与路由设计

应用采用 **持久化 Layout Route 架构**，顶栏、导航侧边栏与背景动效全局常驻，页面切换仅局部内容容器过渡，无全屏销毁闪烁。

| 路由 | 页面组件 | 访问权限 | 说明 |
|---|---|---|---|
| `/login` | `Login.tsx` | 公开 | 密码 / 凭证登录，支持记住会话 |
| `/dashboard` | `Dashboard.tsx` | 需认证 | 系统状态、存储容量、网络概览、最新日志与实时告警横幅 |
| `/cameras` | `Cameras.tsx` | 需认证 | 摄像头列表、实时视频矩阵、全屏控制、PTZ 云台操作与 24/7 录像回放 |
| `/cameras/new` | `DeviceCreate.tsx` | 管理员 | 注册新摄像头设备，支持厂商预设、RTSP 地址实时校验 |
| `/network` | `Network.tsx` | 需认证 | 网络拓扑状态监控、IPv6 直连与中继穿透识别、端点连通性测试 |
| `/users` | `Users.tsx` | 管理员 | 用户管理（CRUD、角色提权、多重防御守卫） |
| `/logs` | `Logs.tsx` | 管理员 | 完整系统操作日志浏览、分页查询与实时刷新 |
| `/mqtt` | `MqttDebug.tsx` | 管理员 | MQTT Broker 实时主题监听、消息手动下发与调试 |
| `/profile` | `Profile.tsx` | 需认证 | 个人账户设置、密码修改与访问令牌查看 |

---

## 核心机制设计

### 1. 统一网络拓扑感知 (`src/lib/network.ts`)
集中统一网络访问判定，避免在各页面分散编写硬编码规则：
- `detectApiPath()`：根据当前协议与主机名自动判断直连 API 路径；
- `isRemoteAccess()`：区分局域网访问与公网访问；
- `isOnRelay()`：精准识别流量是否通过中继穿透隧道，支持纯 AAAA 域名直连判定。

### 2. 缓存与并发安全 (`src/hooks/useCachedFetch.ts`)
针对弱网、频繁翻页及后台轮询场景深度设计：
- **瞬时呈现**：首次挂载同步读取 `sessionStorage` 缓存，消除白屏等待；
- **静默刷新**：后台异步发起网络请求，数据返回后无感替换；
- **竞态消除**：基于 `fetchId` 机制，切页或发起新请求时自动作废旧请求，杜绝跨页数据污染；
- **卸载安全**：组件卸载时立即中断指数退避重试循环，消除内存泄漏。

### 3. 流媒体资源生命周期管理
- **WebRTC 连接回收**：切换至录像回放或切台时，主动关闭并销毁 `RTCPeerConnection`，释放底层 UDP 端口与解码线程；
- **MSE 缓冲安全**：`MediaSource` 结合 `aborted` 标识，快速拖拽时间轴时立即撤销旧 `Blob URL`，防止切片内存堆积；
- **代码分割（Code Splitting）**：将重型库 `hls.js`（~160kB gzip）拆分为独立 `vendor-hls` 异步 chunk，仅在访问摄像头页面时按需加载，保持首屏核心路径轻量。

---

## 本地开发与构建

### 安装依赖
```bash
npm install
```

### 启动开发服务器
```bash
npm run dev
```
开发服务器运行于 `http://localhost:5173`，通过 Vite 代理将 `/api` 与 `/api/v1/ws` 转发至本地后端的 `8080` 端口。

### 生产打包
```bash
npm run build
```
执行 `tsc -b` 类型检查与 `vite build` 生产压缩构建，产物输出至 `dist/`。

### 预览生产构建产物
```bash
npm run preview
```

---

## 生产部署 (Docker & Nginx)

Web 端打包为独立的 Docker 镜像并在根目录 `compose.yaml` 中编排：

- **反向代理**：将 `/api/` 转发至 `api_backend`（开启 upstream keepalive 32 复用连接）；
- **WebSocket 支持**：为 `/api/v1/ws` 配置原生的 `Upgrade` 和 `Connection: "upgrade"` 协议提升；
- **安全响应头**：严格配置 CSP（允许 `self`, `blob:`, `ws:`, `wss:` 等必要资源）、`X-Frame-Options: DENY` 以及 MIME 探测防护；
- **缓存策略**：`index.html` 强制 `no-cache, no-store`；静态 JS/CSS 静态资源享受 30 天强缓存。
