# Home Datacenter Project Context

> For AI assistants taking over this project. Read this first, then see `docs/api-documentation.md` for full API details.

---

## Project Identity

**Name:** Home Datacenter

**Purpose:** Self-hosted authentication and device management for a personal/home network.

**Core Goals:**

- Unified authentication (no passwords, AccessKey-based)
- Unified permission (admin vs non-admin)
- Unified device management (per-device identity, revocation)
- Unified automation control (future)
- Unified service entry point

**Deployment Model:**

- Exposed via **Cloudflare Tunnel**
- **No router ports opened**
- Runs in Docker Compose on a home server

- Exposed via **Cloudflare Tunnel**
- **No router ports opened**
- Runs in Docker Compose on a home server

---

## Current Tech Stack

| Layer | Choice |
|-------|--------|
| Language | Go 1.26 |
| Web | Gin |
| ORM | GORM |
| DB | SQLite (via `glebarez/sqlite`, pure-Go, no CGO) |
| Auth | JWT (365-day long-lived) |
| Config | YAML + viper |
| Container | Docker + Compose |
| Real-time | MQTT (Mosquitto) + WebSocket (gorilla/websocket) |
| Frontend | React + Vite + Tailwind (dashboard SPA) |

---

## Architecture Summary

**Auth Flow (No Traditional Login):**

```
Admin (bootstrap) → User (pre-created)
                    ↓
Admin (offline) → Device (AccessKey created)
                    ↓
User + AccessKey → POST /auth/bind → JWT
```

**Key Properties:**

- Database stores **hash of AccessKey**, never plaintext
- Each device has independent identity, can be revoked
- JWT middleware checks device revocation status per request
- No registration API — admin creates devices offline

---

## Data Models

**User:**

```go
ID uint
Name string (unique)
IsAdmin bool
CreatedAt, UpdatedAt
```

**Device:**

```go
ID uint
UserID uint
DeviceName string
AccessKeyHash string (SHA-256)
LastLoginAt NullTime
RevokedAt NullTime // non-NULL → revoked
LastIP string
CreatedAt, UpdatedAt
```

**NullTime:**

Custom type wrapping nullable `time.Time`. Handles pure-Go SQLite driver returning TEXT datetime as strings. Implements `sql.Scanner` / `driver.Valuer`.

---

## API Endpoints (Summary)

| Endpoint | Auth | Purpose |
|----------|------|---------|
| `GET /health` | None | Docker/Cloudflare health probe |
| `POST /api/v1/auth/bind` | None | Exchange AccessKey for JWT |
| `GET /api/v1/user/me` | JWT | Current user profile |
| `GET /api/v1/device/list` | JWT | List devices (admin=all, non-admin=own) |
| `DELETE /api/v1/device/:id` | JWT | Revoke device (soft delete) |
| `GET /api/v1/system/status` | JWT | Dashboard metrics (MQTT/WS/online devices) |
| `POST /api/v1/mqtt/publish` | JWT+admin | Publish to a `home-datacenter/` topic |
| `GET /api/v1/ws` | JWT | WebSocket upgrade (header or `?token=`) |

**Response Envelope:**

```json
{
  "code": 0,
  "message": "success",
  "data": { ... }
}
```

`code` mirrors HTTP status. `/health` uses `{"status":"ok"}` (exception).

---

**Key Files**

