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
| `GET /api/v1/user` | JWT+admin | List all users with each user's `device_count` |
| `POST /api/v1/user` | JWT+admin | Create user `{name, is_admin}` |
| `GET /api/v1/user/:id` | JWT+admin | Fetch one user |
| `PUT /api/v1/user/:id` | JWT+admin | Partial update `{name?, is_admin?}` (last-admin + self-demote guards) |
| `DELETE /api/v1/user/:id` | JWT+admin | Delete user + cascade-delete their devices; returns `{deleted_devices:N}` |
| `GET /api/v1/device/list` | JWT | List devices (admin=all, non-admin=own) |
| `DELETE /api/v1/device/:id` | JWT | Revoke device (soft delete) |
| `GET /api/v1/system/status` | JWT | Dashboard metrics (MQTT/WS/online devices) |
| `POST /api/v1/mqtt/publish` | JWT+admin | Publish to a `home-datacenter/` topic |
| `GET /api/v1/cameras` | JWT | List cameras (platformized device view) |
| `GET /api/v1/cameras/:id` | JWT | Fetch one camera + live stream URLs |
| `POST /api/v1/cameras` | JWT+admin | Register a camera (encrypts creds, pushes RTSP to go2rtc) |
| `DELETE /api/v1/cameras/:id` | JWT+admin | Unregister a camera (DB + go2rtc) |
| `POST /api/v1/cameras/:id/ptz` | JWT+admin | Send ONVIF PTZ command (auto-discovers profile_token) |
| `GET /api/v1/automation/rules` | JWT+admin | List automation rules |
| `POST /api/v1/automation/rules` | JWT+admin | Create automation rule |
| `PUT /api/v1/automation/rules/:id` | JWT+admin | Update rule |
| `DELETE /api/v1/automation/rules/:id` | JWT+admin | Delete rule |
| `POST /api/v1/automation/rules/:id/test` | JWT+admin | Manually fire a rule (no fire_count bump) |
| `GET /api/v1/automation/metrics` | JWT+admin | Global engine metrics (events/fires/errors/dropped) |
| `GET /api/v1/automation/metrics?reset=1` | JWT+admin | Reset all metrics counters |
| `GET /api/v1/automation/rules/:id/metrics` | JWT+admin | Per-rule metrics |
| `POST /api/v1/automation/rules/:id/cooldown` | JWT+admin | Pin `lastFire` to silence a misbehaving rule (body `{seconds}`) |
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
│   ├── device/manager.go        // Online/offline + heartbeat + MarkAllOffline on disconnect
│   ├── camera/                  // Phase 4 — camera platformization
│   │   ├── doc.go
│   │   ├── go2rtc.go            // HTTP client for /api/streams (query params), /api/webrtc, /api/stream.m3u8
│   │   ├── registry.go          // CRUD + go2rtc sync + BootReplay + UpdateStatus + SaveProfileToken
│   │   ├── onvif.go             // ONVIF PTZ dispatcher (raw SOAP, WS-Security PasswordDigest, lazy-cached)
│   │   ├── health.go            // Background TCP probe → device.status / camera.online / camera.offline on EventBus
│   │   └── json.go
│   ├── automation/              // Phase 5 — Automation Engine (rule CRUD + fire)
│   │   ├── engine.go            // Subscribe "*" → trigger match → condition → action (notify/mqtt/webhook)
│   │   ├── handler.go           // /api/v1/automation/rules CRUD + /test
│   │   └── engine_test.go       // trigger / time / payload / SSRF / MQTT-topic unit tests
│   ├── eventbus/                // In-memory pub/sub (Device/Camera/MQTT → WS + Automation)
│   ├── model/
│   │   ├── user.go
│   │   ├── device.go
│   │   ├── camera.go            // Camera + stream URLs (Phase 4)
│   │   └── automation.go        // Rule + Condition + Action (GORM, JSON TEXT columns)
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
│   │   └── camera_handler.go    // /cameras* — register/list/get/delete/ptz
│   ├── middleware/
│   │   ├── jwt.go               // JWT auth + revocation check
│   │   └── admin.go             // RequireAdmin(db) — must be installed after JWTAuth
│   ├── mqtt/                    // Paho client, topic schema, handler
│   ├── utils/
│   │   ├── key.go               // AccessKey generation + hash
│   │   ├── jwt.go               // JWT signing/parsing
│   │   ├── nulltime.go          // Nullable time wrapper
│   │   ├── response.go          // Unified response + security headers
│   │   └── secret.go            // AES-256-GCM box for camera credentials
│   ├── router/router.go         // (placeholder; routes in main.go)
├── scripts/create_device.go     // Offline device creation tool
├── configs/config.yaml          // Server/DB/JWT/MQTT/WS config (placeholders)
├── configs/config.local.yaml    // gitignored local override (real secret)
├── Dockerfile
└── (compose.yaml at project root)

