# Camera Platformization (Phase 4) + Event-Driven System (Phase 5)

> Status: ✅ Implemented — 2026-07-04 (Phase 4), 2026-07-05 (Phase 5), 2026-07-05 (Phase 7a: ffmpeg opt-in, HLS tuning, nginx auth_request fix), 2026-07-11 (Phase 7b: transport toggle + light/dark theme), 2026-07-11 (Phase 8: user management API + last-admin/self-delete/self-demote state guards)

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
  no credentials — it only knows stream names (the friendly name
  entered in dashboard, e.g. `前门`, `客厅`) and the source URLs the
  API feeds it.
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
| `webrtc_url` | **Primary (Phase 6+).** Sub-200ms latency, p2p UDP/TCP ICE. go2rtc returns 3 UDP + 1 TCP candidates from its `webrtc.candidates` list (defaults to 127.0.0.1:8555 + LAN + STUN-reflected). ICE/DTLS completes in <1s on a healthy link. |
| `hls_url`    | **Fallback.** Works through HTTP-only tunnels and **decodes H.265/HEVC in every modern browser via fMP4/MSE** (Chrome on Windows requires the "HEVC Video Extensions" Microsoft Store plugin; Firefox/Linux Chrome never decode HEVC). 1-3s latency (segment length). |
| `stream_name`| Lookup key in go2rtc — **the user-entered friendly name** (e.g. `"前门"`); non-ASCII is URL-escaped on the wire |

#### Why HLS is the fallback despite being slower

WebRTC is technically the lower-latency transport, but it has a
**hard browser limitation for HEVC cameras on Windows/Chrome**: Chrome
*can* decode H.265 via the MSE/fMP4 path (HLS), but it does **not**
recognise the H.265 RTP payload format (`video/H265`, RFC 7798) that
go2rtc offers in the WebRTC SDP answer. The browser completes ICE
and DTLS, RTP packets flow, but `framesDecoded = 0` because the
decoder rejects the codec.

**Workarounds**, in order of operator preference:

1. **Live with the fallback.** The dashboard's `LiveVideo` always
   tries WebRTC first; on codec failure (no frames within ~1s after
   the track arrives), it tears down the peer connection and remounts
   the same tile with HLS. The user sees an H.265 stream at HLS
   latency. This is the current default and what the rest of this
   section assumes.
2. **Use a browser with native H.265 WebRTC decode.** Safari on
   Apple Silicon decodes H.265 in hardware and accepts the same SDP.
   If the operator can constrain which browser is used to view the
   dashboard (e.g. an iPad kiosk), WebRTC works at sub-200ms latency
   with zero transcoding cost.
3. **Re-introduce ffmpeg in go2rtc** and force the source to
   `rtsp://...#video=h264`. go2rtc transcodes H.265 → H.264 on the
   fly and Chrome happily decodes it via WebRTC. Cost: ~30 MB image
   bloat, ~50% of one CPU core per 1080p H.265 stream.

