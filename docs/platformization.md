# Camera Platformization (Phase 4) + Event-Driven System (Phase 5)

> Status: ✅ Implemented — 2026-07-04 (Phase 4), 2026-07-05 (Phase 5)

This document describes the camera module: how a network camera becomes
a first-class **Device** in the Home Datacenter platform, how live
video reaches the browser, and how PTZ control is exposed. It also
covers Phase 5: the Event-Driven architecture that turns all device
state changes into a unified EventBus stream driving WebSocket push,
the Automation Engine, and future AI layers.

The goal of these phases is the pipeline

> RTSP → go2rtc → WebRTC/HLS

fully driven by the API, with credentials stored encrypted at rest,
online/offline state pushed via the EventBus, and admin-only PTZ — plus

> Device / Camera / MQTT → EventBus → {WebSocket, Automation Engine}

so every state change becomes a queryable, automatable event.

---

## Architecture

```
                                 Cloudflare Tunnel (optional)
                                              │
              ┌───────────────────────────────┼──────────────────────────────┐
              │                               │                              │
       dashboard.feiyemomo.top        api.feiyemomo.top          cam.feiyemomo.top
              │                               │                              │
              ▼                               ▼                              ▼
        ┌──────────┐                  ┌──────────────┐                ┌──────────────┐
        │ home-web │ ─── /api ──────► │   home-api   │ ── RTSP push ─►│  home-go2rtc │
        │  nginx   │                  │  Gin + GORM  │                │  :1984       │
        └──────────┘                  └──────┬───────┘                └──────┬───────┘
                                             │                              │
                                        WebSocket                    WebRTC SDP
                                        EventBus                          │
                                             │                              ▼
                                             ▼                       ┌──────────┐
                                       ┌──────────┐                  │ Browser  │
                                       │  App/Web │ ◄───── video ─── │  <video> │
                                       └──────────┘                  └──────────┘
```

* **`home-go2rtc`** is a stateless RTSP-to-WebRTC/HLS bridge. It owns
  no credentials — it only knows stream names (`cam_1`, `cam_2` …)
  and the source URLs the API feeds it.
* **`home-api`** owns the camera registry (`cameras` table), encrypts
  credentials with AES-GCM, pushes stream definitions to go2rtc at
  register/unregister time, and probes each camera's RTSP port on a
  15-second tick to publish `device.status` events.
* **HLS** is the primary viewing path — it works through any
  HTTP-only proxy (Cloudflare Tunnel, nginx) without UDP relays,
  tolerates browser codec gaps via hls.js, and survives flaky
  networks because segments are regular HTTP GETs. **WebRTC** is
  kept as a low-latency fallback (sub-200ms) for LAN viewing.
  Both URLs are returned in `GET /api/v1/cameras/:id` under
  `stream.hls_url` / `stream.webrtc_url`.

---

## Data Model

```sql
CREATE TABLE cameras (
  id            INTEGER PRIMARY KEY,
  type          TEXT    DEFAULT 'camera',     -- reserved: future device types
  name          TEXT    NOT NULL,
  vendor        TEXT,                          -- hikvision / dahua / onvif / ...
  host          TEXT    NOT NULL,              -- 192.168.31.100
  onvif_port    INTEGER DEFAULT 80,
  rtsp_port     INTEGER DEFAULT 554,
  channel_id    INTEGER DEFAULT 1,             -- 101 = main, 201 = sub (Hik)
  status        TEXT    DEFAULT 'unknown',     -- online / offline / unknown
  last_seen_at  DATETIME,
  capabilities  TEXT,                          -- JSON {"ptz":true,"audio":true}
  credentials   TEXT,                          -- JSON {onvif_user, onvif_pass} — both AES-GCM ciphertext
  meta          TEXT,                          -- JSON {onvif_profile, ...}
  stream_name   TEXT UNIQUE,                   -- friendly name; same as go2rtc stream key
  created_at    DATETIME,
  updated_at    DATETIME,
  deleted_at    DATETIME
);
```

The table is migrated by `database.InitDB` alongside `users` and
`devices`. Credentials are encrypted with `utils.SecretBox` whose
32-byte key is `SHA-256(JWT_SECRET)` — reusing the existing root
secret keeps the threat model flat (one secret to rotate, not two).

---

## API

