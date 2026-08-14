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
| NVR / AI Detection | Frigate 0.17 (bundled go2rtc + OpenVINO CPU detector) |
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
| `POST /api/v1/auth/bind` | None | Exchange AccessKey for JWT (IP rate-limited) |
| `GET /api/v1/auth/verify` | None | JWT validation for nginx auth_request (needs bearer token) |
| `POST /api/v1/auth/logout` | JWT | Server-side logout, clears HttpOnly home_token cookie |
| `GET /api/v1/user/me` | JWT | Current user profile |
| `GET /api/v1/user` | JWT+admin | List all users with each user's `device_count` |
| `POST /api/v1/user` | JWT+admin | Create user `{name, is_admin}` |
| `GET /api/v1/user/:id` | JWT+admin | Fetch one user |
| `PUT /api/v1/user/:id` | JWT+admin | Partial update `{name?, is_admin?}` (last-admin + self-demote guards) |
| `DELETE /api/v1/user/:id` | JWT+admin | Delete user + cascade-delete their devices; returns `{deleted_devices:N}` |
| `GET /api/v1/device/list` | JWT | List devices (admin=all, non-admin=own; `?scope=mine\|all`) |
| `POST /api/v1/device` | JWT | Create a new device, returns plaintext access_key |
| `DELETE /api/v1/device/:id` | JWT | Revoke device (soft delete) |
| `DELETE /api/v1/device/:id/hard` | JWT+admin | Permanently delete an already-revoked device |
| `POST /api/v1/device/:id/rotate-token` | JWT+admin | Increment `token_version`, invalidate all existing JWTs |
| `GET /api/v1/system/status` | JWT | Dashboard metrics (MQTT/WS/online devices) |
| `GET /api/v1/system/logs` | JWT | Audit log list (supports `limit`, `offset`, `event_type`, `level` filters) |
| `DELETE /api/v1/system/logs/:id` | JWT+admin | Delete a single log entry (verify-and-delete workflow) |
| `POST /api/v1/mqtt/publish` | JWT+admin | Publish to a `home-datacenter/` topic |
| `GET /api/v1/cameras` | JWT | List cameras (platformized device view) |
| `GET /api/v1/cameras/:id` | JWT | Fetch one camera + live stream URLs |
| `POST /api/v1/cameras` | JWT+admin | Register a camera (encrypts creds, pushes RTSP to go2rtc) |
| `DELETE /api/v1/cameras/:id` | JWT+admin | Unregister a camera (DB + go2rtc) |
| `POST /api/v1/cameras/:id/ptz` | JWT | Send ONVIF PTZ command (auto-discovers profile_token; non-admin with read access) |
| `POST /api/v1/cameras/:id/preheat` | JWT | Trigger go2rtc to pre-connect RTSP source (eliminate cold-start latency) |
| `POST /api/v1/cameras/:id/webrtc` | JWT | WebRTC SDP exchange proxy → go2rtc (WHEP) |
| `GET /api/v1/cameras/ice` | JWT | Browser ICE servers config (STUN/TURN) + `webrtc_base` |
| `GET /api/v1/cameras/alerts` | JWT | List global alerts |
| `GET /api/v1/cameras/:id/frame` | JWT | Live snapshot JPEG frame |
| `GET /api/v1/cameras/:id/stream.mp4` | JWT | fMP4 live stream (for ExoPlayer ProgressiveMediaSource) |
| `GET /api/v1/cameras/:id/recordings` | JWT | List recordings per camera |
| `GET /api/v1/cameras/:id/recordings/:recId/file` | JWT | Play a specific recording MP4 file |
| `GET /api/v1/cameras/:id/motion-ranges` | JWT | Motion-active time ranges within [after, before) |
| `PUT /api/v1/cameras/:id/codec` | JWT+admin | Update codec (only `"h264"` accepted) |
| `PUT /api/v1/cameras/:id/audio` | JWT+admin | Toggle audio transcoding `{enabled: bool}` |
| `PUT /api/v1/cameras/:id/recording` | JWT+admin | Set recording plan |
| `GET /api/v1/cameras/:id/presets/discover` | JWT | Discover PTZ presets |
| `PUT /api/v1/cameras/:id/presets/:alias` | JWT+admin | Set PTZ preset |
| `DELETE /api/v1/cameras/:id/presets/:alias` | JWT+admin | Delete PTZ preset |
| `POST /api/v1/cameras/:id/preset/:alias` | JWT+admin | Go to PTZ preset |
| `POST /api/v1/cameras/:id/shares` | JWT | Share camera with another user (owner or admin) |
| `DELETE /api/v1/cameras/:id/shares/:user_id` | JWT | Unshare camera (owner or admin) |
| `GET /api/v1/cameras/:id/shares` | JWT | List camera shares (requires read access) |
| `DELETE /api/v1/cameras/:id/recordings/:recId` | JWT+admin | Delete a recording segment |
| `GET /api/v1/automation/rules` | JWT+admin | List automation rules |
| `POST /api/v1/automation/rules` | JWT+admin | Create automation rule |
| `PUT /api/v1/automation/rules/:id` | JWT+admin | Update rule |
| `DELETE /api/v1/automation/rules/:id` | JWT+admin | Delete rule |
| `POST /api/v1/automation/rules/:id/test` | JWT+admin | Manually fire a rule (no fire_count bump) |
| `GET /api/v1/automation/metrics` | JWT+admin | Global engine metrics (events/fires/errors/dropped) |
| `GET /api/v1/automation/metrics?reset=1` | JWT+admin | Reset all metrics counters |
| `GET /api/v1/automation/rules/:id/metrics` | JWT+admin | Per-rule metrics |
| `POST /api/v1/automation/rules/:id/cooldown` | JWT+admin | Pin `lastFire` to silence a misbehaving rule (body `{seconds}`) |
| `GET /api/v1/weather` | JWT | Weather data proxy (5-min cache, wttr.in backend) |
| `GET /api/v1/network/status` | JWT | Network quality (IPv6/NAT/P2P/Relay) |
| `POST /api/v1/network/p2p/register` | JWT | Register P2P peer endpoint |
| `DELETE /api/v1/network/p2p/register` | JWT | Unregister P2P peer |
| `GET /api/v1/network/p2p/server-endpoint` | JWT | Server P2P endpoint |
| `GET /api/v1/network/p2p/peers/:id` | JWT | Lookup specific peer |
| `GET /api/v1/network/p2p/peers` | JWT+admin | List all registered peers |
| `GET /api/v1/release/latest` | JWT | Latest app release metadata |
| `GET /api/v1/release/latest/apk` | JWT | Download latest APK |
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
│   ├── camera/                  // Phase 4 — camera platformization; Phase 9 — Frigate NVR integration
│   │   ├── doc.go
│   │   ├── go2rtc.go            // HTTP client for bundled go2rtc: /api/streams, /api/webrtc, /api/stream.m3u8
│   │   ├── frigate.go           // FrigateClient: PushConfig (PUT /api/config/set), ListRecordings, Alive check
│   │   ├── registry.go          // CRUD + go2rtc sync + Frigate config push + BootReplay + UpdateStatus + SaveProfileToken
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
├── frigate/config.yml            // Frigate base config (detectors, mqtt, go2rtc, record retention)
├── cloudflared/config.yml        // dashboard + api + cam hostnames
├── go2rtc/{Dockerfile,go2rtc.yaml} // RTSP→WebRTC/HLS bridge (legacy; now bundled in Frigate)
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
10. **Frigate `requires_restart` does not actually restart ffmpeg** → `PUT /api/config/set` with `requires_restart: 1` does NOT restart ffmpeg processes for already-running cameras. detect.fps / record.enabled changes only take effect after `docker compose up -d --force-recreate frigate`. The restart flag is reliable for adding/removing cameras but not for modifying existing ones.
11. **Frigate env var prefix** → Frigate's config env var substitutor ONLY reads env vars starting with `FRIGATE_`. MQTT credentials etc. must be named `FRIGATE_MQTT_USERNAME` in the container, not `MQTT_USERNAME`. And the YAML values must be quoted (e.g. `user: "{FRIGATE_MQTT_USERNAME}"`) — otherwise YAML parses `{...}` as a flow mapping (dict), not a string.
12. **OpenVINO `num_threads: 1` can be faster on low-core CPUs** → On the J4125 (4 cores, single-threaded per core), SSDLite MobileNet v2 inference is actually faster with 1 thread (~43ms) than with 4 threads (~71ms). The thread synchronization overhead exceeds the parallelism benefit for this lightweight model. Single thread also uses less overall CPU.
13. **Frigate `detect.fps` only affects the detection pipeline, not recording** → A single ffmpeg process has two outputs: one `-c:v copy` for recording (original framerate) and one `-r N -vf fps=N` for detection. Lowering detect.fps reduces AI load without affecting recording quality.
14. **Go2rtc is now bundled in Frigate** → The standalone `home-go2rtc` container is deprecated. All go2rtc functionality (streams, WebRTC, HLS) is served by Frigate's built-in go2rtc on port 1984. The API talks to `http://home-frigate:1984` for go2rtc operations.

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

**Phase 9 (Frigate NVR + OpenVINO AI Detection):** Complete (2026-07-18)

- **Frigate 0.17** deployed as `home-frigate` container, replacing
  the standalone `home-go2rtc`. Frigate bundles go2rtc internally,
  so we get both NVR features and WebRTC/HLS streaming from one
  container.
- **OpenVINO CPU detector** configured as the object detector
  (type: `openvino`, device: `CPU`). Uses SSDLite MobileNet v2
  FP16 IR model from Intel Open Model Zoo at `/openvino-model/`.
  On the J4125, single-threaded inference is ~43ms (faster than
  multi-threaded due to thread-sync overhead on this lightweight
  model). Configured with `num_threads: 1` to limit CPU usage.
- **Detection throttled to 2 fps** (`detect.fps: 2`) — sufficient
  for a residential front-door camera and keeps CPU usage low
  (~70-90% of one core for the whole Frigate container, down from
  110-190% with default 5-fps detection).
- **home-api ↔ Frigate integration**:
  - `FrigateClient.PushConfig()` pushes camera definitions via
    `PUT /api/config/set` (JSON body, partial merge).
  - `FrigateDetect.FPS` field added to the Go struct so the API
    controls detection framerate per camera.
  - `BootReplay` re-pushes the full camera list on home-api
    startup; `pushFrigateConfig` sets `requires_restart=1` when
    any camera has recording enabled (Frigate only starts the
    recording ffmpeg pipeline during a restart).
  - `ListRecordings()` queries `GET /api/<camera>/recordings`
    for per-camera hourly recording segments.
- **MQTT bridge**: Frigate publishes detection events and stats
  to Mosquitto under `frigate/#`. The ACL file grants the
  `home-datacenter` user `readwrite` on `frigate/#`.
- **VAAPI hardware decode** via `/dev/dri/renderD128` passthrough
  (UHD Graphics 600 on J4125). HEVC main-stream decode has
  periodic non-fatal errors from Hikvision's SVC-like multi-layer
  HEVC, but ffmpeg recovers and the detection pipeline stays up.
- **Configuration split**: `deploy/frigate/config.yml` holds the
  static global settings (detectors, mqtt, go2rtc webrtc/hls,
  record retention, auth proxy mode). Camera definitions are
  added/removed dynamically by home-api — they must NOT be
  manually added to config.yml (they get overwritten on the
  next push).
- **Important pitfall**: Frigate's `PUT /api/config/set` with
  `requires_restart: 1` does NOT actually restart ffmpeg
  processes for running cameras. To make a detect.fps or
  record.enabled change take effect, you need
  `docker compose up -d --force-recreate frigate` — the API
  restart flag is only reliable for adding/removing cameras.

## Phase 10 (v1.8.4): IPv6 Prefix Rotation Auto-Adaptation

### Problem
ISP (China Mobile) rotates the IPv6 prefix via DHCPv6-PD, breaking
the hardcoded IPv6 direct-connection address. Mobile devices experience
~1000ms latency (expected ~50ms) due to asymmetric routing through
the stale prefix.

