# Home Datacenter — Changelog

## 更新日志

### WebRTC 候选地址动态生成 + H3C 隧道地址同步加固 + 回放保留录制音轨 (2026-10-07)

> **背景**：NAS 改用 Tailscale 地址 `100.90.67.71` 后 WebRTC 看不了；关闭拾音后，之前开着拾音录到的片段回放也被强制静音。

**WebRTC 候选地址**（`services/api/internal/camera/frigate.go`、`compose.yaml`）：
- `PushConfig` 以前写死了 `192.168.31.234` 和早已失效的 H3C 端口 `154.8.195.220:32510`。每次带 `requires_restart` 的全量推送（例如切换拾音）都会把候选地址覆盖回这组旧值。
- 新增 `BuildWebRTCCandidates`，全量推送和 `SetWebRTCCandidates` 共用这一份。来源依次为：回环地址、`NAS_LAN_IP` 或自动探测到的局域网 IP、API 被访问过的所有内网及 100.64/10（Tailscale）地址、`WEBRTC_EXTRA_CANDIDATES`、IPv6、H3C TCP 隧道地址、`stun:8555`。
- `compose.yaml` 新增 `WEBRTC_EXTRA_CANDIDATES=100.90.67.71`。
- 查询 H3C 隧道地址的超时从 500ms 放宽到 2s。查询失败时改用上次查到的有效地址，不再退回已失效的 `WEBRTC_TCP_CANDIDATE` 环境变量。

**H3C 保活同步**（`deploy/h3c-keepalive/main.py`）：
- 补回「地址没变就不同步」的判断。仓库版本缺少这个判断，部署后会每 60 秒重启一次 go2rtc。
- 去掉写死的 `192.168.31.234/.235`。局域网和 Tailscale 候选地址由 home-api 负责，保活服务只替换 H3C 隧道地址。

**回放保留录制音轨**（`camera_handler.go`，App 端 `RecordingsDialog.kt`）：
- 回放和缓存不再按当前拾音开关决定是否去掉音轨（`-an` / `_noaudio`），App 也不再强制静音。拾音开关只控制录制：关闭期间录下的片段本身就没有音轨，开启期间录下的片段保留声音。

**App**（`WebRtcClient.kt`）：基础地址是 100.64/10（Tailscale）地址时保留 TCP ICE 候选。NAS 上的 tailscaled 运行在 userspace 模式，UDP 不一定能进来。

### 摄像头拾音开关真正切断录音（ISAPI 通道级写入 + 录像预设联动）(2026-10-06)

> **背景**：App 里关闭「摄像头拾音」后，回放该时间段的录像仍然能听到完整声音。
> 排查发现两层都没生效：① 服务端用于关麦的 ISAPI 写入打在了固件根本不接受的端点上，
> 摄像头麦克风一直开着；② NAS 上运行的 API 镜像比源码落后若干提交，录像侧
> 「关闭拾音即去掉录音音轨」的逻辑压根没部署。两层叠加就是「关了拾音照样有声音」。

**摄像头麦克风真正关闭**（`services/api/internal/camera/isapi.go`）：
- **写入路径改为整文档读改写**：实测海康 DS-2CD 固件上 `PUT /ISAPI/Streaming/channels/101/audio`
  与 `PUT /ISAPI/System/Audio/channels/1` 均返回 403 `Invalid Operation`（部分机型直接 404），
  旧实现三个端点全被拒绝却静默返回，麦克风从未被关闭。
  新实现先 `GET /ISAPI/Streaming/channels/<id>` 取整份 `<StreamingChannel>` 文档，
  只改写 `<Audio><enabled>` 节点后整份 PUT 回去，返回 `<statusCode>1</statusCode>` 即生效。
- **覆盖全部码流通道**：自动枚举 `/ISAPI/Streaming/channels`，对主码流（101）与子码流（102）
  同时改写，避免直播走子码流时仍有声音。
- **实测验证**：关闭后 RTSP 流的 `pcm_alaw` 音轨立即消失（ffprobe 只剩 `hevc` 视频轨），
  重新开启后音轨恢复。

**录像音轨随拾音开关联动**（`registry.go` / `frigate.go` / `camera_handler.go`）：
- `UpdateAudio` 现在同时完成四件事：写 `capabilities.audio`、关/开摄像头麦克风、
  重建 go2rtc 直播流（含 `_1080p` 与 `_hevc` 伴生流）、并把 Frigate 的
  `ffmpeg.output_args.record` 切到 `preset-record-generic`（`-an`，录音无音轨）
  或 `preset-record-generic-audio-aac`（AAC 音轨），带 `requires_restart` 保证录像进程真正重启。
- `.stream-cache` 缓存键加入 `_noaudio` 后缀，避免拾音关闭后仍命中旧的带声音缓存。
- 新增单元测试 `isapi_audio_test.go`（真实机型抓取的 `StreamingChannel` XML 作为夹具），
  覆盖音轨开关改写与录像预设映射。

**部署一致性**：NAS 源码树此前落后于本地提交（`isapi.go` 在 NAS 上根本不存在），
本次整棵 `services/api` Go 源码同步后重建镜像，源码与镜像恢复一致。

**验证结果**（现场实测，前后各 45 秒）：

| 状态 | 摄像头 `Audio.enabled` | RTSP 音轨 | 录像 ffmpeg | 落盘新分片 |
|---|---|---|---|---|
| 拾音开启 | `true` | `audio` | `-c:v copy -c:a aac` | `aac,audio` |
| 拾音关闭 | `false` | 无 | `-c copy -an` | 仅 `hevc` 视频轨 |
| 再次开启 | `true` | `audio` | `-c:v copy -c:a aac` | `aac,audio` |

### v1.9.4 — H3C 简优云运维通道扩展 (SSH 22 端口隧道) 与录像流媒体极速秒开优化 (2026-10-04)

> **背景**：① 扩展 H3C 简优云国内高速穿透能力，将 NAS 宿主机底层 SSH 调试端口（22）纳入自动化保活与租期巡检体系（`nas_ssh`），免去公网海外穿透或手动开放端口的繁琐流程，外出时可直接通过极速低延迟国内中继 SSH 登录 NAS；② 优化摄像头录像回放性能，在服务端引入智能流探测与 `-c copy` 极速直拼接模式，将录像加载等待时间从数秒直接降至 ~0.15s 秒开级别；同时增强 Android 端全屏报警大图弹窗与降级兜底。

**H3C 简优云远程运维隧道扩展**（`deploy/h3c-keepalive/`、`compose.yaml`）：
- **加入 NAS 22 (SSH) 自动化保活**：在 H3C Oasis 项目中动态声明并注册 `nas_ssh` (TCP 22) 终端应用；保活服务默认巡检 `home`, `webrtc_test`, `nas_ssh` 三条高速链路，租期低于 25 分钟自动平滑更新，并将当前 SSH 连接地址实时输出至 `/data/h3c/ssh_url.txt`。

**服务端录像直拼极速秒开**（`services/api/internal/handler/camera_handler.go`）：
- **智能 `-c copy` 无损秒级拼流**：对于同规格标准 H.264/H.265 录像片段，优先采用 FFmpeg Concat Demuxer 模式直接无损复用数据流，完全规避 CPU 重编解码转码开销，录像请求实现百毫秒级极速响应。

### v1.9.3 — H3C 简优云国内高速穿透保活守护进程与固定重定向网关 (2026-10-02)

> **背景**：家庭数据中心远程访问原先依赖海外 Cloudflare 隧道，高延迟（1.4s+）影响流媒体和回放体验。用户拥有 H3C 路由器自带的简优云（ucloud.h3c.com）国内高速穿透能力，但官方单次开启限时 3 小时且端口动态变动。本次通过 CAS 逆向工程与 Oasis 6 协议分析，构建了常态化无人值守的自动化保活与固定域名秒级 302 重定向方案。

**自动化保活守护进程**（`deploy/h3c-keepalive/`、`compose.yaml`）：
- **全自动凭证认证与租期巡检**：基于 Python 3.11 Alpine 轻量容器，周期性巡检 H3C 简优云 NAT 穿透租期；在租期剩余低于 25 分钟或过期时自动触发平滑销毁与重新创建，实现 24/7 永久不中断。
- **固定跳转与状态服务**：守护进程内置轻量 HTTP 服务器（端口 8087），提供 `/redirect`、`/status` 及 `/refresh` 接口；并将当前高速外网地址写入 `/data/h3c/current_url.txt` 与静态引导页。

**网关与前端联动**（`web/nginx.conf`、`web/src/pages/Network.tsx`）：
- **Nginx 秒级 302 重定向**：新增 `https://dashboard.feiyemomo.top/fast` 与 `/h3c` 路由，无需手动记录或更换动态端口，访问该固定 URL 即自动 302 重定向至最新 H3C 国内高速直连端口。
- **网络状态卡片**：Network 页面新增“H3C 简优云国内高速通道”专属面板，实时展示隧道运行状态、动态外网地址、剩余租期、一键复制、手动刷新及直连跳转入口。

### v1.9.2 — 边缘算力平滑补偿、系统审计日志精细化与安全联动 (2026-09-25)

> **背景**：① 边缘节点（RK3588/NPU）在高温与高负载断路保护期间，原有重试超时过短易导致关键安全分析被丢弃。本次将平滑重试窗口扩展至 60 秒，并在算力恢复后对关键跌倒事件自动执行补偿确认；② 系统审计日志体系大幅增强，精细化记录“操作者变更了哪一项具体参数与新取值”，包括安防布防模式切换、家庭人脸特征录入与注销、摄像头各项高级参数修改、用户角色变更及自动化规则开关；③ Web 与 App 同步支持延时补偿检测事件的明确标识提示。

**边缘视觉与安全事件补偿**（`services/api/internal/mqtt/handler.go`、`web/src/components/Layout.tsx`）：
- **平滑重试与算力补偿**：跌倒检测重试窗口延长至 60 秒；在算力熔断恢复后自动调取暂存图像补偿推理，确认后发布带 `delayed=true` 标签的告警事件，杜绝安全盲区。
- **Web 端告警横幅联动**：检测到延时补偿事件时在横幅时间后附加“（负载恢复后补测）”，提升事件追溯透明度。

**系统审计日志精细化**（`services/api/internal/log/subscriber.go`、`services/api/internal/eventbus/events.go`、`services/api/internal/vision/handler.go`、`services/api/internal/handler/`）：
- **安防布防模式审计**：订阅 `security.guard_mode` 事件，记录用户将模式切换为【离家布防】、【在家守护】或【撤防免打扰】。
- **人脸档案管理审计**：新增 `vision.person_register` 与 `vision.person_delete` 事件，记录管理员录入或删除指定家庭成员人脸档案。
- **摄像头参数修改明细**：精确记录预置位保存/删除、H.265/H.264 编码切换、音频开关、全天录像计划（保留天数）调整及多用户权限共享与取消。
- **用户与自动化管理审计**：记录管理员升降级角色、启用/停用自动化规则等操作。

### v1.9.1 — Web 前端架构重构、竞态消除与流媒体健壮性加固 (2026-09-13)

> **背景**：全面重构 Web 前端架构与生命周期安全。① 建立常驻 Layout 嵌套路由架构，杜绝页面跳转时顶栏与侧栏全量销毁闪烁；② 统一网络拓扑感知（`lib/network.ts`）；③ 彻底清理 400+ 行未引用的 Devices 遗留代码；④ 消除 `useCachedFetch` 异步重试竞态、卸载残留及动态 key 缓存不同步；⑤ 修复 WebRTC/MSE 切换回放及时间轴快切时的端口与 Blob URL 泄漏；⑥ 完善 Nginx 安全响应头（CSP/add_header 继承）与样式高对比度。详见 [`web/README.md`](web/README.md)。

**架构与路由**（`web/src/App.tsx`、`web/src/components/Layout.tsx`、`web/src/components/ProtectedRoute.tsx`）：
- **持久化 Layout Shell**：将 React Router 路由表改为 `<Layout />` 嵌套路由，顶栏、侧栏导航与高斯模糊背景常驻；切页时仅内部 `<Outlet />` 容器触发 slide-up 过渡动画。
- **清理遗留死代码**：安全移除未路由的 `Devices.tsx`（400+ 行），瘦身代码库。

**网络与拓扑**（`web/src/lib/network.ts`、`web/src/pages/Network.tsx`、`web/src/pages/Dashboard.tsx`）：
- **统一网络工具库**：抽离 `detectApiPath()`、`isRemoteAccess()` 与 `isOnRelay()`，集中管理 NAS DDNS 域名常量，修复 IPv6 AAAA 直连误判为中继穿透的缺陷。

**流媒体与并发安全**（`web/src/components/LiveVideo.tsx`、`web/src/lib/fmp4Mse.ts`、`web/src/hooks/useCachedFetch.ts`）：
- **WebRTC / MSE 资源生命周期**：切至录像回放时销毁未完成的 WebRTC 预握手连接；MSE 创建 Blob URL 前后增加中止检查并即时 revoke，防止内存堆积。
- **`useCachedFetch` 竞态阻断**：增加 `fetchIdRef` 与 `isMountedRef`，切页或组件卸载时立即废弃旧请求，修复指数退避期间卸载组件导致的泄露以及跨页数据覆盖问题；支持动态 key 缓存即时重置。
- **定时器与空闲队列**：修复 `StatCard` 嵌套定时器泄露；`usePrefetch` 改用 `Set` 集合并在任务执行后自清除，杜绝长时间后台运行的内存单调累积。

**生产部署与样式**（`web/nginx.conf`、`web/index.html`、`web/src/index.css`）：
- **Nginx CSP & 继承陷阱**：修复 `location = /index.html` 覆盖全局安全响应头的配置陷阱；CSP 规则放行 `blob:`、`ws:`、`wss:`。
- **渲染性能与可读性**：`.orb` 动效样式增加 GPU 硬件图层提升；修复 Light 模式下顶层继承导致的文本对比度问题。

### v1.9.0 — Web 录像回放修复 + 前端性能优化（路由拆包） (2026-08-23)

> **背景**：本轮是 Web 端的一批回放修复与首屏性能优化。① 修掉「同一天内切换录像片段黑屏、切日期才恢复」的根因——MSE 从未把 fMP4 的 init 段 append 进 SourceBuffer，demuxer 拿不到 moov/codec；② 让一天内的第一段录像也走渐进式 `/stream`（首字节 ~0.2s）而非整段转码的 `/file`（~4-7s）；③ 路由级代码拆包，首屏关键路径从 ~281kB gzip 降到 ~85kB gzip。

