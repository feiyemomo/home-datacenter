# Home Datacenter 运维工具与 Agent 扩展指南

本文档汇总了为提升项目日常开发、NAS 运维以及 Agent 协同体验而配置的工具链与远控方案。

---

## 一、 NAS 免密 SSH 与 Docker 远程上下文（已持久化）

### 1. SSH 配置 (`~/.ssh/config`)
已将免密配置持久化到 Windows 用户目录：
```ssh-config
Host nas
    HostName 100.90.67.71
    User momo
    Port 22
    IdentityFile ~/.ssh/id_ed25519
    ServerAliveInterval 15
    ServerAliveCountMax 3
    StrictHostKeyChecking accept-new
```
- **日常使用**：终端或脚本中只需执行 `ssh nas`，完全无需输入密码，绝不阻塞交互。

### 2. Docker 远程上下文 (`docker context: nas`)
已在开发机创建并配置了名为 `nas` 的远程 Docker 引擎通道：
- **查看 NAS 上的容器**：
  ```powershell
  docker --context nas ps
  ```
- **查看 NAS 服务实时日志**：
  ```powershell
  docker --context nas logs -f home-api --tail 100
  docker --context nas logs -f home-frigate --tail 50
  ```
- **直接进容器执行排错**：
  ```powershell
  docker --context nas exec -it home-api sh
  ```

---

## 二、 SQLite 数据库实时探查工具 (`tools/db/db_tool.py`)

提供只读模式直接读取 NAS 生产数据库或本地开发数据库，杜绝误写风险：
- **查看 NAS 数据库表概况与数据行数**：
  ```powershell
  python D:\Projects\home-datacenter\tools\db\db_tool.py
  ```
- **查看所有表名**：
  ```powershell
  python D:\Projects\home-datacenter\tools\db\db_tool.py -t
  ```
- **在 NAS 生产库执行只读 SQL 查询**：
  ```powershell
  python D:\Projects\home-datacenter\tools\db\db_tool.py -q "SELECT id, name, is_admin FROM users"
  ```
- **查看本地数据库**：
  ```powershell
  python D:\Projects\home-datacenter\tools\db\db_tool.py --local
  ```

---

## 三、 MQTT 调试与设备事件探针 (`tools/mqtt/mqtt_probe.py`)

依托 NAS 上运行的 `home-mosquitto` 容器，支持自动读取 `.env` 鉴权密码：
- **下发测试指令 / 上报遥测**：
  ```powershell
  python D:\Projects\home-datacenter\tools\mqtt\mqtt_probe.py pub -t "home-datacenter/devices/1/telemetry" -m '{"status":"online"}'
  ```
- **监听捕获特定主题消息**：
  ```powershell
  python D:\Projects\home-datacenter\tools\mqtt\mqtt_probe.py sub -t "home-datacenter/#" -c 5 -w 10
  ```

---

## 四、 全栈快速类型与编译诊断工具 (`tools/diag/project_check.ps1`)

一键排查当前代码是否存在类型或编译破坏：
```powershell
# 1. 快速检查 Web (TypeScript) 与 Go 后端 (Vet & Build)
powershell -File D:\Projects\home-datacenter\tools\diag\project_check.ps1

# 2. 仅检查 Android 客户端编译（自动绑定 JDK 21）
powershell -File D:\Projects\home-datacenter\tools\diag\project_check.ps1 -Android

# 3. 运行特定 Android 单元测试
powershell -File D:\Projects\home-datacenter\tools\diag\project_check.ps1 -AndroidTest "com.homedatacenter.app.util.JwtUtilTest"
```

---

## 五、 DSH 远程控制与多端访问方案（方案 B - Tailscale / 局域网）

DSH 底层安全机制禁止直接监听 `0.0.0.0` 通配地址，但通过 Node.js 轻量反向代理，可无缝支持多端访问及 WebSocket 实时流式交互。

### 1. 启动远程控制代理
在需要进行远程控制时运行：
```powershell
# 前台运行（查看实时访问日志，按 Ctrl+C 退出）
powershell -File D:\Projects\home-datacenter\tools\start-dsh-proxy.ps1

# 或后台静默启动
powershell -File D:\Projects\home-datacenter\tools\start-dsh-proxy.ps1 -Background
```

### 2. 访问方式
- **Tailscale 访问**（推荐，在外网 5G 或任意连接了 Tailscale 的手机/平板）：
  `http://100.99.78.100:8888`
- **局域网 Wi-Fi 访问**：
  `http://192.168.31.51:8888`

> **首次鉴权说明**：
> DSH 启动时具备 Launch Token 安全保护。在电脑桌面的 DSH 窗口按 `F12`，在 Console 控制台输入 `location.href`，获取首次访问的 `?token=...`。
> 用移动设备访问 `http://100.99.78.100:8888/?token=你的Token`，通过首次验证后将自动写入 Session Cookie，之后便可长期无缝远控。
