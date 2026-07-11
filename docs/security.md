# Home Datacenter — Security

> Threat model, hardening pass (2026-07-04), and residual risks.
> Companion to `ai-context.md` (architecture) and `api-documentation.md` (API).

This system is **internet-exposed** via Cloudflare Tunnel. The dashboard
and API hostnames are public, so the auth model is the only thing
standing between the internet and device control. There is no password
login — devices authenticate with an offline-issued AccessKey exchanged
for a 365-day JWT.

---

## Threat Model

| Asset | Exposure | Primary protection |
|-------|----------|--------------------|
| AccessKeys | Issued offline, hash-only in DB | 256-bit entropy; SHA-256 hash; never logged |
| JWT signing secret | Server config / env | Validated at boot; ≥32 char; not committed |
| Mosquitto broker | Internal Docker net only | `allow_anonymous=false` + password + ACL |
| SQLite DB | Bind-mounted volume, not exposed | Filesystem perms; not git-tracked |
| Dashboard | Public hostname | JWT-gated routes; admin-only MQTT page |
| API | Public hostname | JWT middleware + per-request revocation check |
| Camera ONVIF creds | AES-GCM ciphertext in SQLite | WS-Security PasswordDigest over SOAP; key = SHA-256(JWT_SECRET) |

---

## Hardening Pass (2026-07-04)

### 1. JWT secret validation at boot
`internal/config/config.go::validateJWTSecret` rejects empty, placeholder
(`your-secret-key`, `change-me`, `PLEASE_CHANGE_TO_A_LONG_RANDOM_SECRET`),
and <32-char secrets with `log.Fatal`. The committed `config.yaml` keeps
the placeholder; the real secret lives in `config.local.yaml` (gitignored)
or the `JWT_SECRET` env var. Verified: app exits on placeholder, boots on
real secret.

### 2. Mosquitto authentication + ACL
`deploy/mosquitto/mosquitto.conf` now has `allow_anonymous false`, a
`password_file`, and an `acl_file`. `deploy/mosquitto/aclfile` grants the
`home-datacenter` API account `readwrite home-datacenter/#` and denies
everything else (including `$SYS/#` writes). The API authenticates with
`MQTT_USERNAME` / `MQTT_PASSWORD` env vars.

> **Action required before first deploy:** create the password file:
> ```bash
> docker run --rm -it docker.m.daocloud.io/library/eclipse-mosquitto:2 \
>   mosquitto_passwd -c /tmp/passwd home-datacenter
> # then copy /tmp/passwd to deploy/mosquitto/passwd
> ```

### 3. Mosquitto not published to host
`compose.yaml` no longer publishes port `1883`. The API reaches the broker
on the internal Docker network as `mosquitto:1883`. A commented
`MQTT_BIND_PORT` escape hatch exists for local device testing.

### 4. Host port bindings restricted
`web:80` and `api:8080` are now bound to `127.0.0.1` by default. In
production behind the tunnel, prefer not publishing the API port at all.

### 5. WebSocket origin allowlist
`server.allowed_origins` config + `NewWebSocketHandlerWithOrigins` blocks
cross-site WebSocket hijacking (CSWSH) at the application layer. Empty
list (local dev) = permissive. Set the dashboard hostname in production.