**录像回放修复**（`web/src/lib/fmp4Mse.ts`、`web/src/components/RecordingTimeline.tsx`）：
- **MSE 缺 init 段**：`startMseStream` 之前把 `ftyp+moov`（init 段）取出后仅用于解析 codec，却从未 `appendBuffer` 进 SourceBuffer，只喂了 `moof/mdat` 分片 → demuxer 报 `CHUNK_DEMUXER_ERROR_APPEND_FAILED`（`readyState=0`）→ 同一天内切换的第 2 段起录像黑屏。现在先 append init 再喂分片。
- **播放切换竞态**：`playRecording` 加 `playbackGenerationRef` 代数 + `switchingPlaybackRef` 守卫，旧片段异步 MSE/回退回调不会再覆盖新片段状态。
- **同一天切换 reload**：只有 direct-URL 路径才 `v.load()` + 元数据后 seek/play（MSE 由 pump 自行加载）；`seekOnceReady` 在缓冲就绪后 `v.play()` 恢复共享 `<video>` 播放。
- **冷启动改走 /stream**：一天内第一段录像（此时 `<video>` 尚未挂载，`videoRef.current` 为 null）此前直接回退到 `/file`；现在若 MSE 可用，先挂载视频再 `startMseStream`（`waitForVideoEl` 等元素就绪），走渐进式 `/stream`。

**前端性能优化**（`web/src/App.tsx`、`web/vite.config.ts`）：
- **路由级 `lazy()` 拆包** + `Suspense` fallback：hls.js（~162kB gzip）与录像播放引擎不再进登录/仪表盘/网络/日志的关键路径，仅打开摄像头时才加载。
- **`manualChunks` vendor 分包**：`vendor-react` / `vendor-hls` 作为 content-hash 不可变 chunk，发版时只有被改动的 chunk 变化，浏览器可长期缓存。
- `chunkSizeWarningLimit: 600`（覆盖稳定 vendor-hls chunk）。
- 新增 `rollup-plugin-visualizer` devDependency（方便后续做 bundle 组成分析）。

#### 验证（NAS 192.168.31.235 实测，2026-08-23）
- `npm run build`（`tsc -b && vite build`）零错误，产物 `index-DcAFAOf3.js`。
- 部署后 `home-web` healthy、HTTP 200；`home-api` 用缓存镜像未重建；`frigate`/`mosquitto` 未动。
- 端点实测（摄像头 17 / 同 recId）：`/file` 首字节 TTFB ≈ 4.4–7.3s（整段 faststart 转码完才发），`/stream` TTFB ≈ 0.11–0.19s（渐进式 fMP4）；时间轴查询 `/recordings` ≈ 40ms（非瓶颈）。
- 机制级复现（Chrome headless + 真实 fMP4 + 同一 `<video>` 复用）：skip-init → `CHUNK_DEMUXER_ERROR`/`readyState=0`（复现黑屏）；append-init → `readyState=4` 正常播放。

#### 版本
- Web: v1.9.0（回放修复 + 路由拆包；`package.json` 增加 visualizer devDep）
- Backend / Android: 无改动

### v1.8.44 — 三端一键编排 + Release 正式签名 + 后端识别 release 命名 (2026-08-15)

> **背景**：此前三端（Android / Go 后端 / Web）各自独立构建，发布靠手动逐个执行。本轮新增顶层编排脚本把三端
> 串成一条命令，并给 Android 配置正式 Release 签名（keystore），让发布版携带稳定、可验证、可升级的官方身份。

**新增 `build-all.ps1`（顶层三端编排）**：
- Android（Gradle，本地构建）→ 可选推送 APK → Go 后端 + Web（打包经 `deploy-nas.ps1`，在 NAS 的 Dockerfile 内构建）。
- 三端保持独立工具链，Gradle 只负责 Android；编排脚本按序执行、出错即停。
- 参数：`-Flavor debug|release`、`-PushApk`、`-Notes`、`-Password`、`-SkipAndroid`、`-SkipDeploy`、`-DryRun`。
- 全链 `-DryRun` 验证通过（Android → push-apk → deploy-nas）。

**Android 正式 Release 签名**（`android/` 工程）：
- 生成正式 keystore `app/keystore/home-release.jks`（gitignore，`keystore.properties` 存凭据，绝不入库）。
- `app/build.gradle.kts` 新增 `releaseSigning` 签名配置，release 构建用正式私钥签名（V1/V2/V3）。
- release APK 已用 `keytool` 验证：SHA1/SHA256 指纹与正式 keystore 完全一致。
- debug 继续用工程内置 `projectDebug` keystore（多人多机构建同一签名，可覆盖安装）。
- **debug / release 共存**：给 debug 加了 `applicationIdSuffix = ".debug"`，于是 debug 包名
  `com.homedatacenter.app.debug`、release 包名 `com.homedatacenter.app`，两者是独立应用、可同时安装。
  此前因两包签名不同且 applicationId 相同，用 release 覆盖安装 debug 会报"软件包与现有软件包存在冲突"。

**`push-apk.ps1` 支持双 flavor**：
- 新增 `-Flavor debug|release`、`-Password`、`-DryRun`；按 flavor 选择 APK 路径与远程命名 `app-{flavor}-vX.Y.Z.apk`。
- 用哈希表 splat 传参，修复数组 splat 传 switch 跨脚本失效的问题。

**后端识别 release 命名**（`services/api/internal/handler/release_handler.go`）：
- `listAll` 同时匹配 `app-debug-v` 与 `app-release-v` 前缀，flavor 不参与版本比较。
- release APK 推上去后 `/api/v1/release/latest` 正确返回 `app-release-vX.Y.Z.apk`。

#### 验证
- `go build ./internal/... ./cmd/...` 与 `go vet ./internal/handler/` 通过。
- `build-all.ps1 -DryRun -Flavor debug -PushApk` 全链 dry-run 通过。
- Release APK（v1.8.44）签名指纹与正式 keystore 一致。
- `build-all.ps1 -Flavor release -PushApk -Password ...` 端到端发布成功：APK 推送至 NAS、
  `home-api` 重建重启，`/api/v1/release/latest` 返回 `app-release-v1.8.44.apk`（version_code 10844，release_notes 已挂载）。

#### 版本
- Backend: v1.8.44（release_handler 支持 `app-release-` 前缀）
- Web: v1.8.44（无前端改动）
- Android: v1.8.44（正式 Release 签名 + push-apk 双 flavor）

### v1.8.43 — Android 整理为 Gradle 工程并成功构建 APK (2026-08-14)

> **背景**：参考客户端此前是单文件 `deploy/android/HomeDatacenterClient.kt`，无法真正构建、调试、lint 或打 APK。
> 本轮把它整理成标准 Android Gradle 工程（`android/`），让后台保活 / WS 韧性 / 错误上报能作为可安装应用落地。

**工程骨架**（`android/`）：
- `settings.gradle.kts` / `build.gradle.kts` / `app/build.gradle.kts`：module 结构、AGP 8.9.1 + Kotlin 2.0.21 +
  serialization、Retrofit/OkHttp/Coroutines 依赖、`BuildConfig` 里 baked 默认 Base/WS URL。
- Gradle wrapper（8.11.1）+ `local.properties`（sdk.dir 指向本机 SDK，不提交）。

**代码迁移**（`app/src/main/java/com/example/homecenter/`）：
- `HomeCenterClient.kt`：纯协议层——数据模型、Retrofit API、`HomeCenterRepository`、`HomeCenterFactory`、`ClientErrorReporter`、
  `HomeCenterWebSocket`（心跳 + 订阅持久 + 指数退避重连）、`NetworkMonitor`。
- `HomeCenterService.kt`：前台服务（`START_STICKY` + 通知 + `FOREGROUND_SERVICE_CONNECTED_DEVICE`），后台保活 WS；
  缺省权限已在 `AndroidManifest.xml` 声明。
- `TokenStore.kt` / `MainActivity.kt`：token/URL 持久化 + 绑定/启停 UI。
- `AndroidManifest.xml`：`INTERNET` / `ACCESS_NETWORK_STATE` / 前台服务等权限 + Service 声明。

**修复的构建问题**：
- 注释内含 `*/`（`/api/v1/* endpoint.`）导致整个文件被当作未闭合注释、类全部不可解析。
- `NetworkCallback` 实际是 `ConnectivityManager` 的嵌套类，修正 import。
- `retrofit2-kotlinx-serialization-converter:1.0.0` 包名是 `com.jakewharton.retrofit2.converter.kotlinx.serialization`，
  修正 `asConverterFactory` import。
- `HomeCenterService` 缺 `OkHttpClient` import；`ClientErrorReporter` 折叠时 `filter` 返回 `List` 需 `toMutableList()`。

**发布流程**（`android/push-apk.ps1`）：
- 一键构建 debug APK → 更名 `app-debug-v<version>.apk` → 写 `release-notes-v<version>.txt` → scp 到 NAS
  `data/releases/`。版本号从 `app/build.gradle.kts` 自动读取；支持 `-Notes`、`-BuildOnly`、`-DryRun`、`-Password`。

**去冗余**：
- 删除 `deploy/android/HomeDatacenterClient.kt`（980 行单文件参考客户端）——其协议层/Service/Activity 已完整迁移进
  gradle 工程，避免两份源码漂移。

#### 验证
- `./gradlew assembleDebug` 构建成功，产出 `app/build/outputs/apk/debug/app-debug.apk`（约 6.3 MB）。
- APK 已推送至 NAS `data/releases/app-debug-v1.8.43.apk`（versionCode 124, versionName 1.8.43），可直接安装。
- `push-apk.ps1` 端到端跑通（构建/更名/写 notes/推送 NAS）。

#### 版本
- Backend: v1.8.43（无代码改动）
- Web: v1.8.43（无前端改动）
- Android: v1.8.43（整理为 Gradle 工程，构建并推送可安装 debug APK）

### v1.8.42 — 后台保持 + 预加载加固 + Android 端同步 (2026-08-14)

> **背景**：审查发现两个短板——① **后台 WebRTC 保持"未生效"**：全工程无任何 `visibilitychange` 处理，完全依赖
> 浏览器默认；切后台时 ICE 8s 误判定时器照跑，网络一抖回来就被切到 HLS 或报"播放失败"。② **预取可能"旧盖新"**：
> `usePrefetch` 与 `useCachedFetch` 双写同一 sessionStorage key 无版本控制，异步预取可能用旧快照覆盖正在显示的最新
> 数据。同时把 Web 这轮的"上报 + 网络韧性 + 后台保持"能力同步到了 Android 参考客户端（Android 无视频播放器，
> 同步的是错误上报 / WS 韧性 / 前台服务后台保活三类）。

**后台保持**（`web`）：
- **useWebRTCStream**（`hooks/useWebRTCStream.ts`）：`document.hidden` 时**挂起 ICE 8s 误判定时器**（后台节流 +
  网络空转不再错报 error）；新增 `visibilitychange` 处理——回前台时若 ICE 仍 disconnected/failed 或 connectionState
  已 failed/closed，**drop 旧 PC 并重新协商**（retry），用户回来看到的是一路清晰的直播而非吊在后台的陈旧 HLS 回退。
- **PreviewFrame**（`components/LiveVideo.tsx`）：预览帧 10s 轮询在后台**暂停**（没人看的摄像头缩略图不再白刷），
  回前台先立即刷一帧再恢复节奏。
- **useHLSStream**（`hooks/useHLSStream.ts`）：后台不触发 stall 看门狗；回前台**重新武装 15s 看门狗**给 stream
  一个重新缓冲的窗口。

**预加载加固**（`web`）：
- **usePrefetch**（`hooks/usePrefetch.ts`）：**新鲜度守卫**——预取结果仅在缓存缺失或已超 30s TTL 时才写入，绝不
  覆盖新数据（修复"画面倒退/旧盖新"）；预取失败改为**上报后端**（context "prefetch"，不再静默丢弃）。
- **splash 并行预取**（`App.tsx`）：等待 `/user/me` 的 splash 空转期间，并行预取 `home.cameras.list`，Dashboard 与
  Cameras 页首帧都能从缓存渲染、零闪烁。

**Android 客户端**（`android/`，Gradle 工程）：
- **错误上报**：新增 `POST /api/v1/system/client-errors` 接口 + `ClientErrorReport` 数据模型 + `ClientErrorReporter`
  （2s 全局限流 + 60s 去重折叠 count + 协程后台上报，永不抛异常）；WS `onFailure` 接入上报（context
  "android.ws"），失败会出现在 Dashboard 日志面板。
- **WS 网络韧性**：新增 `NetworkMonitor`（ConnectivityManager 网络回调）——网络恢复（WiFi↔蜂窝切换、飞行模式
  关闭）时 `onNetworkAvailable()` 立即重连，不等退避定时器跑完；指数退避重连 + 订阅重放沿用既有实现。
- **后台保活**：新增 `HomeCenterService` 前台服务（`START_STICKY` + 通知 + `FOREGROUND_SERVICE_CONNECTED_DEVICE`
  类型），后台时 WS 不被系统回收；`NetworkMonitor` 触发即时重连；文档化所需 manifest 权限。

#### 验证
- `web`：`npx tsc -b` 零错误 + `npm run build` 成功，已部署 NAS（web 镜像重建，JS 哈希 `index-DpLpzBnB.js`）。
- 后端无改动（`home-api` 未重建，healthy）。
- Android：参考客户端源码整理进 `android/` Gradle 工程（v1.8.43），可构建并推送 APK。

#### 版本
- Backend: v1.8.42（无代码改动）
- Web: v1.8.42（WebRTC/预览帧/HLS 后台保持 + 预取新鲜度守卫/失败上报 + splash 并行预取）
- Android: v1.8.42（客户端错误上报 + WS 网络韧性 + 前台服务后台保活）

### v1.8.41 — 稳定性 × 纠错 × 上报三件套 (2026-08-14)

> **背景**：整条链路（后端核心 + 前端播放 + 前端网络韧性 + 客户端上报）的"稳定性提升、错误后纠错、典型错误
> 服务器上报"。目标是：进程不轻易死（优雅停机/recovery）、挂了能被看见（panic/5xx/渲染错误/播放失败统一
> 落 SystemLog）、网络抖动能自愈（指数退避重试/重连）、客户端上报不刷屏也不丢（按用户限流 + 队列去重）。

**后端核心**（`services/api`）：
- **优雅停机**（`cmd/main.go`）：SIGTERM/SIGINT → `s.Shutdown`（10s 排水窗口）→ 停 audit-log 订阅者 → 关 EventBus，
  各组件经 defer 栈干净退出，替代 Docker SIGKILL 强杀。
- **自定义 Recovery 中间件**（`internal/middleware/recovery.go`）：接管 gin 默认 recovery，捕获 panic → 写
  "server.panic" 关键级 SystemLog + 实时广播 → 返回统一 `{code:500,data:null}`，不再返回空 body。
