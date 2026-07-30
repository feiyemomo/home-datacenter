# Network Policy Review & Reusable Prompt Spec

## Why

The user needs (1) a reusable prompt at the project root capturing production environment access so future sessions don't need to re-collect SSH credentials, deploy scripts, and test accounts; (2) a review of the web dashboard's network scheduling policy ("Relay First, Then Upgrade") and the frontend network display/switching UX, because code inspection surfaced several inconsistencies between `LiveVideo.tsx`, `Dashboard.tsx`, and `Network.tsx` that likely degrade the IPv6-direct and WebRTC experience.

## What Changes

- **Add** `PROMPT.md` at project root — a reusable prompt template encoding production env (fnos 192.168.31.234, ssh fnos-momo, deploy script path), dashboard test credentials (user id=1 + access-key), and the standard workflow (inspect → fix → deploy via `deploy-nas.ps1 -Password '@Fnos324'` → verify via chrome-devtools → git commit & push).
- **Fix** `web/src/components/LiveVideo.tsx` `isRemoteAccess()` — add IPv6-literal detection so IPv6 direct access is classified as LAN (not remote), enabling WebRTC instead of defaulting to HLS on the highest-quality path. Aligns with `Dashboard.tsx` `detectApiPath()` which already handles IPv6.
- **Fix** `web/src/pages/Dashboard.tsx` quality-rating logic — the `clientIPv6 === false` downgrade branch is unreachable because `apiPath === "remote"` already returned. Reorder so the client-lacks-IPv6 case is evaluated before the generic remote clamp, restoring the intended "server has IPv6 but client doesn't → cap at 3 stars" behavior.
- **Fix** `web/src/pages/Network.tsx` "Relay First, Then Upgrade" UX — the page describes an upgrade action but offers no way to trigger it. Add a clickable "切换到 IPv6 直连" link/button that opens `http://[<ipv6>]:8088/` in a new tab when both server and client have IPv6 and the user is currently on relay. Keeps the description honest.
- **Fix** `web/src/api/network.ts` — pass `?refresh=true` on the Dashboard's first network status fetch so the displayed quality reflects current state instead of up to 60s of backend cache staleness on initial load.
- **Update** `docs/ai-context.md` and `README.md` changelog with the fixes.
- **Deploy** to production via `deploy-nas.ps1 -Password '@Fnos324'` and **verify** via chrome-devtools MCP that the dashboard network card, Network page, and LiveVideo transport selector behave correctly on LAN, IPv6, and relay paths.
- **Git commit & push** all changes to remote.

## Impact

- Affected specs: none (no prior specs exist).
- Affected code:
  - `PROMPT.md` (new, project root)
  - `web/src/components/LiveVideo.tsx` — `isRemoteAccess()` function
  - `web/src/pages/Dashboard.tsx` — quality rating IIFE in Network Quality card
  - `web/src/pages/Network.tsx` — Step 2 "Probe & Upgrade" block
  - `web/src/api/network.ts` — `getNetworkStatus` optional refresh parameter usage
  - `docs/ai-context.md` — new Phase entry
  - `README.md` — changelog entry

## ADDED Requirements

### Requirement: Reusable Project Prompt

The project root SHALL contain a `PROMPT.md` file that encodes the standard context needed to work on this project: production SSH credentials, deploy script path, dashboard test account, and the canonical verify-with-chrome-devtools workflow.

#### Scenario: New session reuses prompt
- **WHEN** a new session starts and the user references "the prompt" or asks to deploy/verify
- **THEN** the assistant reads `PROMPT.md` from the project root and follows the encoded workflow without re-asking for SSH credentials, deploy script path, or test account

### Requirement: IPv6 Direct Path Classification

The `LiveVideo.tsx` transport default logic SHALL treat IPv6-literal hostnames (`http://[2001:db8::1]:8088/`) as direct/LAN access, not remote, so WebRTC is attempted first on IPv6 direct connections.

#### Scenario: WebRTC attempted on IPv6 direct
- **WHEN** the dashboard is loaded via an IPv6 literal URL (e.g. `http://[2409:8a70:...]:8088/`)
- **THEN** `isRemoteAccess()` returns `false`
- **AND** `readTransport()` defaults to `"auto"` (WebRTC first), not `"hls"`
- **AND** the LiveVideo component attempts WebRTC before falling back to HLS

### Requirement: Network Page Upgrade Action

The Network page SHALL provide a clickable control to switch to the IPv6 direct URL when both server and client have IPv6 connectivity and the user is currently on the relay path.

#### Scenario: Upgrade available and actionable
- **WHEN** the user is on relay (Cloudflare Tunnel) AND `status.ipv6.reachable === true` AND `clientIPv6 === true`
- **THEN** the Step 2 "Probe & Upgrade" block shows a "切换到 IPv6 直连" link
- **AND** clicking it opens `http://[<status.ipv6.address>]:8088/` in a new tab

#### Scenario: Upgrade not available
- **WHEN** the user lacks IPv6 OR the server lacks IPv6
- **THEN** no clickable upgrade control is shown (current behavior preserved)

## MODIFIED Requirements

### Requirement: Dashboard Quality Rating Accuracy

The Dashboard's Network Quality card star rating SHALL reflect the actual current connection experience, with the client IPv6 capability evaluated independently of the apiPath classification.

#### Scenario: Client lacks IPv6 on relay with server IPv6 available
- **WHEN** `apiPath === "remote"` AND `netStatus.strategy === "ipv6_direct"` AND `clientIPv6 === false`
- **THEN** `currentQuality` is `3` (not `Math.min(netStatus.quality, 3)` which would also be 3, but for the wrong reason — the explicit client-IPv6 check makes the intent clear and survives future refactors)
- **AND** `bestQuality` is `3` (no upgrade possible without client IPv6)

#### Scenario: Client has IPv6 on relay with server IPv6 available
- **WHEN** `apiPath === "remote"` AND `netStatus.strategy === "ipv6_direct"` AND `clientIPv6 === true`
- **THEN** `currentQuality` is `3` (relay experience)
- **AND** `bestQuality` is `5` (IPv6 direct available)
- **AND** the upgrade hint shows "可升级到 IPv6 直连"

### Requirement: Network Status Freshness on Initial Load

The Dashboard's first network status fetch SHALL bypass the backend cache to ensure the displayed quality rating reflects the current network state, not up to 60s of staleness.

#### Scenario: Dashboard initial load
- **WHEN** the Dashboard mounts and fetches network status for the first time
- **THEN** the fetch includes `?refresh=true` to force a fresh backend detection
- **AND** subsequent 5s polling refreshes do NOT include `?refresh=true` (use cached backend response to avoid hammering STUN servers)
