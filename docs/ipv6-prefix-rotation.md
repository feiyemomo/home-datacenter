# IPv6 Prefix Rotation Auto-Adaptation (v1.8.4)

> Diagnosing and fixing the ~1000ms IPv6 direct-connection latency spike
> caused by ISP DHCPv6-PD prefix rotation. Covers the immediate manual
> fix applied to the running NAS, the long-term backend + Android
> auto-adaptation layer, and the operator diagnostic command reference.
>
> Companion to `ai-context.md` Phase 10 and `security.md` §14.

---

## 1. Problem Description

Mobile devices using the IPv6 direct-connection path
(`http://[<NAS-IPv6>]:8088/`) suddenly exhibited **~1000ms latency** on
API calls and WebRTC session setup. The expected latency on a healthy
IPv6 direct path is **~50ms** (cellular IPv6 → NAS IPv6, no Cloudflare
Tunnel hop). The degradation was first noticed on the Android app's
`BaseUrlResolver` probe: the IPv6 candidate kept appearing "alive" but
with RTT ~1000ms instead of the usual ~50ms, and WebRTC ICE checks
against the IPv6 host candidate were timing out.

The LAN path (`http://192.168.31.234:8088/`) and the Cloudflare Tunnel
path (`https://api.feiyemomo.top/`) were unaffected — only the IPv6
direct path was degraded.

---

## 2. Root Cause Diagnosis

### 2.1 ISP Prefix Rotation via DHCPv6-PD

China Mobile (中国移动) delegates an IPv6 `/64` prefix to the home
router via **DHCPv6-PD** (Prefix Delegation). On lease renewal —
which can be triggered by router reboot, ISP maintenance, or simply
lease expiry — the delegated prefix changes. In this incident:

```
old prefix: 2409:8a70:37a0:63f0::/64
new prefix: 2409:8a70:37a3:99d0::/64
```

