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

---

## Residual Risks (accepted, not yet fixed)

1. **No rate limiting on `/auth/bind`.** A 256-bit key makes online brute
   force infeasible, but a limiter (`golang.org/x/time/rate`) is still
   worth adding to blunt traffic and log noise.
2. **No audit log.** Bind and revoke events are not persisted beyond the
   device row's `LastLoginAt` / `RevokedAt` timestamps.
3. **365-day JWTs.** Long-lived; revocation is immediate (per-request DB
   check on `RevokedAt`), but there is no short-lived + refresh rotation.
4. **Permissive WebSocket origin in dev.** `allowed_origins: []` accepts
   any origin — fine on localhost, must be populated for production.
5. **MQTT publish is admin-by-convention, not enforced server-side.** The
   `/mqtt/publish` route is JWT-gated but does not itself check `IsAdmin`;
   admin enforcement lives in the dashboard route guard. If a non-admin
   JWT calls the endpoint directly it will succeed. Consider adding an
   `AdminOnly` middleware.
6. **`core.autocrlf=true`** on Windows means gofmt may locally flag CRLF
   in committed-then-rechecked files; the canonical line ending is LF.

---

## Operational Checklist (before going live)

- [ ] `JWT_SECRET` set in `.env` (≥32 chars, from `openssl rand -hex 32`)
- [ ] `deploy/mosquitto/passwd` created for the `home-datacenter` account
- [ ] `MQTT_PASSWORD` set in `.env` matching the passwd entry
- [ ] `server.allowed_origins` populated with the dashboard hostname in
      `config.yaml` (or via env)
- [ ] Cloudflare Tunnel ingress set to the `dashboard` + `api` hostnames
- [ ] `docker compose up -d` and verify `home-api` logs show
      `mqtt connected` and `server started on :8080`
- [ ] Bind a device, hit `/user/me`, confirm 200
- [ ] Revoke the device, confirm next request 401s

---

**Last Updated:** 2026-07-04