web/                             // React + Vite + Tailwind dashboard SPA
├── src/
│   ├── pages/{Dashboard,Cameras,Devices,DeviceCreate,Login,MqttDebug,Profile}.tsx
│   │                       // Cameras: list + live view + delete (read-mostly)
│   │                       // DeviceCreate: /cameras/new — dedicated full-page
│   │                       //   form for registering a camera (Phase 7)
│   ├── api/{auth,camera,client,device,system}.ts
│   │                  // client.ts: axios + authedFetch() + authHeaderFor()
│   │                  //   (authedFetch attaches the JWT to plain fetch
│   │                  //   requests going through nginx's /go2rtc/ location,
│   │                  //   which is gated by auth_request /api/v1/auth/verify)
│   ├── context/AuthContext.tsx  // /user/me probe, isAdmin
│   ├── hooks/{useAuth,useWebSocket,useHLSStream,useWebRTCStream}.ts
│   │            // useHLSStream: HLS primary path (HEVC over fMP4)
│   │            // useWebRTCStream: low-latency path; auto-fallback to HLS
│   │            //   for HEVC cameras on Chromium (Chrome/Edge/WebView)
│   └── components/              // Layout, Sidebar, ProtectedRoute, ui/*
├── nginx.conf                   // SPA + /api proxy + /api/v1/ws upgrade
└── Dockerfile

deploy/
├── mosquitto/{mosquitto.conf,aclfile,passwd}  // broker + ACL + creds
├── cloudflared/config.yml        // dashboard + api + cam hostnames
├── go2rtc/{Dockerfile,go2rtc.yaml} // RTSP→WebRTC/HLS bridge (go2rtc.yaml COPY'd into image, not bind-mounted)
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
go2rtc:
  base_url: http://home-go2rtc:1984   # in-network Docker hostname
camera:
  webrtc_public_base: ""   # browser-accessible go2rtc URL; "" = LAN-only.
                           # Set to http://localhost:1984 for local dev,
                           # or https://cam.example.com for Cloudflare Tunnel
  ice_servers: ""          # STUN/TURN servers for WebRTC; empty = default STUN
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
9. **Device status payload parsing** → `mqtt.Handler.handleStatus` accepts both strict JSON (`{"status":"online","ts":1}`) and unquoted-key pseudo-JSON (`{status:online,ts:1}`). A bare `status=...` is also tolerated as a last-ditch fallback. Canonical JSON is re-emitted on the EventBus, so downstream consumers can rely on strict JSON. Always re-emit canonical JSON when adding new publishers; do not pass the raw payload downstream.

---

## Project Status

**Phase 1:** Complete (bootstrap + auth + device)

**Phase 2:** Complete (revocation + management API + unified response + config)

**Phase 4 (Platformization):** Complete

- Camera model + registry + go2rtc sync (RTSP → WebRTC/HLS)
- ONVIF PTZ dispatcher (raw SOAP, WS-Security PasswordDigest, auto-discover profile_token)
- `webrtc_public_base` config for browser-accessible go2rtc URLs
- `SaveProfileToken` for cached ONVIF profile persistence
- Health checker (TCP probe + EventBus)
- New routes:
  - `POST   /api/v1/cameras`            (admin) Register
  - `GET    /api/v1/cameras`            List
  - `GET    /api/v1/cameras/:id`        Fetch
  - `DELETE /api/v1/cameras/:id`        (admin) Unregister
  - `POST   /api/v1/cameras/:id/ptz`    (admin) PTZ
- `utils.SecretBox` (AES-256-GCM, key = SHA-256(JWT_SECRET))
- New middleware `RequireAdmin(db)`
- New container `home-go2rtc` + `cam.feiyemomo.top` tunnel ingress

**Phase 5 (Event-Driven System + Automation Engine):** Complete (2026-07-05)

