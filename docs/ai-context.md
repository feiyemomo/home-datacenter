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
| Vision AI | Python 3.11 + OpenCV DNN (YuNet face + SFace + YOLOv8n-pose) |
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
| `GET /api/v1/vision/status` | JWT | Vision AI health, engine readiness, CPU% and gate mode |
| `GET /api/v1/vision/persons` | JWT | List enrolled person identities in face database |
| `POST /api/v1/vision/persons` | JWT | Enroll new person face into face database |
| `DELETE /api/v1/vision/persons/:name` | JWT | Remove person from face database |
| `POST /api/v1/vision/analyze` | JWT | On-demand image analysis (face matching + pose/fall estimation) |
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
│   ├── vision/                  // Phase 11 — Vision AI client & handlers
│   │   ├── client.go            // HTTP client talking to home-vision:8090
│   │   └── handler.go           // /api/v1/vision/* with dynamic CPU gate
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
services/vision/                 // Phase 11 — Edge Vision AI Microservice
├── main.py                      // Threading HTTP server on port 8090
├── face_engine.py               // YuNet detection + SFace matching + faces.json DB
├── pose_engine.py               // YOLOv8n-pose ONNX + 17 keypoints fall analysis
├── download_models.py           // Automated model downloader (YuNet, SFace, YOLOv8n-pose)
├── Dockerfile                   // python:3.11-slim + opencv-python-headless
└── requirements.txt
└── (compose.yaml at project root)

web/                             // React 18 + Vite 5 + Tailwind dashboard SPA
├── README.md                    // Web frontend architecture and developer guide
├── src/
│   ├── pages/{Dashboard,Cameras,DeviceCreate,Login,Logs,MqttDebug,Network,Profile,Users}.tsx
│   │                       // Cameras: live stream (WebRTC/HLS) + timeline playback (MSE)
│   │                       // DeviceCreate: /cameras/new — camera registration wizard
│   │                       // Network: topology, IPv6 direct vs tunnel relay detection
│   │                       // Users: admin CRUD with last-admin/self-delete guards
│   ├── api/{auth,camera,client,device,network,system,user,weather}.ts
│   │                  // client.ts: axios + authedFetch() + authHeaderFor()
│   ├── lib/{network,fmp4Mse,errorReport,utils}.ts
│   │                  // network.ts: unified topology & API path detection
│   │                  // fmp4Mse.ts: MSE fMP4 progressive stream playback engine
│   ├── context/AuthContext.tsx  // /user/me probe, isAdmin, token store
│   ├── hooks/{useAuth,useCachedFetch,useHLSStream,usePrefetch,useTheme,useWebRTCStream,useWebSocket}.ts
│   │            // useCachedFetch: sessionStorage caching, silent refresh & race condition safe
│   │            // useWebSocket: subprotocol auth, heartbeat, persisted subscriptions
│   │            // useHLSStream / useWebRTCStream: streaming video hooks with cleanup
│   └── components/              // Layout (persistent shell), Sidebar, ProtectedRoute, LiveVideo, etc.
├── nginx.conf                   // SPA + /api proxy (keepalive) + /api/v1/ws upgrade + CSP
└── Dockerfile

deploy/
├── mosquitto/{mosquitto.conf,aclfile,passwd}  // broker + ACL + creds
├── frigate/config.yml            // Frigate base config (detectors, mqtt, go2rtc, record retention)
├── cloudflared/config.yml        // dashboard + api + cam hostnames
├── go2rtc/{Dockerfile,go2rtc.yaml} // RTSP→WebRTC/HLS bridge (legacy; now bundled in Frigate)
├── android/                      // Gradle project (v1.8.43+)
│   ├── app/
│   │   └── src/main/java/com/example/homecenter/
│   │       ├── HomeCenterClient.kt      // protocol layer: models, API, WS, NetworkMonitor, error reporter
│   │       ├── HomeCenterService.kt     // foreground service for background WS keepalive
│   │       ├── TokenStore.kt            // token / URL persistence (SharedPreferences)
│   │       └── MainActivity.kt          // binder + service control UI
│   ├── build.gradle.kts, settings.gradle.kts, gradlew, push-apk.ps1, ...
│   └── (legacy single-file `deploy/android/HomeDatacenterClient.kt` removed in v1.8.43)
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

## Active Subsystems & Architecture Contracts

### 1. Camera & Streaming Subsystem (Frigate 0.17 + go2rtc)
- **NVR Engine**: `home-frigate` bundles `go2rtc` (port 1984) and Frigate REST API (port 5000). Standalone `home-go2rtc` container is fully deprecated.
- **RTSP Registration**: Registered via `POST /api/v1/cameras`. Credentials encrypted with `utils.SecretBox` (AES-256-GCM, key = `SHA-256(JWT_SECRET)`). Pushed to Frigate via `PUT /api/config/set`.
- **Live Viewing**: Dual engine support in frontend: WebRTC (sub-second latency via `/go2rtc/api/webrtc`) and HLS (reliable playback via `/go2rtc/api/stream.m3u8`).
- **ONVIF Control**: WS-Security `UsernameToken` with `PasswordDigest` over SOAP (nonce + created timestamp), preventing plaintext credential transmission over LAN.
- **Recording & Playback**: Frigate writes ~10s MP4 segments under `/media/frigate/recordings/YYYY-MM-DD/HH/<cam>/MM.SS.mp4`. `home-api` serves segments directly via `http.ServeFile` with range support and hardware acceleration (`h264_vaapi` via `/dev/dri/renderD128` Intel J4125 iGPU).
- **Recording Quota**: Automated quota manager enforces storage limits (default 400 GiB), automatically shortening Frigate retention policy when storage exceeds threshold.