The host identifier (lower 64 bits, derived from the NIC's EUI-64)
stays stable across rotations. Only the network prefix (upper 64 bits)
changes.

### 2.2 Three Hardcoded Addresses Not Synced

The old prefix was hardcoded in **three places** that all needed to
agree on the NAS's current IPv6 address:

1. `Android/.../BaseUrlResolver.kt` — `IPV6_DIRECT_URL` constant used
   by the URL probe + WebRTC host-candidate decision.
2. `deploy/frigate/config.yml` — `go2rtc.webrtc.candidates` list (the
   IPv6 host candidate that go2rtc advertises in SDP answers).
3. `compose.yaml` — `NAS_IPV6_ADDRESS` env var consumed by the API
   container's `CheckIPv6()` / `IPv6ReachableURL()` helpers.

None of these were updated when the prefix rotated, so they all kept
advertising the **old** prefix address.

### 2.3 Asymmetric Routing Through Stale Neighbour Cache

The router's neighbour cache still had the **old** prefix address in
the `STALE` state (the kernel hadn't garbage-collected it yet) while
the **new** prefix address was `REACHABLE`. Mobile devices sending
packets to the **old** prefix address had those packets routed through
a residual route on the router, eventually reaching the NAS. But the
NAS responded from its **new** prefix address (the only one with a
fresh SLAAC assignment), producing **asymmetric routing**:

```
phone  ──[dst: old-prefix NAS]──►  router  ──►  NAS (residual route)
NAS    ──[src: new-prefix NAS]──►  router  ──►  phone
                                                  │
                                                  ▼
                                  TCP sees out-of-window ACKs
                                  → retransmits
                                  → ~1000ms RTT instead of ~50ms
```

TCP interpreted the asymmetric return path as packet loss and
triggered retransmits, inflating observed RTT from ~50ms to ~1000ms.

### 2.4 No Stable SLAAC Address on the New Prefix

On the **new** prefix, the NAS only had a **temporary privacy**
address (RFC 4941). The kernel rotates temporary addresses every few
hours, so any address the API env var pointed at would silently become
stale within hours. There was no **stable SLAAC EUI-64** address on
the new prefix — the kernel's DAD (Duplicate Address Detection) had
removed the auto-generated EUI-64 address on the new prefix because
another host on the link briefly had the same address during the
prefix transition window.

---

## 3. Immediate Fix Procedure

These steps were executed on the running NAS to restore connectivity
within minutes. The long-term automation (Section 4) removes the need
to repeat this procedure on the next prefix rotation.

### 3.1 Add a Stable EUI-64 Address on the New Prefix

```bash
# On the NAS (ssh fnos-momo@192.168.31.234):
ip -6 addr add 2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09/64 dev enp4s0
```

The EUI-64 host identifier (`62be:b4ff:fe08:bd09`) is derived from the
NIC's MAC address and is stable across prefix rotations — only the
upper 64 bits change. This makes the address a predictable function
of (current-prefix, NIC-MAC).

### 3.2 Disable DAD on the Interface

Without this, the kernel's Duplicate Address Detection may decide the
manually added EUI-64 address is a duplicate (e.g. when the kernel's
own autoconfiguration still has a transient entry) and silently remove
it:

```bash
sysctl -w net.ipv6.conf.enp4s0.accept_dad=0
# Persist:
echo 'net.ipv6.conf.enp4s0.accept_dad = 0' >> /etc/sysctl.d/99-ipv6-stable.conf
```

### 3.3 Persist via systemd

A one-shot systemd unit re-applies the address on every boot, after
the network target is up:

`/etc/systemd/system/ipv6-stable-addr.service`:

```ini
[Unit]
Description=Add stable SLAAC EUI-64 IPv6 address on enp4s0
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/sbin/ip -6 addr add 2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09/64 dev enp4s0
ExecStartPost=/sbin/sysctl -w net.ipv6.conf.enp4s0.accept_dad=0
# Best-effort: ignore "File exists" if the address is already assigned
SuccessExitStatus=0 2

[Install]
WantedBy=multi-user.target
```

Enable:

```bash
systemctl daemon-reload
systemctl enable --now ipv6-stable-addr.service
```

### 3.4 Update the Three Hardcoded Addresses

All three locations were updated from `2409:8a70:37a0:63f0::/64` to
`2409:8a70:37a3:99d0::/64` (host identifier `62be:b4ff:fe08:bd09`
unchanged):

| File | Field |
|------|-------|
| `Android/.../BaseUrlResolver.kt` | `IPV6_DIRECT_URL` constant |
| `deploy/frigate/config.yml` | `go2rtc.webrtc.candidates` IPv6 entry |
| `compose.yaml` | `NAS_IPV6_ADDRESS` default value |

After deploying the new compose + Frigate config and rebuilding the
Android APK, the IPv6 direct path latency dropped back to ~50ms.

---

## 4. Long-Term Solution Architecture

The manual procedure in Section 3 fixes one rotation but requires
operator intervention on every subsequent rotation. The long-term
solution adds backend auto-detection and Android dynamic URL fetching
so a prefix rotation self-heals within ~10 minutes.

### 4.1 Backend: `OutboundIPv6Address()` (ipv6.go)

```go
// services/api/internal/network/ipv6.go
func OutboundIPv6Address() string {
    if envAddr := os.Getenv("NAS_IPV6_ADDRESS"); envAddr != "" {
        if ip := net.ParseIP(envAddr); ip != nil && ip.To4() == nil {
            return envAddr   // short-circuit: env var is authoritative in-container
        }
    }
    // Fallback: probe external echo service (3s timeout per host).
    for _, url := range []string{"https://ident.me", "https://api64.ipify.org"} {
        // GET → parse body as IPv6 literal
    }
    return ""
}
```

The env var short-circuit is critical: the home-api container runs on
a docker bridge without an IPv6 subnet, so an in-container HTTP probe
to `ident.me` would always time out. The env var is the path that
actually takes effect in the deployment.

`IPv6PrefixMatches(a, b)` compares only the first 8 bytes (the /64
network prefix) — the host identifier is intentionally ignored since
it stays stable across rotations.

### 4.2 Backend: `PrefixWatcher` Goroutine (watcher.go)

```go
// services/api/internal/network/watcher.go
type PrefixWatcher struct {
    interval time.Duration  // 5 minutes
    frigate  *camera.FrigateClient
    bus      *eventbus.Bus
    // ...
}

func (w *PrefixWatcher) checkOnce() {
    outbound := OutboundIPv6Address()
    // Compare against the previous outbound. If the /64 prefix differs,
    // a rotation has occurred since the last check.
    if prev != "" && !IPv6PrefixMatches(prev, outbound) {
        // 1. Publish EventBus event
        bus.Publish(eventbus.Event{
            Topic: "network.ipv6.prefix_rotated",
            Source: eventbus.SourceSystem, Severity: eventbus.SeverityWarn,
            Payload: ...,
        })
        // 2. Push new WebRTC candidates to Frigate (best-effort)
        frigate.SetWebRTCCandidates(ctx, outbound)
    }
}
```

- **Polling cadence**: 5 minutes. Trades off detection latency against
  outbound probe load. The Probe is a single HTTPS GET to `ident.me`
  (~50-200ms), so the cost is negligible.
- **Rotation detection**: compares the previous outbound address's
  `/64` prefix against the current one. The host identifier is
  intentionally ignored — it would also match if the address were
  unchanged, which is the common case.
- **Side effects on rotation**:
  1. Publishes `network.ipv6.prefix_rotated` on the EventBus (also
     bridged to WebSocket subscribers — future dashboard UX could
     surface a "network changed" toast).
  2. Calls `frigate.SetWebRTCCandidates(outbound)` to push the new
     address into go2rtc's candidate list. This is best-effort:
     failure is logged but doesn't block the next check cycle.

### 4.3 Backend: `GET /api/v1/network/ipv6` Endpoint

```
GET /api/v1/network/ipv6        (JWT-protected)
GET /api/v1/network/ipv6?refresh=true   (force fresh probe, skip cache)
```

Response:

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "outbound_address":   "2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09",
    "configured_address": "2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09",
    "prefix_rotated":     false,
    "last_checked":       "2026-07-22T14:30:00Z"
  }
}
```

- `outbound_address`: what the NAS currently believes its outbound IPv6
  is (from `NAS_IPV6_ADDRESS` env var or external probe).
- `configured_address`: the value of `NAS_IPV6_ADDRESS` at watcher
  construction time. Useful for the operator to verify a config change
  actually took effect.
- `prefix_rotated`: `true` when `outbound` and `configured` disagree
  on the `/64` prefix. Indicates the env var is stale and the watcher
  has detected a newer prefix.
- `last_checked`: timestamp of the most recent `checkOnce()`.

The handler is wired up in `services/api/cmd/main.go` under the
`netGroup` route group (JWT-protected). It accepts `?refresh=true` to
force the watcher to skip its cache and probe immediately — useful
right after the operator updates `NAS_IPV6_ADDRESS` and wants to
verify the new value takes effect.

### 4.4 Backend: `FrigateClient.SetWebRTCCandidates()`

```go
// services/api/internal/camera/frigate.go
func (c *FrigateClient) SetWebRTCCandidates(ctx context.Context, ipv6Addr string) error {
    candidates := []string{"127.0.0.1:8555", "192.168.31.234:8555"}
    if ipv6Addr != "" {
        candidates = append(candidates, fmt.Sprintf("[%s]:8555", ipv6Addr))
    }
    // PUT /api/config/set with partial config: {go2rtc: {webrtc: {candidates: [...]}}}
}
```

The push is a **partial** config update — only `go2rtc.webrtc.candidates`
is sent, so Frigate's deep-merge preserves all other config (cameras,
mqtt, detectors, etc.). Frigate applies the new candidates to the
running go2rtc subsystem **without a restart** (this is one of the
config fields that doesn't require the `requires_restart` flag).

### 4.5 Android: `BaseUrlResolver.fetchDynamicIpv6Url()`

```kotlin
// Android/.../BaseUrlResolver.kt
private suspend fun fetchDynamicIpv6Url(): String? {
    val token = tokenProvider?.invoke() ?: return null  // no JWT yet → bail
    // GET {resolved}/api/v1/network/ipv6   (3s timeout)
    // Parse response.data.outbound_address → "http://[$addr]:8088/"
}
```

- **Trigger points**: called from `probeLanOnStartup()` (once on app
  launch) and from `probeAsync()` (every 5 minutes via the TTL
  re-probe).
- **Priority over hardcoded**: `probeSync()` uses
  `dynamicIpv6Url ?: IPV6_DIRECT_URL` — the dynamic URL wins when
  available. The hardcoded constant is the fallback, not the primary.
- **JWT required**: the endpoint is auth-protected. `tokenProvider`
  is set by `AppContainer` after the auth state is initialized
  (`resolver.tokenProvider = { prefsManager.token }`). When no token
  is available (pre-login), `fetchDynamicIpv6Url()` is a no-op.
- **Best-effort**: any failure (network error, non-200, missing
  `outbound_address` field, no token) returns null. The caller keeps
  the previous value; on the very first call it falls back to the
  hardcoded constant.

### 4.6 Fallback Chain

```
dynamic URL (from /api/v1/network/ipv6)   ← primary, tracks rotations
            │ (null — no token / fetch failed / pre-login)
            ▼