```
services/api/
├── cmd/main.go                  // Entry point, wiring, routes
├── internal/
│   ├── config/config.go         // YAML loader (viper) + secret validation
│   ├── database/sqlite.go       // DB init
│   ├── device/manager.go        // Online/offline + heartbeat tracking
│   ├── eventbus/                // In-memory pub/sub (MQTT ↔ WS bridge)
│   ├── model/
│   │   ├── user.go
│   │   └── device.go
│   ├── repository/
│   │   ├── user_repository.go
│   │   └── device_repository.go
│   ├── service/
│   │   ├── bootstrap_service.go // Auto-create admin on first run
│   │   ├── auth_service.go      // Bind logic
│   │   ├── device_service.go
│   │   ├── user_service.go
│   ├── handler/
│   │   ├── auth_handler.go
│   │   ├── user_handler.go
│   │   ├── device_handler.go
│   │   ├── system_handler.go    // /system/status + /mqtt/publish
│   │   ├── ws_handler.go        // WebSocket upgrade + origin check
│   ├── middleware/jwt.go        // JWT auth + revocation check
│   ├── mqtt/                    // Paho client, topic schema, handler
│   ├── utils/
│   │   ├── key.go               // AccessKey generation + hash
│   │   ├── jwt.go               // JWT signing/parsing
│   │   ├── nulltime.go          // Nullable time wrapper
│   │   ├── response.go          // Unified response + security headers
│   ├── router/router.go         // (placeholder; routes in main.go)
├── scripts/create_device.go     // Offline device creation tool
├── configs/config.yaml          // Server/DB/JWT/MQTT/WS config (placeholders)
├── configs/config.local.yaml    // gitignored local override (real secret)
├── Dockerfile
└── (compose.yaml at project root)

web/                             // React + Vite + Tailwind dashboard SPA
├── src/
│   ├── pages/{Dashboard,Devices,Login,MqttDebug,Profile}.tsx
│   ├── api/{auth,client,device,system}.ts
│   ├── context/AuthContext.tsx  // /user/me probe, isAdmin
│   ├── hooks/useWebSocket.ts    // WS + auto-reconnect + heartbeat
│   └── components/              // Layout, ProtectedRoute, ui/*
├── nginx.conf                   // SPA + /api proxy + /api/v1/ws upgrade
└── Dockerfile

deploy/
├── mosquitto/{mosquitto.conf,aclfile,passwd}  // broker + ACL + creds
├── cloudflared/config.yml        // dashboard + api hostnames
└── android/HomeDatacenterClient.kt
```

---

## Configuration

**File:** `configs/config.yaml` (committed, placeholders only)

```yaml
server:
  port: 8080
  allowed_origins: []   # WebSocket origin allowlist; empty = allow all (dev)
database:
  path: /data/sqlite/app.db
jwt:
  secret: <change-me>   # placeholder — app refuses to boot with this
  expire_days: 365
mqtt:
  broker: tcp://mosquitto:1883
  client_id: home-datacenter
  username: ""          # set via MQTT_USERNAME env in prod
  password: ""          # set via MQTT_PASSWORD env in prod
  qos: 1
websocket:
  path: /api/v1/ws
  heartbeat_seconds: 30
```

**Secret resolution (in priority order):**

1. `JWT_SECRET` env var (preferred for Docker / `.env`)
2. `configs/config.local.yaml` `jwt.secret` (local dev, gitignored)
3. `configs/config.yaml` `jwt.secret` (placeholder only)

The app **refuses to start** if the secret is empty, a known placeholder
(`your-secret-key`, `change-me`, `PLEASE_CHANGE_TO_A_LONG_RANDOM_SECRET`),
or shorter than 32 chars. Generate with `openssl rand -hex 32`.

**Docker:**

- Config baked into image at `/configs/`
- Compose mounts `./services/api/configs:/configs:ro` for live edits
- Secrets injected via `environment:` in `compose.yaml` (from `.env`)

**Override:** `APP_CONFIG=/custom/path.yaml`

---

## Bootstrap Sequence

1. `main.go` loads config
2. Init SQLite at `database.path`
3. `BootstrapService.InitAdmin()` checks if user `自己` exists
4. If not, create admin: `ID=1, Name=自己, IsAdmin=true`
5. Admin runs `scripts/create_device.go` → AccessKey output
6. Admin distributes AccessKey to first device
7. Device calls `/auth/bind` → obtains JWT

---

## Revocation Mechanism

- `DELETE /api/v1/device/:id` sets `RevokedAt` to now
- JWT middleware checks `device.RevokedAt.Valid`:
  - `true` → reject with 401 `"device revoked"`
- **Immediate effect** — no need to wait for token expiration
- Idempotent — revoking already-revoked device returns success

---

## Known Pitfalls (Must Avoid)

1. **Import path mismatch** → match `go.mod` module name `home-datacenter-api`
2. **Repository typo** → `repository`, not `respository`
3. **SQLite driver** → use `glebarez/sqlite` (pure-Go), not `gorm.io/driver/sqlite` (CGO)
4. **PowerShell JSON** → use `ConvertTo-Json`, not inline string escaping
5. **JWT test token** → always use real token from `/auth/bind`, not jwt.io examples
6. **NullTime** → never use `*time.Time` for nullable datetime columns with glebarez driver
7. **JWT secret** → never commit a real secret; app boots only with a ≥32-char non-placeholder secret
8. **CRLF on Windows** → `core.autocrlf=true` means gofmt may flag CRLF files locally; the canonical line ending in the repo is LF

