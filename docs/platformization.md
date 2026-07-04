# Camera Platformization (Phase 4)

> Status: ✅ Implemented — 2026-07-04

This document describes the camera module: how a network camera becomes
a first-class **Device** in the Home Datacenter platform, how live
video reaches the browser, and how PTZ control is exposed.

The goal of this phase is the pipeline

> RTSP → go2rtc → WebRTC/HLS

fully driven by the API, with credentials stored encrypted at rest,
online/offline state pushed via the EventBus, and admin-only PTZ.

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
* **WebRTC** is the primary viewing path for desktops / Android. HLS
  is the iOS-Safari fallback. Both URLs are returned in
  `GET /api/v1/cameras/:id` under `stream.webrtc_url` / `stream.hls_url`.

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
  stream_name   TEXT UNIQUE,                   -- cam_<id>
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
      "stream_name": "cam_1",
      "webrtc_url":  "http://home-go2rtc:1984/api/webrtc?src=cam_1",
      "hls_url":     "http://home-go2rtc:1984/api/stream.m3u8?src=cam_1"
    }
  }
}
```

* If `profile_token` is empty, the API currently stores it as `""`.
  The PTZ endpoint will reject with `missing onvif profile_token`
  until you re-register with a known token (see "ONVIF profile
  discovery" below).
* If go2rtc is unreachable, the camera row is rolled back and the
  API returns **502 Bad Gateway**.

### POST /api/v1/cameras/:id/ptz

```json
{ "command": "left", "speed": 0.5, "profile_token": "" }
```

`command` ∈ {`left`, `right`, `up`, `down`, `stop`, `zoom_in`, `zoom_out`}.
`speed` is clamped to 0..1; default 0.5. Internally the API issues
an ONVIF `ContinuousMove` with a 2-second timeout (so the camera
auto-stops even if the follow-up `Stop` is lost) and returns 200 on
success, 502 if the camera rejected the SOAP request.

### Stream URLs

The handler returns three URLs in the `stream` field:

| URL | Use |
|-----|-----|
| `webrtc_url` | Primary: SDP exchange, sub-200ms latency |
| `hls_url`    | iOS Safari fallback (HLS) |
| `stream_name`| Lookup key in go2rtc |

For browser use, hit `webrtc_url` with a `POST` whose body is your
SDP offer and `Content-Type: application/sdp`; the response is the
SDP answer.

---

## ONVIF Profile Discovery

The current implementation expects the caller to supply
`profile_token` at register time. To auto-discover it, hit ONVIF
`GetProfiles` on the camera first (any ONVIF client works — e.g.
`onvif-cli`, or a one-off curl with the WS-Security digest) and
include the resulting token in the registration payload.

Adding an automated `GetProfiles` round-trip to the registration
handler is a small follow-up (≈30 lines); it was left out to keep
the first iteration dependency-free (no ONVIF XML library).

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

## Front-End Demo (WebRTC)

```html
<video id="v" autoplay playsinline controls muted style="width:100%"></video>
<script>
  const rtc = new RTCPeerConnection({iceServers: [{urls: "stun:stun.cloudflare.com:3478"}]});
  rtc.addTransceiver("video", {direction: "sendrecv"});
  rtc.createOffer()
    .then(offer => rtc.setLocalDescription(offer))
    .then(() => fetch("https://cam.feiyemomo.top/api/webrtc?src=cam_1", {
      method: "POST",
      headers: {"Content-Type": "application/sdp"},
      body: rtc.localDescription.sdp
    }))
    .then(r => r.text())
    .then(answer => rtc.setRemoteDescription({type: "answer", sdp: answer}));
  rtc.ontrack = e => document.getElementById("v").srcObject = e.streams[0];
</script>
```

HLS fallback (e.g. iOS Safari):

```html
<video id="v" autoplay playsinline controls muted style="width:100%"></video>
<script src="https://cdn.jsdelivr.net/npm/hls.js@1.5.13/dist/hls.min.js"></script>
<script>
  const v = document.getElementById("v");
  if (v.canPlayType("application/vnd.apple.mpegurl")) {
    v.src = "https://cam.feiyemomo.top/api/stream.m3u8?src=cam_1";
  } else if (Hls.isSupported()) {
    const h = new Hls(); h.loadSource("https://cam.feiyemomo.top/api/stream.m3u8?src=cam_1");
    h.attachMedia(v);
  }
</script>
```

---

## WebRTC via Cloudflare Tunnel — Caveats

Cloudflare Tunnel proxies HTTP only. WebRTC needs UDP for media
packets. Two paths to production:

1. **LAN / single-user**: hit `127.0.0.1:1984` directly on the host,
   no tunnel needed. Camera viewing works at full quality with
   ~80 ms glass-to-glass latency.
2. **Multi-site / mobile**: add Cloudflare WARP routing or a TURN
   server (e.g. `coturn`). The HTTP SDP exchange still goes through
   the tunnel; only the UDP media is relayed.

For now, the dashboard / app are designed to take a configurable
base URL for `webrtc_url` / `hls_url` — production routing config is
a follow-up.

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