### 6. HTTP security headers
`utils.applySecurityHeaders` adds `X-Content-Type-Options: nosniff`,
`X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
`Cache-Control: no-store` to every `/api/v1/*` response.

### 7. MQTT publish topic allowlist
`POST /api/v1/mqtt/publish` rejects topics outside `home-datacenter/` or
starting with `$`. Prevents a compromised admin token from writing
retained messages to broker control / third-party topics.

### 8. Bind endpoint enumeration fix
`/auth/bind` returns a single generic `"invalid credentials"` for all
failures (bad user_id, wrong key, revoked) instead of distinct messages.

### 9. Repo hygiene
- New root `.gitignore` covers `/data/`, `config.local.yaml`, `.env`,
  `*.exe`, build artifacts, node_modules.
- `git rm --cached` removed the previously-tracked SQLite DB, Mosquitto
  persistence, `cmd.exe`, and `config.local.yaml` from the index.
- `config.local.yaml` regenerated with a fresh 32-byte JWT secret.

### 10. Camera ONVIF authentication
Camera credentials (`onvif_user` / `onvif_pass`) are stored as
AES-GCM ciphertext via `utils.SecretBox` (key = `SHA-256(JWT_SECRET)`).
At runtime, ONVIF SOAP requests use **WS-Security UsernameToken with
PasswordDigest** (`SHA1(nonce + created + password)`) per the ONVIF
spec — not HTTP Basic Auth. This avoids sending the password in
cleartext over the LAN and is required by most Hikvision / Dahua
firmware (HTTP Basic Auth returns 401). Each request includes a
random 16-byte nonce and UTC timestamp to prevent replay attacks.
See `internal/camera/onvif.go` `wsseHeader`.

### 11. go2rtc public-path authentication (Phase 7)

The go2rtc container exposes its own HTTP API (`/api/streams`,
`/api/webrtc`, `/api/stream.m3u8`) without any built-in auth. If
the Cloudflare Tunnel forwards `/go2rtc/*` straight through, any
browser pointed at `https://cam.feiyemomo.top/go2rtc/api/streams`
can list every camera and pull a live frame without ever touching
home-api — bypassing JWT, admin gating, and the per-camera ACL.
This was the original state; the camera hostname was effectively
a public, unauthenticated webcam portal.

Mitigation: nginx's `auth_request` directive in `web/nginx.conf`
gates the `/go2rtc/` location with a sub-request to
`GET /api/v1/auth/verify` (a new endpoint on home-api that parses
the JWT, re-checks the device's `RevokedAt`, and returns 200/401).
Nginx forwards to go2rtc only on 2xx; on 401 the browser sees a
clean JSON `{"code":401,"message":"unauthorized"}` body.

Front-end changes:

- `web/src/api/client.ts` exports `authedFetch(input, init)` and
  `authHeaderFor()`. Plain `axios` calls already carry the JWT
  via the request interceptor, so only the two paths that bypass
  axios use these helpers:
  - `useWebRTCStream.ts`: `POST` of the SDP offer to `/api/webrtc`
  - `useHLSStream.ts`:    hls.js `xhrSetup` to attach the header to
    every segment/playlist request through nginx
- The helpers read the JWT from the same `localStorage` slot
  (`hd_token`) as the axios interceptor. No second source of truth.

Threat model additions:

| Asset | Exposure | Primary protection |
|-------|----------|--------------------|
| go2rtc public API | Tunnel `/go2rtc/*` | nginx `auth_request` → `/api/v1/auth/verify` → JWT parse + `RevokedAt` re-check |
| go2rtc live media (HLS segments, WebRTC RTP) | Tunnel `/go2rtc/*` | Same — verified once per request, gateway is the same |
| cam.feiyemomo.top | Public tunnel hostname | Auth is enforced at the proxy; an empty `Authorization` header yields 401 before the request ever reaches go2rtc |

Residual risk: nginx's `auth_request` is per-HTTP-request, not
per-segment. A busy dashboard can trigger hundreds of sub-requests
per second; `/auth/verify` is a primary-key lookup (single index
hit) so the cost is bounded, but a future cache layer must be
careful — a revoked device must stop streaming on the *next*
request, not after a TTL. Today: no cache, single-row DB hit, OK.

Residual risk: Cloudflare Tunnel does not enforce auth on its
side; if a future operator adds a non-nginx ingress (e.g. an
edge worker that calls go2rtc directly), they must re-implement
the JWT check. Mitigated by a doc note in `web/nginx.conf`.

### 12. Automation Engine security (Phase 5 + Phase 6)

The Automation Engine (`internal/automation/`) lets admin users create
rules that fire actions on EventBus events. Attack surfaces and
mitigations:

1. **MQTT action — topic injection.** A compromised admin could publish
   to `$SYS/broker/...` or third-party plugin topics. Mitigation: the
   engine rejects any MQTT topic outside the `home-datacenter/`
   namespace, identical to the `/api/v1/mqtt/publish` endpoint. The
   check runs at BOTH CRUD time (immediate feedback) and fire time
   (defence-in-depth). See `isAllowedMQTTTopic`.
2. **Webhook action — SSRF.** A compromised admin could point a webhook
   at `http://169.254.169.254/latest/meta-data/` (cloud metadata) or
   `http://localhost:8080/admin` (internal API). Mitigation: the engine
   resolves the webhook host and rejects private, loopback, link-local,
   and unspecified addresses. The check runs at fire time (not CRUD
   time) because DNS can change between create and fire. HTTPS is
   recommended but not enforced. See `assertPublicHost` / `isPublicIP`.
3. **Rule CRUD — privilege escalation.** All `/api/v1/automation/rules`
   endpoints require JWT + `middleware.RequireAdmin`. Non-admin users
   cannot create, list, or fire rules.
4. **Action storm — DoS via repeated firing.** A flapping camera or a
   noisy MQTT producer could fire thousands of webhooks per minute
   against a paid SaaS, exhausting quota or hiding real alerts.
   Mitigation: per-rule `Throttle` (cooldown + rate-per-min + dedup)
   caps the fire rate. Dropped events are counted in
   `/api/v1/automation/metrics` so an operator can spot the storm.
5. **Webhook — slow or non-responding target.** A webhook against a
   unreachable host would otherwise block the engine's fire goroutine
   until the OS-level timeout (minutes). Mitigation: per-action
   `timeout_ms` (default 5000ms) bounds each attempt. A non-2xx
   response (or 5xx) is retried with exponential backoff up to
   `retry_max` times. **4xx is permanent** — bad URL / wrong
   credentials are not retried (would never succeed).
6. **Cooldown abuse.** A misbehaving rule that has fired too often
   can be silenced with `POST /automation/rules/:id/cooldown` body
   `{seconds}` (admin only). This pins the rule's `lastFire` to
   `now - seconds`, effectively making it dormant for the requested
   window without deleting it.

Residual risk: DNS rebinding could race the SSRF check. Accepted
because the rule surface is admin-only and the home OS is single-user.

---

## §13. `/auth/bind` rate limiting

### Why
- AccessKeys are 256 bits — offline brute force is infeasible. But
  the live `/auth/bind` endpoint is still online-attackable: a
  determined attacker can grind it.
- The limiter is the second line of defense after the keyspace.
  It blunts attack volume and keeps the auth-failure log from
  drowning out real signal.

### Mechanism
- `internal/middleware/ratelimit.go` exposes `IPLimiter`:
  in-process token-bucket per source IP, with a 5-minute background
  GC that evicts entries idle for >10 minutes.
- Defaults: `rps=0.1`, `burst=5` → 5 quick attempts, then
  1 every 10s. Configurable via `auth.rate_limit.*` in
  `configs/config.yaml`.
- Rejected requests get `429 Too Many Requests` with the **same
  body as the 401 path** (`{"code":429,"message":"invalid
  credentials","data":null}`), so a probing attacker cannot
  distinguish throttling from credential failure.

### Limitations (acknowledged)
- **In-process state.** The limiter is per home-api instance. If
  the deployment is horizontally scaled, swap the storage for
  Redis (TODO). For the home use case (single instance) this is
  sufficient.
- **c.ClientIP() trust.** Behind Cloudflare Tunnel, `c.ClientIP()`
  reads `CF-Connecting-IP` only when the request actually came
  from the tunnel; nginx must forward the original IP. Verify
  with `docker logs home-api | grep "401"` that the X-Forwarded-For
  chain terminates at a trusted proxy, otherwise an attacker can
  spoof their IP via `X-Forwarded-For` and reset the bucket
  arbitrarily.
- **Per-IP, not per-account.** A botnet with 1000 IPs gets
  1000 × burst tokens. The 256-bit keyspace still makes the
  attack cost-prohibitive (10^77 attempts), but the limiter alone
  is not a brute-force defense — it just slows the attack down
  to a trickle. The keyspace is the real defense.

### Verification
```
for ($i=1; $i -le 12; $i++) {
  Invoke-WebRequest -Uri "http://localhost:8080/api/v1/auth/bind" `
    -Method POST -ContentType "application/json" `
    -Body '{"user_id":1,"access_key":"wrong"}' -UseBasicParsing
}
# Expect: 1-5 → 401, 6-12 → 429
```

---

## Residual Risks (accepted, not yet fixed)

1. **No audit log.** Bind and revoke events are not persisted beyond the
   device row's `LastLoginAt` / `RevokedAt` timestamps.
2. **365-day JWTs.** Long-lived; revocation is immediate (per-request DB
   check on `RevokedAt`), but there is no short-lived + refresh rotation.
3. **Permissive WebSocket origin in dev.** `allowed_origins: []` accepts
   any origin — fine on localhost, must be populated for production.
4. **MQTT publish is admin-by-convention, not enforced server-side.** The
   `/mqtt/publish` route is JWT-gated but does not itself check `IsAdmin`;
   admin enforcement lives in the dashboard route guard. If a non-admin
   JWT calls the endpoint directly it will succeed. Consider adding an
   `AdminOnly` middleware.
5. **`core.autocrlf=true`** on Windows means gofmt may locally flag CRLF
   in committed-then-rechecked files; the canonical line ending is LF.

---

## Operational Checklist (before going live)

- [ ] `JWT_SECRET` set in `.env` (≥32 chars, from `openssl rand -hex 32`)
- [ ] `deploy/mosquitto/passwd` created for the `home-datacenter` account
- [ ] `MQTT_PASSWORD` set in `.env` matching the passwd entry
- [ ] `server.allowed_origins` populated with the dashboard hostname in
      `config.yaml` (or via env)
- [ ] Cloudflare Tunnel ingress set to the `dashboard` + `api` hostnames
- [ ] `camera.webrtc_public_base` set to the tunnel hostname (e.g.
      `https://cam.feiyemomo.top`) or `http://localhost:1984` for
      local-only access
- [ ] `docker compose up -d` and verify `home-api` logs show
      `mqtt connected` and `server started on :8080`
- [ ] Bind a device, hit `/user/me`, confirm 200
- [ ] Revoke the device, confirm next request 401s

---

**Last Updated:** 2026-07-05 (Phase 7: nginx `auth_request` + `/api/v1/auth/verify` + `authedFetch`/`authHeaderFor` to gate `cam.feiyemomo.top/go2rtc/*` behind JWT)