---

## Project Status

**Phase 1:** Complete (bootstrap + auth + device)

**Phase 2:** Complete (revocation + management API + unified response + config)

**Phase 3:** Complete (MQTT real-time + WebSocket + device online/offline tracking + Web Dashboard)

**Security hardening pass (2026-07-04):** see `docs/Security` section below.

**Next Items (Optional):**

- PostgreSQL migration
- User management API (create/delete users)
- Unit tests
- Rate limiting on `/auth/bind`
- Audit log

---

## Developer Workflow

**Run locally:**

```bash
cd services/api
go run cmd/main.go
```

**Create device:**

```bash
go run scripts/create_device.go
```

**Test with PowerShell:**

```powershell
$body = @{ user_id = 1; access_key = "<key>" } | ConvertTo-Json
Invoke-RestMethod -Uri http://localhost:8080/api/v1/auth/bind `
  -Method POST -Body $body -ContentType "application/json"
```

---

## Document References

- **`docs/api-documentation.md`** — Full API specs, request/response examples
- **`docs/ai-context.md`** — This file (project summary for AI context)
- **`docs/security.md`** — Security model, hardening pass, and residual risks

---

## Security

This project is internet-exposed via Cloudflare Tunnel, so the API and
dashboard are reachable by anyone who knows the hostname. The
authentication model is the AccessKey → 365-day JWT flow; there is no
rate limit on `/auth/bind`. Defence-in-depth layers applied:

**Secrets**
- `jwt.secret` is validated at startup: empty / placeholder / <32 char
  values cause a hard `log.Fatal`. Generate with `openssl rand -hex 32`.
- Real secrets live in `configs/config.local.yaml` (gitignored) or the
  `JWT_SECRET` env var — never in the committed `config.yaml`.
- AccessKeys are stored as SHA-256 hashes only; plaintext is never persisted.

**Transport**
- Cloudflare Tunnel fronts `dashboard.feiyemomo.top` (nginx → SPA) and
  `api.feiyemomo.top` (Go). TLS terminates at Cloudflare.
- Internal Docker network is plain HTTP; only `web:80` and `api:8080`
  are published, bound to `127.0.0.1` by default in `compose.yaml`.
- Mosquitto port `1883` is **not** published to the host.

**Mosquitto**
- `allow_anonymous false` + password file + ACL (`deploy/mosquitto/`).
- The API server authenticates with the `home-datacenter` account and
  has `readwrite home-datacenter/#`; device clients need their own ACL
  entries. `$SYS/#` write is never granted.

**WebSocket**
- JWT verified on upgrade (header preferred, `?token=` as browser fallback).
- Origin allowlist via `server.allowed_origins` blocks cross-site
  WebSocket hijacking (CSWSH) at the app layer. Empty list = dev mode.

**HTTP response headers**
- `utils.applySecurityHeaders` adds `X-Content-Type-Options: nosniff`,
  `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
  `Cache-Control: no-store` to every `/api/v1/*` response.

**MQTT publish endpoint**
- `POST /api/v1/mqtt/publish` is admin-only and rejects topics outside
  the `home-datacenter/` namespace or starting with `$` (broker control).

**Bind endpoint**
- `/auth/bind` returns a single generic `"invalid credentials"` for all
  failures (bad user_id, wrong key, revoked) to prevent enumeration.

**Repo hygiene**
- `data/`, `config.local.yaml`, `.env`, `*.exe`, build artifacts are
  gitignored. The SQLite DB and Mosquitto persistence that were
  previously tracked have been `git rm --cached`.

**Residual risks (accepted, not yet fixed)**
- No rate limiting on `/auth/bind` — a determined attacker could brute
  AccessKeys offline-rate. Mitigated by 256-bit keys; add a limiter
  when feasible (see `golang.org/x/time/rate`).
- No audit log of bind/revoke events.
- 365-day JWTs are long; revocation is immediate (per-request DB check),
  but there is no short-lived-token + refresh-token rotation yet.
- `CheckOrigin` is permissive when `allowed_origins` is empty (local dev).

---

**Last Updated:** 2026-07-04 (security hardening pass + dashboard docs)