- Unified Event model: `id / type / source / severity / payload / timestamp`
- Enhanced EventBus: `publish` / `subscribe` / `*` wildcard / fan-out (goroutine-safe)
- Camera events: `camera.online` / `camera.offline` on health-check transitions
- MQTT → Event conversion (existing handler already publishes to EventBus)
- WebSocket bridge: subscribes EventBus topics (`device`, `camera`,
  `user.notification`, `system.broadcast`, `automation.fired`) and pushes to clients
- Automation Engine: rule = `trigger + condition + action`
  - Trigger: event topic prefix match (segment-boundary aware)
  - Condition: time window (`time_gte` / `time_lte`, midnight wrap) + `payload_eq`
  - Action: `notify` / `mqtt` / `webhook` (SSRF guard + MQTT namespace check)
- New routes (admin-only):
  - `GET    /api/v1/automation/rules`        List
  - `POST   /api/v1/automation/rules`        Create
  - `GET    /api/v1/automation/rules/:id`    Fetch
  - `PUT    /api/v1/automation/rules/:id`    Update
  - `DELETE /api/v1/automation/rules/:id`    Delete
  - `POST   /api/v1/automation/rules/:id/test`  Manually fire (no fire_count bump)
- Security: MQTT topic namespace + webhook SSRF guard (private / loopback /
  link-local / unspecified IPs rejected at fire time); rule CRUD admin-only.

**Phase 6 (Automation Runtime):** Complete (2026-07-05)

- **Enriched Condition**: `source` (exact match), `threshold` (numeric op +
  value), `regex` (RE2), `any` (OR combine). `time_gte`/`time_lte` already
  wrapped midnight.
- **Enriched Action**: `timeout_ms` (per-attempt, default 5000) +
  `retry_max` (webhook only; 4xx permanent, 5xx/network → exponential
  backoff 500ms×2^n capped 30s; `notify`/`mqtt` not retried).
- **Throttle**: `cooldown_s` / `rate_per_min` (60s sliding window) /
  `dedup` (SHA-256 prefix of topic+source+payload). In-memory runtime
  state per rule; pruned on `Reload()`.
- **Metrics** (admin-only, in-memory, no Prometheus dep):
  - `GET /api/v1/automation/metrics` — global counters + per-rule map
  - `GET /api/v1/automation/metrics?reset=1` — zero all counters
  - `GET /api/v1/automation/rules/:id/metrics` — per-rule slice
  - Atomic counters via `sync/atomic`; per-rule map mutex-guarded.
- **Admin escape hatches**:
  - `POST /api/v1/automation/rules/:id/cooldown` body `{seconds:N}`
    pins `lastFire` to silence a misbehaving rule.
  - `POST /api/v1/automation/rules/:id/test` runs action synchronously
    but does NOT increment `fire_count` (operator review metric).
- **Audit event**: every fire publishes `automation.fired` to EventBus
  with rule id/name, trigger, event id, ok/err, duration_ms. WS Hub
  forwards it so the dashboard can render a live activity feed.
- **Verified** end-to-end: `payload_eq` filter, throttle (5 events/1s →
  2 fires + 9 dropped), SSRF (127.0.0.1 / 10.0.0.1 / 169.254.169.254
  rejected at fire time), MQTT namespace (`$SYS/...` and `other-ns/...`
  rejected at CRUD time), `?reset=1`, cooldown endpoint.
- Unit tests in `internal/automation/engine_test.go` cover trigger
  prefix match (segment boundary), `timeInRange` (incl. midnight
  wrap), `conditionMatches` (payload_eq + malformed payload),
  `isAllowedMQTTTopic`, `isPublicIP` (v4 + v6 loopback/private/
  link-local/unspecified).

**Phase 7 (Player UX + Security Hardening):** Complete (2026-07-11)

- **Light/dark theme**: `useTheme` hook persists `home.theme` in
  `localStorage`, applies `data-theme` on `<html>`. `applyThemeEarly()`
  runs in `main.tsx` BEFORE React mounts to avoid a dark→light flash
  on first paint. CSS variables (`--bg`, `--fg`, `--slate-50…950`)
  drive Tailwind colors via `bg-slate-*` / `text-fg-*` / `bg-surface-*`
  utility classes, so the entire palette auto-flips. Header Sun/Moon
  button toggles; cross-tab sync via `storage` event.