All routes are mounted under `/api/v1/cameras` and require JWT auth.
Mutating routes additionally require `RequireAdmin`.

| Method | Path                | Auth   | Purpose |
|--------|---------------------|--------|---------|
| POST   | `/cameras`          | admin  | Register a new camera |
| GET    | `/cameras`          | any    | List cameras |
| GET    | `/cameras/:id`      | any    | Fetch one camera + live stream URLs |
| DELETE | `/cameras/:id`      | admin  | Unregister (DB + go2rtc) |
| POST   | `/cameras/:id/ptz`  | admin  | Send PTZ command |

### POST /api/v1/cameras

```json
{
  "name": "前门",
  "vendor": "hikvision",
  "host": "192.168.31.100",
  "onvif_port": 80,
  "rtsp_port": 554,
  "channel_id": 101,
  "username": "admin",
  "password": "<plaintext — encrypted before insert>",
  "ptz": true,
  "audio": true,
  "motion": true,
  "profile_token": ""
}
```

Response:

```json
{
  "code": 0,
  "data": {
    "id": 1,
    "type": "camera",
    "name": "前门",
    "vendor": "hikvision",
    "host": "192.168.31.100",
    "status": "unknown",
    "capabilities": {"ptz": true, "audio": true, "motion": true},
    "stream": {
      "stream_name": "前门",
      "webrtc_url":  "http://home-go2rtc:1984/api/webrtc?src=%E5%89%8D%E9%97%A8",
      "hls_url":     "http://home-go2rtc:1984/api/stream.m3u8?src=%E5%89%8D%E9%97%A8"
    }
  }
}
```

* If `profile_token` is empty, the API stores it as `""`. The PTZ
  endpoint will auto-discover it on the first call via ONVIF
  `GetProfiles` and persist the result with `SaveProfileToken` —
  no manual intervention needed (see "ONVIF Profile Discovery"
  below).
* If go2rtc is unreachable, the camera row is rolled back and the
  API returns **502 Bad Gateway**.

### POST /api/v1/cameras/:id/ptz

```json
{ "command": "left", "speed": 0.5 }
```

`command` ∈ {`left`, `right`, `up`, `down`, `stop`, `zoom_in`, `zoom_out`}.
`speed` is clamped to 0..1; default 0.5. `profile_token` is optional
and rarely needed — if omitted, the handler uses the DB-cached token;
if that is also empty, it auto-discovers via ONVIF `GetProfiles` and
persists the result. Internally the API issues an ONVIF
`ContinuousMove` with a 2-second timeout (so the camera auto-stops
even if the follow-up `Stop` is lost) and returns 200 on success,
502 if the camera rejected the SOAP request.

ONVIF SOAP requests use **WS-Security UsernameToken with
PasswordDigest** (`SHA1(nonce + created + password)`), per the ONVIF
spec. HTTP Basic Auth is not used — most Hikvision / Dahua firmware
rejects it with HTTP 401.

### Stream URLs

The handler returns three URLs in the `stream` field:

| URL | Use |
|-----|-----|
| `hls_url`    | Primary: hls.js plays HEVC/H.264 in any modern browser, works through HTTP-only tunnels |
| `webrtc_url` | Fallback: SDP exchange, sub-200ms latency, LAN-only (needs UDP) |
| `stream_name`| Lookup key in go2rtc — **the user-entered friendly name** (e.g. `"前门"`); non-ASCII is URL-escaped on the wire |

These URLs are derived from the `camera.webrtc_public_base` config
key. When blank, the API returns the in-network Docker hostname
(`http://home-go2rtc:1984`), which only works server-side. Set it to
a browser-accessible URL — the dashboard runs nginx which proxies
`/go2rtc/` to go2rtc, so the typical value is `/go2rtc` (a relative
path, same-origin, no CORS). For Cloudflare Tunnel access use
`https://cam.example.com`.

The HLS URL responds with a master playlist. The browser follows
the relative media-playback URL inside it. HLS sessions on go2rtc
live ~5 seconds past the last segment request, so the media
playlist must be fetched promptly.

For WebRTC fallback, hit `webrtc_url` with a `POST` whose body is
your SDP offer and `Content-Type: application/sdp`; the response is
the SDP answer.

