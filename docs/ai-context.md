# Home Datacenter 项目上下文

## 项目定位

家庭数据中心（Home Datacenter）。

目标：

```text
统一认证
统一权限
统一设备管理
统一自动化控制
统一服务入口
```

公网访问通过：

```text
Cloudflare Tunnel
```

实现：

```text
不开放任何路由器端口
```

---

# 当前技术栈

```text
Go 1.26
Gin
SQLite
GORM
Docker
Docker Compose
Cloudflare Tunnel
JWT
```

SQLite 后续可升级 PostgreSQL。[1]

---

# 项目结构

```text
services/api/

├── cmd/
│   └── main.go
│
├── internal/
│
│   ├── config/
│   ├── database/
│   │   └── sqlite.go
│   │
│   ├── model/
│   │   ├── user.go
│   │   └── device.go
│   │
│   ├── repository/
│   │   ├── user_repository.go
│   │   └── device_repository.go
│   │
│   ├── service/
│   │   ├── bootstrap_service.go
│   │   ├── device_service.go
│   │   ├── auth_service.go
│   │   └── user_service.go
│   │
│   ├── handler/
│   │   ├── auth_handler.go
│   │   └── user_handler.go
│   │
│   ├── middleware/
│   │   └── jwt.go
│   │
│   └── utils/
│       ├── key.go
│       ├── jwt.go
│       └── response.go
│
├── scripts/
│   └── create_device.go
│
├── Dockerfile
├── go.mod
└── go.sum
```

---

# 认证架构

放弃：

```text
用户名
密码
邮箱
注册
登录
验证码
```

采用：

```text
User
↓
Device
↓
AccessKey
↓
JWT
```

模式类似：

```text
Tailscale
Home Assistant Long-lived Token
Immich API Key
```

---

# 用户模型

```go
type User struct {
    ID        uint
    Name      string
    IsAdmin   bool

    CreatedAt time.Time
    UpdatedAt time.Time
}
```

示例：

```text
自己
爸爸
妈妈
```

---

# 设备模型

```go
type Device struct {
    ID uint

    UserID uint

    DeviceName string

    AccessKeyHash string

    LastLoginAt *time.Time

    RevokedAt *time.Time

    LastIP string

    CreatedAt time.Time
    UpdatedAt time.Time
}
```

设计目标：

```text
每台设备一个 DeviceID
```

支持：

```text
吊销
拉黑
限制
```

符合最初规划。[1]

---

# 数据库

当前：

```text
SQLite
```

数据库文件：

```text
/data/sqlite/app.db
```

初始化：

```go
AutoMigrate(
    &model.User{},
    &model.Device{},
)
```

---

# 已完成功能

## Bootstrap

首次启动自动创建管理员：

```text
ID=1
Name=自己
IsAdmin=true
```

---

## AccessKey

生成：

```go
GenerateAccessKey()
```

长度：

```text
32字节随机数
64位Hex
```

---

## Hash

数据库仅保存：

```text
AccessKeyHash
```

不保存明文。

实现：

```go
HashAccessKey()
VerifyAccessKey()
```

---

## DeviceService

已实现：

```go
CreateDevice()
```

作用：

```text
生成AccessKey
保存Hash
返回明文Key
```

---

## create_device.go

用于：

```text
管理员离线创建设备
```

执行：

```bash
go run scripts/create_device.go
```

输出：

```text
UserID
DeviceName
AccessKey
```

数据库保存：

```text
AccessKeyHash
```

---

# JWT设计

当前采用：

```text
AccessToken = 365天
RefreshToken = 不使用
```

原因：

```text
家庭场景
设备数量少
管理员可控
体验优先
```

JWT Claims：

```go
type Claims struct {
    UserID uint
    DeviceID uint

    jwt.RegisteredClaims
}
```

包含：

```text
UserID
DeviceID
iat
exp
```

---

# Auth 流程

接口：

```http
POST /api/v1/auth/bind
```

请求：

```json
{
  "user_id": 1,
  "access_key": "xxxxx"
}
```

流程：

```text
查询用户
↓
Hash(access_key)
↓
查询设备
↓
验证设备
↓
签发JWT
↓
返回Token
```

返回：

```json
{
  "token":"xxxxx"
}
```

---

# JWT Middleware

已实现：

```text
Authorization Header读取
JWT解析
Claims读取
Context写入
```

Context：

```go
c.Set("user_id", claims.UserID)
c.Set("device_id", claims.DeviceID)
```

---

# 当前接口

健康检查：

```http
GET /health
```

返回：

```json
{
  "status":"ok"
}
```

---

绑定：

```http
POST /api/v1/auth/bind
```

返回：

```json
{
  "token":"..."
}
```

---

当前用户：

```http
GET /api/v1/user/me
```

Header：

```http
Authorization: Bearer xxx
```

返回：

```json
{
  "id":1,
  "name":"自己",
  "is_admin":true
}
```

---

# 已踩过的坑

## 1

导入路径错误

错误：

```text
package home-datacenter/internal/xxx is not in std
```

原因：

```text
go.mod
module名称不一致
```

正确：

```go
home-datacenter-api/internal/xxx
```

---

## 2

repository 拼写错误

错误目录：

```text
respository
```

正确：

```text
repository
```

---

## 3

SQLite驱动

最初：

```go
gorm.io/driver/sqlite
```

Docker报：

```text
CGO_ENABLED=0
go-sqlite3 requires cgo
```

解决：

```go
github.com/glebarez/sqlite
```

无需CGO。

---

## 4

PowerShell curl JSON

错误：

```text
invalid character '\'
invalid character 'u'
```

原因：

```text
PowerShell转义问题
```

正确测试方式：

```powershell
$body = @{
    user_id = 1
    access_key = "xxx"
} | ConvertTo-Json

Invoke-RestMethod ...
```

---

## 5

JWT测试

错误：

```text
invalid token
```

原因：

```text
使用了JWT官网示例Token
```

必须使用：

```text
/auth/bind
返回的真实Token
```

---

# 当前待解决问题

Step13 设备吊销功能开发中。

已经增加：

```go
RevokedAt *time.Time
```

以及：

```go
DeviceRepository.Revoke()
DeviceRepository.IsRevoked()
```

但 JWT Middleware 接入后出现过：

```text
device not found
```

以及：

```text
Scan error:
revoked_at
string -> *time.Time
```

需要继续排查：

```text
glebarez/sqlite
+
GORM
+
nullable datetime
```

兼容问题。

---

# 下一阶段计划

## Step13

完成：

```text
设备吊销
JWT中间件校验设备状态
```

目标：

```text
管理员解绑设备
↓
立即失效
```

---

## Step14

设备管理接口：

```http
GET /api/v1/device/list
DELETE /api/v1/device/:id
```

---

## Step15

统一响应结构：

```json
{
  "code":0,
  "message":"success",
  "data":{}
}
```

---

## Step16

配置文件化：

```yaml
jwt:
  secret:
database:
  path:
```

替换硬编码。

---

# 当前状态评估

```text
第一阶段：
100% 完成

第二阶段：
约 90% 完成
```

已具备：

```text
? 用户体系
? 设备体系
? AccessKey
? JWT认证
? 长期登录
? 设备独立身份
? Cloudflare公网访问
? Docker部署
? SQLite持久化
? /user/me
? /auth/bind
```

当前最优先事项：

```text
完成设备吊销（RevokedAt）
然后开发设备管理API
```