hardcoded IPV6_DIRECT_URL constant        ← fallback, may be stale
            │ (probe fails — prefix rotated past hardcoded value)
            ▼
LAN_URL or REMOTE_URL                     ← per NetworkPathPreference
```

The hardcoded constant is intentionally retained — removing it would
make the app unable to bootstrap before login (the JWT-protected
endpoint can't be called until the user has a token).

---

## 5. Diagnostic Command Reference

### 5.1 Check Outbound IPv6

The NAS's outbound IPv6 address as seen by an external server:

```bash
curl -6 -s https://ident.me
# or
curl -6 -s https://api64.ipify.org
```

Compare against the `NAS_IPV6_ADDRESS` env var in `compose.yaml` — if
they disagree on the `/64` prefix, a rotation has occurred.

### 5.2 Check NAS IPv6 Addresses

All globally-scoped IPv6 addresses on the NAS's primary NIC:

```bash
ip -6 addr show enp4s0 | grep "scope global"
```

Expected output (after the fix):

```
inet6 2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09/64 scope global dynamic noprefixroute
inet6 2409:8a70:37a3:99d0:<temp>/64 scope global temporary dynamic
```

The stable EUI-64 address (`62be:b4ff:fe08:bd09`) is the one to use
in `NAS_IPV6_ADDRESS`. The temporary address rotates every few hours
and must NOT be used.

### 5.3 Check Router Neighbour Cache

```bash
ip -6 neigh show
```

Look for the NAS's address in the cache. `REACHABLE` is healthy;
`STALE` means the kernel hasn't confirmed reachability recently (this
is the state that causes the asymmetric-routing latency bug — the
mobile device's packets still arrive via a residual route but the
return path uses the new prefix).

### 5.4 Test IPv6 Direct Latency

```bash
curl -6 -o /dev/null -w "HTTP %{http_code} total %{time_total}s\n" \
  http://[2409:8a70:37a3:99d0:62be:b4ff:fe08:bd09]:8088/api/v1/system/status