> **Stream key = friendly name.** `Registry.Register` uses the
> dashboard's `name` field (e.g. "前门", "backyard") as the go2rtc
> stream key instead of the legacy `cam_<id>`. This means `GET
> /api/streams` shows the operator's names directly. The
> `stream_name` column keeps its `UNIQUE` constraint, so two cameras
> with the same friendly name will fail to register the second one
> — rename one. Migration: existing rows still have `cam_<id>` as
> their stream name; delete and re-create them to get the friendlier
> key, or run the operator script described in
> [Operational Notes](#operational-notes).

---

## ONVIF Profile Discovery

The PTZ handler auto-discovers the ONVIF media profile token on
first use. When `profile_token` is empty in both the request body
and the DB row, the handler calls `ONVIF.DiscoverProfiles` (a raw
SOAP `GetProfiles` request with WS-Security PasswordDigest auth) and
persists the first returned token via `Registry.SaveProfileToken`.
Subsequent PTZ calls reuse the cached token, skipping the discovery
round-trip.

If discovery fails (camera offline, ONVIF not enabled, wrong
credentials), the handler returns `400 missing onvif profile_token`
with a log line detailing the ONVIF error. To troubleshoot:

1. Verify ONVIF is enabled on the camera's web interface
   (Hikvision: Configuration → Network → Advanced → Integration
   Protocol → Enable ONVIF).
2. Verify the ONVIF port (default 80) matches `onvif_port` in the
   camera registration.
3. Check `docker logs home-api` for `ptz: onvif discover` lines.

You can also supply `profile_token` explicitly at registration time
or in the PTZ request body to bypass auto-discovery.

---

## EventBus

Every probe tick emits a canonical `device.status` event so the
Dashboard / App update without polling:

```json
{
  "device_id": 1,
  "type": "camera",
  "status": "online",
  "ts": 1751619200
}
```

The existing WebSocket layer (`/api/v1/ws`) already forwards
`device.*` events to connected clients — no client-side changes
needed.

---

## Front-End Demo (HLS)

The dashboard's `useHLSStream` hook (`web/src/hooks/useHLSStream.ts`)
handles the full flow: it attaches the supplied `hls_url` to either
a native `<video>` element (Safari) or an hls.js-backed MSE source
(Chrome, Firefox, Edge). The hook surfaces fatal codec errors
(typically "your browser can't decode H.265") so the UI can render
a clear message instead of stalling silently.

```tsx
import Hls from "hls.js";
import { useEffect, useRef } from "react";

function CameraView({ hlsUrl }: { hlsUrl: string }) {
    const ref = useRef<HTMLVideoElement>(null);
    useEffect(() => {
        const v = ref.current!;
        if (v.canPlayType("application/vnd.apple.mpegurl")) {
            v.src = hlsUrl;            // Safari
        } else if (Hls.isSupported()) {
            const hls = new Hls();
            hls.loadSource(hlsUrl);
            hls.attachMedia(v);
            return () => hls.destroy();
        }
    }, [hlsUrl]);
    return <video ref={ref} autoPlay playsInline muted controls />;
}
```

### Browser / Codec requirement (HARD)

**The dashboard REQUIRES an HEVC-capable browser to view the
live HLS stream.** There is no server-side transcoder and there
will not be one — this is a deliberate, hard-won trade-off
(see the image-size and CPU rationale in
`deploy/go2rtc/Dockerfile`).

go2rtc's HLS endpoint defaults to **passthrough**: an HEVC
source stays HEVC in the m3u8 (`CODECS="hvc1.1.6.L153.B0"`).
What a typical user actually sees with each browser:

| Browser | HEVC decode | Required action |
|---|---|---|
| Safari (macOS / iOS) | Yes (hardware) | None — works out of the box |
| Edge (current, Windows 11) | Yes | None — built in |
| Chrome on macOS Apple Silicon | Yes (hardware) | None — works out of the box |
| Chrome on Windows 11 | Yes **only with extension** | Install "HEVC Video Extensions" from the Microsoft Store (free tier works for decode) |
| Chrome on Linux | No | **Will not work** — no HEVC path |
| Firefox (any OS) | No | **Will not work** — Mozilla has not licensed HEVC |

The hook probes `video.canPlayType('video/mp4; codecs="hvc1.1.6.L153.B0"')`
up front and short-circuits with a clear "Browser cannot
decode H.265/HEVC" error if the browser returns `""`. If
`canPlayType` claims support but the decoder still produces
black frames (a real possibility on Chrome-on-Windows without
the extension), the `<video>` element will never fire
`playing`, hls.js will keep pulling segments, and the stall
watchdog will eventually surface the same error message after
45s. Either way the operator gets a precise pointer instead
of a silent blank canvas.

What this means in practice:

- If you have control of the operator's browser, install
  HEVC Video Extensions on Chrome-on-Windows and you are done.
- If you cannot dictate the operator's browser, you have two
  options: (1) swap the camera for an H.264 model (no
  server-side change required — go2rtc passthrough will just
  deliver H.264 to hls.js), or (2) accept the constraint and
  document it (the route this project takes).
- We do **not** ship ffmpeg in the go2rtc image. Re-adding it
  would buy us "any browser plays", at the cost of ~30 MB of
  image size and continuous CPU for HEVC→H.264 transcode.
  Reviewed and rejected; the constraint stays.

---

## Tunnel / Proxy Caveats

Cloudflare Tunnel proxies HTTP only. Two transport options:

1. **HLS** (primary): HTTP-only, works through any tunnel. Latency
   is 1-3s (segment length). The Cloudflare Tunnel hostname is set
   in `camera.webrtc_public_base` (e.g. `https://cam.example.com`).
2. **WebRTC** (fallback): needs UDP for media. Cloudflare Tunnel
   cannot relay UDP — you'd need Cloudflare WARP routing or a TURN
   server (e.g. `coturn`). The HTTP SDP exchange still goes through
   the tunnel; only the UDP media is relayed. For LAN-only viewing,
   hit the host's `127.0.0.1:1984` directly and skip the tunnel.

The `camera.webrtc_public_base` config key drives the URLs returned
by the API. Set it to your tunnel hostname (e.g.
`https://cam.feiyemomo.top`) for remote access, to `/go2rtc` for
the dashboard's nginx reverse proxy, or leave blank for LAN-only
(returns the in-network Docker hostname). The `useHLSStream` hook
uses the same field — `webrtc_public_base` is a misnomer kept for
backward compatibility; it actually drives all stream URLs.

---

## Operational Notes

* `go2rtc` is started **before** `api` via `depends_on: [go2rtc]`
  in `compose.yaml`. The API replays all persisted cameras on boot
  (`Registry.BootReplay`) so a container restart doesn't drop streams.
* The `cameras` table is migration-only — there is no auto-seed.
* Deleting a camera is soft-delete (`gorm.DeletedAt`). go2rtc is
  asked to drop the stream synchronously; failure to drop it is
  logged but not returned to the client (DB is the source of truth).
* Health check is a plain TCP-dial against the RTSP port (3-second
  timeout, 15-second interval). This catches "device offline"
  immediately but does not detect "RTSP auth broken". A future
  iteration can layer an ONVIF `GetSystemDateAndTime` probe on top.

### go2rtc API integration

* **PUT /api/streams uses query parameters**, not a JSON body. The
  correct call is:
  ```
  PUT /api/streams?src=<url-encoded-rtsp-url>&name=<stream-name>
  ```
  The old code sent a JSON body `{"cam_1": "rtsp://..."}` which
  go2rtc silently ignored (HTTP 200, stream never created). This was
  the root cause of the "registered a camera but go2rtc's stream list
  is empty" bug. See `internal/camera/go2rtc.go` AddStream.
* **go2rtc.yaml is NOT bind-mounted** in `compose.yaml`. The
  Dockerfile COPYs it into the image at `/etc/go2rtc.yaml`. go2rtc
  rewrites this file on every PUT /api/streams call to persist
  in-memory streams for next-boot replay. A bind-mounted file on
  Windows / Docker Desktop can be read-only at the filesystem level
  even without the `:ro` flag, causing the write to fail silently.
  Relying on the Dockerfile COPY ensures the file lives in the
  container's writable layer.
* **Config save quirk**: go2rtc v1.9.5 has a YAML serialization bug
  where RTSP URLs containing `:` (which is all of them) are not
  properly quoted when written to the config file. The PUT returns
  HTTP 400, but the stream IS added to go2rtc's in-memory state and
  works for live viewing. `AddStream` works around this by doing a
  GET /api/streams after a failed PUT: if the stream exists in the
  response, the error is suppressed.

---

# Phase 5: Event-Driven System + Automation Engine

## Overview

Phase 5 upgrades the platform into an **Event-Driven Home OS**. All
device state changes — camera online/offline, MQTT device status,
telemetry, system alerts — flow through a single EventBus and drive:

1. **WebSocket push** — already existed; now subscribes to `camera.*`
   and `automation.fired` in addition to `device.*`.
2. **Automation Engine** — new. A rule engine that triggers actions
   (notify / mqtt / webhook) when events match a condition.
3. **Future AI Layer** — out of scope for this phase; the EventBus is
   the only integration point needed.

Architecture:

```
   Device / Camera / MQTT / System
               │
               ▼
          EventBus  (subscribe "*")
               │
   ┌───────────┼───────────┐
   │           │           │
   ▼           ▼           ▼
WebSocket   Automation   Future AI
            Engine
```

## 1. Event Model

All events share a unified struct (`internal/eventbus/events.go`):

```go
type Event struct {
    ID        uint64    `json:"id"`        // auto-incremented
    Topic     string    `json:"type"`      // e.g. "camera.online"
    Source    string    `json:"source"`    // mqtt | ws | system | camera | automation
    Severity  string    `json:"severity"`  // info | warn | error | critical
    Payload   []byte    `json:"payload"`   // opaque JSON
    Timestamp time.Time `json:"timestamp"` // auto-filled
}
```

Canonical topics:

| Topic | Emitted by | Severity |
|-------|-----------|----------|
| `device.status` | MQTT handler, device Manager, camera HealthChecker | info |
| `device.telemetry` | MQTT handler | info |
| `device.command` | MQTT handler | info |
| `device.event` | MQTT handler (camera motion/AI) | info |
| `camera.online` | camera HealthChecker (offline→online) | info |
| `camera.offline` | camera HealthChecker (online→offline) | warn |
| `camera.rtsp_lost` | (reserved) | warn |
| `camera.status_changed` | (reserved) | info |
| `camera.motion` | (reserved for future AI layer) | warn |
| `system.alert` | (reserved) | error |
| `user.notification` | Automation Engine, system | info |
| `system.broadcast` | MQTT handler, system | info |
| `automation.fired` | Automation Engine (audit) | info |

## 2. EventBus

`internal/eventbus/bus.go` — goroutine-safe in-memory pub/sub.

- **Subscribe(prefix, cb)** — prefix matching with segment boundary
  (`device.1` matches `device.1.status` but NOT `device.10.status`).
  Special prefixes: `*` and `""` match everything (wildcard).
- **Publish(e)** — auto-fills `ID` (atomic counter), `Timestamp`,
  `Severity` (default `info`). Calls subscribers in the publisher's
  goroutine.
- **PublishAsync(e)** — same, but each subscriber runs in its own
  goroutine (fan-out, non-blocking). Used by the Automation Engine to
  avoid stalling the publisher on slow webhook actions.

## 3. Camera Event Integration

`internal/camera/health.go` — the HealthChecker now tracks per-camera
`prevStatus` and emits transition events:

- `camera.online` (severity=info) on offline→online
- `camera.offline` (severity=warn) on online→offline
- `device.status` (always, for backward compatibility)

Only transitions are emitted — a camera that stays online does not
generate a `camera.online` event every tick. This keeps the EventBus
volume proportional to actual state changes.

## 4. MQTT → Event Conversion

Already implemented in Phase 3 (`internal/mqtt/handler.go`). MQTT
messages under `home-datacenter/devices/+/status`,
`home-datacenter/devices/+/telemetry`,
`home-datacenter/devices/+/events`, and
`home-datacenter/cameras/+/event` are parsed and re-published on the
EventBus as `device.status`, `device.telemetry`, `device.command`, and
`device.event` respectively. Canonical JSON is re-emitted so downstream
consumers always get valid JSON even from lenient publishers.

## 5. WebSocket Bridge

`internal/ws/hub.go` subscribes to the EventBus on creation. As of
Phase 5, the subscription list is:

- `device` (prefix — catches `device.status`, `device.telemetry`, etc.)
- `camera` (prefix — catches `camera.online`, `camera.offline`, etc.)
- `user.notification` (targeted to a specific user)
- `system.broadcast` (broadcast to all clients)
- `automation.fired` (audit trail — admins see all fires)

The Hub does not implement business logic; it only routes EventBus
events to connected WebSocket clients based on their subscriptions.

## 6. Automation Engine

`internal/automation/` — a minimal rule engine.

### Rule Model

```go
type Rule struct {
    ID         uint       // primary key
    Name       string     // human-readable
    Trigger    string     // event topic or prefix ("camera.offline", "device", "*")
    Condition  Condition  // JSON: time window + payload filter
    Action     Action     // JSON: notify | mqtt | webhook
    Enabled    bool
    FireCount  uint64     // incremented on each fire
    LastFireAt *time.Time
    CreatedAt, UpdatedAt time.Time
}
```

### Condition

```json
{
  "time_gte": "22:00",           // optional: current time >= 22:00
  "time_lte": "06:00",           // optional: wraps midnight if gte > lte
  "payload_eq": {                // optional: payload fields must match
    "status": "offline"
  }
}
```

All specified fields must match (logical AND). Empty condition `{}`
matches every event.

### Action

Three types are supported:

```json
// 1. Notify — publish a user.notification event (Hub routes to user)
{
  "type": "notify",
  "user_id": 1,                  // 0 = broadcast to admins
  "title": "Camera offline",
  "body": "Front door camera went offline"
}

// 2. MQTT — publish a raw MQTT message
{
  "type": "mqtt",
  "topic": "home-datacenter/devices/1/command",
  "payload": "{\"command\":\"turn_on\"}",
  "qos": 1
}

// 3. Webhook — HTTP POST to an external URL
{
  "type": "webhook",
  "url": "https://example.com/hook",
  "method": "POST",              // default POST
  "headers": {"Authorization": "Bearer xxx"},
  "payload": "{\"event\":\"camera_offline\"}"
}
```

### API

All routes are under `/api/v1/automation` and require JWT + admin.

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/automation/rules` | List all rules |
| POST | `/automation/rules` | Create a rule |
| GET | `/automation/rules/:id` | Fetch one rule |
| PUT | `/automation/rules/:id` | Update a rule |
| DELETE | `/automation/rules/:id` | Delete a rule (soft) |
| POST | `/automation/rules/:id/test` | Manually fire a rule (no fire_count increment) |

### Example: Camera offline notification at night

```bash
curl -X POST http://localhost:8080/api/v1/automation/rules \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Night camera offline alert",
    "trigger": "camera.offline",
    "condition": {
      "time_gte": "22:00",
      "time_lte": "06:00"
    },
    "action": {
      "type": "notify",
      "user_id": 1,
      "title": "Camera offline at night",
      "body": "A camera went offline during night hours"
    }
  }'
