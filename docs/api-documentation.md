# Home Datacenter API Documentation

## Project Overview

**Home Datacenter** is a self-hosted authentication and device management system for personal/home use.

**Core Goals:**

- Unified authentication across all home services
- Unified permission management
- Unified device management
- Unified automation control
- Unified service entry point

**Public Access:**

- Exposed via **Cloudflare Tunnel**
- **No router ports opened** — zero inbound firewall rules

---

## Tech Stack

| Component | Technology |
|-----------|------------|
| Language | Go 1.26 |
| Web Framework | Gin |
| Database | SQLite (GORM) |
| Auth | JWT (long-lived, 365 days) |
| Container | Docker + Docker Compose |
| Driver | `github.com/glebarez/sqlite` (pure-Go, no CGO) |
| Config | YAML via `github.com/spf13/viper` |

SQLite can be upgraded to PostgreSQL in the future.

---

## Architecture

### Authentication Flow (No Passwords)

Traditional username/password/email/registration/login/captcha flow is **rejected**.

Instead, the system uses an **AccessKey-based flow** similar to:

- Tailscale device auth
- Home Assistant long-lived tokens
- Immich API keys

**Flow:**

```
User (pre-created by admin)
    ↓
Device (created offline by admin)
    ↓
AccessKey (32-byte random, 64-char hex)
    ↓
JWT (365-day long-lived token)
```

**Key Properties:**

- Database stores **hash only**, never plaintext AccessKey
- Each device has its own identity and can be independently revoked
- Admin creates devices offline (no registration endpoint)
- Users "bind" a device by presenting its AccessKey to obtain a JWT

---

## Data Models

### User

```go
type User struct {
    ID        uint      `gorm:"primaryKey"`
    Name      string    `gorm:"uniqueIndex;not null"`
    IsAdmin   bool      `gorm:"default:false"`
    CreatedAt time.Time
    UpdatedAt time.Time
}
```

**Example Users:**

- `自己` (Self) — admin, ID=1, created on bootstrap
- `爸爸` (Father)
- `妈妈` (Mother)

### Device

```go
type Device struct {
    ID           uint           `gorm:"primaryKey"`
    UserID       uint           `gorm:"index;not null"`
    DeviceName   string         `gorm:"not null"`
    AccessKeyHash string        `gorm:"not null"`
    LastLoginAt  utils.NullTime // wrapper for nullable datetime
    RevokedAt    utils.NullTime // revoked_at != NULL → device is revoked
    LastIP       string
    CreatedAt    time.Time
    UpdatedAt    time.Time
}
```

**Design Goals:**

- One `Device` row per physical device (phone, laptop, tablet, etc.)
- Supports revocation, blocking, and per-device audit
- Revoked devices' JWTs are rejected immediately by middleware

### NullTime (Custom Type)

```go
type NullTime struct {
    Time  time.Time
    Valid bool // true → non-NULL, false → NULL
}
```

**Why Needed:**

- SQLite stores `*time.Time` as TEXT columns
- `glebarez/sqlite` (modernc driver, pure-Go) returns TEXT datetime values as **strings**, not `time.Time`
- Standard `*time.Time` cannot `Scan` from string → error:
  ```
  Scan error: revoked_at string -> *time.Time
  ```
- `NullTime` implements `sql.Scanner` / `driver.Valuer` to handle this correctly

---

## API Endpoints

### Response Envelope (Step15)

All `/api/v1/*` endpoints use a unified envelope:

**Success:**

```json
{
  "code": 0,
  "message": "success",
  "data": { ... }
}
```

**Error:**

```json
{
  "code": <http_status>,
  "message": "<error_description>",
  "data": null
}
```

`code` mirrors HTTP status (401, 403, 404, 500, etc.). Client can check either.

**Exception: `/health`** — kept as `{"status":"ok"}` for Docker / Cloudflare Tunnel probes.

---

### Health Check

**Endpoint:**

```
GET /health
```

**Response:**

```json
{
  "status": "ok"
}
```

**Notes:**