- **5xx 错误日志中间件**（`internal/middleware/errorlog.go`）：每次 >=500 响应持久化为 "server.error" SystemLog，
  带 **60/min 窗口限流 + 10min 去重**，避免错误风暴刷屏。
- **统一错误工具**（`internal/utils/errors.go`）：sentinel error + `APIError` + `error→HTTP 状态码` 映射。
- **统一重试工具**（`internal/utils/retry.go`）：指数退避 + 抖动；`pushRetentionWithRetry` 由固定 30s 改为
  5s→40s 退避。
- **健康检查**：新增 `/health/ready` 就绪探针（DB ping 门控；MQTT 仅上报、可选），供 Docker/CF 探针区分
  "进程活"与"可服务"。
- **客户端上报强化**（`internal/handler/client_error_handler.go` + `model/system_log.go`）：限流改为**按
  用户/IP 分桶**（默认 60/min/key，防单用户吃光全局配额）；去重改为索引列 `context` 精确比较（替代脆弱的
  payload JSON LIKE）；`LevelWarning` 纳入清理上限（200 条）。

**前端播放上报**（`web`）：
- **ErrorBoundary**（新增 `components/ErrorBoundary.tsx`）：捕获渲染/生命周期异常（window onerror 抓不到的
  那类），上报 `render.error.*` 并渲染重试卡片；已包在 App 路由外层。
- **HLS 失败上报**（`hooks/useHLSStream.ts`）：hls.js fatal 错误 + 15s stall 看门狗 → `playback.hls(.stall)`。
- **WebRTC 失败上报**（`hooks/useWebRTCStream.ts`）：SDP/连接失败、ICE 8s 超时、`connectionState failed`、
  `<video>` 解码错误(3/4) → `playback.webrtc(.decode)`。
- **token 防御**（`api/client.ts`）：`getToken/setToken/clearTokenAndRedirect` 全部 try/catch 包裹，私有浏览/
  存储被禁用时不再崩溃，降级为"无 token → 401 → 回登录"。

**前端网络韧性**（`web`）：
- **axios 幂等重试**（`api/client.ts`）：网络错误(0) 与 5xx 对**幂等方法**(GET/HEAD/OPTIONS/PUT/DELETE) 指数
  退避重试（1s→4s，最多 2 次）；POST 永不自动重试（避免副作用重复提交）。
- **WS 指数退避 + 重连刷新**（`hooks/useWebSocket.ts`）：固定 3s 改为 1s→15s 封顶指数退避 + ±20% 抖动（多
  tab 不齐步回连）；订阅集合持久化，重连后自动重发 `subscribe`，非 admin 断线不再静默丢事件。

**客户端上报强化**（`web`）：
- **上报器队列 + 去重**（`lib/errorReport.ts`）：内存队列 + 离线缓冲（网络故障时缓存、8s 退避重试、上限 5 次
  后丢弃）；tab 内 60s 去重折叠（同 context+message 只入队一次并累加 count）；队列上限 50 防内存爆炸。
  后端 `client.error` 沿用既有按用户限流 + 结构化去重合并。

#### 验证
- `go test ./internal/... ./cmd/... ./tools/...` 全部通过。
- `web`：`npx tsc -b` 零错误 + `npm run build` 成功。（顶层 `scripts` 包为历史遗留多 `main` 问题，非按包编译，与本次无关。）

#### 版本
- Backend: v1.8.41（优雅停机 + recovery/errorlog 中间件 + 统一 retry + /health/ready + 客户端上报按用户限流）
- Web: v1.8.41（ErrorBoundary + HLS/WebRTC 播放上报 + token 防御 + axios 幂等重试 + WS 指数退避/重连刷新 + 上报队列去重）
- Android: v1.8.41（无改动）

### v1.8.40 — 波次2 单元测试扩展：自动化引擎 / WS hub / gorm 仓储 (2026-08-14)

> **背景**：测试覆盖计划第 2 波。此前仓内单测基本都是纯函数测试（`triggerMatches`、
> `timeInRange`、`conditionMatches`、MQTT 载荷解析），本次补齐**运行时路径**测试，覆盖引擎有状态
> 热路径、WS hub 扇出路由、以及基于 in-memory SQLite 的 GORM 仓储层。

**1. 自动化引擎**（`internal/automation/engine_runtime_test.go`）：
- `handleEvent` 全链路（真事件 → mock MQTT 发布 → `cooldown_s` 节流丢弃，断言 `Fires`/`Dropped`/`EventsSeen`）；
  条件不匹配 → 不触发也不丢弃。
- `throttleAllows`（cooldown / rate_per_min 滑窗 / dedup）、`dedupKey`、`recordFire`。
- `Reload` 只载入启用规则 + 运行时剪枝、存活规则保留在线冷却；`PinCooldown`（含未知规则报错）。
- Action：`mqtt`（命名空间白名单、默认 QoS1、nil handler）、`notify`（EventBus 发布 + 默认标题/正文）、未知类型。
- Webhook：SSRF 防护（loopback/私网/link-local/坏 scheme/缺 host 全拒）+ 注入 `roundTripFunc` 传输层
  （不触网）验证成功、5xx 重试、4xx 不重试。

**2. WS hub**（`internal/ws/hub_test.go`）：
- `Broadcast` / `SendToUser` / `SendToAdmins` / `routeDeviceEvent` / `matchesSubscription` 扇出路由；
  `onEvent` 的目标通知、全量广播、设备事件路由；`Register`/`Unregister`/`Close`。
- 测试直接构造 Client（缓冲 `send` 通道，无真实 `*websocket.Conn`），因 hub 扇出只写入通道。

**修复一个真实 bug**（由新测试暴露）：`hub.go::onEvent` 的 `user.notification` 分支本意在载荷解析失败时
回退到广播，但 Go 的 switch **没有隐式 fallthrough**，缺 `fallthrough` 关键字导致畸形通知被静默丢弃。已补上。

**3. gorm 仓储**（`internal/repository/`）：
- `:memory:` SQLite + `SetMaxOpenConns(1)`（保证共享同一内存库）+ 迁移 Device / User。
- Device：Create / GetByID / GetByUserID / GetAll / GetByAccessKeyHash / GetByUserIDAndHash / Update /
  Revoke / IsRevoked / IncrementTokenVersion / UpdateLastSeen / Delete / DeleteByUser 级联 + **释放 ID 复用**。
- User：Create / GetByID / GetByName / List / Update / Delete / CountAdmins / CountDevicesByUser + ID 复用 + 唯一名约束。

#### 验证
`go test ./internal/... ./cmd/... ./tools/...` 全部通过。（顶层 `scripts` 包为历史遗留构建问题——
内含多个 `main` 文件，按 `go run scripts/<file>.go` 单个运行，非按包编译，与本次无关。）

#### 版本
- Backend: v1.8.40（波次2 单测 + hub fallthrough 修复）
- Web: v1.8.40（无前端改动）
- Android: v1.8.40（无改动）

### v1.8.39 — 环境诊断：NAS IPv6 曾被整体禁用 + ddns 指向已断开的 enp4s0 (2026-08-14)

> **背景**：用户反馈"当前环境没有 IPv6、摄像头不在线"。排查发现两个与 v1.8.38 前缀同步**无关但更根本**的问题，
> 均属**运维/物理层**，代码无需改动。

**问题 1 — NAS IPv6 被 NetworkManager 整体禁用（已修复）**：
- NAS 活跃口 `eno1` 在 NetworkManager 连接 `Wired connection 1` 上 `ipv6.method=disabled`，且内核
  `net.ipv6.conf.eno1.disable_ipv6=1`、`accept_ra=0` → NAS 完全没有 IPv6（连 link-local 都没有）。
- 已执行：`nmcli connection modify "Wired connection 1" ipv6.method auto` + `nmcli connection up`，
  并 `echo 0 > /proc/sys/net/ipv6/conf/eno1/disable_ipv6`。恢复后 `eno1` 获得 VLAN 全局地址
  `2409:8a70:37ad:6870::e43`、SLAAC `...bd08` 及默认路由（经网关 `fe80::a6a9:30ff:fe91:3b25`），
  LAN 内 `http://[...bd08]:8088/` 返回 200。

**问题 2 — ddns `nas.feiyemomo.top` 指向已物理断开的 enp4s0（待用户处理）**：
- ddns 解析到 `2409:8a70:37ad:6870:62be:b4ff:fe08:bd09`，这是 NAS **另一网卡 enp4s0** 的 EUI-64 稳定地址。
- 实测 `enp4s0` 到网关 100% 丢包、ARP 邻居表全空、carrier 虽 up 但无法上联 → **该口网线/端口物理断开**，
  `...bd09` 收不到包，ddns 当前不可达。系统已有 `ipv6-stable-addr.service` 在 enp4s0 上维护该地址，
  本次已将其内旧前缀 `2409:8a70:37a3:99d0` 更新为当前 `2409:8a70:37ad:6870`。
- 恢复路径二选一：① 接好 enp4s0 网线（ddns 按现状即可用，代码零改动）；② 把 ddns / 代码三处
  （compose、frigate、Android）改指活跃口 `...bd08`。**待用户确认方向后处理。**

**摄像头（前门 / 院子）**：`192.168.31.100` / `.101` 仍物理离线（ARP FAILED、554/80 全关），
与 v1.8.38 结论一致，需现场检查电源与网线。

#### 版本
- Backend: v1.8.39（无代码改动，运维+文档）
- Web: v1.8.39（无前端改动）
- Android: v1.8.39（无改动）

### v1.8.38 — IPv6 前缀轮换：配置同步到 ddns 当前前缀 + 摄像头离线排查 (2026-08-14)

> **背景**：运营商 DHCPv6-PD 再次轮换 /64 前缀，IPv6 ddns `nas.feiyemomo.top` 已自动更新到新前缀
> `2409:8a70:37ad:6870`（SLAAC EUI-64 接口 ID `62be:b4ff:fe08:bd09` 稳定），但后端配置仍停留在旧前缀
> `2409:8a70:37a8:80c0`，导致 API `network/status` 报告不可达的 IPv6 地址、误导客户端走 IPv6 直连。

**改动**（旧前缀 → ddns 当前新前缀 `2409:8a70:37ad:6870:62be:b4ff:fe08:bd09`）：
- `compose.yaml`：`NAS_IPV6_ADDRESS` 默认值更新。
- `deploy/frigate/config.yml`：`go2rtc.webrtc.candidates` IPv6 条目更新。
- `.env.example`：`NAS_IPV6_ADDRESS` 示例值更新。
- NAS `/vol1/docker/home-datacenter/.env`：`NAS_IPV6_ADDRESS` 更新（`.env.bak-*` 备份，部署脚本不覆盖 .env）。

> 说明：Android `BaseUrlResolver.IPV6_DIRECT_URL` 自 v1.6.32 起已用 ddns 域名 `http://nas.feiyemomo.top:8088/`，
> 无需改动；web 端 Network 页也已识别域名。后端 `NAS_IPV6_ADDRESS` 因需 `net.ParseIP` 校验 + WebRTC ICE 需 IP:port，
> 仍用 IP 字面量，本次同步到当前前缀。

#### 验证（NAS 192.168.31.235 实测，2026-08-14）
- `docker compose up -d` 重建 `home-api`（env 变化触发 recreate），`/health` 返回 `{"status":"ok"}`。
- api 日志 `network:` 行已报新地址 `direct=http://[2409:8a70:37ad:6870:62be:b4ff:fe08:bd09]:8088/`，
  且 `camera: webrtc candidates pushed after boot replay` 表明新 candidates 已推送到 go2rtc。

#### 摄像头离线排查（前门 / 院子）
- 从 NAS 探测：两台（`192.168.31.100` / `.101`）ping / RTSP:554 / ONVIF:80 **全部失败**，ARP 邻居表 `FAILED`。
- 全 `/24` 扫描（端口 80 + 554）未发现任何 RTSP 摄像头，确认**摄像头已从局域网消失**（断电 / 断网 / IP 变更），
  属物理层问题，待物理检查电源与网线后恢复；非配置问题。

#### 版本
- Backend: v1.8.38（IPv6 前缀同步）
- Web: v1.8.38（无前端改动）
- Android: v1.8.38（无改动）

### v1.8.37 — 修复录像回放间歇性转码失败导致的 CPU 占满 (2026-08-14)

> **"一看录像 CPU 就占满"的根因**：录像回放的 ffmpeg 转码输出直接写到 btrfs 绑定挂载的缓存卷（`/data/recordings/.transcode-cache`）。`-movflags faststart` 需要在写完后**重开输出文件**做第二遍扫描把 moov 移到文件头；在 btrfs 上这个重开**间歇性失败**（报 `Unable to re-open output file for shifting data`），此时**完整编码已经跑完**却最终失败。代码原有的软件兜底（libx264）用同样的参数/路径重转，**同样失败**，并在 J4125 上又烧掉约 40 秒 CPU——用户看到的是"录像打不开 + CPU 被占满"，重试会反复触发，症状持续。

**改动**：
- `internal/handler/camera_handler.go`：转码输出改写到容器 **overlay 文件系统（/tmp）**——faststart 重开在其上稳定可靠；转码完成后用新增的 `copyToCache` 把成品**复制**进缓存卷（同文件系统临时文件 + 原子 rename，读者永远看不到半成品）。跨文件系统不能 rename（EXDEV），故用复制而非移动。
- 效果：btrfs 的 faststart 重开问题从关键路径上消除，VAAPI 硬件转码一次成功、低 CPU；软件兜底同样受益。

#### 验证（NAS 192.168.31.235 实测，2026-08-14）
- 容器内对真实录像做 6 段（整分钟）多段拼接转码：写到 `/tmp` 成功（faststart 正常），复制到 btrfs 缓存卷后 `cmp` 字节完全一致。
- Go 主程序编译 + `go vet ./internal/handler/` 通过。

#### 录像配额上调至 400 GiB（同日）
- `configs/config.yaml`：`recording_quota_bytes` 由 300 GiB（322122547200）上调至 **400 GiB（429496729600）**——约为 466G 数据卷的 86%。超限时保留自动从 7 天降到 3 天，回落时恢复 7 天。
- 已 scp 到 NAS 并重启 `home-api`，容器内 `/configs/config.yaml` 确认生效，`/health` 返回 `{"status":"ok"}`。