### 2. Edge Vision AI Subsystem (`home-vision`)
- **Microservice Architecture**: Lightweight Python 3.11 container using OpenCV DNN with pure SSE4.2 / oneDNN (no AVX2 / no PyTorch, tailored for Intel Celeron J4125).
- **Face Recognition**: YuNet face detector + SFace recognizer (cosine similarity >= 0.363) against local face vector database (`./data/vision/faces.json`).
- **Pose & Fall Detection**: YOLOv8n-pose ONNX model extracts 17 keypoints. Geometric fall rule: bounding box aspect ratio W/H > 1.25 and torso inclination angle theta < 35 deg.
- **Dynamic CPU Gating**: Go backend samples host CPU before inference:
  - >= 80%: Circuit breaker activates, skips inference to protect recording/streaming.
  - 60% - 80%: Degrades to face recognition only.
  - < 60%: Full pipeline (face + fall detection). Single-concurrency worker queue prevents thread contention.
- **Event Bus Integration**: Dispatches `camera.person_recognized` and `camera.fall_detected` events, triggering system logs and notifications.

### 3. Event-Driven & Automation Engine
- **Unified Event Model**: `id / type / source / severity / payload / timestamp`.
- **EventBus**: Thread-safe in-process publish/subscribe with wildcard (`*`) matching and worker fan-out.
- **WebSocket Bridge**: `/api/v1/ws` pushes real-time EventBus topics to connected clients (`device`, `camera`, `user.notification`, `system.broadcast`, `automation.fired`).
- **Automation Engine**: Evaluates rules defined as `trigger + condition + action`.
  - **Triggers**: Event topic prefix match.
  - **Conditions**: Time window (`time_gte`/`time_lte` with midnight wrap), `payload_eq`, `source`, `threshold`, `regex` (RE2), `any` (OR logic).
  - **Actions**: `notify`, `mqtt` (restricted to `home-datacenter/` namespace), `webhook` (with SSRF protection rejecting non-public IPs).
  - **Throttling**: `cooldown_s`, `rate_per_min`, and SHA-256 deduplication.

### 4. Multi-Tier Network & Client Routing
- **Topology Awareness**: Clients (Android & Web) adapt between three connection tiers via `BaseUrlResolver`:
  1. **LAN Direct**: `http://192.168.31.235:8088/` (when on same Wi-Fi subnet).
  2. **IPv6 Direct**: `http://nas.feiyemomo.top:8088/` (~50ms TTFB over cellular when IPv6 available).
  3. **Cloudflare Tunnel (WAN)**: `https://api.feiyemomo.top` and `https://dashboard.feiyemomo.top` (zero port-forwarding fallback).
- **Prefix Rotation Resilience**: Host IPv6 changes are dynamically monitored; container environment `NAS_IPV6_ADDRESS` and Frigate candidate sync handle prefix updates.

### 5. Storage, Backup & Quota Maintenance
- **SQLite Persistence**: Database stored in `./data/sqlite/home.db` with WAL mode.
- **Automated Snapshots**: Daily SQLite `.backup` snapshots stored in `./data/sqlite/backups/`.
- **Offsite Cloud Backup (`home-backup`)**: Scheduled `rclone` daemon syncs database backups to Bitiful (亿安云) S3-compatible bucket.
- **Backup Monitor**: Inspects backup status file, triggering system warnings if sync stalls or fails.

### 6. Security & Access Control
- **Authentication**: Offline-issued 256-bit AccessKeys; only SHA-256 hash stored in DB; exchanged for 365-day JWT.
- **Revocation**: Per-request check on `device.RevokedAt` and `device.TokenVersion`. Admin token rotation invalidates prior tokens immediately.
- **Object-Level Authorization (BOLA)**: Camera read permissions (`CanRead`) scoped to owner or shared users. Alert events, snapshots, and thumbnails strictly enforce camera access control.
- **Nginx Reverse Proxy Security**: `/go2rtc/` and `/frigate/` gated by `/api/v1/auth/verify`. Native Frigate dashboard strictly restricted to admin users (`/internal/auth/verify-admin`). CORS headers dynamically restricted to allowed origins (`$cors_origin`).

---

## Document References

- [`README.md`](../README.md) — Quickstart, deployment, configuration, and operations
- [`CHANGELOG.md`](../CHANGELOG.md) — Comprehensive historical release changelog
- [`docs/api-documentation.md`](api-documentation.md) — Complete REST & WebSocket API specification
- [`docs/security.md`](security.md) — Threat model, security audit findings, and hardening history
- [`docs/platformization.md`](platformization.md) — Architectural notes on camera and streaming pipeline