- No authentication required
- Used by Docker HEALTHCHECK and Cloudflare Tunnel origin checks

---

### Bind Device → Obtain JWT

**Endpoint:**

```
POST /api/v1/auth/bind
```

**Headers:**

```
Content-Type: application/json
```

**Request Body:**

```json
{
  "user_id": 1,
  "access_key": "e6b9b928fc277d062943a46942c07d85b6a99ef4c4d5bc74d737c9cfd1ff304a"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `user_id` | uint | Yes | Target user to bind device to |
| `access_key` | string | Yes | 64-char hex AccessKey created by admin |

**Process:**

1. Load user by `user_id`
2. Compute `Hash(access_key)` (SHA-256)
3. Find device by `(user_id, access_key_hash)`
4. Reject if `RevokedAt.Valid == true` (device revoked)
5. Update `LastLoginAt` to now
6. Sign JWT with `(user_id, device_id)` claims
7. Return token

**Success Response:**

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
  }
}
```

**Error Responses:**

| Status | `message` | Scenario |
|--------|-----------|----------|
| 400 | `invalid access_key` | Missing/invalid JSON body |
| 401 | `invalid access key` | No device matches (user_id, hash) |
| 401 | `device revoked` | Device has been revoked |

**JWT Claims:**

```json
{
  "user_id": 1,
  "device_id": 3,
  "iat": 1782618533,
  "exp": 1814154533
}
```

`exp` is 365 days from `iat`.

---

### Get Current User

**Endpoint:**

```
GET /api/v1/user/me
```

**Headers:**

```
Authorization: Bearer <jwt_token>
```

**Success Response:**

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "id": 1,
    "name": "自己",
    "is_admin": true
  }
}
```

**Error Responses:**

| Status | `message` | Scenario |
|--------|-----------|----------|
| 401 | `missing authorization header` | No `Authorization` header |
| 401 | `invalid authorization format` | Header not `Bearer <token>` |
| 401 | `invalid token` | JWT malformed/expired/invalid signature |
| 401 | `device not found` | Device row deleted from DB |
| 401 | `device revoked` | `RevokedAt` is non-NULL |
| 401 | `device lookup failed` | Internal DB error |
| 404 | `user not found` | User row deleted from DB |

---

### List Devices

**Endpoint:**

```
GET /api/v1/device/list
```

**Headers:**

```
Authorization: Bearer <jwt_token>
```

**Authorization:**

- **Admin (`is_admin=true`)** → sees all devices
- **Non-admin** → sees only devices where `device.user_id == current_user.id`

**Success Response:**

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "devices": [
      {
        "id": 1,
        "user_id": 1,
        "device_name": "MacBook-Pro",
        "last_login_at": "2026-06-28T15:30:00Z",
        "revoked_at": null,
        "last_ip": "",
        "created_at": "2026-06-28 15:00:00",
        "updated_at": "2026-06-28 15:30:00"
      },
      {
        "id": 2,
        "user_id": 1,
        "device_name": "iPhone-15",
        "last_login_at": null,
        "revoked_at": "2026-06-27T10:00:00Z",
        "last_ip": "",
        "created_at": "2026-06-20 10:00:00",
        "updated_at": "2026-06-27 10:00:00"
      }
    ]
  }
}
```

**Notes:**

- `AccessKeyHash` is **never** exposed via API
- `last_login_at` / `revoked_at` are `null` when NULL in DB (via `NullTime.MarshalJSON`)
- Revoked devices are included for audit purposes

**Error Responses:**

Same as `/user/me` auth errors (401).

---

### Revoke Device

**Endpoint:**

```
DELETE /api/v1/device/:id
```

**Headers:**

```
Authorization: Bearer <jwt_token>
```

**Authorization:**

- **Admin** → may revoke any device
- **Non-admin** → may only revoke devices where `device.user_id == current_user.id`

**Behavior:**

- Sets `revoked_at` to current timestamp (soft delete)
- Device row is retained for audit
- JWT middleware immediately rejects tokens for revoked devices on next request
- **Idempotent** — revoking an already-revoked device still returns success