- **WebRTC/HLS transport toggle**: `LiveVideo.tsx` segmented control
  with `auto` (default: WebRTC→HLS fallback) / `webrtc` (sticky,
  error overlay) / `hls` (sticky, forced fragmented-MP4). Stored in
  `home.transport` localStorage key. The auto→HLS fallback fires
  only in `auto` mode; explicit selections suppress it so the operator
  can pin a single transport during codec-bug triage.
- **HLS fragmented-MP4 fix**: both `go2rtc.go` `HLSURL` helper and
  the inline public-base branch in `registry.go` `StreamConfig` now
  append `&mp4=` so hls.js requests `segment.m4s` (fMP4) instead of
  `segment.ts` (MPEG-TS). hls.js's TS demuxer silently drops HEVC
  frames even when the segments arrive — `<video>` never fires
  `playing` and the dashboard looks stalled. `useHLSStream` now
  probes both `video.canPlayType` and `MediaSource.isTypeSupported`
  for `hvc1` so the "browser cannot decode H.265" error message
  fires earlier.
- **ffmpeg opt-in**: `Camera.Transcode=true` rewrites the go2rtc
  source URL to `ffmpeg:rtsp://…#video=h264`. The `ffmpeg:` scheme
  prefix is required — go2rtc's rtsp producer silently ignores
  `#video=h264` and forwards whatever codecs the SDP advertises.
  We do NOT add `#audio=…` to the ffmpeg URL (any non-empty audio
  value is fed raw to ffmpeg, e.g. `audio=0` produces `-0`, a
  malformed command line). Omitting `audio=` causes `parseArgs` to
  inject `-an` so ffmpeg drops the camera's PCMA track cleanly.
  Dashboard shows a small "x264" badge on transcoding cameras.
- **`/auth/bind` rate limit**: `internal/middleware/ratelimit.go`
  exposes a per-IP token-bucket limiter using `golang.org/x/time/rate`.
  Defaults `rps=0.1, burst=5` (5 quick attempts, then 1 per 10s),
  configurable via `auth.rate_limit.*` in `configs/config.yaml`. 429
  response body is identical to the 401 body to prevent enumeration.
  See `docs/security.md` §13 for the limitations discussion
  (in-process state, `c.ClientIP()` trust, per-IP-not-per-account).

**Phase 8 (User Management API):** Complete (2026-07-11)