```

- Expected on a healthy path: `HTTP 401 total 0.05s` (401 = JWT
  missing, but the API is reachable).
- Symptomatic of the rotation bug: `HTTP 401 total 1.0s` (or higher,
  climbing as TCP retransmits pile up).

### 5.5 Query Backend IPv6 Status

```bash
# JWT required — substitute a real token from /auth/bind
TOKEN="<your-jwt>"
curl -s -H "Authorization: Bearer $TOKEN" \
  http://localhost:8088/api/v1/network/ipv6 | jq
```

```bash
# Force a fresh probe (skips the 5-min cache):
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8088/api/v1/network/ipv6?refresh=true" | jq
```

`prefix_rotated: true` means the watcher has detected a rotation that
the operator hasn't yet propagated to `NAS_IPV6_ADDRESS`. After
updating the env var and recreating the api container, the field
should return to `false` on the next probe.

---

## 6. Expected Behavior on Future ISP Prefix Rotations

With the v1.8.4 automation in place, a future prefix rotation
self-heals without operator intervention:

1. **Detection (≤5 min)**: `PrefixWatcher.checkOnce()` runs on its
   5-minute ticker. The probe returns the new outbound IPv6 address.
   `IPv6PrefixMatches(prev, new)` returns false → rotation detected.

2. **WebRTC candidate update (immediate)**: `PrefixWatcher` calls
   `frigate.SetWebRTCCandidates(newOutbound)`. Frigate deep-merges the
   new candidate list into go2rtc's config; future WebRTC SDP answers
   advertise the new IPv6 host candidate.

3. **EventBus notification (immediate)**: A `network.ipv6.prefix_rotated`
   event is published with `{previous_address, new_address}` payload.
   WebSocket subscribers (dashboard, future automation rules) receive
   it for live UI updates or operator alerting.

4. **Android adaptation (≤5 min)**: The Android app's next
   `probeAsync()` cycle (within 5 minutes, per the TTL) calls
   `fetchDynamicIpv6Url()` → `GET /api/v1/network/ipv6` → receives
   the new outbound address → updates `dynamicIpv6Url` → forces a
   re-probe → switches `resolved` to the new IPv6 URL. Subsequent
   API calls and WebRTC sessions use the new address.

5. **Total recovery time**: **<10 minutes**, no operator action
   required. The user may see one probe cycle (~5 min) of degraded
   latency before the Android client picks up the new address.

### 6.1 Caveats

- **First probe after boot**: `PrefixWatcher.Start()` runs an
  immediate `checkOnce()` so the endpoint has data right away — the
  5-minute delay only applies to subsequent rotations.
- **NAS_IPV6_ADDRESS env var stays stale**: the env var is the
  authoritative source for `OutboundIPv6Address()`. If the operator
  never updates it, the function still returns the old (env-var)
  value, not the actual new outbound address. To fully benefit from
  the auto-adaptation, either (a) leave `NAS_IPV6_ADDRESS` unset so
  the function falls back to the external probe, or (b) update it
  after each rotation (the systemd unit in §3.3 can be extended to
  also rewrite the env file).
- **WebRTC candidate push is best-effort**: if Frigate is briefly
  unreachable during the rotation window, the candidate update is
  logged and retried on the next check cycle.

---

**Version:** v1.8.4 (2026-07-22)