**Success Response:**

```json
{
  "code": 0,
  "message": "success",
  "data": null
}
```

**Error Responses:**

| Status | `message` | Scenario |
|--------|-----------|----------|
| 400 | `invalid device id` | `:id` not a valid uint |
| 401 | auth errors | Same as `/user/me` |
| 403 | `forbidden` | Non-admin trying to revoke another user's device |
| 404 | `device not found` | Device row does not exist |
| 404 | `user not found` | Current user row missing (edge case) |
| 500 | `failed to revoke device` | DB update error |

---

## JWT Middleware Behavior

**Flow:**

1. Read `Authorization: Bearer <token>` header
2. Parse JWT, verify signature and expiration
3. Extract `user_id` and `device_id` from claims
4. Load device row from DB by `device_id`
5. Check `device.RevokedAt.Valid`:
   - `true` → reject with 401 `device revoked`
   - `false` → proceed
6. Set `user_id` and `device_id` into Gin context for downstream handlers

**Revocation is Immediate:**

Once an admin calls `DELETE /api/v1/device/:id`, the next request with that device's JWT receives 401. No need to wait for token expiration.

---

## Configuration

**File:** `configs/config.yaml`

```yaml
server:
  port: 8080

database:
  path: /data/sqlite/app.db

jwt:
  secret: your-secret-key
  expire_days: 365
```

**Defaults:**

- If a field is missing, defaults are:
  - `server.port`: 8080
  - `database.path`: `/data/sqlite/app.db`
  - `jwt.secret`: `""` (caller should validate)
  - `jwt.expire_days`: 365

**Override Path:**

Environment variable `APP_CONFIG` can override the config file path:

```bash
APP_CONFIG=/etc/home-api/prod.yaml ./server
```

**Docker:**

- Dockerfile copies `configs/` into `/configs/`
- `compose.yaml` mounts `./services/api/configs:/configs` so local edits apply without rebuild

**Security Warning:**

`jwt.secret` must be changed to a long random string before production. Changing the secret invalidates all existing JWTs.

---

## Offline Device Creation

**Tool:** `scripts/create_device.go`

**Purpose:** Admin creates devices before distributing AccessKeys to users (no registration API).

**Run (local dev):**

```bash
cd services/api
go run scripts/create_device.go
```

**Output:**

```
===================================
Device Created Successfully
===================================
User ID     : 1
Device Name : MacBook-Pro

Access Key:
e6b9b928fc277d062943a46942c07d85b6a99ef4c4d5bc74d737c9cfd1ff304a

请立即保存该密钥，数据库不会保存明文。
===================================
```

**DB Saved:**

- `access_key_hash` (SHA-256 of the key)
- **Plaintext key is never stored** — admin must record it immediately

---

## Bootstrap

On first startup, if no user named `自己` exists:

- Auto-create admin user:
  - `id=1`
  - `name=自己`
  - `is_admin=true`

Admin then runs `create_device.go` to generate an AccessKey for their first device.

---

## Known Pitfalls (Lessons Learned)

### 1. Import Path Mismatch

**Error:**

```
package home-datacenter/internal/xxx is not in std
```

**Cause:** `go.mod` module name mismatch.

**Fix:** Import path must match module name:

```go
import "home-datacenter-api/internal/xxx"
```

---

### 2. Repository Typo

**Wrong directory:** `respository`

**Correct:** `repository`

---

### 3. SQLite Driver CGO

**Original:** `gorm.io/driver/sqlite` (requires CGO)

**Docker error:**

```
CGO_ENABLED=0
go-sqlite3 requires cgo
```

**Fix:** Use pure-Go driver:

```go
import "github.com/glebarez/sqlite"
```

No CGO required, works in Alpine containers.

---

### 4. PowerShell JSON Escaping

**Error:**

```
invalid character '\'
invalid character 'u'
```

**Cause:** PowerShell escaping when constructing JSON inline.

**Fix:**