```

### Engine lifecycle

- `Start()` loads enabled rules from DB and subscribes to `*` on the
  EventBus.
- `handleEvent()` runs in the publisher's goroutine; condition checks
  are O(1) per rule. Actions fire in their own goroutines so a slow
  webhook cannot stall the bus.
- `Reload()` re-reads rules from DB after any CRUD operation.
- `fire()` increments `fire_count`, updates `last_fire_at`, and emits
  an `automation.fired` audit event.

### Security

- **MQTT action**: topic must be inside `home-datacenter/` namespace
  (same rule as `/api/v1/mqtt/publish`). `$SYS` and arbitrary topics
  are rejected at CRUD time AND at fire time.
- **Webhook action**: URL must be `http://` or `https://`. The
  resolved host must NOT be private, loopback, or link-local — this
  blocks SSRF attacks like `http://169.254.169.254/latest/meta-data/`.
  The check runs at fire time (not CRUD time) because DNS can change
  between create and fire.
- **All rule CRUD endpoints are admin-only** via `middleware.RequireAdmin`.

## Next Steps (Future Extensions)

- **Persistence**: rules are already SQLite-backed; consider a rule
  import/export endpoint for backup.
- **Templating**: action bodies could support Go templates
  (`{{.event.topic}}`, `{{.event.payload.status}}`) for dynamic
  payloads. Currently static.
- **Rate limiting**: per-rule cooldown to prevent runaway firing on
  flapping cameras.
- **AI Layer**: a future subscriber on `*` could feed events into an
  LLM for anomaly detection or natural-language summaries.