#### Android：录像进度条按实际覆盖跨度显示（v1.8.37 / versionCode 123）
> **问题**：录不满一天的录像（如摄像头定时/离线只录了几小时），进度条仍按固定的 24h 排布，几个小时的录像和红色告警段被压缩到进度条一小段里，看起来"进度条还是按 24h 来的"。
- `RecordingsDialog.kt`：`dayTotalMs` 从恒定的 24h 改为**实际录像覆盖跨度**（第一段录像开始 → 最后一段录像结束，`最后结束 = 最后一段 startAt + durationSeconds`）。SeekBar max、告警 overlay max、clip 偏移 clamp、时长标签（不再跨到次日）全部随之取实际窗口；满一天录像仍 ≈24h，行为不变。
- 已 `gradlew :app:compileDebugKotlin` 编译通过。

#### 版本
- Backend: v1.8.37（含 v1.8.37 转码修复 + 配额 400 GiB）
- Web: v1.8.37（无前端改动）
- Android: v1.8.37（progress bar 修复）

### v1.8.36 — 配额告警落地为 warning 事件 + Android 管理员专用琥珀横幅 (2026-08-14)

> **补齐配额功能"最后一块拼图"**：此前配额超限时后端只缩短 Frigate 留存（`PushRecordRetention`），**不发布任何 `system.recordings_size` 事件**；而配额本身的告警阈值（`recording_size_warn_bytes` / `recording_size_crit_bytes`）为 0（禁用）。结果是**配额用尽时前端（Dashboard / Android）完全感知不到**，只有留存被悄悄缩短。本次让配额超限/恢复也发布事件，并在 Android 端以管理员专属横幅呈现。

**改动**：
- **后端**（`internal/maintenance/recordings_size.go`）：配额超限时发布 `system.recordings_size` 事件（`level=warning`，消息"录像配额已用尽…已自动缩短录像保留天数"）；配额恢复时发布 `level=normal` 恢复事件。新增 `emitQuotaEvent` 辅助方法，与既有磁盘/备份/资源告警统一走 LevelWarning。
- **后端告警级别统一**：磁盘、备份容量、内存/CPU 等监控的 warn 档均从 `LevelNormal` 改为 `LevelWarning`（`disk.go` / `backup.go` / `sysres.go`，见 `internal/model/system_log.go` 新增 `LevelWarning`）。
- **Android**（v1.8.36 / versionCode 122）：`SystemLogLevel.WARNING` + 两个日志 adapter 琥珀色渲染；Dashboard 新增**配额告警专用横幅**（`quotaAlertBanner`），仅在 `system.log` WebSocket 分支的 **admin 门控**内调用 `handleQuotaAlert`——非 admin 用户收不到也看不到；收到 `level=normal` 恢复事件自动隐藏；点击横幅跳转日志页。

#### 验证（NAS 192.168.31.235 实测，2026-08-14）
- 临时降配额至 10M 并重建 api（录像 ~737M > 10M）：日志落库 `id=4331 level=warning type=system.recordings_size msg=录像配额已用尽 736.9 MiB（已自动缩短录像保留天数）`。
- 恢复配额 300GiB + 重建 api：容器 healthy，Frigate 留存回到 7 天。
- Android APK `app-debug-v1.8.36.apk` 构建成功并推送至 NAS `data/releases`，`/api/v1/release/latest` 返回最新版本。

#### 版本
- Backend: v1.8.36
- Web: v1.8.36（无前端改动）
- Android: v1.8.36

### v1.8.35 — 修复启动竞态：配额缩减不再被全量配置推送覆盖 (2026-08-14)

> **实测配额触发链路时揪出的真实 bug**：api 启动时，`RecordingSizeMonitor` 会**立即采样一次**（`Run()` 首行），与 `BootReplay` 的全量配置推送（`requires_restart=1`）**并发**执行。两个 goroutine 谁后落地谁生效——若配额动作先发出（把留存缩到 3 天）而全量配置推送随后用正常值（7 天）覆盖，则**最终停留在 7 天**；而配额推送本身已"成功"，`quotaActive=true`，监控不再重试——**超配额却永远不缩减留存**，配额功能形同虚设。

**修复思路（让"当前留存值"成为共享权威）**：
- `FrigateClient.PushRecordRetention` 成功前先把目标天数写回共享字段 `c.retentionDays`（新增 `retentionMu` 锁保护，见 `internal/camera/frigate.go`）。
- `PushConfig`（全量推送 / 相机增删）在锁内快照 `c.retentionDays` 作为留存值。
- 效果：无论 goroutine 顺序如何，最终留存都等于配额最近一次设定的值——两种顺序下都收敛到 3 天，不再被覆盖。

#### 验证（NAS 192.168.31.235 实测，2026-08-14）
- 配额 100M 下重建 api：日志完整走通**重试链路**——attempt 1/6 连接拒绝（Frigate 重启中）→ attempt 2/6 连接拒绝 → attempt 3/6 500（Frigate 加载中）→ attempt 4/6 成功 `record retention set to 3 days`。
- 读回 Frigate `/api/config`：`continuous.days=3.0`、`motion.days=3.0`（此前实测会被全量推送覆盖回 7.0）。
- 恢复配额 300GiB 重建 api：留存回到 `continuous.days=7.0`、`motion.days=7.0`。

#### 版本
- Backend: v1.8.35
- Web: v1.8.35（无前端改动）
- Android: v1.8.24（无改动）

### v1.8.34 — 录像配额动作带回退重试机制 (2026-08-14)

> **前序问题的加固**：配额监控首次采样（api 启动即触发）可能与 BootReplay 触发的 Frigate 重启重叠，此时 `/api/config/set` 返回瞬态 400/500/连接拒绝。若动作失败就被永久跳过（`quotaActive` 提前置真），配额缩减永远不生效。

**修复内容**：
- **回调返回 error**（`internal/maintenance/maintenance.go` + `recordings_size.go`）：`OnQuotaExceeded` / `OnQuotaRecovered` 改为返回 `error`，监控**仅在动作成功后才**提交跨越状态（`quotaActive`）。失败记录日志并保持原状态，下次采样重试。
- **`pushRetentionWithRetry`**（`cmd/main.go`）：6 次尝试、间隔 30s（约 3 分钟窗口），覆盖 Frigate 重启（约 2 分钟完成）；Frigate 自身清理任务在下次运行时应用新窗口。

#### 版本
- Backend: v1.8.34
- Web: v1.8.34（无前端改动）
- Android: v1.8.24（无改动）

### v1.8.33 — 修复服务监控"不可达"误报 + 打通被静默禁用的 MQTT 链路 (2026-08-14)

> **根因不是"启动竞态"，而是主机名写错**：compose 为容器设置了 `container_name`（`home-web` / `home-mosquitto`），Docker 只注册**容器名**为网络别名，裸服务名 `web` / `mosquitto` 根本不可解析。从 v1.8.28 起服务监控用 `web` / `mosquitto` 探测——**每次探测都因 "bad address" 失败**，对每个服务都报了假"不可达"；同时 api 的 MQTT broker 和 Frigate 的 MQTT host 也用了 `mosquitto`，导致**整个 MQTT 实时链路（api 订阅 + Frigate 检测事件上报）自始就未连通**。

**修复内容**：
- **服务监控探针主机名**（`cmd/main.go`）：`http://web/` → `http://home-web/`，`mosquitto:1883` → `home-mosquitto:1883`。
- **服务监控启动宽限期**（`internal/maintenance/service.go` + `maintenance.go`）：`ServiceStartupDelay` 新增，`Run()` 首次探测前先等待（本机 60s），避免容器重启瞬间的短瞬 DNS/网络抖动触发误报（纵深防御；主因仍是主机名）。
- **api MQTT broker**（`configs/config.yaml` + `config.go` 默认值）：`tcp://mosquitto:1883` → `tcp://home-mosquitto:1883`。
- **Frigate MQTT host**（`deploy/frigate/config.yml`）：`host: mosquitto` → `host: home-mosquitto`。
- **compose.yaml 注释**更正为正确的主机名。

#### 验证（NAS 192.168.31.235 实测，2026-08-14）
- api 日志：`mqtt connected to tcp://home-mosquitto:1883` 并订阅全部主题（含 `frigate/events`）；`service monitor startup grace 1m0s before first probe`；重启后 5 分钟内 **0 条**"不可达"告警。
- mosquitto 日志：`New client connected ... as frigate` + `as home-datacenter`——**Frigate 与 api 均成功连上 broker**（此前 MQTT 全链路不可用）。
- api `/health` 返回 `{"status":"ok"}`。

#### 版本
- Backend: v1.8.33
- Web: v1.8.33（无前端改动）
- Android: v1.8.24（无改动）

### v1.8.32 — 录像配额自动缩短留存 + WebRTC 候选重启感知推送 (2026-08-14)

> **两块能力补全**：① 录像配额监控——录像总占用超阈值时**自动**把 Frigate 留存从 7 天缩短到 3 天（旧片在下一个清理周期被删），回落后自动恢复 7 天，避免小磁盘被录像悄悄撑满；② 修复 WebRTC `candidates` 推送的重启竞态——顺带揪出真正的根因是 API 载荷格式错误（一直 400），重构为"重启前推送 + 深合并持久化"，重启后候选不再丢失。

**录像配额自动缩短留存**：
- **`RecordingSizeMonitor` 新增配额边沿检测**（`internal/maintenance/recordings_size.go`）：`recording_quota_bytes` 非 0 时，录像总占用跨越配额即触发 `OnQuotaExceeded`（推送缩短留存），回落触发 `OnQuotaRecovered`（恢复正常留存）。边沿触发，不重复告警。
- **`FrigateClient.PushRecordRetention`**（`internal/camera/frigate.go`）：`PUT /api/config/set` 只发 `record.continuous.days` / `record.motion.days` 部分配置，`requires_restart=0` 无需重启，Frigate 下次清理周期即删超出窗口的旧片。
- **留存可配置**：`PushConfig` 的 retention 不再硬编码 7 天，`SetRetentionDays` 由 `recording_retention_days` 控制（默认 7）。
- **配置**（`config.yaml`）：`recording_quota_bytes`（默认 0 关闭，本机设 300GiB≈466G 盘的 65%）/ `recording_retention_days`（7） / `recording_reduced_retention_days`（3，必须 < 正常值）。

**WebRTC 候选重启感知推送**（修复遗留技术债）：
- **根因**：`SetWebRTCCandidates` 把方案裸发 `{go2rtc:...}`，而 Frigate `/api/config/set` 要求 `{config_data:...}` 包裹——所以**每次**候选推送（BootReplay 和 PrefixWatcher 兜底）都返回 400 `No configuration data provided`，候选一直没真正更新过。已改为与 `PushRecordRetention` 相同的 `config_data` 包裹格式。
- **重启感知**：`BootReplay` 改为**先推送候选、再推全量配置**（全量配置触发重启）。候选是深合并部分更新，先持久化进 `config.yml`，随后重启从文件加载——彻底消除"先重启后推送撞上 connection refused"的竞态。`pushWebRTCCandidatesWithRetry` 只吸收启动窗口（Frigate REST 5000 比 go2rtc 1984 慢）。

#### 验证（NAS 192.168.31.235 实测，2026-08-14）
- api 日志：`camera: webrtc candidates pushed after boot replay`（此前是 `400 No configuration data provided`）。
- 运行时 `config.yml` 候选为 `127.0.0.1:8555` + `192.168.31.235:8555` + `[IPv6]:8555` 三个；`docker restart home-frigate` 后三者完整保留、容器 healthy。
- Frigate 留存当前 `continuous.days: 7` / `motion.days: 7`（正常值）；录像当前 738M，远低于 300GiB 配额，配额动作不触发（符合预期）。
- 配额监控已随维护循环运行，指向 `/media/frigate/recordings`，1h 采样。

#### 版本
- Backend: v1.8.32
- Web: v1.8.32（无前端改动）
- Android: v1.8.24（无改动）

### v1.8.30 — 异地备份恢复演练 + 备份健康/留存监控 (2026-08-14)

> **把 v1.8.29 的异地备份从"能推上去"补全到"能验证、能告警、能恢复"**：新增一键恢复演练脚本证明 bucket 可恢复；新增 `BackupMonitor`，同步失败/停滞会写 `system.backup` 严重告警触达 dashboard；bucket 对象/容量超阈值告警，防止免费额度悄悄被撑爆。

**新增/修改**：
- **`scripts/restore-dry-run.sh`**（恢复演练 SOP）：从亿安云拉取最新快照 → `PRAGMA integrity_check` + 关键表计数校验 → 打印 `RESTORE OK` → 清理。NAS 上跑 `sh scripts/restore-dry-run.sh`。
- **`BackupMonitor`**（`internal/maintenance/backup.go`）：边沿触发，两路告警——
  - **失败/停滞**：状态文件 `ok:false`、或超过 `backup_stale_after_minutes` 未同步、或健康状态文件丢失 → `system.backup` critical。
  - **留存/容量**：bucket 对象数或总字节超 `backup_warn/crit_files` / `backup_warn/crit_bytes` → `system.backup` warn/crit。
- **`deploy/backup/entrypoint.sh`**：每次 sync 后用 `rclone size` 统计远端对象/字节，原子写入 `data/backup-state/last.json`（`ok` 为 JSON 布尔）；新增 `ONCE=1` 单次执行模式。
- **`compose.yaml`**：`backup` 加可写 `/state` 挂载；`api` 加只读 `/data/backup-state:ro` 挂载。
- **配置**：`backup_state_path` / `backup_monitor_interval_minutes`(5) / `backup_stale_after_minutes`(720) / 四个留存阈值（默认 0 关闭）。

#### 验证（NAS 192.168.31.235 实测，2026-08-14）
- backup 重启后状态文件正确：`{"ok":true,"remote_files":1,"remote_bytes":716800}`
- 失败告警：置 `ok:false` → api 写 `system.backup critical 异地备份失败：rclone sync failed`，落库完整
- 自愈：恢复 `ok:true` → 不再告警（边沿触发）
- 容量告警：`backup_warn_files:1` → api 写 `system.backup warning 异地备份容量告警`，测后恢复 0
- 恢复演练：`sh scripts/restore-dry-run.sh` → `app-20260814-110103.db` integrity ok、users 4 / cameras 2、`RESTORE OK`
- 测试告警已清理，生产状态健康（`ok:true`，0 条残留）

#### 版本
- Backend: v1.8.30
- Web: v1.8.30（无前端改动）
- Android: v1.8.24（无改动）

### v1.8.31 — 修复安卓端 "All 3 attempts failed for /api/v1/user" 502 根因 (2026-08-14)

> **解决了一个会反复复发的部署级故障**：安卓 app 报"加载失败：All 3 attempts failed for /api/v1/user"。根因不是权限，而是 nginx 的 `upstream api_backend` 只在启动时解析一次 `api` 域名并缓存 IP——每次 redeploy 重建 api 容器换新 IP 后，运行中的 web/nginx 仍指向旧 IP，所有 `/api/*` 请求返回 502，安卓端 `RetryInterceptor` 连续 3 次收到 5xx 后抛出该报错。