- **Backend** (`services/api/internal/service/user_service.go`,
  `services/api/internal/handler/user_handler.go`):
  - Domain errors: `ErrUserNotFound` / `ErrInvalidName` / `ErrNameTaken` /
    `ErrLastAdmin` / `ErrSelfDelete` / `ErrSelfDemote`. Centralised
    `writeUserServiceError` maps each to a stable HTTP code
    (400 / 400 / 409 / 400 / 400 / 400).
  - `isValidUserName`: 1..32 runes, unicode letter/digit/`_`/`-`,
    leading/trailing whitespace silently trimmed, internal whitespace
    rejected. Unicode is fully supported (e.g. `小明`, `自己`).
  - `Create` / `Update` / `Delete` enforce: pre-check + DB unique
    constraint (TOCTOU-safe), last-admin guard on demote/delete,
    self-delete + self-demote rejected. Cameras are NOT cascaded.
  - `Delete` cascades to `devices` (devices-first order so a partial
    failure leaves the user row recoverable).
  - `isUniqueViolation` matches both SQLite ("UNIQUE constraint
    failed") and Postgres ("duplicate key value") error strings.
- **Routes** (all admin-only, mounted under `/api/v1/user`):
  - `GET    /api/v1/user`       List + `device_count` per row
  - `POST   /api/v1/user`       Create `{name, is_admin}`
  - `GET    /api/v1/user/:id`   Fetch one
  - `PUT    /api/v1/user/:id`   Partial update `{name?, is_admin?}`
  - `DELETE /api/v1/user/:id`   Delete + cascade devices
  - `GET    /api/v1/user/me`    Self (any authenticated user, existed
                                before Phase 8 — now reused as the
                                dashboard's "you" indicator)
- **Frontend** (`web/src/api/user.ts`, `web/src/pages/Users.tsx`,
  `web/src/types.ts`):
  - Admin-only `/users` route in `Layout.tsx` nav.
  - CRUD table with per-row rename / role toggle / delete confirm.
  - Client-side guards mirror the server's last-admin + self-
    delete/self-demote rules (disabled buttons, inline error
    banner) so the operator never round-trips a guaranteed-reject.
  - The `you` badge on the caller's own row disables the
    delete + role-toggle controls even before the request leaves
    the browser.
- **Tests**: `services/api/internal/service/user_service_test.go`
  covers `isValidUserName` (ascii / unicode / whitespace / length
  boundaries), `normalizeUserName` (trim + reject internal ws),
  and `isUniqueViolation` (sqlite + postgres error shapes).
- **Documentation**: full per-endpoint section in
  `docs/api-documentation.md` (request/response/error matrices);
  `docs/security.md` and `docs/ai-context.md` reference the new
  state guards.

**Security hardening pass (2026-07-04):** see `docs/Security` section below.

**Codec restriction (2026-07-18): WebRTC only supports H.264**

The dashboard codec dropdown (`web/src/pages/Cameras.tsx`) and the
backend `PUT /api/v1/cameras/:id/codec` endpoint
(`services/api/internal/camera/registry.go` `UpdateCodec`) now only
accept `"h264"`. The `"passthrough"` and `"h265"` options were removed
because WebRTC's RTP codec registry mandates H.264 (plus VP8/VP9/AV1)
but does NOT include H.265 — `codec=h265` and `codec=passthrough`
(with an H.265 camera) always return SDP 502 "codecs not matched:
video:H265 => video:VP8, video:VP9, video:H264, video:AV1" on
Chrome/Edge/Firefox. This is a protocol-level limitation, not a bug.

- **Frontend** (`web/src/pages/Cameras.tsx`): the `<Select>` only
  renders `<option value="h264">H.264</option>`. Legacy cameras with
  `codec=passthrough`/`h265` (set before this restriction) get a
  disabled `<option value={currentCodec} disabled>…(legacy)</option>`
  so the dropdown still reflects server state; the operator can
  select "H.264" to migrate. `codecBadgeLabel` still renders the
  actual codec label ("直通" / "H.265") in the badge for observability.
- **Frontend API** (`web/src/api/camera.ts`): `updateCodec` signature
  tightened from `"passthrough" | "h264" | "h265"` to `"h264"`.
- **Backend** (`UpdateCodec`): the switch now only matches `case "h264"`
  (and `case ""` → `"h264"`); any other value returns 400 with
  `invalid codec %q (only "h264" is accepted — WebRTC does not support H.265)`.
- **Backward compat**: `effectiveCodec` / `rtspURL` still handle
  `passthrough` and `h265` for existing DB rows so legacy cameras
  don't break on boot replay — they just can't be (re)set to those
  values via the API. The `RegisterInput.Codec` field is unchanged
  (registration uses the `transcode` boolean toggle, not the codec
  string, so no UI change needed there).
- **Model** (`model.Camera.Codec`): doc comment updated to mark
  `passthrough`/`h265` as LEGACY (not settable via `UpdateCodec`).

**Next Items (Optional):**

- PostgreSQL migration
- Unit tests (automation rule cases, WS hub fan-out, gorm repositories)
- Audit log (record who created/deleted users, bound devices, fired rules)
- Recordings (continuous HLS archive per camera → searchable playback)
- Per-camera user ownership transfer (currently `cameras.owner_id` is
  set at register time and never reassigned)

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
- On `OnConnectionLost` the MQTT client calls `device.Manager.MarkAllOffline`
  before logging the disconnect, so the dashboard reflects the loss
  immediately instead of waiting up to `heartbeatTimeout` (90s) for the
  sweeper to time each device out. Devices that come back online
  re-mark themselves via `SetOnline` / `Heartbeat`.

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
- `/auth/bind` rate limiter is in-process and per-IP (not per-account),
  so a botnet can still grind at 1 attempt per 10s × N IPs. The
  256-bit keyspace is the load-bearing defense; the limiter only
  blunts volume. See `docs/security.md` §13.
- No audit log of bind/revoke/user-lifecycle events.
- 365-day JWTs are long; revocation is immediate (per-request DB check),
  but there is no short-lived-token + refresh-token rotation yet.
- `CheckOrigin` is permissive when `allowed_origins` is empty (local dev).

---

**Last Updated:** 2026-07-18 (Codec restriction: WebRTC only supports H.264 — passthrough/h265 options removed from dashboard + UpdateCodec API)