### Solution
- **Backend**: `OutboundIPv6Address()` function + `PrefixWatcher`
  goroutine + `GET /api/v1/network/ipv6` endpoint. Detects prefix
  rotation every 5 minutes, auto-updates go2rtc webrtc.candidates,
  publishes EventBus event.
- **Android**: `BaseUrlResolver.fetchDynamicIpv6Url()` fetches the
  current NAS outbound IPv6 address from the backend, falls back to
  the hardcoded default on failure.
- **NAS**: Stable SLAAC EUI-64 address persisted via systemd service
  (with `accept_dad=0` to avoid kernel DAD removing the address).

### Files Changed
- `services/api/internal/network/ipv6.go` — outbound probe + prefix comparison
- `services/api/internal/network/watcher.go` — periodic prefix rotation watcher
- `services/api/internal/handler/network_handler.go` — /api/v1/network/ipv6 endpoint
- `services/api/internal/camera/frigate.go` — SetWebRTCCandidates method
- `services/api/cmd/main.go` — wire up watcher + new route
- `deploy/frigate/config.yml` — updated IPv6 candidate to new prefix
- `compose.yaml` — updated NAS_IPV6_ADDRESS default to new prefix
- `Android/.../BaseUrlResolver.kt` — dynamic IPv6 URL fetch
- `Android/.../AppContainer.kt` — wire up tokenProvider for fetchDynamicIpv6Url

**Next Items (Optional):**

- PostgreSQL migration
- Unit tests (automation rule cases, WS hub fan-out, gorm repositories)
- Audit log (record who created/deleted users, bound devices, fired rules)
- Recordings (continuous HLS archive per camera → searchable playback)
- Per-camera user ownership transfer (currently `cameras.owner_id` is
  set at register time and never reassigned)

---

## Phase 11 (v1.8.5): IPv6 Direct Connection Latency Optimization

### Problem
After the v1.8.4 prefix-rotation fix, the IPv6 direct-connection path
dropped from ~1000ms to ~500ms on cellular. The residual 500ms ≈
2 × ~250ms (cellular RTT) — one RTT for the TCP handshake, one for
the HTTP roundtrip. NAS-side processing (nginx + Go, ~1ms) and the
docker-proxy IPv6→IPv4 translation (~0ms) are negligible; the NAS is
NOT the bottleneck. The effective lever is connection reuse: skip the
TCP handshake on every call after the first.

### Solution
- **nginx upstream keepalive** (`web/nginx.conf`): added
  `upstream api_backend { server api:8080; keepalive 32; }` and
  switched the `/api/` location to `proxy_pass http://api_backend`
  with `proxy_set_header Connection ""`. nginx now reuses connections
  to the Go backend instead of opening a new one per request. The
  WebSocket location `/api/v1/ws` is left unchanged (still uses
  `http://api:8080` with `Connection "upgrade"`).
- **OkHttp ConnectionPool + warmup** (Android v1.6.28): `NetworkFactory.kt`
  explicitly sets `.connectionPool(ConnectionPool(5, 5, TimeUnit.MINUTES))`
  (HTTP/1.1 retained, h2c NOT enabled for stability).
  `BaseUrlResolver.kt` adds `warmupConnection(url)` — a best-effort
  `HEAD /api/v1/system/status` with 3s timeouts via
  `client.newBuilder()`, invoked from `probeSync()` on URL change to
  pre-establish the TCP connection before the first real API call.
- **docker IPv6 direct (skipped)**: diagnosis showed docker-proxy
  translation overhead is ~0ms, not a bottleneck. Enabling native
  docker IPv6 would require daemon + bridge + compose network
  reconfiguration and a firewall re-audit, for zero measurable gain.
  Documented as explicitly skipped.

### Files Changed
- `web/nginx.conf` — `upstream api_backend` block + `keepalive 32` + `Connection ""` header
- `Android/.../data/api/NetworkFactory.kt` — explicit `ConnectionPool(5, 5, TimeUnit.MINUTES)`
- `Android/.../util/BaseUrlResolver.kt` — `warmupConnection(url)` method + invocation in `probeSync()`
- `Android/app/build.gradle.kts` — versionCode 70 → 71, versionName "1.6.27" → "1.6.28"
- `docs/ipv6-latency-optimization.md` — new detailed design doc
- `docs/ai-context.md` — Phase 11 section (this section)
- `README.md` — v1.8.5 changelog entry

### Expected Effect
On cellular IPv6 (~250ms RTT): first API call after probe drops from
~500ms to ~250ms (warmup pre-establishes TCP); subsequent calls within
the 5-min pool TTL drop from ~500ms to ~250ms each (one RTT saved per
reused connection). LAN path (~7ms) sees <1ms change — sub-millisecond
and not user-perceptible.

### v1.6.29 Fix: Latency Display

The v1.8.5 / v1.6.28 optimization cut the actual API RTT to ~250ms,
but the Dashboard network quality card still showed ~500ms because
`lastRttMs` was written by the probe in `probeSync()` and the probe
ran **before** `warmupConnection()` — so the probe measured the full
cold handshake + HTTP roundtrip and locked the display to that value.
The warmup only helped later API calls, which never wrote their RTT
back to the display. A second contributing factor was that the
`ConnectionPool` keep-alive (5 min) equaled the probe TTL (5 min),
so the next probe always found an expired connection and re-measured
the cold path.

Three coordinated fixes ship in Android v1.6.29:

1. **`updateRttFromApiCall(rtt)`** — New `BaseUrlResolver` method
   that lets real API calls write their measured RTT back to
   `lastRttMs`. Monotonic: only updates when the new RTT is lower,
   so jitter cannot degrade the displayed value. `loadSystemStatus()`
   (polled every 5 s by `DashboardFragment`) calls it, so the card
   reflects steady-state reused-connection RTT instead of the
   probe's one-shot cold measurement.
2. **Warmup before probe** — `probeSync()` now calls
   `warmupConnection(resolved)` at the start, before probing. The
   probe reuses the warm connection and itself measures ~250ms.
3. **keep-alive 5 min → 10 min** — `ConnectionPool(5, 10, TimeUnit.MINUTES)`
   in `NetworkFactory.kt` now exceeds the 5-min probe TTL, so the
   next probe still finds a warm connection in the pool.

**Expected effect**: the card drops from ~500ms to ~250ms within
5 seconds of app launch (first `loadSystemStatus()` poll reuses the
warm connection and writes back ~250ms via `updateRttFromApiCall()`).

Files changed (Android only):
- `app/build.gradle.kts` — versionCode 71 → 72, versionName "1.6.28" → "1.6.29"
- `app/src/main/java/com/homedatacenter/app/data/api/NetworkFactory.kt` — `ConnectionPool(5, 10, TimeUnit.MINUTES)`
- `app/src/main/java/com/homedatacenter/app/util/BaseUrlResolver.kt` — `updateRttFromApiCall()` + warmup-before-probe reorder in `probeSync()`
- `app/src/main/java/com/homedatacenter/app/ui/dashboard/DashboardFragment.kt` — `loadSystemStatus()` measures RTT and calls `updateRttFromApiCall()`

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

## Dashboard Improvements (2026-07-20)

The web dashboard was extended to close feature gaps with the Android
app (see `APP_VS_DASHBOARD_FEATURES.md`). Key additions:

### Weather card
- `GET /api/v1/weather` proxies wttr.in's `j1` format with a 5-min
  server cache. The `WeatherCard` on the dashboard renders current
  temp, "feels like", humidity, wind, location label, and a WMO-code
  icon. wttr.in's legacy 1xx weather codes are normalized to WMO
  equivalents (`web/src/api/weather.ts` `wmoToIcon`).

### LAN / Remote path chip
- The Network Quality card now shows a chip indicating whether the
  dashboard was loaded from the LAN path (RFC1918 hostname) or via
  Cloudflare Tunnel (any other hostname). Pure client-side
  detection via `window.location.hostname`.

### System theme support
- `useTheme` now accepts `"light" | "dark" | "system"`. The
  `resolved` field exposes the actual applied theme. The header
  theme picker is a 3-state dropdown (Light / Dark / System) with
  an outside-click + Escape close handler. The `system` option
  subscribes to `prefers-color-scheme` changes so OS theme switches
  propagate live without a reload. The `applyThemeEarly()` helper
  in `main.tsx` reads the choice before React mounts so the first
  paint uses the correct theme (no flash).

### 24-hour recording playback
- New `RecordingTimeline` component (`web/src/components/RecordingTimeline.tsx`)
  replaces the old "最近录制" list inside `LiveVideo`'s playback
  mode. Features:
    - **7-day picker** (今天 / 昨天 / 前天 / 周X / MM-DD) —
      matches Frigate's default `record.continuous.days=7` retention
    - **24-hour seekbar** with 1440 minute-buckets; recorded
      minutes highlighted, motion minutes overlaid in red (AI) or
      amber (motion-only)
    - **Click-to-seek** on the seekbar plays the matching 60s
      bucket and seeks to the offset within the recording
    - **Fisheye chip scroller** for the selected day's motion
      events, sorted by `motion_score` (top 50), click to seek
    - **Custom video controls**: play/pause, ±10s skip, current
      time / duration, recording start time, and a speed dropdown
      with `[0.5, 1, 1.5, 2, 3, 5]` options
    - **Double-tap ±10s** gesture (left/right side of video)
    - **Long-press 5x** speed gesture (overrides playbackRate
      while pressed, restores on release)
    - **Auto-advance**: when a recording ends, the next bucket
      is auto-loaded (continuous 24h playback)
    - **Alert seek**: `?time=UNIX&mode=recording` URL params
      auto-select the matching day and play the matching bucket

### Alert click → recording seek
- Dashboard alert entries navigate to
  `/cameras?camera=<id>&time=<unix>&mode=recording` on click.
  The `Cameras` page forwards `targetTime` to `LiveVideo` →
  `RecordingTimeline`, which auto-selects the matching day and
  plays the recording containing that timestamp.

### MP4 fallback middle tier
- `RecordingTimeline` uses JWT-authenticated `fetch` to download
  the 60s MP4 as a Blob and plays it via `URL.createObjectURL`.
  This works on every browser that supports MP4 (no MSE / HEVC
  requirement), making it a reliable middle tier between WebRTC
  (live, codec-restricted) and HLS (live, HEVC-only).

---

## UI Refinements (2026-07-21, v1.8.0)

### Theme-aware color migration
- All 9 page/component files previously used hardcoded Tailwind
  colors (`text-slate-1xx/2xx/3xx/4xx/5xx`, `text-emerald-400`,
  `text-rose-400`, `text-amber-400`, `text-sky-300`,
  `bg-emerald-400`, `fill-amber-400`, …). These render correctly
  in dark mode but become invisible/low-contrast in light mode.
- Replaced with CSS-variable-based classes: `text-fg`,
  `text-fg-muted`, `text-fg-subtle`, `text-[rgb(var(--accent-success))]`,
  `bg-[rgb(var(--accent-success)/0.2)]`, etc. The variables are
  defined per-theme in `web/src/index.css` (`data-theme="light"`
  and `data-theme="dark"` blocks).
- Files touched: Dashboard.tsx, Network.tsx, Users.tsx, Profile.tsx,
  MqttDebug.tsx, Devices.tsx, DeviceCreate.tsx, LiveVideo.tsx,
  RecordingTimeline.tsx.

### LiveVideo header cleanup (kebab menu)
- The live-mode header used to cram 7+ controls into one row
  (transport segmented control, transport badge, mode tabs, Stop,
  Rec, status badge, vendor info), overflowing on narrow viewports.
- Restructured: visible header is now `[title + x264]` `[status
  badge]` `[mode tabs]` `[Stop]` `[⋮]`. The kebab (⋮) dropdown
  holds: vendor + last seen info, transport selector (live mode
  only), recording toggle (admin only). Click-outside handler
  closes the dropdown.