```powershell
$body = @{
    user_id = 1
    access_key = "xxx"
} | ConvertTo-Json

Invoke-RestMethod -Uri http://localhost:8080/api/v1/auth/bind `
  -Method POST -Body $body -ContentType "application/json"
```

---

### 5. JWT Test Token

**Error:**

```
invalid token
```

**Cause:** Using example JWT from jwt.io.

**Fix:** Always use a real token from `/auth/bind`.

---

### 6. NullTime Scan Error

**Error:**

```
Scan error: revoked_at string -> *time.Time
```

**Cause:** modernc.org/sqlite (pure-Go) returns TEXT datetime columns as strings; `*time.Time` cannot scan from string.

**Fix:** Custom `utils.NullTime` type implementing `sql.Scanner` / `driver.Valuer`.

---

## Deployment

### Docker Compose

**File:** `compose.yaml`

```yaml
services:
  api:
    build:
      context: ./services/api
    container_name: home-api
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - ./data/sqlite:/data/sqlite
      - ./services/api/configs:/configs
```

**Run:**

```bash
docker compose up -d
```

**Logs:**

```bash
docker compose logs -f api
```

### Cloudflare Tunnel

- Tunnel connects to `http://home-api:8080` (internal Docker network)
- Public URL managed by Cloudflare
- No router ports opened

---

## Project Status

**Phase 1 (Bootstrap + Auth + Device):** 100%

**Phase 2 (Revocation + Management + Unified Response + Config):** 100%

**Completed Features:**

| Feature | Status |
|---------|--------|
| User system (bootstrap admin) | ✅ |
| Device system (per-device identity) | ✅ |
| AccessKey generation & hash-only storage | ✅ |
| JWT authentication (365-day) | ✅ |
| Device revocation (immediate JWT rejection) | ✅ |
| Device management API (list, revoke) | ✅ |
| Unified response envelope | ✅ |
| YAML configuration | ✅ |
| Docker deployment | ✅ |
| SQLite persistence | ✅ |
| Health check endpoint | ✅ |

---

## Quick Start (Local Dev)

```powershell
# 1. Start API
cd D:\Projects\home-datacenter\services\api
go run cmd/main.go

# 2. Create admin device (in another terminal)
go run scripts/create_device.go
# → copy AccessKey

# 3. Bind → get token
$accessKey = "<paste-key>"
$body = @{ user_id = 1; access_key = $accessKey } | ConvertTo-Json
$resp = Invoke-RestMethod -Uri http://localhost:8080/api/v1/auth/bind `
  -Method POST -Body $body -ContentType "application/json"
$token = $resp.data.token

# 4. List devices
$h = @{ Authorization = "Bearer $token" }
Invoke-RestMethod -Uri http://localhost:8080/api/v1/device/list -Headers $h `
  | ConvertTo-Json -Depth 5

# 5. Revoke device
Invoke-RestMethod -Uri http://localhost:8080/api/v1/device/1 `
  -Method DELETE -Headers $h

# 6. Verify revoked
Invoke-RestMethod -Uri http://localhost:8080/api/v1/user/me -Headers $h
# → 401 "device revoked"
```

---

## Future Roadmap

| Item | Notes |
|------|-------|
| PostgreSQL migration | Optional, SQLite sufficient for home use |
| User management API | Create/delete users, assign admin |
| Unit tests | Handler/service/repository layers |
| Rate limiting | Protect `/auth/bind` from brute force |
| Audit log | Record who revoked which device when |
| Web UI | Dashboard for device management |

---

## API Summary Table

| Endpoint | Method | Auth | Description |
|----------|--------|------|-------------|
| `/health` | GET | No | Health check |
| `/api/v1/auth/bind` | POST | No | Bind device, obtain JWT |
| `/api/v1/user/me` | GET | JWT | Get current user profile |
| `/api/v1/device/list` | GET | JWT | List visible devices |
| `/api/v1/device/:id` | DELETE | JWT | Revoke a device |

---

**Document Version:** 2026-06-28 (post Step16)