The dashboard does **not** try option 3 because the project
deliberately excludes ffmpeg from the go2rtc image (see
`deploy/go2rtc/Dockerfile` — "We deliberately do NOT install
ffmpeg"). Operators who need WebRTC on H.265 cameras on Windows
should switch to option 1 (current behaviour) or option 2
(Safari/Apple Silicon).

#### Why HLS works for H.265 but WebRTC doesn't

`MediaSource.isTypeSupported('video/mp4; codecs="hvc1.1.6.L153.B0"')`
returns `true` in Chrome when the "HEVC Video Extensions" plugin is
installed (and on Safari / Edge-with-HEVC out of the box). HLS uses
this MSE path.

WebRTC's RTP stack uses a separate codec registry that only
recognises the H.264/VP8/VP9/AV1 RTP payload formats. The H.265 RTP
payload format (RFC 7798) is registered with `video/H265` in the
SDP, but Chrome's WebRTC implementation does not include an H.265
decoder on the WebRTC side. (The same is true for Firefox.)

The **fMP4 container** (`segment.m4s`) is required for H.265 in HLS
because `hls.js`'s TS demuxer has weak HEVC support and silently
drops frames even when MSE would accept them. The API's HLS URL
builder appends `&mp4=` to force the fMP4 mode (see
`internal/camera/registry.go` `StreamConfig`).

#### WebRTC infrastructure (always wired up)

Even when the camera is HEVC and the operator uses Chrome, the
WebRTC path is fully implemented and tested end-to-end:

- **go2rtc** listens on TCP and UDP `:8555` (verified via
  `netstat -tunlp` inside the container). The 1.9.5 release binds
  both transports only with `webrtc.listen: ":8555"` — the
  split-into-`/tcp` + `udp: ":8555"` form silently drops UDP.
- **SDP candidates** are configured via `webrtc.candidates: [127.0.0.1:8555]`
  in `go2rtc.yaml`. Without this, go2rtc advertises the Docker
  bridge IP (e.g. `172.19.0.3:8555`) and the STUN-reflected public
  IP; the former is unreachable from a host browser, the latter
  needs UDP port forwarding that home routers almost never do.
- **Compose port mapping** is `0.0.0.0:8555:8555` (TCP+UDP), not
  `127.0.0.1`, so the same go2rtc instance is also reachable from
  phones/tablets on the LAN.
- **`useWebRTCStream.ts` waits for ICE gathering to complete**
  (up to 3s) before POSTing the SDP offer, so all host + STUN
  candidates are in the offer. Without this, the offer ships with
  only the first host candidate — usually the Docker bridge IP —
  and the connection dies even though a 127.0.0.1:8555 candidate
  would have arrived 200ms later.
- **Fallback only triggers on `connectionState: failed/closed`**.
  The browser briefly reports `iceConnectionState: failed` as it
  cycles through candidate pairs, and treating that transient as
  fatal would tear down a connection that's about to stabilise.
  The hook logs the transient state to `console.debug` but does
  not surface it as an error.

These URLs are derived from the `camera.webrtc_public_base` config
key. When blank, the API returns the in-network Docker hostname
(`http://home-go2rtc:1984`), which only works server-side. Set it to
a browser-accessible URL — the dashboard runs nginx which proxies
`/go2rtc/` to go2rtc, so the typical value is `/go2rtc` (a relative
path, same-origin, no CORS). For Cloudflare Tunnel access use
`https://cam.example.com`.

The HLS URL responds with a master playlist. The browser follows
the relative media-playback URL inside it. HLS sessions on go2rtc
live ~30 seconds past the last segment request (we patched the
upstream 5s to 30s in `deploy/go2rtc/Dockerfile` — HEVC segments
are ~1.4 MB at 22 Mbps and take >5s to deliver on a <2.5 Mbps
link), so the media playlist must be fetched promptly.

For WebRTC, hit `webrtc_url` with a `POST` whose body is your SDP
offer and `Content-Type: application/sdp`; the response is the
SDP answer.

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

### HLS startup tuning (Phase 7)

go2rtc's upstream HLS default is `segment: 6, window: 3` (six-
second segments, three-segment window). On the home network with
~22 Mbps HEVC cameras, that means the operator sees a spinner for
~6s before the first frame — most of that wait is "go2rtc hasn't
finished writing a single segment yet." We tune the HLS server
in `deploy/go2rtc/go2rtc.yaml` to:

| Key | Value | Why |
|---|---|---|
| `hls.segment` | 2s | First frame lands in ~1-2s instead of ~6s. Cameras are bandwidth-stable on the LAN, so a smaller segment doesn't hurt recovery. |
| `hls.partial` | true | Splits each 2s segment into ~200ms parts; hls.js can start decoding the first part before the segment is fully written. Sub-second time-to-first-frame on healthy streams. |
| `hls.window` | 4 | 4 × 2s = 8s of forward-buffered segments; absorbs a 2-3s home-api GC pause without forcing a playlist refetch. |

The front-end stall watchdog in `useHLSStream.ts` was tightened
from 45s to 20s to match: a healthy stream now reaches `playing`
in ~1-2s, so a 20s watchdog is plenty of headroom for cold-RTSP-
producer scenarios, while a genuinely broken session surfaces an
error to the user in 20s instead of 45s.

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

#### Per-transport browser matrix