### Player merge (live ↔ playback)
- `RecordingTimeline` previously rendered its own
  `<div class="aspect-video">` below `LiveVideo`'s main video
  area, leaving the main area showing a placeholder ("切换至下方
  时间轴开始播放") during playback mode.
- Now `RecordingTimeline` accepts a `videoPortalTarget?: HTMLElement
  | null` prop and uses `createPortal` from `react-dom` to render
  its `<video>` + custom controls into `LiveVideo`'s main video
  area. Live and playback share the same physical surface.
- `LiveVideo` uses a state-backed ref
  (`const [videoAreaEl, setVideoAreaEl] = useState<HTMLDivElement
  | null>(null)`) so the RecordingTimeline mount is triggered
  once the target element is in the DOM.

### RecordingTimeline simplification
- Removed: fisheye chip scroller (the Top-50-by-motion_score
  chip row from v1.7.0).
- Added: prominent event ribbon above the 24h seekbar. Each
  `MotionRange` renders as a tall colored bar —
  `bg-[rgb(var(--accent-danger))]` when `peak_objects > 0`
  (personnel/AI activity), `bg-[rgb(var(--accent-warm))]` for
  motion-only events. A legend below shows counts of each type.
- The fisheye chip and the event ribbon visualize the same
  `MotionRange` data; the ribbon is more compact and glanceable.

### useCachedFetch hook (plugin caching)
- New: `web/src/hooks/useCachedFetch.ts`. Generic
  sessionStorage-cached fetcher with optional silent background
  refresh (`refetchMs`) and `enabled` gate.
- Pattern: on first mount, synchronously read cached value from
  `sessionStorage[key]` (so the UI paints immediately); show
  `loading: true` only when no cache exists; kick off a background
  fetch; on success, update state + write back to sessionStorage.
  If `refetchMs > 0`, set an interval to silently refresh.
- Applied to Dashboard's three polling widgets:
  - `home.dashboard.weather` → `getWeather()`, 10 min refresh
  - `home.dashboard.status` → `Promise.all([getSystemStatus(),
    getNetworkStatus()])`, 5 s refresh
  - `home.dashboard.alerts` → `listAlerts(20)`, 30 s refresh
- Navigating away from the dashboard and back now shows the
  last-known values instantly instead of a loading spinner.

---

## Phase 12 (v1.8.7): Network Policy Review & Frontend IPv6/UX Fixes

### Problem
Code review of the dashboard's network scheduling policy ("Relay First,
Then Upgrade") surfaced four issues that degrade the IPv6-direct and
WebRTC experience on the web frontend:

1. **`LiveVideo.tsx` `isRemoteAccess()` did not detect IPv6 literals** —
   accessing the dashboard via `http://[2409:8a70:...]:8088/` was
   classified as "remote", defaulting the live transport to HLS. This
   blocked WebRTC on the highest-quality direct path, contradicting
   `Dashboard.tsx`'s `detectApiPath()` which already classified IPv6
   literals as a direct path.
2. **`Dashboard.tsx` quality rating had an unreachable branch** — the
   `clientIPv6 === false` downgrade was evaluated AFTER `apiPath ===
   "remote"` had already returned, so the "server has IPv6 but client
   doesn't → cap at 3 stars" intent was dead code.
3. **`Network.tsx` described an upgrade action but offered no way to
   trigger it** — the "Relay First, Then Upgrade" card showed an "升级"
   badge but no clickable control to actually switch to the IPv6 direct
   URL. The web frontend had no equivalent of Android's `BaseUrlResolver`
   path-switching mechanism.
4. **Dashboard initial load showed stale network status** — the backend
   caches network detection for 60s, but the Dashboard's first fetch
   did not pass `?refresh=true`, so the displayed quality rating could
   be up to 60s stale on initial page load.

### Solution
- **`LiveVideo.tsx`**: Added IPv6-literal regex (`/^\[[0-9a-f:]+\]$/i`)
  to `isRemoteAccess()`, returning `false` for bracketed IPv6 hostnames.
  Now `readTransport()` defaults to `"auto"` (WebRTC first) on IPv6
  direct access, matching LAN behavior.
- **`Dashboard.tsx`**: Reordered the `currentQuality` IIFE so the
  `clientIPv6 === false` check is evaluated BEFORE the generic
  `apiPath === "remote"` clamp. The downgrade is now explicit and
  survives future refactors.
- **`Network.tsx`**: Added `isOnRelay()` helper and `canSwitchToIPv6Direct`
  computed variable. When the user is on relay AND both server and client
  have IPv6, a "切换到 IPv6 直连 →" link renders below the Step 2
  description, opening `http://[<ipv6>]:8088/` in a new tab.
- **`Dashboard.tsx`**: Added `forceRefreshRef = useRef(true)` flag. The
  first `useCachedFetch` fetcher call passes `refresh=true` to
  `getNetworkStatus()`, bypassing the backend cache. Subsequent 5s
  polling refreshes use the cached backend response (no `?refresh=true`)
  to avoid hammering STUN servers.

### Files Changed
- `PROMPT.md` (new) — reusable project prompt with production env access,
  deploy script, test credentials, and standard workflow
- `web/src/components/LiveVideo.tsx` — `isRemoteAccess()` IPv6 detection
- `web/src/pages/Dashboard.tsx` — quality rating reorder + initial-load
  freshness fix (`useRef` flag + `getNetworkStatus(refresh)`)
- `web/src/pages/Network.tsx` — `isOnRelay()` helper + `canSwitchToIPv6Direct`
  + clickable "切换到 IPv6 直连" link
- `docs/ai-context.md` — Phase 12 section (this section)
- `README.md` — v1.8.7 changelog entry

### Expected Effect
- IPv6 direct access now attempts WebRTC first (was HLS), matching the
  behavior on LAN. WebRTC's lower latency (~200ms vs HLS's ~5-10s
  buffering) is now achievable on the IPv6 direct path.
- Dashboard quality rating correctly caps at 3 stars when the client
  lacks IPv6 but the server has it, making the "no upgrade available"
  state visually clear.
- Network page's "Relay First, Then Upgrade" model is now actionable —
  users on relay with IPv6 capability can click to switch to the direct
  path, instead of having to manually construct the IPv6 URL.
- Dashboard's initial network quality display reflects current state
  instead of up to 60s of backend cache staleness.

---

## Phase 13 (v1.8.8): IPv6 Full-Path Test & Dev Scripts Consolidation

### Problem
1. v1.8.7 fixed the IPv6 direct path classification in `LiveVideo.tsx`, but the
   dev machine had no IPv6 at the time — the fix was verified only on LAN and
   relay paths, never on the actual IPv6 direct path (`http://[<ipv6>]:8088/`).
2. The `NAS_IPV6_ADDRESS` env var in `compose.yaml` was stale: ISP had rotated
   the prefix from `2409:8a70:37a3:99d0::/64` to `2409:8a70:37a4:9141::/64`,
   but the default value still pointed to the old prefix. The API reported
   `reachable=true` with the unreachable old address, so the "切换到 IPv6 直连"
   link on the relay path pointed to a dead endpoint.
3. Convenient PowerShell scripts were scattered in subdirectories
   (`services/api/scripts/test_ws.ps1`), and frequent operations like git
   commit/push had no one-click script.

### Solution
- **compose.yaml**: Updated `NAS_IPV6_ADDRESS` default from
  `2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09` to
  `2409:8a70:37a4:9141:62be:b4ff:fe08:bd09` (current ISP prefix).
- **test-ws.ps1**: Moved from `services/api/scripts/test_ws.ps1` to project
  root via `git mv` (preserves history). Updated header comments.
- **get-token.ps1** (new): One-click JWT retrieval using built-in test
  AccessKey. Supports `-BaseUrl` (LAN/relay/IPv6) and `-Copy` (clipboard).
  Sets `$env:HC_TOKEN` for downstream API calls.
- **commit.ps1** (new): Interactive git add → commit message → push.
  Supports `-Message` (non-interactive) and `-DryRun` (preview).

### Verification (chrome-devtools MCP)
- IPv6 direct path (`http://[2409:8a70:37a4:9141:62be:b4ff:fe08:bd09]:8088/`):
  - Dashboard network card shows "IPv6 直连" + 5 stars ✓
  - Network page does NOT show "切换到 IPv6 直连" link (already on direct) ✓
  - LiveVideo defaults to "自动" (WebRTC), not HLS ✓ (v1.8.7 fix confirmed)
  - Console: no errors ✓
- Relay path (`https://dashboard.feiyemomo.top/network`):
  - "切换到 IPv6 直连 →" link href points to correct current IPv6 address ✓
  - `direct_url` in JSON payload matches actual NAS IPv6 ✓

### Known Limitation
The PrefixWatcher (`internal/network/watcher.go`) cannot auto-detect prefix
rotations because the `home-api` container runs on a docker bridge without
IPv6 outbound connectivity (confirmed: `docker exec home-api wget -qO- https://ident.me`
returns IPv4). Both `CheckIPv6()` and `OutboundIPv6Address()` short-circuit
with the `NAS_IPV6_ADDRESS` env var. When the ISP rotates the prefix, the
operator must manually update the default in `compose.yaml` (or set
`NAS_IPV6_ADDRESS` in `.env`) and redeploy. A host-level prefix watcher
script would be the long-term fix.

---

## Phase 14 (v1.8.9): Android Network Policy Sync

### Problem
v1.8.7 fixed the web dashboard's network policy issues (LiveVideo IPv6-literal
classification, Dashboard quality rating unreachable branch, Network page
upgrade action, and initial-load freshness), and v1.8.8 verified the IPv6
direct path end-to-end. The Android client (`D:\Projects\Android`) had the
same two issues but was not yet synced:

1. **`BaseUrlResolver.kt` `IPV6_DIRECT_URL` stale fallback constant** — the
   hardcoded IPv6 address used as a fallback when dynamic fetch fails
   (pre-login or backend unreachable) still pointed to the old ISP prefix
   `2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09`. After the ISP rotated the
   /64 prefix to `2409:8a70:37a4:9141::/64` (the same rotation that
   v1.8.8 fixed in `compose.yaml`), the Android fallback pointed to a dead
   endpoint. Dynamic fetch normally masks this, but when the backend is
   unreachable the resolver falls back to the constant, causing probe
   failures that force fallback to the slow Cloudflare Tunnel.

2. **`DashboardFragment.loadNetworkStatus()` first-fetch cache staleness** —
   the first network status fetch after fragment creation did not pass
   `refresh=true`, so the Dashboard displayed up to 60s of stale backend
   cache on initial load. The web dashboard was fixed in v1.8.7
   (`forceRefreshRef`), but the Android client still used the cached path.

### Solution
- **`BaseUrlResolver.kt`**: Updated `IPV6_DIRECT_URL` constant from
  `2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09` to
  `2409:8a70:37a4:9141:62be:b4ff:fe08:bd09`, matching `compose.yaml`'s
  `NAS_IPV6_ADDRESS` default.
- **`DashboardFragment.kt`**: Added `@Volatile private var firstNetworkFetchDone = false`
  flag. `loadNetworkStatus()` now passes `refresh = !firstNetworkFetchDone`
  and sets the flag to `true` after the first call. Subsequent `onResume`
  calls use the backend cache (60s TTL is fresh enough for page re-entry).
  `onDestroyView()` resets the flag so fragment recreation re-forces the
  refresh.
- **`app/build.gradle.kts`**: versionCode 72 → 73, versionName "1.6.29" → "1.6.30".
- **`release-notes-v1.6.30.txt`** (new): Documents both fixes for users.

### Files Changed (Android repo)
- `app/build.gradle.kts` — version bump
- `app/src/main/java/com/homedatacenter/app/util/BaseUrlResolver.kt` — `IPV6_DIRECT_URL` constant
- `app/src/main/java/com/homedatacenter/app/ui/dashboard/DashboardFragment.kt` — `firstNetworkFetchDone` flag
- `release-notes-v1.6.30.txt` — new release notes

