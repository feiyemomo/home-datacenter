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

All `/api/v1/*` endpoints use a unified envelope, and every response
carries the following security headers (applied by `utils.applySecurityHeaders`):

- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY`
- `Referrer-Policy: no-referrer`
- `Cache-Control: no-store`

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
| 400 | `invalid request body` | Missing/invalid JSON body |
| 401 | `invalid credentials` | No device matches (user_id, hash), user not found, or device revoked |

> **Security note:** all bind failures return the same generic
> `invalid credentials` to prevent user/key enumeration. The detailed
> causes (invalid access key / device revoked / user lookup) are still
> distinguished internally but not exposed to the client.

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

### System Status (Dashboard)

**Endpoint:**

```
GET /api/v1/system/status
```

**Headers:**

```
Authorization: Bearer <jwt_token>
```

**Purpose:** Live metrics for the web dashboard (polled every 5s).

**Success Response:**

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "mqtt_connected": true,
    "ws_clients": 2,
    "online_device_count": 1,
    "online_device_ids": [1],
    "uptime_seconds": 3600,
    "server_time": "2026-07-04 16:00:00"
  }
}
```

**Notes on `online_device_count` / `online_device_ids`:**

- The list reflects `device.Manager`'s in-memory state, fed by MQTT
  status messages, telemetry, and heartbeats. A device is considered
  online until it has been silent for `heartbeatTimeout` (90s) or the
  API has lost its broker connection.
- When the API's MQTT client disconnects from the broker
  (`OnConnectionLost`), `device.Manager.MarkAllOffline` flips every
  online device to offline and emits a `device.status` event for each.
  The dashboard reflects the loss immediately rather than waiting for
  the 90s sweeper.
- The Devices page in the dashboard updates the online set
  optimistically from incoming `device.status` WebSocket events
  (add on `online`/`heartbeat`, delete on `offline`) and reconciles
  with this endpoint in the background.
- `online_device_ids` is `null` rather than `[]` when no devices are
  online (legacy `[]` is also tolerated by the frontend).

**Error Responses:** same 401 auth errors as `/user/me`.

---

### MQTT Publish (Admin)

**Endpoint:**

```
POST /api/v1/mqtt/publish
```

**Headers:**

```
Authorization: Bearer <jwt_token>
```

**Authorization:** admin only (a non-admin JWT receives 403 from the
route guard is *not* applied here — the endpoint is JWT-only; admin
enforcement is the caller's responsibility via the dashboard route.
See `web/src/App.tsx` `adminOnly` on `/mqtt`).

**Request Body:**

```json
{
  "topic": "home-datacenter/devices/1/command",
  "payload": "{\"cmd\":\"reboot\"}",
  "qos": 1
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `topic` | string | Yes | Must be within the `home-datacenter/` namespace |
| `payload` | string | Yes | Raw payload (JSON string for the dashboard) |
| `qos` | byte | No | 0/1/2; defaults to 1 |

**Topic allowlist:** the server rejects any topic that does not start
with `home-datacenter/` or that starts with `$` (broker control topics
like `$SYS/...`). This prevents a compromised admin token from writing
retained messages to arbitrary broker topics.

**Success Response:**

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "topic": "home-datacenter/devices/1/command",
    "payload": "{\"cmd\":\"reboot\"}",
    "qos": 1
  }
}
```

**Error Responses:**

| Status | `message` | Scenario |
|--------|-----------|----------|
| 400 | `invalid request body` | Missing/invalid JSON body |
| 400 | `topic must be within the home-datacenter/ namespace` | Topic outside allowlist |
| 503 | `mqtt not connected` | Broker unreachable |

---

### WebSocket

**Endpoint:**

```
GET /api/v1/ws
```

**Auth (one of):**

- `Authorization: Bearer <jwt_token>` header (preferred — keeps token out of URL/logs)
- `?token=<jwt_token>` query parameter (browser fallback; exposes token in URL/referer)

**Origin policy:** when `server.allowed_origins` is configured, only
requests whose `Origin` host is in the allowlist are upgraded. This
blocks cross-site WebSocket hijacking (CSWSH). Empty list (local dev)
accepts any origin.

**Lifecycle:** after upgrade, the connection is kept alive by
ping/pong (30s) and an application-level heartbeat. The JWT's
`(user_id, device_id)` identify the connection; admins receive all
device events, non-admins receive only events whose topic matches one
of their subscriptions.

See `docs/ai-context.md` → Phase 3 and `deploy/android/HomeDatacenterClient.kt`
for the wire format (`{type, topic, payload, ts}`).

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
  allowed_origins: []   # WebSocket origin allowlist (empty = dev)