The HEVC story is **transport-specific**: a browser may decode
HEVC fine for `<video src="…mp4">` and for hls.js/MSE while
*still* failing to decode the same HEVC stream over WebRTC. The
two paths use different decoder registries (MSE via the system
codec, WebRTC via the browser's RTP stack), so the table below
splits them.

| Browser | MSE/HLS HEVC | WebRTC HEVC | Result for an HEVC camera |
|---|---|---|---|
| Safari (macOS / iOS) | Yes (hardware) | Yes (hardware) | WebRTC primary, sub-200ms latency |
| Edge (Windows 11) | Yes (OS extension) | **No** — Chromium WebRTC codec registry does not include H.265 | WebRTC connects, `framesDecoded=0`, dashboard auto-fallback to HLS |
| Edge (macOS / Linux) | No | No | HLS fails too; "cannot decode H.265" error |
| Chrome on macOS Apple Silicon | Yes (hardware) | **No** — same as Edge | WebRTC fallback to HLS |
| Chrome on Windows 11 | Yes with "HEVC Video Extensions" | No | WebRTC fallback to HLS; HLS works after extension install |
| Chrome on Linux | No | No | HLS fails too; "cannot decode H.265" error |
| Firefox (any OS) | No | No | HLS fails too; "cannot decode H.265" error |
| Chrome / WebView on Android | Yes (Android 9+ MediaCodec, vendor-dependent) | **No** — Chromium WebRTC codec registry does not include H.265 | WebRTC fallback to HLS; HLS works on most modern devices |
| iOS native WebView (WKWebView) | Yes (hardware) | Yes (hardware, same as Safari) | WebRTC primary |

Why the asymmetry: WebRTC's RTP stack uses a separate codec
registry (the WebRTC "stack codec" set) that only includes
H.264 / VP8 / VP9 / AV1. RFC 7798's `video/H265` is registered
in the SDP, but **Chromium-derivatives (Chrome, Edge, WebView) do
not route that to the system HEVC decoder on the WebRTC side**.
The MSE path *does* route to the system decoder, which is why
the same browser happily plays the HLS variant. The same is true
for Firefox (no HEVC at all on either path). Safari is the only
desktop-or-mobile browser that wires system HEVC into both
pipelines.

Operator-facing consequences:

- **The dashboard always shows *something*.** The Phase 6
  fallback in `LiveVideo.tsx` switches to HLS on the first
  HEVC-decoder failure, so an HEVC camera on Chrome/Edge/Android
  ends up on HLS within ~1s of the first frame arriving
  (connection state never goes to "failed" — the connection
  succeeds, but `framesDecoded` stays 0 and the `<video>` element
  never fires `playing`; the hook's stall watchdog flips the path
  to HLS). Users do not see a black tile.
- **The fallback badge in the corner flips from "WebRTC" to "HLS"**
  as the tile remounts. If you need WebRTC for an HEVC camera on
  a non-Apple platform, you have two options:
  (a) **Per-camera ffmpeg transcode** — turn on the
  `transcode` toggle in the camera register form (or
  `POST /api/v1/cameras` with `"transcode": true`). The
  registry rewrites the source URL to
  `ffmpeg:rtsp://...#video=h264` (see
  `services/api/internal/camera/registry.go` rtspURL). The
  `ffmpeg:` scheme prefix is required — a bare
  `rtsp://...#video=h264` is silently ignored by go2rtc's
  rtsp producer, which just connects to the camera and
  forwards whatever codecs the SDP advertises. With the
  `ffmpeg:` prefix, go2rtc routes the source through its
  internal `streams.RedirectFunc` → `parseArgs` → `exec:`
  pipeline (see `build-host/go2rtc/internal/ffmpeg/ffmpeg.go`),
  spawns an ffmpeg child process with the `h264` preset
  (libx264, high@4.1, superfast/zerolatency, yuv420p), and
  feeds the H.264 output to the WebRTC peer. The Hikvision
  audio (PCMA / G726) is dropped automatically by ffmpeg's
  `-an` (we deliberately do NOT add `#audio=...` to the
  ffmpeg URL — any non-empty audio value would be fed
  raw to ffmpeg, producing a malformed command line).
  ffmpeg is part of the go2rtc image (~30 MB on top of
  the existing footprint, only consumes CPU for cameras with
  the flag on). The Dashboard shows a small "x264" badge next
  to the camera name so the operator can see at a glance which
  cameras are paying the transcode cost.
  (b) Swap the camera for an H.264 model.
- **Android app, when shipped, will hit the same behaviour as
  Chrome-on-Android.** If the app uses the platform `WebView`
  (default for most cross-platform stacks), WebRTC will fall
  back to HLS for HEVC cameras. If the app embeds a real
  Chromium (e.g. `flutter_inappwebview` with hardware
  acceleration), the same applies. The only path that gives
  WebRTC sub-200ms on Android for HEVC is a fully native WebRTC
  client with Android's `MediaCodec` HEVC decoder wired into
  the WebRTC stack — out of scope for the current web dashboard.

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

---

## 7. Automation Runtime (Phase 6)

Phase 5 wired the basic rule engine. Phase 6 turns it into a
**runtime**: rules now have richer conditions, configurable actions
with retries and timeouts, per-rule throttle, in-memory metrics, and
admin escape hatches for silencing a misbehaving rule. The goal is a
system you can deploy unattended — flapping sensors, slow webhooks,
and event floods must not bring the engine down or hammer external
services.

### 7.1 Architecture (unchanged shape, deeper internals)

```
EventBus (subscribe "*")
    │
    ▼
Engine.handleEvent
    │  ─ per-rule fan-out, O(rules) per event
    │  ─ Throttle:  cooldown / rate limit / dedup
    ▼
Condition.eval              ── time / source / payload_eq / threshold / regex / Any(OR)
    │
    ▼
Action.execute              ── notify | mqtt | webhook
    │  ─ per-action timeout (default 5s)
    │  ─ webhook: retry with exponential backoff (4xx is permanent)
    ▼
Metrics.RecordFire          ── atomic counters + per-rule runtime
    │
    ▼
Publish "automation.fired"  ── audit event for UI / external log
```

The engine keeps an in-memory copy of all enabled rules plus a
per-rule runtime (last-fire, recent-fires sliding window, last-seen
event hash for dedup) so the hot path is allocation-free. CRUD
endpoints call `engine.Reload()` after every mutation.

### 7.2 Enriched `Condition`

```json
{
  "time_gte":   "22:00",                        // 24h "HH:MM"
  "time_lte":   "06:00",                        // wraps midnight if gte > lte
  "source":     "camera",                       // exact match on Event.Source
  "payload_eq": { "status": "offline" },        // JSON-normalised equality
  "threshold":  { "confidence": { "op": ">", "val": 0.8 } },
  "regex":      { "device_id": "^cam_[0-9]+$" },
  "any":        false                           // true = OR, default = AND
}
```

| Field        | Semantics |
|--------------|-----------|
| `time_gte/lte` | "HH:MM" bounds; `gte > lte` wraps midnight |
| `source`       | exact match — `mqtt` / `ws` / `system` / `camera` / `automation` / `test` |
| `payload_eq`   | each top-level payload field is JSON-normalised and compared; missing keys fail |
| `threshold`    | numeric comparison; payload field must coerce to a number |
| `regex`        | RE2 pattern; non-string fields fail |
| `any`          | combines the rest with OR (default AND); no fields = match all |

**Example: high-confidence camera motion at night**

```json
{
  "time_gte":  "22:00",
  "time_lte":  "06:00",
  "source":    "camera",
  "payload_eq":{ "event": "motion" },
  "threshold": { "confidence": { "op": ">=", "val": 0.8 } }
}
```

### 7.3 Enriched `Action`

```json
{
  "type":        "webhook",
  "url":         "https://example.com/hook",
  "method":      "POST",                  // default POST
  "headers":     { "Authorization": "Bearer xxx" },
  "payload":     "{\"event\":\"motion\"}", // body (defaults to event payload)
  "timeout_ms":  3000,                    // per-attempt; default 5000
  "retry_max":   3                        // total attempts = 1 + retry_max
}
```

Retry policy for `webhook` only:

- 4xx response → **permanent** error, no retry.
- 5xx, network error, timeout → **retry** with backoff
  `500ms × 2^n` capped at 30s.
- `notify` and `mqtt` are not retried: `notify` is an in-process
  EventBus publish (best-effort, the Hub already dedups downstream),
  and `mqtt` is delegated to the broker which has its own QoS
  guarantees.

### 7.4 `Throttle` (event-flood protection)

```json
{
  "cooldown_s":   30,        // silent for N seconds after a fire
  "rate_per_min": 5,         // max fires in a 60s sliding window
  "dedup":        true       // collapse identical events (same topic+source+payload)
}
```

All three checks run before the action. The hot path keeps:

- `lastFire`        — for cooldown
- `fireHistory`     — circular buffer for rate limit (cap 256)
- `lastEventKey`    — SHA-256 prefix of topic+source+payload for dedup

`RecordFire` mutates all three. `throttleAllows` returns `(false,
reason)`; dropped events are logged + counted in metrics.

### 7.5 Metrics (admin-only)

The engine maintains a `Metrics` struct with `sync/atomic` counters
and a mutex-guarded per-rule map. No external dependency (no
Prometheus client) — this is a home OS, the operator reads it via
the admin UI or the audit event stream.

| Endpoint                                          | Purpose |
|---------------------------------------------------|---------|
| `GET  /api/v1/automation/metrics`                 | Global counters + per-rule map |
| `GET  /api/v1/automation/metrics?reset=1`         | Reset all counters (admin only) |
| `GET  /api/v1/automation/rules/:id/metrics`       | Per-rule slice (Fires, Errors, Dropped, AvgMs, MaxMs) |

**Global metrics shape:**

```json
{
  "events_seen":     1234,
  "fires":           57,
  "errors":          2,
  "dropped":         8,
  "avg_duration_ms": 12.3,
  "max_duration_ms": 4300,
  "started_at":      "2026-07-05T07:00:00Z",
  "uptime_seconds":  9000,
  "per_rule": {
    "5": { "Fires": 50, "Errors": 0, "Dropped": 5, "AvgMs": 10, "MaxMs": 200 }
  }
}
```

### 7.6 Admin escape hatches

- **`POST /api/v1/automation/rules/:id/cooldown`** — set
  `lastFire = now - seconds`, silencing the rule for the requested
  window without deleting it. Handy when a downstream webhook is
  down and a rule is firing on every event.
- **`POST /api/v1/automation/rules/:id/test`** — fire the action
  synchronously with a synthetic event. Does **not** increment
  `fire_count` (it would distort the persistent counter for
  operator review).

### 7.7 Audit event

Every fire publishes `automation.fired` to the EventBus with the
full context: rule id/name, trigger, event id, ok/err, duration_ms.
The WebSocket Hub already forwards it to subscribed clients, so the
dashboard can render a live "rule activity" feed without polling.

### 7.8 Verified behaviour (E2E)

The following were verified against the live stack on 2026-07-05:

| Scenario                         | Result | Evidence                                  |
|----------------------------------|--------|-------------------------------------------|
| Rule with empty condition        | fires  | every EventBus event matches               |
| `payload_eq` filter              | fires  | MQTT `status=online` → `device-online-notify` rule fires |
| `cooldown_s` + `rate_per_min`    | throttled | 5 events / 1s → 2 fires, 9 dropped         |
| Webhook to `127.0.0.1`           | blocked | `action failed: webhook host 127.0.0.1 is private/loopback/link-local` |
| Webhook to `169.254.169.254`     | blocked | `webhook host ... is private/loopback/link-local` |
| Webhook to public IP             | passes | SSRF check passes; HTTP error is reported as a fire error |
| MQTT topic `home-datacenter/...` | allowed | CRUD + fire both pass                      |
| MQTT topic `$SYS/...`            | rejected | CRUD returns 400                           |
| MQTT topic `other-ns/...`        | rejected | CRUD returns 400                           |
| `?reset=1`                       | zeros counters | snapshot shows `events_seen=0, fires=0`     |
| `cooldown` endpoint              | pins `lastFire` | rule 5 silenced for 3600s                |

Unit tests in `internal/automation/engine_test.go` cover
`triggerMatches`, `timeInRange` (including midnight wrap),
`conditionMatches` (payload_eq + malformed payload),
`isAllowedMQTTTopic`, and `isPublicIP` (loopback, private,
link-local, unspecified, v6).

### 7.9 Operational notes

- **Reload is eager**: any CRUD operation calls `engine.Reload()`
  synchronously. A spike of 100 creates will trigger 100 reloads —
  acceptable for a home dashboard where rule count is in the
  dozens, but a batch-import endpoint should consider deferring.
- **Per-rule runtime is in-memory**: a container restart resets
  cooldowns. The `fire_count` and `last_fire_at` in SQLite are
  durable but are only updated on actual fires, not on
  engine-state changes.
- **Metrics are process-local**: the `Reset` button resets the
  container's view only. There is no cluster-wide aggregation —
  this is a single-node home OS, not a SaaS.

---

## 8. Transport Toggle (Phase 7)

`LiveVideo.tsx` ships a three-position segmented control adjacent to
the live path badge, giving the operator direct control over which
transport the playback hook mounts:

| Mode | Behavior |
|---|---|
| `auto` | Default. Try WebRTC first; on `state === "error"` (SDP 5xx, ICE failure, codec mismatch) remount with the HLS sub-component. The path badge in the header flips from "WebRTC" to "HLS" so the operator can see what landed. |
| `webrtc` | Sticky. Mount the WebRTC sub-component and never auto-fallback. A failure surfaces as an error overlay with a `Retry` button. Useful when a transcode fix has just landed and the operator wants to confirm WebRTC plays without HLS masking the regression. |
| `hls` | Sticky. Force the fragmented-MP4 HLS path. Useful for comparing HLS playback against WebRTC during codec-bug triage. |

The selection is stored in `localStorage` under `home.transport` and
read synchronously during component init, so reloads preserve the
choice. It is a **global** preference (one toggle for every camera);
per-camera preference was considered but adds UI surface for a value
that almost-always wants the same setting across cameras.

**Reset on camera change.** When the operator switches between
cameras (or the dashboard re-fetches the camera list), the
`useEffect([camera.id, transport])` resets `path` to the requested
transport's default and zeroes the `generation` counter, so a
mid-session WebRTC failure on camera A does not leak into camera B.

**Why expose this at all?** The auto-fallback is the right default
for the user-facing flow, but it makes codec-bug triage noisy — the
operator triggers a known-broken WebRTC scenario and the dashboard
"fixes itself" by switching to HLS, hiding the bug. The explicit
modes pin the transport so a regression is visible in the error
overlay. The same control also lets an operator on a browser that
*does* support WebRTC HEVC (Safari) opt out of the fallback path
entirely.

---

## 9. Light / Dark Theme (Phase 7)

The dashboard ships with a two-theme palette, switchable from the
header Sun/Moon button. Implementation is intentionally minimal:

- **CSS variables in `index.css`** define a `--bg` / `--bg-raised` /
  `--fg` / `--slate-50…950` palette. Two blocks:
  ```css
  :root, [data-theme="light"] { --bg: 248 250 252; ... }
  [data-theme="dark"]        { --bg: 11 17 32; ... }
  ```
  `:root` resolves to the light palette so users without a
  preference (or with `prefers-reduced-motion`) see the bright
  version on first visit; `data-theme="dark"` overrides it.
- **Tailwind is bound to the variables**, not to baked-in colors.
  `tailwind.config.js` overrides the default slate palette to
  `rgb(var(--slate-50) / <alpha-value>)` etc., so every
  `bg-slate-*` / `text-slate-*` / `border-slate-*` utility class
  flips automatically. The four `surface` / `fg` design tokens
  (`bg-surface`, `text-fg-muted`, etc.) point at the same variables
  for components that want a deliberate semantic name.
- **`useTheme()` is the single source of truth.** Reads
  `localStorage["home.theme"]` synchronously on hook init; writes
  the `data-theme` attribute on `<html>` in a `useEffect`. The
  `storage` event listener propagates a toggle from one tab to
  every other tab of the same origin without a refresh.
- **`applyThemeEarly()` runs in `main.tsx` BEFORE React mounts** to
  set the `data-theme` attribute on `<html>` from the same
  `localStorage` read, so the first paint already has the right
  palette. Without it, the page renders with the default dark
  theme for ~16ms before `useTheme`'s first effect runs — a
  visible dark→light flash on slow devices.

The choice is persisted in `localStorage` only; there is no
server-side theme preference. Adding a "system" mode that follows
`prefers-color-scheme` is a one-line change in `useTheme.ts`
(`readInitial` checks `matchMedia` when no stored value) and was
deferred to keep the surface small.

---

## 10. User Management (Phase 8)

The user-management API is **admin-only CRUD over the `users` table**,
with three state guards that protect the system from accidentally
locking itself out:

| Guard | Service error | HTTP code | What the UI does |
|---|---|---|---|
| Last-admin | `ErrLastAdmin` | 400 | Disables the role checkbox + delete button on the only admin row |
| Self-delete | `ErrSelfDelete` | 400 | Disables the delete button on the caller's own row |
| Self-demote | `ErrSelfDemote` | 400 | Disables the role checkbox on the caller's own row |

The service layer (`internal/service/user_service.go`) is the single
source of these errors. The handler layer
(`internal/handler/user_handler.go`) centralises the
`writeUserServiceError` mapping so the HTTP code is consistent
across every endpoint. The dashboard's `Users.tsx` page **mirrors the
same guards client-side** by disabling the offending buttons; this
keeps the operator from round-tripping a request that the server is
guaranteed to reject with 400.

**Routes** (mounted under `/api/v1/user` with `JWTAuth + RequireAdmin`,
except `/me` which is any-authenticated user):

| Method | Path | Purpose |
|---|---|---|
| GET    | `/user`        | List all users with each user's `device_count` |
| POST   | `/user`        | Create user `{name, is_admin}` |
| GET    | `/user/:id`    | Fetch one user |
| PUT    | `/user/:id`    | Partial update `{name?, is_admin?}` |
| DELETE | `/user/:id`    | Delete user + cascade-delete their devices |

**Name validation** (`isValidUserName` in
`services/api/internal/service/user_service.go`):

- Trimmed length 1..32 runes
- Each rune: unicode letter / digit / `_` / `-`
- Leading/trailing whitespace is silently trimmed (forgiving
  paste-from-clipboard behaviour)
- Internal whitespace (space / tab / newline) is rejected outright
  because it would corrupt the friendly-name path used by cameras,
  mqtt topics, etc.
- Unicode is fully supported (e.g. `小明`, `自己`)

**Cascade semantics on `DELETE /user/:id`:**

- The user row is removed AND every device they own is removed
  (`DELETE FROM devices WHERE user_id = :id`). Devices-first:
  if the device delete fails, the user row is still around and
  the admin can retry; the inverse order would orphan devices
  pointing at a now-missing user.
- Cameras are **not** cascaded. The `cameras.owner_id` column
  already drives list/get scoping, so a deleted user's cameras
  stay in the DB but become invisible to non-admin callers. An
  admin who wants the camera gone calls
  `DELETE /api/v1/cameras/:id` separately.
- `AccessKeyHash` is never exposed by the API. A deleted user's
  devices are removed from the table, which is enough to reject
  their JWTs via the existing revocation check.

**TOCTOU note on `Create` / `Update` rename:** the service does a
pre-check on `users.name` uniqueness, then issues the `INSERT` /
`UPDATE`. The DB `UNIQUE` index is the load-bearing defense — if
two admins race to register the same name, the loser sees a
unique-constraint violation, which `isUniqueViolation(err)` maps
back to `ErrNameTaken` and the handler turns into 409. Both SQLite
("UNIQUE constraint failed") and Postgres ("duplicate key value")
error strings are matched so the future PG migration is drop-in.

**Frontend surface:**

- `web/src/pages/Users.tsx` — the admin-only CRUD page (linked
  from `Layout.tsx` with an `adminOnly` flag and a "you" badge on
  the caller's own row).
- `web/src/api/user.ts` — `listUsers` / `createUser` / `updateUser` /
  `deleteUser` (typed against `web/src/types.ts`).
- The `me` endpoint (`GET /api/v1/user/me`) is unchanged; the
  dashboard's AuthContext already polls it for the "you" indicator
  and the admin guard.

**Tests:** `services/api/internal/service/user_service_test.go`
covers the validation paths (`isValidUserName` ascii + unicode +
whitespace + length boundaries; `normalizeUserName` trim + reject
internal whitespace) and the unique-violation detection
(`isUniqueViolation` against both SQLite and Postgres error
strings).

---

## Next Steps (Future Extensions)

- **Rule templating**: action bodies with Go templates
  (`{{.event.topic}}`, `{{.event.payload.status}}`) for dynamic
  payloads. Currently static.
- **Per-rule audit log**: persist `automation.fired` events to
  SQLite for a durable history (currently EventBus-only, in-process).
- **AI Layer**: a future subscriber on `*` could feed events into
  an LLM for anomaly detection or natural-language summaries.
- **Rule import/export**: backup and version-control rules as
  YAML/JSON.