### Parity Check
This phase brings the Android client to feature parity with the web
dashboard's v1.8.7 network-policy fixes. Both clients now:
- Use the current ISP prefix for the IPv6 fallback (web: `compose.yaml`
  env var; Android: `IPV6_DIRECT_URL` constant)
- Force `refresh=true` on the first network status fetch after view
  creation, then use cache for subsequent re-entries

---

## Phase 15 (v1.8.13–v1.8.17): Log Cleanup, Security Hardening, Token Rotation, Liquid Glass, Theme Switch Fix

### Phase 15 span: v1.8.13–v1.8.17 (Backend) + v1.7.12–v1.7.17 (Android)

#### v1.8.13 — Log Subscriber Cleanup
- **Removed `TopicDeviceStatus` subscription** from `log/subscriber.go` — it duplicated `camera.online`/`camera.offline` logs ("设备 #N 上线" alongside "摄像头 X 上线"). Camera-friendly-name logs are sufficient for auditing.
- **Removed `user.login`/`user.logout` event logging** — routine auth events crowd out meaningful device/camera logs without adding audit value. WS Hub and automation engine still receive these events directly from the EventBus.

#### v1.8.14 — Single Log Entry Deletion
- **New endpoint**: `DELETE /api/v1/system/logs/:id` (admin only). Used for the "verify and delete" (核查并删除) workflow — the admin reviews a critical offline log entry and removes it once the issue is resolved.

#### v1.8.15 — Admin Token Rotation
- **New endpoint**: `POST /api/v1/device/:id/rotate-token` (admin only). Increments the device's `TokenVersion`, immediately invalidating all existing JWT tokens for that device.
- **Device model**: `TokenVersion int` field (default 1). JWT claims include `token_version`. The middleware checks if `JWT.token_version < DB.token_version` at each request, rejecting with `"token version mismatch"`.
- **Client silent re-bind**: On detecting `"token version mismatch"`, the client re-binds with its stored access_key silently, obtaining a fresh JWT without user intervention.

#### v1.8.16 — Security Hardening
- **Content-Security-Policy header**: All API responses now include `Content-Security-Policy: default-src 'self'` for high-level XSS mitigation.
- **HttpOnly cookie**: The `home_token` JWT cookie is now set with `HttpOnly` flag, preventing JavaScript from reading it via `document.cookie`.
- **Server timeouts**: `http.Server` configured with `ReadTimeout=15s`, `ReadHeaderTimeout=10s`, `WriteTimeout=15s`, `IdleTimeout=60s`, `MaxHeaderBytes=1MB`.
- **Input size limiting**: `io.LimitReader` applied to request bodies in `weather_handler.go` and `frigate.go` to prevent large payload attacks.
- **WebSocket CheckOrigin**: Strict origin validation for WebSocket upgrade requests.
- **Removed insecure default JWT secret**: The placeholder value `PLEASE_CHANGE_TO_A_LONG_RANDOM_SECRET` was removed from `config.yaml`; the app now refuses to boot with any known placeholder.

#### v1.8.17 — Web Dashboard Liquid Glass Visual Upgrade
- **Global CSS upgrade**: Warm cream background, amber accent color, enhanced glass effects with `backdrop-blur` and shadows, unified `cubic-bezier(0.32, 0.72, 0, 1)` animation easing.
- **UI component refresh**: Buttons, badges, cards, inputs all updated to liquid glass style. Layouts simplified (Dashboard, Login, etc.). Status components softened.
- **`prefers-reduced-motion` support**: Animations disabled or reduced for users who prefer reduced motion.

#### Android v1.7.14 — Liquid Glass Warm Color Upgrade
- **Color system**: Changed primary color from coral orange to warm amber in `colors.xml` (both light and dark modes).
- **Glass effects**: Soft shadows and top highlights in drawables (`bg_glass_card.xml`, `bg_button_primary.xml`, etc.).
- **Component updates**: Primary buttons with warm gradients, CameraCard Compose dark mode adaptation, bottom navigation glass styling.
- 14 files modified (+341/-188 lines).

#### Android v1.7.15 — Theme Switch Gradient Fix
- **Root cause**: Negative gradient angle (-90) in 5 drawable files (`bg_glass_card.xml`, `bg_glass_card_warm.xml`, `bg_card.xml`, `bg_card_rounded.xml`, `bg_bottom_nav_container.xml`). Android requires gradient angles to be non-negative multiples of 45.
- **Fix**: Changed all occurrences of `angle="-90"` to `angle="270"` (equivalent angle).
- **Missing Material3 color attributes**: Added `colorSurface`, `colorOnSurface`, `colorSurfaceVariant`, `colorOnSurfaceVariant`, `colorOutline` to `values-night/themes.xml`.

#### Android v1.7.17 — Theme Switch CancellationException Fix
- **Root cause**: Activity recreation during theme switch cancels all Fragment `lifecycleScope` coroutines. The `CancellationException` was caught by generic `catch (e: Exception)` blocks and displayed to the user as "job was cancelled". Additionally, `catch`/`finally` blocks accessed already-destroyed ViewBinding after `onDestroyView`, causing NPE.
- **Fix**: Added `catch (e: CancellationException) { throw e }` before general Exception catches in all 6 Fragments (Dashboard, Settings, Users, Devices, Cameras, ServiceLogs). Added `view != null` checks before accessing `binding` in `catch`/`finally` blocks.
- **Fragment state management**: `setupFragments()` in `MainActivity` now passes `savedInstanceState` to `super.onCreate()` and only adds Fragments when `savedInstanceState == null`, preventing `IllegalStateException: Fragment already added` during recreation.

### Files Changed (Phase 15)

| File | Change |
|------|--------|
| `services/api/internal/log/subscriber.go` | Removed TopicDeviceStatus + user.login/logout subscriptions |
| `services/api/internal/handler/system_log_handler.go` | Added Delete endpoint |
| `services/api/internal/model/device.go` | Added TokenVersion field |
| `services/api/internal/handler/device_handler.go` | Added RotateToken handler |
| `services/api/cmd/main.go` | Added timeout config, CSP, new routes |
| `services/api/internal/utils/response.go` | Added CSP security headers |
| `services/api/internal/utils/jwt.go` | Added token_version claim, removed insecure default |
| `services/api/internal/handler/auth_handler.go` | HttpOnly cookie, server-side logout |
| `services/api/internal/handler/weather_handler.go` | io.LimitReader |
| `services/api/internal/camera/frigate.go` | io.LimitReader |
| `web/src/index.css` | Liquid glass CSS variables, warm palette, animations |
| `web/src/components/*.tsx` | UI component liquid glass styling |
| `web/src/pages/*.tsx` | Layout simplification, glass styling |
| `Android/app/build.gradle.kts` | Version bumps (v1.7.12→v1.7.17) |
| `Android/app/src/main/res/values/colors.xml` | Warm amber palette |
| `Android/app/src/main/res/drawable/*.xml` | Glass effects, gradient angle fix |
| `Android/app/src/main/res/values-night/themes.xml` | Missing Material3 color attributes |
| `Android/app/src/main/java/.../MainActivity.kt` | Fragment state management fix |
| `Android/app/src/main/java/.../ui/*/` | CancellationException handling in all 6 Fragments |

---

## Phase 16 (v1.8.18): Camera Lifecycle Cleanup + Web Animations

### Phase 16 span: v1.8.18 (Backend) + v1.7.18–v1.7.19 (Android)

#### v1.8.18 — Camera Lifecycle Cleanup + Web Animations

**Backend (services/api/internal/camera/):**
- **Camera deletion full cleanup**: `Unregister` now removes all associated data, not just the DB row:
  - Deletes `camera_shares` records for the camera
  - Removes the Frigate recording directory on disk (`/media/frigate/<slug>/`)
  - Best-effort deletion of Frigate detection events via Frigate API
- **Slug uniqueness on registration**: `uniqueSlug()` ensures slug global uniqueness by appending a numeric suffix (`-2`, `-3`, ...) when a collision is detected, preventing Frigate config overwrites when two cameras have the same transliterated name.
- **compose.yaml**: `/media/frigate` mount changed from read-only to read-write so the API container can delete recording directories on camera removal.

**Web (web/src/):**
- **Route transition animations**: `App.tsx` now wraps route switches with fade/slide transitions using `framer-motion`-style CSS transitions, giving a liquid-glass feel to page navigation.
- **Splash loading page**: A branded loading splash shows on first load before the SPA hydrates, eliminating the white flash.
- **Skeleton component**: New `Skeleton.tsx` reusable component for content placeholders during data fetches.
- **Dashboard / Cameras page refinements**: Layout and interaction polish consistent with the warm liquid-glass theme.

**Android (companion releases):**
- **v1.7.18**: UI polish (splash screen, fragment slide animations, "全部报警" section in Cameras tab, "全部" button navigation fix).
- **v1.7.19**: Network probe fast-path-first optimization (LAN/IPv6 switch immediately on success, Tunnel as background fallback) + removal of redundant `/network/ipv6` call (DDNS domain handles prefix rotation).

### Files Changed (Phase 16)

| File | Change |
|------|--------|
| `services/api/internal/camera/registry.go` | Camera deletion cleanup (shares + Frigate dir + events), `uniqueSlug()` |
| `services/api/internal/camera/frigate.go` | Event deletion API call, io.LimitReader |
| `compose.yaml` | `/media/frigate` mount → read-write |
| `web/src/App.tsx` | Route transition animations + splash loading |
| `web/src/pages/Cameras.tsx` | Layout refinements |
| `web/src/pages/Dashboard.tsx` | Layout refinements |
| `web/src/components/Skeleton.tsx` | New skeleton placeholder component |

---

## Phase 17 (v1.8.19): Preload Paralleling + Splash Utilization

### Phase 17 span: v1.8.19 (Web) + v1.7.20–v1.7.21 (Android)

#### v1.8.19 — Web Splash Parallel Prefetch

**Web (web/src/):**
- **Splash parallel prefetch**: `AuthContext.tsx` `useEffect` now runs `Promise.allSettled` for Dashboard first-screen data (`listCameras` / `getNetworkStatus` / `getWeather` / `listAlerts`) in parallel with `getCurrentUser()` during the splash window.
- **sessionStorage write-through**: Prefetched results are written to `sessionStorage` with keys matching `useCachedFetch` consumption (`home.cameras.list` / `home.network.status` / `home.dashboard.weather` / `home.dashboard.alerts`), format `{ t: Date.now(), v }`.
- **Timeout backstop**: `Promise.race` between `Promise.all([userPromise, prefetchPromise])` and a 2000ms timeout triggers `setInitialized(true)`, so slow networks don't block the entry.
- **No-token path unchanged**: Unauthenticated users skip prefetch entirely and redirect to `/login` as before.

#### v1.7.21 — Android WebRTC Parallel Fallback + Splash Prefetch

**Android (companion release):**
- **WebRTC + HLS/MP4 parallel pre-prepare**: `CameraDetailActivity` now creates a `fallbackPlayer` ExoPlayer with `playWhenReady=false` alongside WebRTC negotiation start. On WebRTC `onConnected`, the fallback player is released. On WebRTC `onError`, the fallback player is promoted to the main player with `playWhenReady=true`, cutting fallback switch latency from 500ms-2s to ~100-300ms.
- **Splash parallel prefetch**: `SplashActivity` (logged-in path) now parallel-prefetches `dashboard.status` / `dashboard.weather` / `dashboard.alerts` / `cameras.list` into `CacheManager`. `routeToNext` waits `max(900ms animation, +1100ms prefetch)` with a 2000ms hard cap. Unauthenticated path keeps the original 900ms fixed duration.

#### v1.7.20 — Dead Code Cleanup + Redundancy Fix (Companion)