**根因定位**（`RetryInterceptor.kt` 逻辑推导）：
- 该报错**只在连续 3 次收到 5xx 服务端错误时**触发；若是权限 403，Interceptor 会直接返回（4xx 不重试），报错会是 `HTTP 403` 而非这个文案。
- NAS 实测：`curl :8088/api/v1/user` 返回 **502 Bad Gateway**（nginx），而 api 容器 `:8080/health` 直连 200 正常；从 web 容器内 `wget api:8080/health` 也 200。
- 判定：web/nginx 缓存的 `api` 容器旧 IP 已失效（api 42 分钟前被重建，web 已运行 3 小时）。
- 瞬时修复：重启 web 容器 → `/api/v1/user` 立即恢复 200。

**永久修复**（`compose.yaml`）：
- **`home-net` 显式声明 `/24` 子网**（`172.18.0.0/24`），使容器可分配静态地址。
- **api 服务固定 `ipv4_address: 172.18.0.10`**：api 重建后 IP 不变，nginx 缓存的 upstream 永远有效，web 不再需要重启。
- 网络重建一次性完成（全部容器重启，数据卷不受影响）。

#### 验证（NAS 192.168.31.235 实测，2026-08-14）
- 部署后全部容器 healthy，api IP = `172.18.0.10`。
- `/api/v1/user` 经 nginx 返回 200，`users` 4 个。
- **自愈验证**：`docker compose up -d --no-deps --force-recreate api`（重建 api，**不动 web**）→ api 仍为 `172.18.0.10`，web 未重启（StartedAt 不变）→ `/api/v1/user` 经 nginx 仍 200。证明后续每次 api 重建都不会再触发 502。

#### 版本
- Backend: v1.8.31（无代码改动）
- Web: v1.8.31（无前端改动，仅 compose 网络配置）
- Android: v1.8.24（无改动）

### v1.8.29 — 异地备份：SQLite 每日快照同步到亿安云（Bitiful）S3 (2026-08-14)

> **解决了"数据库只有一份、且和主库同盘"的最后一块短板**：v1.8.27 的每日快照存在本 NAS 磁盘上，磁盘故障时主库和备份一起没。新增 `backup` 容器，用 rclone 把 `data/sqlite/backups/` 镜像到亿安云（Bitiful）S3 bucket，实现真正的异地第二副本。

- **新增 `backup` 容器**（`compose.yaml` `rclone/rclone:1.68`，无对外端口、仅出站、`cap_drop: ALL` + `no-new-privileges`）
- **`deploy/backup/entrypoint.sh`**：无限 `rclone sync` 循环，把 `data/sqlite/backups`（只读挂载）同步到 `s3://nas-data/home-datacenter/sqlite`。用 `sync`（镜像）而非 `copy`——本地清理掉的备份远端也删，bucket 不再膨胀
- **凭据走 `.env`**（`BITIFUL_ENDPOINT` / `BITIFUL_REGION` / `BITIFUL_BUCKET` / `BITIFUL_ACCESS_KEY` / `BITIFUL_SECRET_KEY` / `BITIFUL_SYNC_INTERVAL`），rclone 用 CLI 参数内联配置，仓库里不落任何密钥；置空即禁用（入口 exit 1，容器停在 exited）
- **NAS `.env` 单独补写**：部署 tarball 排除 `.env`，故在 NAS 上原位追加 BITIFUL 块

#### 验证（NAS 192.168.31.235 实测，2026-08-14）
- `docker compose up -d backup` 拉取镜像并启动，日志 `rclone backup loop started (interval 21600s, source /backups, dest s3://nas-data/home-datacenter/sqlite)`
- 空目录首测：`There was nothing to transfer`（确认 S3 认证 + bucket 可访问）
- 放入真实快照 `app-20260814-110103.db`（700KiB），重启容器 → `Copied (new)`、`Transferred: 1 / 1`、`sync OK`
- 容器内 `rclone lsl` 回读：`716800 2026-08-14 11:01:03 app-20260814-110103.db`，字节数与本地一致，远端对象确认存在

#### 版本
- Backend: v1.8.29（无代码改动）
- Web: v1.8.29（无前端改动）
- Android: v1.8.24（无改动）

### v1.8.28 — 主动监控增强：录像活性/服务存活/录像容量/CPU内存 + 全容器健康检查 (2026-08-14)

> **本次把"长时间运行如何自愈"落到监控层**：摄像头 RTSP 探活只能证明端口通，证明不了 Frigate 录像流水线真的在写盘。新增录像活性监控（在线但片段停滞即告警）、从属服务存活探针（web/MQTT 挂掉即告警）、录像目录容量监控（Frigate 按时间而非大小留存，1080p 集群可能先撑满盘）、CPU/内存监控，并为所有容器补齐 `healthcheck`。

#### 后端（v1.8.28）
- **录像活性监控**（`maintenance/recording.go`）：按周期扫描每台"应录像"摄像头的录像目录，最新片段 mtime 超过阈值（默认 10 分钟）即写 `system.recording` 告警并标记停滞；边缘触发、自愈（片段恢复后自动清除）
  - 离线摄像头排除在目标集外（离线本就不写盘，由 `camera.offline` 覆盖）——修掉了离线相机被误判为录像停滞的假告警
- **服务存活监控**（`maintenance/service.go`）：每 30s 探测 web（HTTP）与 mosquitto（TCP:1883），连续 3 次失败写 `system.service` 严重告警；边缘触发
- **录像容量监控**（`maintenance/recordings_size.go`）：统计录像目录总大小，超过 warn/crit 字节阈值即告警（在通用磁盘 80%/90% 告警之前先暴露录像自身增长）
- **CPU/内存监控**（`maintenance/sysres.go` + 平台实现）：采样 `/proc/stat` + `/proc/meminfo`，CPU/内存超阈值告警；build tag 跨平台
- **frigate.db 纳入每日备份**（`maintenance/sqlite.go`）：`SetFrigateDBPath` 让 Frigate 事件库也做每日 `VACUUM INTO` 快照到备份目录

#### 运维（compose.yaml）
- **全容器 healthcheck**：api 探 `/health`、web 探 nginx 根、mosquitto `pgrep`、frigate 探 `:5000/api/config`，`docker ps` 与 `depends_on` 能对死容器做出反应
- **web 探针用 `127.0.0.1` 而非 `localhost`**：nginx:alpine 的 `/etc/hosts` 把 `localhost` 映射到 IPv6 `::1`，且 nginx 只监听 IPv4 `0.0.0.0:80`，busybox wget 解析 `localhost` 先走 `::1` 导致对健康服务也 `Connection refused`——改用 `127.0.0.1` 修复

#### 验证（NAS 192.168.31.235 实测，2026-08-14）
- `docker compose ps`：api / web / mosquitto / frigate 全部 `(healthy)`；cloudflared 无文档化健康端点故不设探针
- `GET /health` → `{"status":"ok"}`；api 日志含 `maintenance: background loops started` + `sqlite WAL checkpointed (TRUNCATE)`
- 离线相机排除后无 `system.recording` 假告警，SystemLog 恢复正常事件流

#### 版本
- Backend: v1.8.28
- Web: v1.8.28（无前端改动）
- Android: v1.8.24（无改动）

### v1.8.27 — 持久化运行加固：Frigate 数据库持久化 + SQLite 维护 + 日志轮转 + 磁盘告警 + 旧包清理 (2026-08-14)

> **针对"长时间运行会不会慢慢烂掉"的审计整改**：Frigate 事件库此前存在容器可写层、容器重建即丢；`app.db-wal` 涨到 17.4MB 而主库仅 ~700KB；容器日志无上限；`data/releases` 囤了 5.1GB 旧 APK；磁盘将满时没有任何告警。

- **Frigate `/config` 持久化**（`compose.yaml`）：挂载 `./data/frigate/config:/config`，`frigate.db`、`backup.db`、`.jwt_secret`、`model_cache/` 在容器重建/升级后仍保留
- **SQLite 维护**（`maintenance/sqlite.go`）：周期性 `wal_checkpoint(TRUNCATE)`（默认 6h + 启动时一次）把 WAL 折回 `app.db` 并截断；每日 `VACUUM INTO` 快照到 `backups/`，保留最新 7 份
- **磁盘监控**（`maintenance/disk.go`）：每 10 分钟采样数据盘，80% 告警 / 90% 严重告警，写 `system.disk` 并实时推送 dashboard
- **Docker 日志轮转**（`compose.yaml`）：全局 `json-file`，`max-size: 10m` + `max-file: 3`，闲聊日志撑不死磁盘
- **旧包清理**（`release_handler.go`）：启动 + 每日清理，只留最新 5 个 APK（`data/releases` 从 5.1GB 缩到 428MB）

#### 验证（NAS 192.168.31.235 实测，2026-08-14）
- `data/frigate/config/` 已含 `frigate.db`（3.3MB）、`backup.db`、`.jwt_secret`、`model_cache/`，跨容器生命周期保留
- `docker inspect` 确认所有服务 LogConfig = `json-file / max-file:3 / max-size:10m`
- `data/releases/` 恰留 5 个最新 APK；`/vol1` 磁盘 12%（低于告警阈值故无告警，符合预期）
- `backups/` 目录 app 用户可写，`VACUUM INTO` 机制验证通过（`BACKUP_OK size=143360`）

#### 版本
- Backend: v1.8.27
- Web: v1.8.27（无前端改动）
- Android: v1.8.24（无改动）

### v1.8.26 — 回放硬件转码加速 + 缓存自动清理 + 错误上报聚合 + 按路由超时 (2026-08-14)

> **本次把上一版的四项优化建议全部落地**：回放转码从纯软件（约 40s）升级为 Intel iGPU 硬件加速（约 3s），转码缓存自动清理防止磁盘无限增长，客户端错误上报去重聚合避免日志刷屏，写超时改为仅回放接口放宽、其余路由保持 15s 安全上限。同时新增交互式 SSH 工具箱 `ssh-nas.ps1`，日常运维不再需要手敲 docker 命令。

#### 新增：SSH 工具箱
- **`ssh-nas.ps1`**：交互式 NAS 管理工具，菜单化操作，无需记忆 docker/ssh 命令
  - 服务状态 / 容器列表 / 磁盘占用 / 转码缓存占用一键查看
  - 清理 N 天前的转码缓存、健康检查、自定义命令
  - 自动读取 `deploy-nas.ps1` 的 NAS 地址与路径配置，支持密码或 SSH 密钥认证

#### 后端
- **回放硬件转码（VAAPI）**：`camera_handler.go` 的 `buildTranscodeCmd` 新增硬件流水线（`h264_vaapi` + `-hwaccel vaapi`），60s 片段转码从约 40s CPU 降到约 3s iGPU
  - `Dockerfile` 安装 `intel-media-driver` + `libva`；`compose.yaml` 将 `/dev/dri/renderD128` 传入容器并加入 render 组（GID 105）
  - `vaapiAvailable()` 防御性探测：设备缺失或驱动异常时自动回退软件 `libx264`，回放永不中断
- **转码缓存自动清理**：`StartCacheCleaner` 后台协程默认保留 7 天、每 6 小时清扫一次 `.transcode-cache`，启动时也立即清扫一次（升级即清理存量）
- **客户端错误上报聚合/去重**：`client_error_handler.go` 按（context + message）在 10 分钟窗口内去重，重复错误累加 count 而非插入 N 条重复日志；全局限流保持 60 条/分钟
- **按路由写超时**：`main.go` 全局 `WriteTimeout` 恢复 15s（避免慢客户端拖住所有连接），仅 `PlayRecording` 用 `http.NewResponseController.SetWriteDeadline` 单独放宽到 120s

#### 验证（NAS 192.168.31.235 实测，2026-08-14）
- VAAPI 可用性：容器内 `vainfo` 确认 J4125 iGPU 渲染节点可达，`h264_vaapi` 编码器可用
- 硬件转码：60s 片段转码约 3s（对比软件 libx264 约 40s），产物 ffprobe 确认 `codec_name=h264`
- 缓存清理：构造过期缓存文件后触发清扫，文件被删除且日志记录清理数量
- 错误聚合：连续上报相同错误，SystemLog 仅 1 条记录且 count 递增
- 超时行为：回放接口 120s 预算内正常返回；普通接口 15s 超时不受影响

#### 版本
- Backend: v1.8.26
- Web: v1.8.25（无改动）
- Android: v1.8.24（无改动）

### v1.8.24 — 主机 IP 变化自适应 + 网络鲁棒性增强 (2026-08-13)

#### 后端
- **LAN IP 自动检测**：`frigate.go` 新增 `lanIPDetector`，从每个 HTTP 请求的 Host 头自动提取 NAS 局域网 IPv4 地址（仅接受 RFC 1918 私有地址）
  - `main.go` 新增全局中间件，将请求 Host 头喂给 `camera.GlobalLanIP`（热路径零开销，IP 未变化时短路）
  - IP 首次检测或变化时，异步回调推送更新后的 WebRTC candidates 到 Frigate，无需手动配置 `NAS_LAN_IP`
  - 优先级：`NAS_LAN_IP` 环境变量 > HTTP 请求自动检测
- **ONVIF profile_token 自动重试**：`registry.go` 摄像头注册时若未发现 profile_token，后台每 30 秒重试一次，持续 10 分钟，网络抖动后自动恢复
- **健康检查去抖**：`health.go` 状态变更增加去抖机制，避免瞬时网络波动导致误判离线
- **Frigate 配置推送重试**：`registry.go` 摄像头注册/注销后的 config push 增加 3 次重试（2s/4s 退避），应对 Frigate 瞬时负载
- **go2rtc stop 参数可配置**：`registry.go` 摄像头流 `#stop=` 参数改为可配置，配合 `compose.yaml` 环境变量，避免 API 重启导致的流重连间隙
- **IPv6 上报开关**：`ipv6.go` 支持 `NAS_IPV6_DISABLED` 环境变量，无外网 IPv6 的局域网环境可关闭 IPv6 探测，避免误导性网络检测

#### Android
- **自定义局域网地址**：`BaseUrlResolver` 支持用户在「设置 → 局域网地址」中配置自定义 NAS 地址，持久化到 SharedPreferences，IP 变更后无需重新编译
  - `SettingsFragment` 新增配置 UI：显示当前地址、输入框、保存/恢复默认按钮
  - 保存后立即触发重新探测，切换网络路径

#### 版本
- Backend: v1.8.24
- Web: v1.8.24（无改动）
- Android: v1.8.24

