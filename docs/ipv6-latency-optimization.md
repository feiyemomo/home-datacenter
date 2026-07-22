# IPv6 Direct Connection Latency Optimization (v1.8.5 / v1.6.28)

> Further reducing the ~500ms residual latency on the IPv6
> direct-connection path after the v1.8.4 prefix-rotation fix. Covers
> the diagnosis that isolated the cellular RTT as the dominant cost,
> the nginx upstream keepalive and OkHttp connection-pool + warmup
> optimizations that save one RTT per reused call, and the explicitly
> skipped docker IPv6 direct path with its rationale.
>
> Companion to `ai-context.md` Phase 11 and
> `docs/ipv6-prefix-rotation.md`.

---

## 1. Problem Description

After the v1.8.4 prefix-rotation fix, the IPv6 direct path
(`http://[<NAS-IPv6>]:8088/`) recovered from the ~1000ms spike down to
**~500ms** on the cellular network. That is a major improvement, but
still far above the ~50ms ceiling observed before the rotation
incident and well above the LAN path (~7ms).

The 500ms floor is acceptable for status polling but adds up across
the app's burst of API calls (camera list → camera detail → event
stream) and slows the cold-start `BaseUrlResolver` probe. The goal of
v1.8.5 / v1.6.28 is to cut that 500ms closer to the theoretical
minimum of one cellular RTT (~250ms) by **reusing connections**
instead of paying the full TCP handshake + HTTP roundtrip on every
call.

---

## 2. Root Cause Diagnosis

A breakdown of the ~500ms per-call latency was measured end-to-end
from the Android client through nginx to the Go backend:

| Segment | Latency | Notes |
|---------|---------|-------|
| NAS localhost (loopback) | ~1.3ms | In-process baseline |
| Dev machine LAN | ~7ms | Healthy LAN ceiling |
| ICMP RTT (LAN path) | ~2ms | NAS ↔ dev machine |
| ICMP RTT (cellular path) | ~250ms | NAS ↔ cellular phone |
| docker-proxy IPv6→IPv4 translation | ~0ms | **Not a bottleneck** |
| nginx + Go backend processing | ~1ms | Application layer |

### 2.1 Cellular RTT Dominates

Summing the segments on the cellular path:

```
~500ms ≈ 2 × ~250ms (cellular RTT)
         ├─ 1 × RTT  TCP handshake (SYN / SYN-ACK / ACK)
         └─ 1 × RTT  HTTP request/response roundtrip
```

The NAS-side processing (nginx + Go, ~1ms) and the docker-proxy
translation (~0ms) are negligible compared to the cellular RTT. **The
NAS is NOT the bottleneck.** Any optimization on the NAS host itself
would yield sub-millisecond gains lost in the noise of a 250ms RTT.

### 2.2 docker-proxy IPv6→IPv4 Translation Is Free

A candidate optimization was to enable native IPv6 on the docker
bridge so the API container could listen on IPv6 directly, skipping
the host-level docker-proxy that forwards `[NAS-IPv6]:8088` →
`api:8080` (IPv4). Measurement showed the proxy adds **~0ms** of
measurable overhead — it is a kernel-level NAT/forward, not a
userspace hop. Enabling docker IPv6 would require reconfiguring the
daemon, the bridge subnet, and the compose network, all for zero
measurable gain. **Skipped.**

### 2.3 The Effective Lever: Connection Reuse

Since the cellular RTT is irreducible (physics of the radio link),
the only way to subtract a full RTT from a call is to **skip the TCP
handshake** by reusing an already-established connection. A reused
keep-alive connection pays only the HTTP roundtrip (~250ms) instead
of handshake + roundtrip (~500ms).

Two layers must cooperate for this to work end-to-end:

1. **nginx → Go backend**: the upstream keepalive pool so nginx
   reuses connections to `api:8080` instead of opening a new one per
   request.
2. **Android → nginx**: the OkHttp `ConnectionPool` so the client
   reuses connections to nginx instead of opening a new one per
   request, plus a `warmupConnection()` call that pre-establishes the
   TCP connection during the URL probe so the first real API call
   doesn't pay the handshake.

---

## 3. Optimization Solutions