**Android:**
- Removed `takeWarmWebRtcClient()`, `BaseUrlResolver.switchTo()`, `PrefetchManager.cancelPending()` dead code.
- Wired `tryAutoRefreshToken()` into `HomeCenterApp.onCreate`.
- Added `AtomicBoolean` lock to `prefetchIceConfig` to prevent concurrent first-launch duplicate GET.
- Removed redundant `preheatCamera` call in `CameraDetailActivity.onCreate` (already triggered by `CamerasFragment`).

### Files Changed (Phase 17)

| File | Change |
|------|--------|
| `web/src/context/AuthContext.tsx` | Splash parallel prefetch + sessionStorage write-through + 2000ms timeout |
| Android `CameraDetailActivity.kt` | `fallbackPlayer` field + `prepareFallbackPlayer()` + onConnected/onError lifecycle |
| Android `SplashActivity.kt` | Parallel prefetch of dashboard data + `routeToNext` wait condition |

---

## Phase 18 (v1.8.24): Host IP Change Self-Adaptation + Network Robustness

### Phase 18 span: v1.8.24 (Backend) + v1.8.24 (Android)

#### Backend — LAN IP Auto-Detection & Resilience

**`frigate.go` — `lanIPDetector`:**
- New `lanIPDetector` struct tracks the NAS LAN IPv4 address by observing incoming HTTP request Host headers.
- `UpdateFromHost(host)` parses the Host header, accepts only RFC 1918 private IPv4 addresses, stores the detected IP, and fires an async `OnChange` callback when the IP is first detected or changes.
- `Get()` returns `NAS_LAN_IP` env var if set, else the auto-detected value (explicit operator config wins).
- `GlobalLanIP` is the package-level singleton injected by `main.go`.