### v1.8.25 — Web 回放修复 + 客户端错误上报 + 转码缓存 (2026-08-13)

> **本次修复了 Web 监控回放"看不了"的根因**：之前回放接口转码耗时超过服务器 15s 写超时，连接被强制关闭，浏览器收不到任何响应。详见下方「后端」第 1、2 条。

#### 后端
- **回放转码为 H.264**：`camera_handler.go` 的 `PlayRecording` 不再直接串流 Frigate 录制的原始码流（海康摄像头为 HEVC/H.265，Chrome `<video>` 无法解码），改为用 ffmpeg 实时转码为 H.264/AAC（libx264 veryfast + crf23 + faststart），任意浏览器均可播放
- **修复写超时断连**：`main.go` 将 `http.Server.WriteTimeout` 从 15s 提升到 120s。转码约需 40s，旧配置在转码期间超时导致连接被 Go 服务器关闭（回放表现为黑屏/无响应）
- **转码结果磁盘缓存**：`transcodeRecording` 将转码产物缓存到 `/data/recordings/.transcode-cache/<摄像头ID>/<分钟起始时间戳>.mp4`
  - 首次播放某分钟约 40s（转码），之后同分钟瞬时播放（<0.2s）
  - 产物写入缓存目录内临时文件后原子 rename（避免跨文件系统 `invalid cross-device link`）
  - 仅缓存"已结束"的分钟（minuteStart 早于当前 90s），避免缓存正在写入的片段
- **客户端错误上报端点**：新增 `POST /api/v1/system/client-errors`（`client_error_handler.go`），接收前端上报的 JS 异常 / 播放失败，写入 SystemLog（`event_type=client.error`），带频率限制（60条/分钟）
- **API 容器 ffmpeg**：`Dockerfile` 安装 `ffmpeg` + `tzdata`（Alpine 自带 libx264/aac 编码器）

#### Web
- **全局错误上报**：`main.tsx` 安装 `window.onerror` + `unhandledrejection` 监听，未捕获异常自动上报后端
- **播放失败上报**：`RecordingTimeline.tsx` 的 `<video>` onError 时上报 `recording.playback`（含 MediaError code 与 URL），便于远程定位回放问题
- 新增 `lib/errorReport.ts` 上报工具（本地 2s 限流，静默失败不影响业务）

#### 验证（NAS 192.168.31.235 实测，2026-08-13 复验）
- 首次请求 `GET /cameras/10/recordings/1786617960/file`：HTTP 200，耗时 39.6s，返回 52MB H.264 视频（ffprobe 确认 `codec_name=h264, profile=High, 2560x1440`）
- 二次请求同 recId：HTTP 200，耗时 0.075s（缓存命中，字节一致）
- `POST /system/client-errors` 上报成功，SystemLog 中可查询到 `client.error` 记录（`event_type=client.error`）

#### 版本
- Backend: v1.8.25
- Web: v1.8.25
- Android: v1.8.24（无改动）

### v1.8.22 — 审计日志大幅拓展 (2026-08-12)

#### 后端
- **摄像头管理审计事件**：新增 `camera.create` / `camera.update` 事件
  - `eventbus/events.go` 新增 `CameraManagePayload` 结构体
  - `camera_handler.go` 的 `Register` / `UpdateCodec` / `UpdateAudio` / `SetRecordingPlan` 方法发布事件
  - 日志格式："管理员 X 注册摄像头 Y" / "管理员 X 更新摄像头 Y 的 编码/音频/录制计划"
- **自动化规则审计事件**：新增 `automation.create` / `update` / `delete` 事件 + 订阅 `automation.fired`
  - `eventbus/events.go` 新增 `AutomationManagePayload` 结构体
  - `automation/handler.go` 的 `Create` / `Update` / `Delete` 方法发布事件
  - 日志格式："自动化规则 X 触发，执行 Y 动作（成功/失败）" / "管理员 X 创建/更新/删除自动化规则 Y"
- **设备管理审计事件**：新增 `device.hard_delete` / `device.token_rotate` 事件
  - `eventbus/events.go` 新增 `DeviceManagePayload` 结构体
  - `device_handler.go` 的 `HardDelete` / `RotateToken` 方法发布事件
  - 日志格式："管理员 X 永久删除设备 Y" / "管理员 X 轮换设备 Y 的访问令牌"
- **运动检测报警日志**：subscriber 订阅 `camera.motion` 事件
  - 日志格式："摄像头 X 检测到运动"（level=info）
- **修复 dead topic**：`camera.status_changed` 事件之前已被 subscriber 订阅但从未发布
  - `camera/health.go` 在状态变更时补发 `TopicCameraStatusChanged` 事件

#### 版本
- Backend: v1.8.22
- Web: v1.8.22
- Android: v1.7.26 (versionCode 120) — 核查缓存修复 + 审计日志展示

### v1.8.21 — 日志核查降级 API + PATCH 路由 (2026-08-12)

#### 后端
- **新增 `PATCH /api/v1/system/logs/:id` 路由**：将日志级别从 `critical` 降级为 `normal`
  - `system_log_handler.go` 新增 `Verify` 方法，使用 GORM `Update("level", LevelNormal)` 降级
  - 路由注册在 `systemAdmin` 组下（管理员专属）
  - 降级后日志从"待处理日志"栏消失，但保留在"所有日志"栏，审计轨迹完整
- 替代原 `DELETE /api/v1/system/logs/:id` 的核查工作流（DELETE 路由保留但不再被客户端使用）

#### 版本
- Backend: v1.8.21
- Web: v1.8.21
- Android: v1.7.25 (versionCode 119) — 日志核查降级 + 下拉刷新修复 + 更新流程优化 + 用户列表调整

### v1.8.20 — 审计日志扩展 (2026-08-12)

#### 后端
- **用户登录/登出日志重新启用**：`subscriber.go` 重新订阅 `user.login` / `user.logout` 事件，记录"用户 X 登录（设备 Y）"/"用户 X 登出（设备 Y 已撤销）"
- **用户管理审计事件**：新增 `user.create` / `user.update` / `user.delete` 事件
  - `eventbus/events.go` 新增 `UserManagePayload` 结构体（admin_id / target_id / target_name / action / is_admin）
  - `user_handler.go` 在 Create / Update / Delete 方法中发布事件，Delete 前快照用户名以保留友好标签
  - 日志格式："管理员 X 创建/更新/删除用户 Y"
- **摄像头删除审计事件**：新增 `camera.delete` 事件
  - `eventbus/events.go` 新增 `CameraDeletePayload` 结构体
  - `camera_handler.go` 在 Delete 方法中发布事件，删除前快照摄像头名称
  - 日志格式："管理员 X 删除摄像头 Y"
- **subscriber 优化**：`CameraDelete` 事件的管理员名称支持 DB 回退查询（与 `UserManage` 一致）

#### 版本
- Backend: v1.8.20
- Web: v1.8.20
- Android: v1.7.24 (versionCode 118) — 日志核查按钮修复 + 审计日志展示

### v1.8.19 — Web 开屏并行预取 (2026-08-11)

#### Web
- **开屏期间并行预取**：`AuthContext.tsx` 在 `/user/me` 探测飞行期间，并行预取 Dashboard 首屏数据（`cameras.list` / `network.status` / `weather` / `alerts`），写入 `sessionStorage`（key 与 `useCachedFetch` 一致，格式 `{ t, v }`）
- **超时兜底**：`Promise.race` 与 2000ms 超时先到者触发 `setInitialized(true)`，不阻塞入口
- **未登录路径不变**：无 token 时不触发预取，直接重定向到 `/login`
- **效果**：已登录用户进入 Dashboard 时首屏数据从 sessionStorage 秒开，无 loading spinner

#### 版本
- Backend: v1.8.19
- Web: v1.8.19
- Android: v1.7.21 (versionCode 115) — WebRTC + HLS/MP4 并行预 prepare + 开屏预取首屏数据

### v1.8.18 — 摄像头生命周期清理 + Web 动画 (2026-08-11)

#### 后端
- **摄像头删除全量清理**：`Unregister` 现在删除所有关联数据，而非仅删除 DB 行：
  - 删除 `camera_shares` 分享记录
  - 删除 Frigate 磁盘录像目录（`/media/frigate/<slug>/`）
  - 尽力删除 Frigate 检测事件（通过 Frigate API）
- **Slug 唯一性**：`uniqueSlug()` 在冲突时追加数字后缀（`-2`、`-3`...），防止同名摄像头转译后覆盖 Frigate 配置
- **compose.yaml**：`/media/frigate` 挂载由只读改为可读写，以便 API 容器删除录像目录

#### Web
- **路由切换动画**：`App.tsx` 为路由切换添加淡入/滑动过渡动画，消除白屏闪烁
- **开屏加载页**：品牌加载页在 SPA 水合前显示
- **骨架屏组件**：新增 `Skeleton.tsx` 可复用占位组件
- **Dashboard / Cameras 页面**：布局与交互优化，与液态玻璃暖色主题一致

#### 版本
- Backend: v1.8.18
- Web: v1.8.18
- Android: v1.7.19 (versionCode 113) — 网络探测快速路径先行 + 开屏动画

### v1.8.17 — Web 仪表盘液态玻璃视觉升级 (2026-08-02)

#### 新增
- **液态玻璃风格**：全局 CSS 升级为暖色调液态玻璃风格（warm cream 背景、琥珀色强调色、增强的毛玻璃效果 with blur + shadows）
- **统一动画曲线**：使用 `cubic-bezier(0.32, 0.72, 0, 1)` 缓动曲线，优化 hover / 过渡 / 微交互
- **`prefers-reduced-motion` 支持**：为偏好减少动效的用户禁用或减弱动画

#### 优化
- **UI 组件升级**：按钮、徽章、卡片、输入框全部更新为液态玻璃样式
- **布局简化**：Dashboard、Login 等页面布局优化，减少视觉杂乱
- **状态组件柔和化**：状态指示器、chip、标签等组件使用更柔和的暖色

#### 版本
- Backend: v1.8.17（无后端改动）
- Web: v1.8.17
- Android: v1.7.17 (versionCode 111)

### v1.8.16 — 安全加固 (2026-08-02)

#### 修复
- **Content-Security-Policy 头**：所有 API 响应添加 `Content-Security-Policy: default-src 'self'`，XSS 缓解
- **HttpOnly Cookie**：JWT `home_token` cookie 设置为 HttpOnly，防止 JS 读取
- **服务器超时配置**：`http.Server` 配置 `ReadTimeout=15s`、`ReadHeaderTimeout=10s`、`WriteTimeout=15s`、`IdleTimeout=60s`、`MaxHeaderBytes=1MB`
- **输入大小限制**：`weather_handler.go` 和 `frigate.go` 的请求体使用 `io.LimitReader` 限制大小
- **WebSocket CheckOrigin**：严格化 WebSocket 的 Origin 校验

#### 版本
- Backend: v1.8.16
- Web: v1.8.16（无改动）
- Android: v1.7.16（无改动）

### v1.8.15 — 管理员令牌轮换 (2026-08-01)

#### 新增
- **令牌轮换端点**：`POST /api/v1/device/:id/rotate-token`（管理员 only），递增设备的 `token_version`，立即使该设备的所有现有 JWT 失效
- **TokenVersion 字段**：`Device` 模型新增 `TokenVersion int` 字段（默认 1），JWT 中携带 `token_version`，中间件验证时若 JWT 中的版本 < DB 中的版本则拒绝并返回 `"token version mismatch"`
- **客户端无感重连**：客户端检测到 `"token version mismatch"` 后使用 access_key 静默重新绑定获取新令牌，无需用户干预

#### 版本
- Backend: v1.8.15
- Web: v1.8.15（无改动）
- Android: v1.7.15（无改动）

### v1.8.14 — 日志单条删除 (2026-08-01)

#### 新增
- **日志删除端点**：`DELETE /api/v1/system/logs/:id`（管理员 only），用于"核查并删除"工作流——管理员审核关键离线日志后，确认问题已解决即可删除

#### 版本
- Backend: v1.8.14
- Web: v1.8.14（无改动）
- Android: v1.7.14（无改动）

### v1.8.13 — 日志订阅者清理 (2026-07-31)

#### 优化
- **日志去重**：`log/subscriber.go` 移除 `TopicDeviceStatus` 订阅——它重复了 `camera.online`/`camera.offline` 的日志（"设备 #N 上线" 与 "摄像头 X 上线" 同时出现），摄像头友好名称的日志足够审计
- **移除 auth 事件日志**：`user.login`/`user.logout` 不再写入 SystemLog 表——日常认证事件会淹没有意义的设备/摄像头日志

#### 版本
- Backend: v1.8.13
- Web: v1.8.13（无改动）
- Android: v1.7.13（无改动）

### v1.8.12 — 日志保留 + IPv6 前缀修复 + 摄像头状态显示 (2026-07-31)

#### 修复
- **IPv6 直连失败（前缀旋转）**：ISP DHCPv6-PD 续约导致 /64 前缀从 `37a4:9140` 旋转到 `37a8:80c0`，但 `compose.yaml` 的 `NAS_IPV6_ADDRESS` 仍为旧值。后端 `/api/v1/network/ipv6` 报告 `PrefixRotated=true` 且 `configured_address` 过时，导致 Android `fetchDynamicIpv6Url` 误判 IPv6 不可用。已更新默认值为 `2409:8a70:37a8:80c0:62be:b4ff:fe08:bd09`（当前 NAS SLAAC EUI-64 mngtmpaddr）。注意：DDNS 记录（`nas.feiyemomo.top` AAAA）已由 DDNS 提供商自动更新到新前缀，无需手动干预。

#### 优化
- **日志保留分级**：`SystemLog` 表按 `level` 分级保留，避免普通事件淹没紧急日志。`subscriber.go` 在每次写入后调用 `pruneSystemLogs`：
  - `critical`（摄像头/设备掉线）：**无限保留**（审计追踪）
  - `normal`（用户登录/登出、设备上线）：保留最新 **500** 条
  - `info`（摄像头状态变更）：保留最新 **200** 条
  - 清理使用单条 `DELETE ... WHERE id IN (subquery)` 语句，配合 `level` 列索引，开销极低。
- **Android 摄像头当前状态显示**：服务日志 Tab 的 `camera.*` 日志项新增"当前状态：在线/离线"副标题。
  - `ServiceLogsFragment` 拉取 `/api/v1/cameras` 构建摄像头快照 `Map<cameraId, Camera>`，传给 `ServiceLogAdapter`。
  - `ServiceLogAdapter.bindCameraStatus` 解析日志 payload 中的 `camera_id`，从快照查找当前状态，绿色显示"在线"、红色显示"离线"。
  - WebSocket 收到 `camera.*` 事件时自动刷新快照，确保副标题实时反映最新状态（例如摄像头恢复后，之前的"离线"日志副标题立即变为"当前状态：在线"）。