### 3.1 nginx Upstream Keepalive (Backend v1.8.5)

File: `web/nginx.conf`

Added a named upstream block with a keepalive pool and switched the
`/api/` location to use it:

```nginx
upstream api_backend {
    server api:8080;
    keepalive 32;
}

server {
    # ...
    location /api/ {
        proxy_pass http://api_backend;
        proxy_set_header Connection "";   # enable keep-alive to backend
        # ...
    }
}
```

- `keepalive 32` keeps up to 32 idle connections cached per worker,
  ready to be reused for the next request to `api:8080`.
- `proxy_set_header Connection ""` is required: without it nginx
  sends `Connection: close` to the upstream by default, which would
  defeat the keepalive pool.
- The WebSocket location `/api/v1/ws` is intentionally **unchanged** —
  it still uses `proxy_pass http://api:8080` with
  `Connection "upgrade"`. WebSocket is a long-lived single connection;
  the keepalive pool doesn't help it, and the `Connection ""` header
  would break the upgrade handshake.

**Impact**: on LAN (~1ms RTT) the saving is sub-millisecond and not
user-perceptible; on the cellular path it's a proper-practice cleanup
that compounds with the client-side pool below. The main benefit is
removing nginx as a per-request connection opener on the backend side.

### 3.2 OkHttp ConnectionPool + Warmup (Android v1.6.28)

Files:
- `Android/.../data/api/NetworkFactory.kt`
- `Android/.../util/BaseUrlResolver.kt`

**ConnectionPool** — `NetworkFactory.kt` now explicitly configures the
pool rather than relying on OkHttp defaults:

```kotlin
.connectionPool(ConnectionPool(5, 5, TimeUnit.MINUTES))
```

- 5 max idle connections, 5-minute keep-alive timeout. Adequate for
  the app's API surface (one host, a handful of concurrent calls).
- `protocols(listOf(okhttp3.Protocol.HTTP_1_1))` is **retained**.
  HTTP/2 over cleartext (h2c) was considered and deliberately NOT
  enabled: h2c negotiation adds complexity and edge cases on
  mobile networks (some carriers' middleboxes mishandle the
  Upgrade: h2c flow), and the primary win here is TCP reuse, which
  HTTP/1.1 keep-alive already delivers. Stability over throughput.
- All timeout configs are unchanged (connect 30s, read 60s, write
  60s, call 90s).

**Warmup** — `BaseUrlResolver.kt` gained a `warmupConnection(url)`
method that fires a `HEAD /api/v1/system/status` against the
just-resolved base URL with short (3s) timeouts, using
`client.newBuilder()` so it doesn't pollute the main client's state:

```kotlin
private fun warmupConnection(url: String) {
    val warmClient = client.newBuilder()
        .connectTimeout(3, TimeUnit.SECONDS)
        .readTimeout(3, TimeUnit.SECONDS)
        .writeTimeout(3, TimeUnit.SECONDS)
        .build()
    val request = Request.Builder()
        .url(url.trimEnd('/') + "/api/v1/system/status")
        .head()
        .build()
    // fire-and-forget; result is best-effort
}
```

It is invoked from `probeSync()` inside the `if (changed) { ... }`
block, immediately after `onUrlChanged?.invoke(changed)`. The point
is to **pre-establish the TCP connection** (and TLS, if any) during
the probe phase, so the first real API call the app makes on the new
base URL reuses the warm connection instead of paying the full
handshake.

- **Best-effort**: any failure (timeout, refused, DNS) is swallowed.
  The warmup is a hint, not a gate; if it fails, the first real call
  just falls back to the normal handshake path.
- **No new imports**: `okhttp3.Request` and `TimeUnit` were already
  imported in the file.

### 3.3 docker IPv6 Direct (Skipped)

Diagnosis in §2.2 showed docker-proxy's IPv6→IPv4 translation adds
~0ms — it is not a bottleneck. Enabling native docker IPv6 would
require:

- Reconfiguring the docker daemon (`/etc/docker/daemon.json` with
  `ipv6: true` and a fixed `fixed-cidr-v6`).
- Adding an IPv6 subnet to the compose network.
- Adding IPv6 addresses to each service (or relying on IPv6-only
  service DNS, which has its own quirks).