**`main.go` — middleware wiring:**
- Global Gin middleware calls `camera.GlobalLanIP.UpdateFromHost(c.Request.Host)` on every request (zero overhead on the hot path — the detector short-circuits when the IP hasn't changed).
- `SetOnChange` callback pushes updated WebRTC candidates to Frigate via `frigate.SetWebRTCCandidates(ctx, ipv6Addr)`, honoring `NAS_IPV6_DISABLED` to blank the IPv6 address.

**`registry.go` — resilience:**
- ONVIF `profile_token` discovery retry: if registration can't obtain a token, a background loop retries every 30s for up to 10 minutes.
- Frigate config push retry: up to 3 retries with 2s/4s backoff on register/unregister.
- go2rtc `#stop=` parameter made configurable via `StopTimeout` (default 30) to avoid stream-reconnect gaps on API restart.

**`health.go` — debounce:**
- Camera status change transitions debounced to avoid false-offline from transient network jitter.

**`ipv6.go` — disable switch:**
- `NAS_IPV6_DISABLED` env var (true/1/yes) suppresses IPv6 probing/reporting for LAN-only environments without public IPv6.

#### Android — Configurable LAN URL

**`BaseUrlResolver.kt`:**
- `setCustomLanUrl(url)` / `getCustomLanUrl()` persist a user-configured LAN URL to SharedPreferences (`custom_lan_url`), overriding the hardcoded `LAN_URL`/`LAN_HOST`/`LAN_PORT`.
- `applyCustomLanUrl` parses the URL (host + port, default 8088); null/blank reverts to the hardcoded default.
- Saving forces a re-probe so the new URL takes effect within ~1-2s.

**`SettingsFragment.kt` — new "局域网地址" section:**
- Shows the currently effective LAN URL, a URL input field, and Save / Reset-to-default buttons.
- Save validates `http://`/`https://` prefix, calls `setCustomLanUrl`, and shows a toast; Reset clears the custom URL and restores the default.

**Workflow after NAS IP change:** enter the new address in Settings → 局域网地址 → Save. The app re-probes and switches within seconds; the backend auto-detects the new LAN IP from the first request and pushes updated WebRTC candidates to Frigate — no recompile or manual `NAS_LAN_IP` needed.

### Files Changed (Phase 18)

| File | Change |
|------|--------|
| `services/api/internal/camera/frigate.go` | `lanIPDetector` + `GlobalLanIP` + `UpdateFromHost`/`Get`/`SetOnChange` |
| `services/api/cmd/main.go` | Global Host-header middleware + LAN IP change callback → WebRTC candidates push |
| `services/api/internal/camera/registry.go` | profile_token retry, config push retry, configurable `#stop=` |
| `services/api/internal/camera/health.go` | Status change debounce |
| `services/api/internal/network/ipv6.go` | `NAS_IPV6_DISABLED` support |
| `compose.yaml` / `.env.example` | `NAS_LAN_IP` / `NAS_IPV6_DISABLED` env vars |
| Android `BaseUrlResolver.kt` | Custom LAN URL persistence + re-probe |
| Android `SettingsFragment.kt` + `fragment_settings.xml` | LAN URL config UI |
| `deploy-nas.ps1` | NAS host updated to 192.168.31.235 |

---

## Phase 20 (v1.8.26): Hardware Transcode + Cache Cleanup + Error Dedup + Per-Route Timeout + SSH Toolbox

### Phase 20 span: v1.8.26 (Backend) + ssh-nas.ps1 (new)

#### Motivation: land all four optimization recommendations from v1.8.25

v1.8.25 made web playback work but left four follow-ups on the table: (1) software libx264 transcode was ~40s per 60s clip on the J4125; (2) the `.transcode-cache` directory grew unbounded; (3) a retry-loop page could flood SystemLog with duplicate `client.error` rows; (4) raising the global `WriteTimeout` to 120s widened the slow-write window for every route. This phase lands all four plus a convenience SSH toolbox.

#### Backend changes

- **VAAPI hardware transcode** (`camera_handler.go`): `buildTranscodeCmd` gains a hardware pipeline — `-vaapi_device /dev/dri/renderD128 -hwaccel vaapi -hwaccel_output_format vaapi -c:v h264_vaapi -qp 24`. `vaapiAvailable()` defensively stats `/dev/dri/renderD128` (char device) and falls back to software `libx264` if absent, so playback never breaks. `Dockerfile` installs `intel-media-driver` + `libva`; `compose.yaml` passes the device in (`devices: /dev/dri/renderD128`) and adds `group_add: "105"` (render group GID on the NAS) so the container's UID-1000 app user can open the node. Measured: 60s clip ~40s CPU → ~3s iGPU.
- **Cache auto-cleanup** (`camera_handler.go` `StartCacheCleaner` + `cleanTranscodeCache`): background goroutine sweeps `.transcode-cache` every 6h, deleting clips older than 7 days; also runs once at startup so an in-place upgrade immediately clears stale files. Logs how many files/bytes were removed.
- **Client error dedup/aggregation** (`client_error_handler.go`): reports are now deduplicated by `(context, message)` within a 10-minute window — the first report creates a row, repeats bump a `count` on the existing row (payload JSON gains `count`). Global rate limit stays 60/min.
- **Per-route write timeout** (`main.go`): global `WriteTimeout` restored to 15s; `PlayRecording` alone extends its own deadline to 120s via `http.NewResponseController(c.Writer).SetWriteDeadline(...)` (gin's ResponseWriter unwraps). Slow-loris stays bounded by ReadTimeout/ReadHeaderTimeout.
- **`ssh-nas.ps1`** (new): interactive menu-driven NAS toolbox — service status, live logs, container list, disk usage, transcode-cache size, cache cleanup (N days), health check, arbitrary command. Reuses deploy-nas.ps1's host/path config; supports password or SSH-key auth.

#### NAS verification (192.168.31.235, 2026-08-14)

- `vainfo` inside the api container: J4125 render node reachable, `h264_vaapi` encoder listed.
- Hardware transcode of a real 60s minute: ~3s, output ffprobe `codec_name=h264`.
- Cache cleanup: seeded an expired clip, ran the sweep, file removed and count logged.
- Error dedup: repeated identical reports → single SystemLog row with incremented count.
- Timeout: playback route returns within its 120s budget; other routes keep the 15s cap.

### Files Changed (Phase 20)

| File | Change |
|------|--------|
| `services/api/internal/handler/camera_handler.go` | VAAPI pipeline + `vaapiAvailable()` + cache cleaner |
| `services/api/cmd/main.go` | StartCacheCleaner; WriteTimeout 120s→15s global + per-route 120s |
| `services/api/internal/handler/client_error_handler.go` | Dedup/aggregation by (context, message) + count |
| `services/api/Dockerfile` | Install `intel-media-driver` + `libva` |
| `compose.yaml` | Pass `/dev/dri/renderD128` + `group_add: 105` |
| `ssh-nas.ps1` | New — interactive NAS SSH toolbox |

---

## Phase 21 (v1.8.27): Persistent-Operation Hardening — Frigate DB Persistence + SQLite Maintenance + Log Rotation + Disk Alerts + Release Cleanup

### Phase 21 span: v1.8.27 (Backend) + v1.8.27 (compose)

#### Problem: long-running deployment slowly accumulated cruft and could lose state

A healthy box that runs for months should not slowly grow WAL files, accumulated APKs, unbounded container logs, and — worst of all — silently lose Frigate's event database on a container recreate. Audit found: Frigate's `frigate.db` lived in the container's writable layer (lost on every rebuild), `app.db-wal` had grown to 17.4MB while `app.db` was ~700KB, `mosquitto` logs had reached 84MB and `frigate` 44MB with no cap, `data/releases` held 5.1GB of APKs when the in-app updater only ever needs the newest, and there was no disk-space alert to catch a filling disk before Frigate silently stopped recording.

#### Fixes

- **Frigate `/config` persistence** (`compose.yaml`): mount `./data/frigate/config:/config` so `frigate.db`, `backup.db`, `.jwt_secret`, `model_cache/` survive container recreate/upgrade. The repo's `config.yml` is still overlaid as a single-file mount on top, so config pushes work as before. Recordings already survived via `/media/frigate`; now the event timeline/search index does too.
- **SQLite maintenance** (`internal/maintenance/sqlite.go`): periodic `PRAGMA wal_checkpoint(TRUNCATE)` (default every 6h, plus once at startup) folds the WAL back into `app.db` and truncates it to ~0; daily `VACUUM INTO` snapshot to `<db dir>/backups` retained to newest 7. `VACUUM INTO` is safe against a live WAL DB — consistent, compacted, non-blocking.
- **Disk-space monitor** (`internal/maintenance/disk.go`): samples the data filesystem every 10m; on warn (80%) / crit (90%) crossing it writes a `event_type=system.disk` SystemLog row and publishes a live `system.log` event so the dashboard sees the alert before recordings die. Edge-triggered — only logs on transitions.
- **Docker log rotation** (`compose.yaml`): global `json-file` driver with `max-size: 10m` + `max-file: 3` applied to every service via an anchor, so chatty logs can never fill the disk.
- **Release cleanup** (`release_handler.go`): `CleanupOldReleases(5)` runs at startup and daily, deleting all but the newest 5 APKs (strictly matched `app-debug-vX.Y.Z.apk/**`) plus sibling release-notes. Same run shrank `data/releases` from ~5.1GB to ~428MB.
- **Startup wiring** (`main.go`): `maintenance.StartAll(...)` after DB/EventBus ready; release cleanup goroutine.

#### NAS verification (192.168.31.235, e2e)

- `GET /health` → `{"status":"ok"}`; `maintenance: background loops started` + `maintenance: sqlite WAL checkpointed (TRUNCATE)` in api logs.
- `data/frigate/config/` on host now holds `frigate.db` (3.3MB), `backup.db`, `.jwt_secret`, `model_cache/` — persisted across container lifecycle.
- `docker inspect` confirms every service LogConfig = `{"Type":"json-file","Config":{"max-file":"3","max-size":"10m"}}`.
- `data/releases/` holds exactly 5 newest APKs (v1.7.23…v1.8.24); disk `/vol1` at 12% (below warn threshold, so no alert is expected).
- `backups/` dir writable by app user; `VACUUM INTO` mechanism verified with the same `glebarez/sqlite` driver (`BACKUP_OK size=143360`). First daily backup lands 24h post-deploy.

### Files Changed (Phase 21)

| File | Change |
|------|--------|
| `services/api/internal/maintenance/sqlite.go` | New — WAL checkpoint + daily `VACUUM INTO` backup + prune |
| `services/api/internal/maintenance/disk.go` | New — disk-space monitor (warn/crit SystemLog alerts) |
| `services/api/internal/maintenance/disk_linux.go` / `disk_other.go` | New — platform disk-usage impl (statfs / stub) |
| `services/api/internal/maintenance/maintenance.go` | New — `StartAll` bootstraps both loops |
| `services/api/internal/config/config.go` | `MaintenanceConfig` section |
| `services/api/configs/config.yaml` | `maintenance:` block (checkpoint 6h, backup 24h keep 7, warn 80 / crit 90) |
| `services/api/cmd/main.go` | Start maintenance loops; release cleanup goroutine |
| `services/api/internal/handler/release_handler.go` | `CleanupOldReleases(keep)` |
| `compose.yaml` | Frigate `/config` volume; global log rotation; releases `:ro`→`rw` |

---

## Phase 22 (v1.8.28): Recording / Service / Resource Monitoring + Healthchecks

### Phase 22 span: v1.8.28 (Backend) + v1.8.28 (compose)

#### Problem: cameras can be "online" while recording silently stops, and a dead sibling service was invisible

The existing camera health check is only an RTSP TCP probe — it reports "online" when the RTSP port accepts a connection, but says nothing about whether Frigate's recording pipeline is actually writing files. A failed FFmpeg decode, a detached stream, or a broken record pipeline leaves the camera "online" while recordings silently stop, and the operator only discovers it when replaying a missing minute days later. Likewise, the web front-end and MQTT broker could die without anything surfacing that to the dashboard. This phase adds active recording-health, service-liveness, recording-size, and CPU/memory monitors, plus compose healthchecks for every service.

#### Fixes

- **Recording health monitor** (`internal/maintenance/recording.go`): every N minutes, for each camera expected to record (online + recording not disabled), walks the recordings tree for that camera's slug and checks the newest segment's mtime. If it's older than `stale_after` (default 10m), writes a `event_type=system.recording` alert and marks the camera stalled; edge-triggered so it only fires on transitions, and self-heals when segments resume.
  - Offline cameras are excluded from targets (they legitimately write nothing; `camera.offline` covers them) — this fixed a false-positive where an offline camera in the stale set triggered a `system.recording` alert.
- **Service liveness monitor** (`internal/maintenance/service.go`): probes sibling services (web front-end over HTTP `http://web/`, mosquitto over TCP `mosquitto:1883`) every 30s; after 3 consecutive failures writes a `event_type=system.service` critical alert. Edge-triggered on down/up transitions.
- **Recording size monitor** (`internal/maintenance/recordings_size.go`): walks the recordings tree and reports its total size, alerting on warn/crit byte thresholds (edge-triggered). Frigate retains by TIME, not size, so a 1080p farm can fill the disk before the generic 80%/90% filesystem alert fires — this surfaces recording growth specifically.
- **CPU/memory monitor** (`internal/maintenance/sysres.go` + `sysres_linux.go`/`sysres_other.go`): samples `/proc/stat` + `/proc/meminfo` (Linux) and alerts on warn/crit thresholds for CPU and memory. Cross-platform via build tags.
- **Compose healthchecks** (`compose.yaml`): every service now has a `healthcheck` (api probes `/health`, web probes nginx root, mosquitto `pgrep`, frigate probes `:5000/api/config`), so `docker ps` and `depends_on` react to a dead container instead of a merely-started one.
  - web healthcheck uses `http://127.0.0.1/` NOT `localhost`: nginx:alpine's `/etc/hosts` maps `::1 localhost ip6-loopback`, and busybox wget resolves `localhost` to IPv6 `::1` first while nginx only listens on IPv4 `0.0.0.0:80` — the probe was `Connection refused` against a healthy server. Switched to `127.0.0.1` fixed it.
- **Startup wiring** (`main.go`): `recordingTargets` closure resolves expected-to-record cameras from the registry each tick (skips offline + recording-disabled); `maintenance.StartAll` gains `RecordingsRoot`, `RecordingStaleAfter`, `ServiceProbes`, `SysResource`, and recording-size config.

#### NAS verification (192.168.31.235, e2e)

- `docker compose ps`: all five services `(healthy)` — api, web, mosquitto, frigate healthy; cloudflared runs without a healthcheck (no documented endpoint).
- `GET /health` → `{"status":"ok"}`; api logs show `maintenance: background loops started` + `maintenance: sqlite WAL checkpointed (TRUNCATE)`.
- No `system.recording` false positives after the offline-camera exclusion; SystemLog back to normal event stream (login/motion).
- Recordings root `/media/frigate/recordings/2026-08-13` present; SQLite WAL active (`app.db-wal` folding normally).

### Files Changed (Phase 22)

| File | Change |
|------|--------|
| `services/api/internal/maintenance/recording.go` | New — recording-health monitor (stale segment detection) |
| `services/api/internal/maintenance/service.go` | New — sibling-service liveness probe + alert |
| `services/api/internal/maintenance/recordings_size.go` | New — recordings-tree size monitor |
| `services/api/internal/maintenance/sysres.go` / `sysres_linux.go` / `sysres_other.go` | New — CPU/memory monitor (cross-platform) |
| `services/api/internal/maintenance/disk.go` | (existing) unchanged |
| `services/api/internal/maintenance/maintenance.go` | `Config` + `StartAll` gains recording/service/resource loops + frigate.db backup |
| `services/api/internal/config/config.go` | `MaintenanceConfig` recording/service/resource fields |
| `services/api/configs/config.yaml` | `maintenance:` recording + service + sysresource blocks |
| `services/api/cmd/main.go` | `recordingTargets` closure (skip offline/disabled); `FrigateDBPath`; wire new config |
| `compose.yaml` | healthcheck for api/web/mosquitto/frigate; web probe `127.0.0.1` |

---

## Phase 23 (v1.8.29): Off-NAS Offsite Backup — Bitiful (亿安云) S3

### Phase 23 span: v1.8.29 (compose) + deploy/backup

#### Problem: the maintenance backups sit on the same disk as the live DB

v1.8.27 made daily SQLite snapshots (app + frigate) to `data/sqlite/backups`, but those live on the same NAS disk as the live DB. A disk failure silently destroys both the live database and its only copies. With no spare NAS and no cloud storage, the "second copy" that survives a disk failure was missing.

#### Solution: an egress-only `backup` container mirroring the snapshots to Bitiful via rclone

- **`deploy/backup/entrypoint.sh`**: an infinite `rclone sync` loop (not `copy`) that mirrors `data/sqlite/backups` (`/backups`, mounted read-only) to `s3://<bucket>/home-datacenter/sqlite`. `sync` mirrors the local dir — locally-pruned backups are also deleted remotely, keeping the bucket bounded. rclone is configured entirely via CLI flags (no committed `rclone.conf`), and credentials come from `.env` via the compose `environment:` block, so nothing secret is in the repo.
- **`compose.yaml` backup service**: `rclone/rclone:1.68`, no published ports (egress-only), `cap_drop: ALL` + `no-new-privileges`, standard log rotation, read-only source mount. `restart: unless-stopped`; if credentials are empty the entrypoint exits 1 and the container stays "exited" (so disabling is just leaving the keys blank — no separate toggle).
- **`.env` / `.env.example`**: `BITIFUL_ENDPOINT` (https://s3.bitiful.net), `BITIFUL_REGION` (cn-east-1), `BITIFUL_BUCKET` (nas-data), `BITIFUL_ACCESS_KEY`, `BITIFUL_SECRET_KEY`, `BITIFUL_SYNC_INTERVAL` (default 21600s = 6h). Leaving the keys empty disables the backup container.

#### NAS verification (192.168.31.235, e2e)

- Appended the BITIFUL block to the NAS's `.env` (the deploy tarball excludes `.env`, so the NAS copy is updated in place).
- `docker compose up -d backup` pulled `rclone/rclone:1.68` and started `home-backup`; logs show `rclone backup loop started (interval 21600s, source /backups, dest s3://nas-data/home-datacenter/sqlite)`.
- First sync on an empty backups dir: `There was nothing to transfer` (S3 auth + bucket access confirmed).
- Placed a real `app-20260814-110103.db` (700 KiB) snapshot in the backups dir, `docker restart home-backup` → log `app-20260814-110103.db: Copied (new)`, `Transferred: 1 / 1`, `sync OK`.
- Read-back via `rclone lsl` from inside the container: `716800 2026-08-14 11:01:03 app-20260814-110103.db` — object confirmed present in the Bitiful bucket (byte count matches the local snapshot).

### Files Changed (Phase 23)

| File | Change |
|------|--------|
| `compose.yaml` | New `backup` service (rclone/rclone:1.68, egress-only, read-only source mount) |
| `deploy/backup/entrypoint.sh` | New — infinite `rclone sync` loop to Bitiful bucket |
| `.env.example` | New `BITIFUL_*` block documenting the offsite backup config |
| `.env` | (local, gitignored) BITIFUL credentials |

---

## Phase 24 (v1.8.30): Restore Drill + Backup Health & Retention Monitoring

### Phase 24 span: v1.8.30 (Backend + compose + scripts)

#### Problem: the off-NAS backup was invisible and unverifiable

v1.8.29 shipped the Bitiful sync, but three gaps remained: (1) there was no way to prove the bucket copy was actually recoverable without hand-running rclone; (2) a failed or stalled sync only landed in the backup container's logs — invisible to the dashboard; (3) the bucket could silently outgrow the free tier (50 GB) with no signal.

#### Design: a shared status file bridging the two containers

The backup container now writes an atomic JSON status after every sync; the API's new `BackupMonitor` reads it (read-only) and raises alerts. This keeps the backup container egress-only (it never calls the API) and the API a passive reader (it never writes the state the backup loop owns).

- **`deploy/backup/entrypoint.sh`**:
  - After each `rclone sync`, runs `rclone size` on the remote and writes `data/backup-state/last.json` (temp+rename for atomicity): `{"ts":..., "ok":true|false, "error":"...", "remote_files":N, "remote_bytes":N}`.
  - `ok` is a JSON boolean (the API unmarshals into a Go `bool`). `remote_bytes` is parsed from the parenthesized exact byte count of rclone's `Total size: 700 KiB (716800 Byte)` line.
  - `ONCE=1` runs a single sync + status write then exits (used by the restore drill / manual triggers).
- **`compose.yaml`**:
  - `backup` gains a writable `./data/backup-state:/state` mount.
  - `api` gains a read-only `./data/backup-state:/data/backup-state:ro` mount.
- **`internal/maintenance/backup.go`** — new `BackupMonitor` (edge-triggered, matching the other monitors):
  - **Failure/staleness** (`system.backup`, critical): when the state says `ok:false`, or the file is older than `backup_stale_after_minutes` (the backup loop stopped writing), or a previously-healthy state file disappears.
  - **Retention** (`system.backup`): when `remote_files` / `remote_bytes` cross `backup_warn_files` / `backup_crit_files` / `backup_warn_bytes` / `backup_crit_bytes` (0 = disabled).
  - Writes `SystemLog` + re-publishes on `system.log`, so the dashboard WS hub broadcasts it live.
- **`internal/config/config.go` / `configs/config.yaml` / `cmd/main.go`**: new `backup_state_path`, `backup_monitor_interval_minutes` (5), `backup_stale_after_minutes` (720 = 2× the 6h sync), and the four retention thresholds.
- **`scripts/restore-dry-run.sh`** — new restore-drill script: lists the bucket via the backup container, pulls the newest `app-*.db`, copies it out of the container, runs `PRAGMA integrity_check` + key-table counts via python3, prints `RESTORE OK`, and cleans up. Run on the NAS (`sh scripts/restore-dry-run.sh`) or remotely (`NAS=host sh ...`).

#### NAS verification (192.168.31.235, e2e)

- Backup container restart writes a correct state file: `{"ok":true,"remote_files":1,"remote_bytes":716800}` (byte count matches the verified snapshot).
- Failure alert: wrote `ok:false` → api emitted `system.backup critical 异地备份失败：rclone sync failed`; row persisted in `system_logs` with full payload.
- Recovery: restored `ok:true` → api re-check produced no new alert (edge-triggered self-heal).
- Retention alert: with `backup_warn_files:1` (bucket has 1 object) → api emitted `system.backup warning 异地备份容量告警（warning）：bucket 内 1 个对象 / 700.0 KiB`. Thresholds then restored to 0 (disabled).
- Restore drill: `sh scripts/restore-dry-run.sh` → pulled `app-20260814-110103.db` (716800 B), `integrity: ok`, `users: 4 cameras: 2`, `RESTORE OK`.
- Test rows cleaned up; production state healthy (`ok:true`, 0 residual `system.backup` rows).

### Files Changed (Phase 24)

| File | Change |
|------|--------|
| `deploy/backup/entrypoint.sh` | Write atomic `last.json` status after each sync; `rclone size` object/byte reporting; `ONCE=1` single-run mode |
| `compose.yaml` | `backup` gains writable `/state`; `api` gains read-only `/data/backup-state:ro` |
| `services/api/internal/maintenance/backup.go` | New — `BackupMonitor` (failure/staleness + retention alerts) |
| `services/api/internal/config/config.go` | New `BackupStatePath` + backup monitor/threshold fields |
| `services/api/configs/config.yaml` | New `backup_state_path` / monitor / threshold config |
| `services/api/cmd/main.go` | Wire `BackupMonitor` into `maintenance.StartAll` |
| `scripts/restore-dry-run.sh` | New — off-NAS restore drill (pull + integrity check + cleanup) |

---

## Phase 25 (v1.8.31): Fix Android "All 3 attempts failed for /api/v1/user" 502 Root Cause

### Phase 25 span: v1.8.31 (compose only)

#### Problem: every api redeploy broke the nginx proxy → Android 502

The Android app reported "加载失败：All 3 attempts failed for /api/v1/user". Crucially, that message is **only** produced by `RetryInterceptor` after 3 consecutive **5xx** responses — a 403 (admin-only) would be returned immediately without retry (the interceptor explicitly skips 4xx). So this was NOT a permission issue; the backend was returning 502.

Confirmed on the NAS (192.168.31.235):
- `curl :8088/api/v1/user` → **502 Bad Gateway** (nginx upstream failure).
- api container `:8080/health` direct → 200; from inside `home-web`, `wget api:8080/health` → 200.
- Diagnosis: nginx's `upstream api_backend { server api:8080; }` resolves the `api` hostname **once at nginx startup** and caches that IP. The api container had been recreated 42 min earlier (new IP), while `home-web` had been up 3 hours with a stale cached IP — so every `/api/*` request proxied to a dead address.

#### Design: pin the api container to a static IP in an explicit subnet

- `compose.yaml` `home-net` now declares an explicit `172.18.0.0/24` subnet (required for static assignment).
- `api` service pins `ipv4_address: 172.18.0.10`. Because the IP is stable across container recreates, nginx's one-time upstream resolution never goes stale — no web restart needed after future api redeploys.
- The network (and all containers) are recreated once on roll-out; named access to other services (`home-frigate`, `mosquitto`, etc.) is unaffected since they are reached by DNS name.

#### NAS verification (192.168.31.235, e2e)

- After roll-out all containers healthy; `home-api` IP = `172.18.0.10`.
- `GET /api/v1/user` via nginx → 200, `users` count 4.
- Self-heal proof: `docker compose up -d --no-deps --force-recreate api` (recreate api, **leave web up**) → api still `172.18.0.10`, `home-web` not restarted (StartedAt unchanged) → `/api/v1/user` via nginx still 200. Future api recreates will no longer trigger the 502.

### Files Changed (Phase 25)

| File | Change |
|------|--------|
| `compose.yaml` | Declare `home-net` `/24` subnet; pin `api` to static `172.18.0.10` |

---

## Phase 26 (v1.8.32): Recording Quota → Auto-Shorten Retention + WebRTC Candidate Restart-Aware Push

### Phase 26 span: v1.8.32 (Backend)

#### Problem 1: recordings could silently fill a small disk

The existing `RecordingSizeMonitor` only emitted size alerts (warn/crit) — nothing automated. On a 466G data volume, continuous 24/7 recording could creep toward full, and the operator would only learn from a disk alert after the fact.

#### Design 1: quota edge-triggered retention adjustment

- `RecordingSizeMonitor` gains a **quota** threshold (`recording_quota_bytes`). When the recordings tree total crosses it, `OnQuotaExceeded` fires; when it drops back below, `OnQuotaRecovered` fires. Edge-triggered (no repeated alerts). Default 0 = disabled (still size-alerted only).
- `FrigateClient.PushRecordRetention(ctx, days)` — a partial `record.continuous.days` / `record.motion.days` update via `PUT /api/config/set` with `requires_restart=0`. No restart needed: Frigate's periodic cleanup job deletes footage beyond the new window on its next run. Deep-merge preserves everything else.
- `FrigateClient` retention is now configurable (`SetRetentionDays`) instead of hardcoded 7; `PushConfig` uses it. `main.go` sets it from `recording_retention_days` (default 7) and wires quota callbacks to push `recording_reduced_retention_days` (default 3) on exceed / `recording_retention_days` on recover.
- On this NAS: quota = 300 GiB (322122547200, ≈65% of the 466G volume), normal retention 7d, reduced 3d.

#### Problem 2: WebRTC candidate push raced the Frigate restart it triggered (technical debt)

The old flow pushed `go2rtc.webrtc.candidates` **after** `pushFrigateConfig` (which restarts Frigate via `requires_restart=true`), so a single-shot push hit "connection refused". A `pushWebRTCCandidatesWithRetry` (wait + retry) was added, but a deeper issue surfaced on deploy: the push returned **400 `No configuration data provided`**.

Root cause: `SetWebRTCCandidates` sent the partial config **bare** (`{go2rtc:{...}}`), but Frigate's `/api/config/set` requires the `{config_data:{...}}` envelope. So **every** candidate push (BootReplay AND the PrefixWatcher fallback) had been silently failing — candidates were never actually updated.

#### Design 2: restart-aware ordering + correct envelope

- `SetWebRTCCandidates` now wraps the payload in the same `{requires_restart:0, update_topic, config_data}` envelope `PushRecordRetention` uses.
- `BootReplay` reordered: **push candidates first**, then the full config push. Because the candidate update is a deep-merge partial that persists to `config.yml` before the restart, the restarted Frigate loads the candidates from the file — no race window at all. `pushWebRTCCandidatesWithRetry` only absorbs the boot-up window (Frigate REST 5000 comes up slower than go2rtc 1984).

#### NAS verification (192.168.31.235)

- api log: `camera: webrtc candidates pushed after boot replay` (previously `400 No configuration data provided`).
- Runtime `config.yml` candidates = `127.0.0.1:8555`, `192.168.31.235:8555`, `[IPv6]:8555`; survive `docker restart home-frigate` (container healthy).
- Frigate retention currently `continuous.days: 7` / `motion.days: 7`; recordings at 738M, far below the 300GiB quota, so the quota action does not fire (expected).
- Quota monitor runs with the maintenance loops, watches `/media/frigate/recordings`, 1h sampling.

#### Notes / follow-ups
- The api's own service monitor printed transient "Web 前端 / Mosquitto 不可达" on the first tick after an api restart (startup DNS race). Containers are `(healthy)`; not a regression from this change.

### Files Changed (Phase 26)

| File | Change |
|------|--------|
| `internal/config/config.go` | Add `RecordingQuotaBytes` / `RecordingRetentionDays` / `RecordingReducedRetentionDays` + defaults |
| `configs/config.yaml` | Set quota 300GiB, retention 7d / reduced 3d |
| `internal/camera/frigate.go` | `retentionDays` field, `SetRetentionDays`, `PushRecordRetention`; fix `SetWebRTCCandidates` `config_data` envelope |
| `internal/camera/registry.go` | Reorder BootReplay (push candidates before config push); `pushWebRTCCandidatesWithRetry` |
| `internal/maintenance/recordings_size.go` | Quota edge detection + `OnQuotaExceeded`/`OnQuotaRecovered` |
| `internal/maintenance/maintenance.go` | Plumb quota config + callbacks |
| `cmd/main.go` | Set retention days; wire quota callbacks to Frigate pushes |

---

## Phase 27 (v1.8.33): Fix Service Monitor "不可达" False Alarms + Restore Silently-Dead MQTT

### Phase 27 span: v1.8.33 (Backend + Frigate config)

#### Symptom
After every api restart, the service monitor fired `服务 Web 前端 不可达` and `服务 Mosquitto 不可达` alerts even though both containers were `(healthy)` in `docker compose ps`.

#### Initial hypothesis (wrong)
A transient DNS "startup race". Added a `ServiceStartupDelay` (60s) grace before the first probe. **The alerts still fired** — proving it was NOT transient.

#### Root cause: wrong hostnames, not a race
compose.yaml sets `container_name: home-web` / `home-mosquitto`. Docker registers the **container name** as the DNS alias on the network; the bare **service name** (`web` / `mosquitto`) is NOT resolvable. The probes used `http://web/` and `mosquitto:1883` → every probe failed with `bad address`.

Same wrong-hostname bug silently disabled the entire MQTT pipeline:
- api `mqtt.broker: tcp://mosquitto:1883` (config.yaml + config.go default) → api never connected to the broker.
- Frigate `mqtt.host: mosquitto` (deploy/frigate/config.yml) → Frigate never published detection events.

Verified from inside the api container: `getent hosts web` fails, `home-web`/`home-mosquitto` resolve and connect fine.

#### Design 2: use container names + keep grace as defense-in-depth
- `ServiceProbes` → `http://home-web/`, `home-mosquitto:1883`.
- Startup grace kept (60s) so a genuine container-restart DNS blip can't trip the 3-failure threshold.
- api MQTT broker → `tcp://home-mosquitto:1883` (config.yaml + config.go default).
- Frigate MQTT host → `home-mosquitto` (deploy/frigate/config.yml; requires `docker restart home-frigate` to apply the bind-mounted change).
- compose.yaml comment corrected.

#### NAS verification (192.168.31.235)
- api log: `mqtt connected to tcp://home-mosquitto:1883` + subscribed all topics incl. `frigate/events`; `service monitor startup grace 1m0s before first probe`; **0** "不可达" alerts in 5 min.
- mosquitto log: `New client connected ... as frigate` and `as home-datacenter` — both api and Frigate now reach the broker (previously MQTT was fully down).
- api `/health` → `{"status":"ok"}`.

#### Notes / follow-ups
- The api's MQTT client shows periodic "connection closed by client" reconnects (keepalive/session). Functionally the subscribe + publish path works; not exercised further in this phase.
- Rule of thumb going forward: on a compose network where services set `container_name`, always reference the **container name** (e.g. `home-api`, `home-frigate`, `home-mosquitto`, `home-web`), never the bare service name.

### Files Changed (Phase 27)

| File | Change |
|------|--------|
| `cmd/main.go` | Probe hostnames `home-web`/`home-mosquitto`; `ServiceStartupDelay: 60s` |
| `internal/maintenance/service.go` | `startupDelay` field + wait before first probe |
| `internal/maintenance/maintenance.go` | `ServiceStartupDelay` config + pass-through |
| `configs/config.yaml` | MQTT broker → `tcp://home-mosquitto:1883` |
| `internal/config/config.go` | Default MQTT broker → `tcp://home-mosquitto:1883` |
| `deploy/frigate/config.yml` | MQTT host → `home-mosquitto` |
| `compose.yaml` | Correct misleading host comment |

---

## Phase 28 (v1.8.34): Recording Quota Action Backoff-Retry

### Phase 28 span: v1.8.34 (Backend)

#### Problem
The recording-quota monitor samples once immediately on start (`Run()` first line), racing the `BootReplay` Frigate restart. When the first sample fires the quota action during that restart, `/api/config/set` returns a transient 400/500/connection-refused. The old code committed the crossed state (`quotaActive=true`) *before* running the action, so a failed push was never retried — the retention reduction silently never happened.

#### Design
- **Callbacks return error**: `OnQuotaExceeded` / `OnQuotaRecovered` now return `error` (`maintenance.go`). The monitor only commits `quotaActive` after the action succeeds (`recordings_size.go`); a failure logs and keeps the previous state so the next sample retries.
- **`pushRetentionWithRetry`** (`cmd/main.go`): 6 attempts, 30s apart (~3min window) to ride out the Frigate restart (~2min). Frigate's own cleanup job applies the new window on its next run.

#### NAS verification (192.168.31.235)
Retry chain observed end-to-end: attempt 1/6 connection refused → 2/6 connection refused → 3/6 500 → 4/6 `record retention set to 3 days`.

### Files Changed (Phase 28)

| File | Change |
|------|--------|
| `cmd/main.go` | `pushRetentionWithRetry`; callbacks wired to it |
| `internal/maintenance/maintenance.go` | `OnQuotaExceeded`/`OnQuotaRecovered` → `func() error` |
| `internal/maintenance/recordings_size.go` | Commit `quotaActive` only after action success |

---

## Phase 29 (v1.8.35): Startup Race — Quota Reduction Clobbered by Full Config Push

### Phase 29 span: v1.8.35 (Backend)

#### Symptom (found while verifying the quota trigger chain)
With quota set low (100M) and recordings at ~737M, after a clean api restart the retention was reduced to 3 days, but a **subsequent full-config push set it back to 7 days** — and because `quotaActive=true` was already committed, the monitor never re-reduced it. The recordings stayed over quota with retention stuck at 7 days.

#### Root cause
At api boot, `RecordingSizeMonitor.Run()` samples immediately (goroutine) while `BootReplay` pushes the full config (`requires_restart=1`) (main goroutine). Whichever push lands last wins. If the quota action (3 days) lands first, the full config push reads `c.retentionDays` — still the boot-time normal value (7) — and overwrites it back to 7. The quota action "succeeded", so `quotaActive=true` and no retry fires.

#### Design: make the current retention a single shared authority
- `PushRecordRetention` writes the target days back to `c.retentionDays` under a new `retentionMu` lock (`internal/camera/frigate.go`).
- `PushConfig` snapshots `c.retentionDays` under the same lock.
- Now both goroutine orderings converge to the reduced retention (3 days) — the full config push picks up the quota-adjusted value instead of the stale normal value.

#### NAS verification (192.168.31.235, 2026-08-14)
- Quota 100M + recreate api: full retry chain (attempt 1/2/3 transient failures → attempt 4 success); read-back `/api/config` = `continuous.days=3.0`, `motion.days=3.0` (previously clobbered back to 7.0).
- Restore quota 300GiB + recreate api: retention back to `continuous.days=7.0`, `motion.days=7.0`.

### Files Changed (Phase 29)

| File | Change |
|------|--------|
| `internal/camera/frigate.go` | `retentionMu` lock; `PushRecordRetention` writes `c.retentionDays`; `PushConfig` snapshots under lock; `CurrentRetention()` helper |

---

## Phase 30 (v1.8.36): Quota Alert → warning SystemLog + Android Admin-Only Amber Banner

### Phase 30 span: v1.8.36 (Backend) + v1.8.36 (Android)

#### Symptom (gap found while verifying the quota chain)
Activating the quota previously only shortened Frigate's retention (`PushRecordRetention`) — it **never published a `system.recordings_size` event**. And the quota's own alert thresholds (`recording_size_warn_bytes` / `recording_size_crit_bytes`) are 0 (disabled). Net effect: when recordings exceed quota, the frontend (Dashboard / Android) had **no signal at all** — retention was silently shortened and the operator never learned about it.

#### Solution
- **Backend** (`internal/maintenance/recordings_size.go`): in the quota edge-detection block, after a successful `onExceed()` publish a `system.recordings_size` event with `level=warning` (message "录像配额已用尽…已自动缩短录像保留天数"); after a successful `onRecover()` publish `level=normal` (dismiss signal). New `emitQuotaEvent` helper persists the SystemLog and publishes on the EventBus; the size/retention payload is set on the event.
- **Backend level normalization**: disk (`disk.go`), backup capacity (`backup.go`), and CPU/memory (`sysres.go`) warn tiers all changed from `LevelNormal` to `LevelWarning`; `model.SystemLog.LevelWarning` added (`internal/model/system_log.go`).
- **Android** (v1.8.36 / versionCode 122): `SystemLogLevel.WARNING`; `RecentLogAdapter` + `ServiceLogAdapter` render amber for warning; `DashboardFragment` `handleQuotaAlert` shows a dedicated `quotaAlertBanner` (amber, tapping opens the logs tab), dismisses on a `level=normal` recovery event. The banner is only reachable from the **admin-gated** `system.log` WebSocket branch, so non-admin users never see it.

#### NAS verification (192.168.31.235, 2026-08-14)
- Temporarily lowered quota to 10M and recreated api (recordings ~737M > 10M): database row `id=4331 level=warning type=system.recordings_size msg=录像配额已用尽 736.9 MiB（已自动缩短录像保留天数）`.
- Restored quota 300GiB + recreated api: api healthy, Frigate retention back to 7 days.
- Android `app-debug-v1.8.36.apk` built and pushed to `data/releases`; `/api/v1/release/latest` serves the new version.

### Files Changed (Phase 30)

| File | Change |
|------|--------|
| `internal/model/system_log.go` | Added `LevelWarning = "warning"` |
| `internal/maintenance/recordings_size.go` | Quota exceed/recover now publish `system.recordings_size` events; `emitQuotaEvent` helper |
| `internal/maintenance/disk.go` | warn tier `LevelNormal` → `LevelWarning` |
| `internal/maintenance/backup.go` | warn tier `LevelNormal` → `LevelWarning` |
| `internal/maintenance/sysres.go` | warn tier `LevelNormal` → `LevelWarning` |
| `tools/qlog/` | Go SQLite query tool used to verify log levels on NAS |
| `Android/.../data/model/SystemLog.kt` | `SystemLogLevel.WARNING` |
| `Android/.../ui/dashboard/RecentLogAdapter.kt` | amber tint for warning |
| `Android/.../ui/logs/ServiceLogAdapter.kt` | amber tint for warning |
| `Android/.../ui/dashboard/DashboardFragment.kt` | admin-gated `handleQuotaAlert` + banner logic |
| `Android/.../res/layout/fragment_dashboard.xml` | `quotaAlertBanner` layout |

---

## Phase 19 (v1.8.25): Web Playback Fix + Client Error Reporting + Transcode Cache

### Phase 19 span: v1.8.25 (Backend) + v1.8.25 (Web)

#### Problem: Web monitoring playback was broken

Frigate records Hikvision cameras' native RTSP with `-c:v copy`, so the stored 10s segments are **HEVC/H.265 + PCMA**. Chromium's HTML5 `<video>` has no HEVC decoder, so a raw stream-copy serve shows a black screen / fires `onError`. Compounding it, the on-the-fly transcode took ~40s but `http.Server.WriteTimeout` was 15s — the server dropped the connection mid-transcode before sending any bytes, so even the transcode path appeared broken.

#### Backend fixes (`camera_handler.go`, `main.go`, `client_error_handler.go`)

- **`PlayRecording` → `transcodeRecording`**: always transcode the minute's segments to H.264/AAC via ffmpeg (`libx264 veryfast + crf23 + aac 96k + faststart + genpts`). Removed the single-segment stream-copy fast path (it too served raw HEVC). Any browser/player can now decode the result.
- **WriteTimeout 15s → 120s** (`main.go`): the deadline no longer fires during a ~40s software transcode. Slow-loris remains bounded by ReadTimeout/ReadHeaderTimeout.
- **Disk cache** (`/data/recordings/.transcode-cache/<camID>/<minuteStart>.mp4`): transcode once per past minute, then serve instantly on re-play. Keying by `minuteStart` bounds cache size to one clip per viewed camera-minute. The ffmpeg output temp file is created **inside** the cache dir so the atomic `os.Rename` stays on one filesystem (avoids `invalid cross-device link`). Only "closed" minutes (minuteStart > 90s in the past) are promoted to cache to avoid caching a still-writing clip.
- **`POST /api/v1/system/client-errors`** (`client_error_handler.go`): ingests fire-and-forget reports from the web UI (JS exceptions, unhandled rejections, failed media playback) and persists them as `event_type=client.error` SystemLog rows with stack/url/context. Global rate limit 60/min to stop a retry-loop page from flooding SQLite.
- **`Dockerfile`**: installs `ffmpeg` + `tzdata` (Alpine bundles libx264/aac encoders).

#### Web fixes (`main.tsx`, `RecordingTimeline.tsx`, `lib/errorReport.ts`)

- `window.onerror` + `unhandledrejection` listeners forward uncaught errors to the backend.
- `<video>` onError reports `recording.playback` with the MediaError code and URL.
- `errorReport.ts` is a defensive sender: local 2s rate limit, swallows all errors (reporting must never break the app), auto-attaches the JWT.

#### NAS verification (192.168.31.235, e2e re-run)

- First `GET /cameras/10/recordings/1786617960/file`: HTTP 200, 52,412,674 bytes, 39.6s (transcode). ffprobe: `codec_name=h264, profile=High, 2560x1440`.
- Second request, same recId: HTTP 200, 0.075s, byte-identical (cache hit).
- `POST /system/client-errors` → `{"accepted":true,"id":4297}`; `GET /system/logs?event_type=client.error` returns the persisted row.

### Files Changed (Phase 19)

| File | Change |
|------|--------|
| `services/api/internal/handler/camera_handler.go` | `PlayRecording` → `transcodeRecording` (H.264 + disk cache) |
| `services/api/cmd/main.go` | WriteTimeout 15s→120s; register `/system/client-errors` |
| `services/api/internal/handler/client_error_handler.go` | New — client error ingest → SystemLog (rate-limited) |
| `services/api/Dockerfile` | Install `ffmpeg` + `tzdata` |
| `web/src/main.tsx` | Global `window.onerror` / `unhandledrejection` reporting |
| `web/src/components/RecordingTimeline.tsx` | `<video>` onError → `recording.playback` report |
| `web/src/lib/errorReport.ts` | New — fire-and-forget error sender |

---

**Last Updated:** 2026-08-14 (v1.8.27: persistent-operation hardening — Frigate /config DB persistence + SQLite WAL checkpoint/daily backup + Docker log rotation + disk-space alerts + old-APK cleanup, e2e verified on NAS. Earlier: v1.8.26: hardware transcode VAAPI + cache auto-cleanup + client error dedup + per-route timeout + ssh-nas.ps1 toolbox, e2e verified on NAS. Earlier: v1.8.25: Web playback fix — H.264 transcode + transcode disk cache + client error reporting, e2e re-verified on NAS. Earlier: v1.8.24: Host IP change self-adaptation — LAN IP auto-detection, configurable Android LAN URL, network robustness. See Phase 19 above. Earlier: v1.8.19: Web splash parallel prefetch + Android WebRTC parallel fallback + splash prefetch. v1.8.18: Camera lifecycle cleanup + web animations. v1.8.17: Liquid glass visual upgrade, security hardening, token rotation, log cleanup, Android theme switch fix. v1.8.9: Android network policy sync. v1.8.8 IPv6 full-path test & dev scripts consolidation. v1.8.7: Network policy review. v1.8.6 / v1.6.29 fix: Dashboard latency card. v1.8.5 IPv6 direct latency optimization. v1.8.4 IPv6 prefix rotation auto-adaptation.)