#### 版本
- Backend: v1.8.11
- Web: v1.8.11
- Android: v1.7.12 (versionCode 104)

### v1.8.11 — App Experience Optimizations (2026-07-31)

#### 新增
- **设备管理作用域**：`GET /api/v1/device/list` 新增 `scope` 查询参数（`mine` | `all`）。默认 `mine` 只返回调用者自己的设备；管理员可传 `all` 查看全部设备。非管理员传 `all` 时也只返回自己的设备（服务端强制）。
- **设备创建端点**：`POST /api/v1/device` 为当前用户创建新设备，响应中一次性返回明文 `access_key`（后续 GET 不再包含）。
- **服务日志系统**：新增 `SystemLog` 模型 + `system_log_handler.go` + `internal/log/` 日志中间件，记录用户登录/登出、设备上下线等事件。通过 WebSocket `system.log` 主题实时推送。
- **摄像头预热端点**：`POST /api/v1/cameras/:id/preheat` 触发 go2rtc 提前连接 RTSP 源，避免首次播放的 1-10s 冷启动。
- **ICE 配置 HTTP 缓存**：`GET /api/v1/cameras/ice` 响应添加 `ETag` + `If-None-Match` 304 处理。

#### 优化
- **Web 直播预加载**：`useWebRTCStream.ts` 预创建 RTCPeerConnection + ICE 收集；`useHLSStream.ts` 启用 `lowLatencyMode`；`LiveVideo.tsx` 预览模式每 10s 刷新 JPEG 保持 RTSP 源热；`camera.ts` 模块级缓存 `getIceConfig`。
- **Android SDP 预协商**：`WebRtcClient.prepareOffer()` 在 `CameraDetailActivity.onCreate` 中预创建 PeerConnection + 完成 ICE 收集，`startStream` 直接复用预协商结果，跳过 800ms LAN / 5s remote 的 ICE 收集阶段。
- **Android 网络并行探测**：`BaseUrlResolver.probeSync()` 改用协程 `async/awaitAll` 并行探测 LAN → IPv6 → Tunnel，最坏耗时从 7.5s 降至 4s。
- **Android 直播缓冲调优**：ExoPlayer `DefaultLoadControl` minBuffer=1s, maxBuffer=3s, bufferForPlayback=500ms；`hls.js` 从 CDN 改为本地 asset 加载。
- **Android Dashboard 日志卡片**：Dashboard 新增"最近日志"卡片显示最近 5 条服务日志，支持 WebSocket 实时更新。

#### 修复
- **Android 服务日志不可见**：`SystemLog.kt` 字段名与后端 PascalCase JSON 不匹配（`ignoreUnknownKeys=true` 导致静默反序列化为空对象）。为所有字段添加 `@SerialName` 注解映射后端字段名（`ID`, `Ts`, `EventType`, `Source`, `Message`, `Payload`）。
- **心跳误判为设备上线**（v1.6.36）：两处根因。
  (1) `device/manager.go` `SetOnline()` 缺少 `wasOffline` 转换守卫——每次 WebSocket 重连（用户打开 App / 切换标签页）都会调用 `SetOnline`，即使设备从未离线也会发布 `device.status=online` 事件，导致 `system_logs` 被重复"设备上线"日志淹没。`SetOffline()` 同样缺少 `wasOnline` 守卫。两者现已对齐 `Heartbeat()` 的转换检查逻辑：只有真实的 offline→online / online→offline 转换才发布事件。
  (2) `mqtt/handler.go` `handleStatus()` 尾部无条件重发 `device.status` 事件——每次 MQTT 心跳（`status=heartbeat`）都额外写一条 `设备 #N heartbeat` 日志，且真实 online/offline 转换会被发布两次（Manager 内部一次 + 这里一次）。已移除冗余重发，由 Manager 内部的 `publishStatus` 统一负责。

#### 优化（v1.6.36 增量）
- **日志分级**：`SystemLog` 模型新增 `Level` 字段（`critical` / `normal` / `info`），后端 `subscriber.buildEntry` 按事件类型赋级——摄像头/设备掉线 = `critical`，用户上线/登出/设备上线 = `normal`，摄像头状态变更 = `info`。REST `GET /api/v1/system/logs` 支持 `level` 过滤参数。SQLite 启动时自动 backfill 历史行的 level（按 event_type + payload 推断）。Android `SystemLog` 模型同步加 `level` 字段，`ServiceLogAdapter` / `RecentLogAdapter` 按 level 着色 icon（critical=红 / normal=橙 / info=灰）。
- **ICE 配置预取提前**：`prefetchIceConfig()` 从 `DashboardFragment.onResume` 提前到 `HomeCenterApp.onCreate` 已登录分支 + `LoginActivity` 登录成功回调。App 启动即开始预热 ICE 配置（远程 1.4s 往返），用户进入摄像头详情页时配置已在内存中。

#### 版本
- Backend: v1.8.11
- Web: v1.8.11
- Android: v1.6.36 (versionCode 79)

### v1.8.10 — DDNS 域名统一识别 (2026-07-30)

#### 修复
- **Web Dashboard 通过 DDNS 域名访问时误判为中继**：`detectApiPath()` 和 `LiveVideo.isRemoteAccess()` 只能识别 IPv6 **字面量**（如 `[2001:db8::1]`），无法识别 DDNS **域名**（`nas.feiyemomo.top`）。当用户通过 `http://nas.feiyemomo.top:8088/` 访问时，系统误判为"Cloudflare Tunnel 中继"，网络质量卡片显示"可升级到 IPv6 直连"——但实际上用户已经在走 IPv6 直连了。
  - `Dashboard.tsx` `detectApiPath()` 新增 `nas.feiyemomo.top` 域名识别，返回 `"ipv6"` 路径。
  - `LiveVideo.tsx` `isRemoteAccess()` 同步识别 `nas.feiyemomo.top` 为直连路径，WebRTC 不再被错误降级为 HLS。
  - `Network.tsx` "切换到 IPv6 直连"链接从 `http://[${status.ipv6?.address}]:8088/`（硬编码 IPv6 字面量）改为 `http://nas.feiyemomo.top:8088/`（DDNS 域名），前缀轮换时无需更新代码。
- **Android `fetchDynamicIpv6Url()` 返回字面量 URL**：该函数从后端 `/api/v1/network/ipv6` 获取 IPv6 地址后构造 `http://[<addr>]:8088/`，但 `IPV6_DIRECT_URL` 已经是 DDNS 域名。改为返回 `IPV6_DIRECT_URL`（域名），仍调用后端验证 IPv6 可达性。DDNS 提供商自动跟踪前缀轮换，无需重建字面量 URL。

#### 设计决策
- `compose.yaml` 的 `NAS_IPV6_ADDRESS` 和 `deploy/frigate/config.yml` 的 `go2rtc.webrtc.candidates` **保留** IPv6 字面量，因为：
  - 后端 `CheckIPv6()` 用 `net.ParseIP` 验证环境变量值，只接受 IP 字面量。
  - WebRTC ICE candidate 的 `address` 字段按 RFC 8445 要求必须是 IP，不能用域名。
  - 这些是**基础设施层配置**，不是**应用层 URL**。应用层（Web 前端 + Android）已统一使用 DDNS 域名。

#### 版本
- Web: v1.8.10
- Android: v1.6.33（versionCode 75 → 76）

### v1.8.9 — Android 网络策略同步修正 (2026-07-30)

#### 修复
- **Android `BaseUrlResolver` IPv6 回退地址陈旧**：`IPV6_DIRECT_URL` 常量从旧前缀 `2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09` 更新为新前缀 `2409:8a70:37a4:9141:62be:b4ff:fe08:bd09`，与 `compose.yaml` 的 `NAS_IPV6_ADDRESS` 默认值保持一致。当动态获取失败（pre-login 或后端不可达）时，回退常量现在指向有效地址，避免探测失败强制降级到慢速 Cloudflare Tunnel。
- **Android Dashboard 首次网络状态缓存陈旧**：`DashboardFragment.loadNetworkStatus()` 首次调用未传 `refresh=true`，使用后端 60s 缓存数据。新增 `@Volatile private var firstNetworkFetchDone` 标志位，首次调用传 `refresh=true` 强制后端刷新，后续 `onResume` 使用缓存（60s TTL 足够新鲜）。`onDestroyView` 重置标志位，Fragment 重建时重新强制刷新。同步 Web Dashboard v1.8.7 修复。

#### 版本
- Android v1.6.30（versionCode 72 → 73）

#### 文档
- 新增 `docs/ai-context.md` Phase 14 章节
- 新增 `D:\Projects\Android\release-notes-v1.6.30.txt`

### v1.8.8 — IPv6 全链路测试与开发脚本整合 (2026-07-30)

#### 修复
- **NAS_IPV6_ADDRESS 前缀轮换**：`compose.yaml` 默认值从
  `2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09`（旧前缀）更新为
  `2409:8a70:37a4:9141:62be:b4ff:fe08:bd09`（当前前缀）。ISP 轮换了
  /64 前缀但 env var 未更新，导致"切换到 IPv6 直连"链接指向不可达地址。

#### 新增
- `test-ws.ps1`（项目根目录）— 从 `services/api/scripts/test_ws.ps1` 移动，
  WebSocket 连接测试一键脚本。
- `get-token.ps1`（项目根目录）— 使用内置测试 AccessKey 获取 JWT，
  支持 `-BaseUrl`（LAN/中继/IPv6）和 `-Copy`（复制到剪贴板）。
- `commit.ps1`（项目根目录）— 交互式 git add → commit → push 一键脚本，
  支持 `-Message`（非交互）和 `-DryRun`（预览）。

#### 验证
- IPv6 直连路径（`http://[<ipv6>]:8088/`）：Dashboard 显示"IPv6 直连" + 5 星，
  LiveVideo 默认使用 WebRTC（v1.8.7 修复确认有效）。
- 中继路径：Network 页面"切换到 IPv6 直连"链接指向正确的当前 IPv6 地址。

### v1.8.7 — 网络策略审查与前端 IPv6/UX 修复 (2026-07-30)

### 修复
- **LiveVideo IPv6 直连分类**：`isRemoteAccess()` 新增 IPv6 字面量检测（`/^\[[0-9a-f:]+\]$/i`），IPv6 直连地址不再被分类为"远程"，默认传输方式从 HLS 改为 auto（WebRTC 优先）。根因：`Dashboard.tsx` 的 `detectApiPath()` 已识别 IPv6 字面量为直连路径，但 `LiveVideo.tsx` 的 `isRemoteAccess()` 未同步，导致 IPv6 直连时 WebRTC 被阻塞。
- **Dashboard 质量评分不可达分支**：`currentQuality` IIFE 中 `clientIPv6 === false` 检查原位于 `apiPath === "remote"` 返回之后，永远不会执行。重排序使客户端无 IPv6 的降级逻辑在通用远程钳制之前评估。
- **Network 页面升级动作缺失**："中继优先，然后升级"卡片描述了升级动作但无实际触发方式。新增 `isOnRelay()` 辅助函数和 `canSwitchToIPv6Direct` 计算变量，当用户在中继路径且双方均有 IPv6 时，渲染"切换到 IPv6 直连 →"链接，点击在新标签页打开 `http://[<ipv6>]:8088/`。
- **Dashboard 初始加载网络状态陈旧**：后端缓存网络检测结果 60s，但 Dashboard 首次获取未传 `?refresh=true`，导致初始显示可能滞后 60s。新增 `forceRefreshRef = useRef(true)` 标志，首次调用传 `refresh=true` 强制后端刷新，后续 5s 轮询使用缓存避免 STUN 服务器压力。

### 新增
- `PROMPT.md`（项目根目录）— 可复用提示词，编码生产环境 SSH 凭证、部署脚本路径、Dashboard 测试账号、标准工作流，新会话无需重复收集

### 文档
- 新增 `docs/ai-context.md` Phase 12 章节
- 更新 `README.md` 更新日志

### v1.8.6 — IPv6 直连延迟显示修复 (2026-07-22)

### 修复
- Dashboard 网络质量卡片显示值从 ~500ms（含 TCP 握手）降到 ~250ms（稳态连接复用）
- 根因：v1.6.28 的 `warmupConnection()` 在 probe 之后调用，probe RTT 含握手但被直接显示；warmup 只让后续 API 调用变快但未反映到卡片
- 修复：新增 `updateRttFromApiCall()` 让真实 API 调用 RTT 写回显示值；`probeSync()` 在 probe 前先 warmup 当前 resolved URL；ConnectionPool keep-alive 从 5 分钟延长到 10 分钟（超过 5 分钟 probe TTL）
- Android v1.6.29（versionCode 72）

### v1.8.5 — IPv6 直连延迟优化 (2026-07-22)

### 优化
- **nginx upstream keepalive**（`web/nginx.conf`）：新增 `upstream api_backend` 块（`keepalive 32`），`/api/` location 切换到 `proxy_pass http://api_backend` 并设置 `proxy_set_header Connection ""`，nginx 与 Go backend 复用连接；WebSocket `/api/v1/ws` location 保持不变（仍使用 `http://api:8080` + `Connection "upgrade"`）。
- **OkHttp ConnectionPool**（Android v1.6.28，`NetworkFactory.kt`）：显式配置 `.connectionPool(ConnectionPool(5, 5, TimeUnit.MINUTES))`；保留 `protocols(listOf(HTTP_1_1))`（未启用 h2c，稳定性优先）；所有超时配置不变。
- **OkHttp warmupConnection**（Android v1.6.28，`BaseUrlResolver.kt`）：新增 `warmupConnection(url)` 方法，在 `probeSync()` 检测到 URL 变化时通过 `client.newBuilder()` 发送 `HEAD /api/v1/system/status`（3s 超时，best-effort）预建 TCP 连接，首次真实 API 调用复用该连接，省去一次 TCP 握手。
- **Android 版本号**：`versionCode` 70 → 71，`versionName` "1.6.27" → "1.6.28"。
- **docker IPv6 直连（跳过）**：诊断显示 docker-proxy 的 IPv6→IPv4 转换开销约 0ms，非瓶颈；启用原生 docker IPv6 需重配 daemon / bridge / compose network 并重新审计 ip6tables 防火墙，收益为零而风险较高，明确跳过。

### 文档
- 新增 `docs/ipv6-latency-optimization.md`：500ms 延迟根因诊断（cellular RTT × 2 = TCP 握手 + HTTP 往返）、三项优化方案（nginx keepalive / OkHttp pool + warmup / docker IPv6 跳过）、文件变更清单、预期效果。
- 更新 `docs/ai-context.md`：新增 Phase 11 (v1.8.5) 章节，更新 Last Updated 行。