database:
  path: /data/sqlite/app.db

jwt:
  secret: your-secret-key
  expire_days: 365

mqtt:
  broker: tcp://mosquitto:1883
  client_id: home-datacenter
  username: ""
  password: ""
  qos: 1

websocket:
  path: /api/v1/ws
  heartbeat_seconds: 30
```

**Defaults:**

- If a field is missing, defaults are:
  - `server.port`: 8080
  - `server.allowed_origins`: `[]` (allow all — dev only)
  - `database.path`: `/data/sqlite/app.db`
  - `jwt.secret`: `""` (rejected at startup — see below)
  - `jwt.expire_days`: 365
  - `mqtt.*` / `websocket.*`: see `internal/config/config.go`

**Secret resolution (priority order):**

1. `JWT_SECRET` env var (preferred for Docker / `.env`)
2. `configs/config.local.yaml` `jwt.secret` (gitignored local dev)
3. `configs/config.yaml` `jwt.secret` (placeholder)

The API **refuses to start** with an empty, placeholder
(`your-secret-key`, `change-me`, `PLEASE_CHANGE_TO_A_LONG_RANDOM_SECRET`),
or <32-char secret. Generate a real one:

```bash
openssl rand -hex 32
```

**Override Path:**

Environment variable `APP_CONFIG` can override the config file path:

```bash
APP_CONFIG=/etc/home-api/prod.yaml ./server
```

**Docker:**

- Dockerfile copies `configs/` into `/configs/`
- `compose.yaml` mounts `./services/api/configs:/configs:ro` so local edits apply without rebuild
- Secrets (JWT, MQTT password) are injected via `environment:` from `.env` (gitignored)

**Security Warning:**

`jwt.secret` must be a long (≥32 char) random string. The app refuses
to boot with the placeholder. Changing the secret invalidates all
existing JWTs. Never commit a real secret — use `config.local.yaml`
or the `JWT_SECRET` env var.

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

### 7. Devices Stuck Offline Despite Status Messages

**Symptom:** `GET /api/v1/system/status` returns
`online_device_count: 0` and `online_device_ids: null` even though
`mosquitto_sub` shows the device publishing
`home-datacenter/devices/5/status` payloads, and `docker logs home-api`
shows the message being received.

**Cause:** Two common root causes.

1. The publisher sends *unquoted-key pseudo-JSON*, e.g.
   `{status:online,ts:1234567890}`. `encoding/json` rejects it with
   `invalid character 's' looking for beginning of object key string`,
   so `device.Manager.SetOnline` is never called.
2. The publisher sends valid JSON but the API is unable to see the
   message (broker ACL, wrong topic, no default publish handler
   registered on the paho client).

**Fix:**

- `mqtt.Handler.handleStatus` is now tolerant: it first tries strict
  JSON, then a lenient re-quote pass (`lenientJSON`), and finally a
  bareword regex fallback. The canonical parsed status is re-emitted
  on the EventBus so downstream consumers always get strict JSON.
- `mqtt.Client` registers a default publish handler at construction
  so messages are never silently dropped between the broker and the
  handler.
- See `services/api/internal/mqtt/handler_test.go` for the payload
  shapes that must keep working.

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

# 7. Simulate a Device Going Online (Mosquitto)

Useful for smoke-testing the dashboard without a real device:

```bash
# Standard JSON — preferred
MSYS_NO_PATHCONV=1 docker exec home-mosquitto \
  mosquitto_pub -u home-datacenter -P "$MQTT_PASSWORD" \
    -t 'home-datacenter/devices/1/status' \
    -m '{"status":"online","ts":1234567890}'

# Loosely formatted JSON (key/value unquoted) — also accepted
MSYS_NO_PATHCONV=1 docker exec home-mosquitto \
  mosquitto_pub -u home-datacenter -P "$MQTT_PASSWORD" \
    -t 'home-datacenter/devices/1/status' \
    -m '{status:online,ts:1234567890}'
```

Then `GET /api/v1/system/status` should return
`online_device_count: 1` and `online_device_ids: [1]` within 5s.
Publish `{"status":"offline",...}` to flip it back.

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
| `/api/v1/system/status` | GET | JWT | Dashboard metrics |
| `/api/v1/mqtt/publish` | POST | JWT | Publish within `home-datacenter/` (dashboard admin) |
| `/api/v1/ws` | GET (upgrade) | JWT | WebSocket real-time channel |

---

**Document Version:** 2026-07-04 (security hardening + Phase 3 endpoints + dashboard)