- Re-validating that the firewall / `ip6tables` rules still isolate
  services correctly.

The implementation risk (network reconfiguration, potential loss of
IPv4 connectivity during testing, firewall re-audit) is **higher
than the zero measurable benefit**. Documented as explicitly skipped
so a future maintainer doesn't re-investigate the same path.

---

## 4. Files Changed

### Backend (`home-datacenter`)

| File | Change |
|------|--------|
| `web/nginx.conf` | Added `upstream api_backend` block with `keepalive 32`; switched `/api/` `proxy_pass` to `http://api_backend`; added `proxy_set_header Connection ""`. WebSocket `/api/v1/ws` location left unchanged. |
| `docs/ipv6-latency-optimization.md` | New — this document. |
| `docs/ai-context.md` | Added Phase 11 (v1.8.5) section; updated Last Updated line. |
| `README.md` | Added v1.8.5 changelog entry. |

### Android

| File | Change |
|------|--------|
| `app/build.gradle.kts` | `versionCode` 70 → 71; `versionName` `"1.6.27"` → `"1.6.28"`. |
| `app/src/main/java/com/homedatacenter/app/data/api/NetworkFactory.kt` | Explicit `.connectionPool(ConnectionPool(5, 5, TimeUnit.MINUTES))`. |
| `app/src/main/java/com/homedatacenter/app/util/BaseUrlResolver.kt` | Added `warmupConnection(url)` method; invoked from `probeSync()` on URL change. |

---

## 5. Expected Effect

On the cellular IPv6 path (~250ms RTT), the optimizations compound:

| Call | Before (v1.8.4) | After (v1.8.5 / v1.6.28) | Saving |
|------|-----------------|--------------------------|--------|
| First API call after probe | ~500ms (handshake + HTTP) | ~250ms (HTTP only — warmup pre-established TCP) | ~250ms |
| Subsequent calls (within 5min pool TTL) | ~500ms each | ~250ms each | ~250ms per call |
| LAN path | ~7ms | ~6-7ms | <1ms (negligible) |

- **First call**: the `warmupConnection()` call during `probeSync()`
  pays the TCP handshake in the background. The first real API call
  reuses the warm connection and pays only the HTTP roundtrip.
- **Subsequent calls**: the OkHttp `ConnectionPool` (5-min TTL)
  keeps the connection to nginx alive; the nginx `keepalive 32`
  upstream pool keeps the connection to the Go backend alive. Both
  ends reuse, so only the HTTP roundtrip is paid.
- **LAN path**: RTT is already ~2ms, so saving one RTT is
  sub-millisecond and not user-perceptible. The optimization is a
  no-op on the LAN experience.

### 5.1 What Is NOT Improved

- **ICMP RTT** is physics; nothing in software changes it.
- **docker-proxy translation overhead** was already ~0ms; skipping it
  would yield nothing measurable.
- **WebSocket `/api/v1/ws`**: unchanged. It's a single long-lived
  connection; the handshake cost is amortized over the session
  lifetime.

### 5.2 Verification

After deployment (Task 5 of the spec):

- nginx config in the running container verified to contain
  `upstream api_backend` + `keepalive 32`.
- API latency on LAN stable at 7-10ms (no regression from the
  keepalive change).
- Android APK v1.6.28 (89MB) built and pushed to NAS releases;
  `/api/v1/release/latest` returns `version_name=1.6.28`,
  `version_code=10628`.

---

## 6. Companion Documentation

- **`docs/ipv6-prefix-rotation.md`** — v1.8.4 fix for the underlying
  ~1000ms spike that this v1.8.5 optimization builds on top of. Read
  that first if the IPv6 path is broken (no route, wrong prefix);
  this document only addresses the residual latency floor on a
  healthy IPv6 path.
- **`docs/ai-context.md`** — Phase 11 (v1.8.5) section summarizes
  this work for AI context. Phase 10 covers the prefix-rotation fix.
- **`docs/security.md`** §14 — IPv6 security considerations (the
  direct path bypasses Cloudflare Tunnel's DDoS / TLS termination).

---

**Version:** v1.8.5 / v1.6.28 (2026-07-22)