### 预期效果
- 蜂窝 IPv6 直连路径（~250ms RTT）：首次 API 调用从 ~500ms 降至 ~250ms（warmup 预建 TCP）；连接池 TTL（5min）内的后续调用每次省一个 RTT（~250ms）。
- LAN 路径（~7ms）：<1ms 变化，无感知。

### v1.8.4 — IPv6 前缀轮换自动适配 (2026-07-22)

### 新增
- `GET /api/v1/network/ipv6` 端点（JWT 保护）：返回 NAS 当前出站 IPv6 地址、配置地址、前缀是否轮换、最后检查时间
- `PrefixWatcher` 后台 goroutine：每 5 分钟检测 ISP 前缀轮换，自动更新 go2rtc webrtc.candidates 并发布 EventBus 事件 `network.ipv6.prefix_rotated`
- `FrigateClient.SetWebRTCCandidates()` 方法：通过 Frigate PUT /api/config/set 推送更新后的 WebRTC candidates
- `network.OutboundIPv6Address()` + `IPv6PrefixMatches()` + `OutboundIPv6Status` 工具函数
- Android `BaseUrlResolver.fetchDynamicIpv6Url()`：从后端动态获取 NAS IPv6 地址，避免硬编码失效

### 修复
- 移动设备 IPv6 直连延迟从 ~1000ms 降至 ~50ms（根因：ISP 前缀轮换后三处硬编码地址未同步更新导致非对称路由）
- 更新硬编码 IPv6 地址从旧前缀 `2409:8a70:37a0:63f0::/64` 到新前缀 `2409:8a70:37a3:99d0::/64`
- NAS 添加稳定 SLAAC EUI-64 地址 `2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09` 并通过 systemd service 持久化

### 文档
- 新增 `docs/ipv6-prefix-rotation.md`：诊断步骤、即时修复流程、长期方案架构
- 更新 `docs/ai-context.md`：新增 Phase 10 (v1.8.4) 章节

### v1.8.3（2026-07-21）HLS 延迟提示 + Android v1.6.24 同步

- **Web 端 HLS 延迟提示徽标**：
  - 在 `LiveVideo` 直播模式 + HLS 传输路径下（用户手动选择 `hls`，或 `auto` 模式下 WebRTC 回退到 HLS 时），视频容器左上角（`absolute left-2 top-2 z-20`）显示一个"网络质量差，延迟较大"徽标。
  - 暖色液态玻璃风格：`--accent-warm` 作为图标/文字颜色、`--glass-bg` 作为半透明背景、`backdrop-blur-md` 实现毛玻璃效果。
  - 图标使用 `lucide-react` 的 `AlertTriangle`（沿用已有导入）。
  - 仅在直播模式 + HLS 路径触发，回放与其他模式不显示。
- **Android v1.6.24 — Tunnel 路径尝试 WebRTC**：
  - `CameraDetailActivity.startPlayback()` 不再在调用 `startWebRtcStream()` 前检查 `isDirectPath()`，WebRTC 现在会在所有路径（LAN / IPv6 直连 / Cloudflare Tunnel）下被尝试，只要摄像头在线且 WebRTC client 可用。
  - Tunnel 路径上 WebRTC 通常会失败（Cloudflare Tunnel 无法中继 UDP），但已有的自适应超时（5s ICE 收集、6s 连接）+ TCP candidate 启用让 STUN / P2P / IPv6 直连场景仍有机会成功。
  - 失败后走原有回退阶梯（MP4 → HLS），最终可用性不受影响。
- **Android v1.6.24 — HLS 延迟提示**：
  - 新增 `bg_hls_notice.xml` drawable + `tvHlsNotice` TextView（`activity_camera_detail.xml`），HLS 激活时显示"网络质量差，延迟较大"。
  - 暖色液态玻璃风格与 `CameraCard.kt` 调色板一致（`#F2FFFFFF` 背景、`#66FFD4B8` 桃色边框）。
  - `versionCode` 66 → 67，`versionName` "1.6.23" → "1.6.24"，构建验证通过。
- **跨端 UX 一致性**：本次变更统一了 Web 端与 Android 端的 HLS 延迟提示样式与文案，用户在任一端遇到 HLS 回退时都能得到一致的视觉提示。

### v1.8.2（2026-07-21）活动事件拉取修复 + kebab 按钮文字遮挡修复

- **活动事件返回 0 的根因修复**：
  - **axios 超时**：前端全局 axios 超时为 15 秒，但后端的 motion-ranges 端点需要将 24 小时窗口分成 24 个每小时 Frigate API 请求（每个 1-2 秒），总计 24-48 秒。15 秒超时会在后端完成前静默中断请求，导致前端缓存 null 并显示「0 事件」。为 `getMotionRanges` 单独设置 90 秒超时。
  - **无限重试循环**：原 useEffect 在请求失败时缓存 `null`，但跳过条件为 `motionCache[key] !== undefined && motionCache[key] !== null`，`null` 不被跳过，导致每次失败后立即重试——无限循环冲击后端。改为 `motionCache[key] !== undefined`（任何值都跳过，包括 null），失败后停止自动重试，用户可点击「刷新」按钮清除缓存强制重试。
  - **后端调试日志**：在 MotionRanges handler 和 ListMotionRanges 中添加临时调试日志，记录 cam_id、stream_name、slug、after/before、每块的 segment 数和 with_motion 数、最终返回的 ranges 数。便于定位 Frigate 是否返回数据、slug 是否正确、时间窗口是否匹配。
- **kebab 按钮文字被边框遮挡修复**：
  - **kebab 按钮**：从 `h-7 w-7 p-0 ring-1 variant=secondary` 改为 `size=icon variant=ghost h-8 w-8`，移除 ring 边框，使用更大的 32×32 尺寸，图标从 16px 提升到 18px。
  - **mode 标签（直播/回放）**：容器从 `h-6 text-[10px]` 提升到 `h-7 text-[11px]`，按钮从 `h-5 px-1.5 tracking-wider` 改为 `inline-flex h-6 items-center justify-center px-2`（移除 tracking-wider 避免文字溢出，添加 flex 居中确保文字垂直居中）。
  - **传输方式选择器**：容器从 `h-7` 提升到 `h-8`，按钮从 `h-6 px-1.5 tracking-wider` 改为 `inline-flex h-7 items-center justify-center px-2`。
  - **停止/录像按钮**：从 `h-6/h-7` 提升到 `h-7/h-8`，移除 `tracking-wider`。
  - **根因**：`tracking-wider` 在小按钮上导致文字宽度超出按钮可视区域；缺少 `inline-flex items-center justify-center` 导致文字未垂直居中，紧贴按钮上边框。

### v1.8.1（2026-07-21）UI 修复：可见性、z-index、汉化

- **事件带可见性修复**：原事件带使用柔和的 `--accent-warm` / `--accent-danger` CSS 变量，在奶油色背景上几乎不可见。改为饱和的 Tailwind 调色板（`bg-red-500` / `bg-amber-500`）+ 深色底带（`bg-[rgb(var(--slate-900)/0.85)]`）+ 发光阴影 + AI 事件 `animate-pulse`。高度从 14px 提升到 24px，最小宽度从 0.8% 提升到 1.2%，新增小时网格线参考。
- **LiveVideo kebab 按钮可见性**：原 `variant="outline"`（`glass-subtle`，0.35 不透明度）在浅色模式下几乎只剩一个虚框。改为 `variant="secondary"`（`glass`，0.6 不透明度），明暗两种模式下均清晰可见。
- **摄像头卡片浅色模式灰色修复**：原 Card 基类 `glass`（0.6 不透明度）在奶油色页面背景上呈现半透明灰洗外观。新增 `bg-[rgb(var(--glass-bg)/0.92)]` 提升到 0.92 不透明度，恢复卡片应有的实体感。
- **ThemeMenu 下拉框 z-index 层级修复**：原下拉框 `z-50` 但 Header 无显式 z-index，导致卡片内容（`glass-glow` 阴影 + transforms）会盖住下拉框且无法点击。建立明确层级：Header `z-40` < ThemeMenu 容器 `z-50` < 下拉框 `z-[100]`，并新增 `ring-1 ring-[rgb(var(--border)/0.4)]` 边框 + `shadow-xl`。
- **RecordingTimeline 加载逻辑优化**：学习 Android 端 `RecordingsDialog.kt` 的加载模式——motion ranges 静默加载（不显示 spinner，失败不阻塞 UI），录像列表加载显示独立的"正在读取录像列表…"状态，活动计数仅在 > 0 时显示。原本"一直转圈"的问题消除。
- **全站汉化**：所有用户可见文本中文化，包括：
  - Layout：导航、品牌、管理员徽章、角色、退出登录、主题菜单（亮色/暗色/跟随系统）
  - Login：登录卡片、表单标签、按钮、提示、错误消息
  - Cameras：标题、刷新/注册按钮、空状态、删除确认、状态徽章（在线/离线/未知）、编码选择器
  - Dashboard：StatCard 标签、网络质量、检测报警、系统快照、天气卡
  - LiveVideo：直播/回放/停止、传输方式、录像计划、PTZ 方向（上转/下转/左转/右转/停止转动）、拉近/拉远、仅观看、预览不可用、加载/错误/重试
  - RecordingTimeline：今天/昨天/前天/周X、24 小时时间轴、事件 tooltip（强度/段/个目标/时长）、空状态、速度菜单
  - Users / Devices / DeviceCreate / Network / MqttDebug / Profile：全部表单、按钮、提示、错误消息
  - 移除中文标签上的 `uppercase tracking-wider` 类（中文无大小写之分），保留 `tracking-wider`

### v1.8.0（2026-07-21）UI 精修：颜色对比、播放器合并、缓存

- **全局颜色对比度修复（明暗两种模式）**：9 个页面/组件文件中的硬编码 Tailwind 颜色（`text-slate-100/200/300/400/500`、`text-emerald-400`、`text-rose-400`、`text-amber-400`、`text-sky-300`、`bg-emerald-400`、`fill-amber-400` 等）全部替换为基于 CSS 变量的主题感知类（`text-fg`、`text-fg-muted`、`text-fg-subtle`、`text-[rgb(var(--accent-success))]`、`bg-[rgb(var(--accent-success)/0.2)]` 等）。涉及 Dashboard、Network、Users、Profile、MqttDebug、Devices、DeviceCreate、LiveVideo、RecordingTimeline。明色模式下原本"白色字在浅色背景上看不清"的问题彻底消除。
- **LiveVideo 头部精简（kebab 菜单）**：原头部在 live 模式下塞了 7+ 控件（transport 分段控件、transport 徽章、mode 标签、Stop、Rec、状态、厂商），窄屏溢出。重构后可见头部精简为：`[标题 + x264]` `[状态徽章]` `[mode 标签]` `[Stop]` `[⋮]`。Transport 选择器、录制开关、厂商信息和 last seen 移入 `⋮` 下拉菜单。
- **录像/直播播放器合并**：`RecordingTimeline` 原本在主视频区下方独立渲染一个 `aspect-video` 容器（主视频区显示"切换至下方时间轴开始播放"占位符）。现在通过 React `createPortal` 将 `<video>` + 自定义控件渲染到 `LiveVideo` 的主视频区，直播和回放共享同一物理视频面。
- **RecordingTimeline 简化（移除鱼眼，新增事件带）**：删除按 `motion_score` 取 Top 50 的鱼眼芯片滚动条，改为在 24h 时间轴上方新增显著事件带——每个 `MotionRange` 渲染为高彩色条（**红色 = 人员活动/AI**，**琥珀色 = 仅画面变动**），并附图例（带计数）。事件在一眼之间即可识别。
- **`useCachedFetch` 通用缓存 Hook**：新增 `web/src/hooks/useCachedFetch.ts`，提供 sessionStorage 缓存的 fetcher + 后台静默刷新（可选轮询）。首次加载显示 loading，之后从缓存瞬时渲染，后台静默刷新数据。Dashboard 的三个轮询组件已应用：
  - **WeatherCard**：`home.dashboard.weather`，10 分钟刷新
  - **System + Network status**：`home.dashboard.status`，5 秒刷新
  - **Alerts**：`home.dashboard.alerts`，30 秒刷新
  
  切换页面再切回 Dashboard 时，立即显示上一次的数据，而不是空白 + 转圈。

### v1.7.0（2026-07-20）Dashboard 对齐 Android 端

详细差异见 [`APP_VS_DASHBOARD_FEATURES.md`](APP_VS_DASHBOARD_FEATURES.md)。

- **天气卡片**：Dashboard 顶部新增天气卡，调用 `GET /api/v1/weather`（代理 wttr.in，5 分钟缓存），显示当前温度、体感、湿度、风速、WMO 天气代码图标。
- **LAN / Remote 路径标识**：Network Quality 卡片新增路径标识（绿点 LAN / 琥珀点 Remote），客户端通过 `window.location.hostname` 判定。
- **System 主题**：`useTheme` 新增 `"system"` 选项，跟随 OS `prefers-color-scheme`。Header 主题切换器改为三状态下拉菜单（Light / Dark / System），支持外部点击 + Escape 关闭，`applyThemeEarly()` 在 React 挂载前应用主题避免闪烁。
- **24 小时录像回放**：新增 `RecordingTimeline` 组件，替换 LiveVideo 原本的"最近录制"列表：
  - 7 天日期选择器（今天 / 昨天 / 前天 / 周X / MM-DD），匹配 Frigate 默认 7 天保留策略
  - 24 小时时间轴（1440 个分钟桶），录制区间高亮，活动区间覆盖红色（AI）或琥珀色（仅运动）
  - 点击时间轴 → 播放对应的 60 秒桶并定位到偏移
  - 活动事件鱼眼芯片（按 `motion_score` 取 Top 50），点击跳转
  - 自定义视频控件：播放/暂停、±10s 跳过、当前时间/总时长、速度下拉菜单 `[0.5, 1, 1.5, 2, 3, 5]`
  - 双击 ±10s 手势（视频左右两侧）
  - 长按 5x 倍速手势（按下时切换到 5x，松开恢复）
  - 自动续播：当前录像播放结束自动加载下一桶
  - Alert 跳转：`?time=UNIX&mode=recording` URL 参数自动选择匹配日期并播放对应桶
- **MP4 兜底中间层**：`RecordingTimeline` 使用 JWT 鉴权的 `fetch` 下载 60 秒 MP4 Blob 并通过 `URL.createObjectURL` 播放，不依赖 MSE / HEVC，在任何支持 MP4 的浏览器上都能工作。

---

## License

Private / 家庭项目，未指定开源协议。

