# Tasks

- [x] Task 1: Create `PROMPT.md` at project root
  - [x] SubTask 1.1: Write reusable prompt encoding production env (fnos 192.168.31.234, ssh fnos-momo, password @Fnos324), deploy script path (`D:\Projects\home-datacenter\deploy-nas.ps1`), dashboard test credentials (user id=1, access-key=ebc94f7fe99b497a9bdec7bd45add929360e4386be3cad96a8b3922d1d680a05), and the standard workflow (inspect → fix → deploy via `deploy-nas.ps1 -Password '@Fnos324'` → verify via chrome-devtools → git commit & push)
  - [x] SubTask 1.2: Include project overview pointers (README.md, docs/ai-context.md, docs/api-documentation.md) so the assistant reads them first

- [x] Task 2: Fix `LiveVideo.tsx` `isRemoteAccess()` IPv6 detection
  - [x] SubTask 2.1: Add IPv6-literal hostname detection (regex `/^\[[0-9a-f:]+\]$/i`) returning `false` (direct/LAN), matching `Dashboard.tsx` `detectApiPath()` behavior
  - [x] SubTask 2.2: Verify `readTransport()` now defaults to `"auto"` (WebRTC first) on IPv6 direct access instead of `"hls"`

- [x] Task 3: Fix `Dashboard.tsx` quality rating logic
  - [x] SubTask 3.1: Reorder the `currentQuality` IIFE so the `clientIPv6 === false` check is evaluated BEFORE the generic `apiPath === "remote"` clamp, making the client-lacks-IPv6 downgrade explicit and reachable
  - [x] SubTask 3.2: Verify `bestQuality` also caps at 3 when client lacks IPv6 (already correct, but confirm after refactor)

- [x] Task 4: Add IPv6 direct upgrade action to `Network.tsx`
  - [x] SubTask 4.1: Detect "user is on relay" via `window.location.hostname` (same `detectApiPath` logic — import or inline)
  - [x] SubTask 4.2: When `status.ipv6.reachable === true` AND `clientIPv6 === true` AND user is on relay, render a clickable "切换到 IPv6 直连" link in the Step 2 block that opens `http://[<status.ipv6.address>]:8088/` in a new tab (`target="_blank" rel="noopener noreferrer"`)
  - [x] SubTask 4.3: When upgrade conditions not met, preserve current "不适用" badge behavior

- [x] Task 5: Force fresh network status on Dashboard initial load
  - [x] SubTask 5.1: Modify `getNetworkStatus` usage in `Dashboard.tsx`'s `useCachedFetch` fetcher to pass `refresh=true` on the first call only (use a `useRef` flag or split first fetch from polling)
  - [x] SubTask 5.2: Ensure subsequent 5s polling refreshes do NOT pass `refresh=true` (avoid STUN spam)
  - [x] SubTask 5.3: Verify the `useCachedFetch` cache key (`home.dashboard.status`) still works for instant remount display

- [x] Task 6: Update documentation
  - [x] SubTask 6.1: Add new Phase entry to `docs/ai-context.md` documenting the IPv6-direct classification fix, quality-rating logic fix, Network page upgrade action, and initial-load freshness fix
  - [x] SubTask 6.2: Add changelog entry to `README.md` under a new version (e.g. v1.8.7) describing the four fixes
  - [x] SubTask 6.3: Update "Last Updated" line in `docs/ai-context.md`

- [x] Task 7: Deploy to production and verify with chrome-devtools
  - [x] SubTask 7.1: Run `.\deploy-nas.ps1 -Password '@Fnos324'` from `D:\Projects\home-datacenter\` to build and deploy
  - [x] SubTask 7.2: Use chrome-devtools MCP to navigate to `http://192.168.31.234:8088/` (LAN path), log in with test access-key, verify Dashboard network card shows "局域网" + 5 stars, Network page loads, LiveVideo attempts WebRTC
  - [x] SubTask 7.3: Use chrome-devtools MCP to navigate to `http://dashboard.feiyemomo.top/` (relay path), verify Dashboard shows "远程" + upgrade hint, Network page shows "切换到 IPv6 直连" link when IPv6 available
  - [x] SubTask 7.4: Verify LiveVideo transport selector kebab menu switches between auto/webrtc/hls without errors
  - [x] SubTask 7.5: Check browser console for errors during all verification steps

- [x] Task 8: Git commit and push to remote
  - [x] SubTask 8.1: `git add` the changed files (PROMPT.md, LiveVideo.tsx, Dashboard.tsx, Network.tsx, network.ts, ai-context.md, README.md, .trae/specs/)
  - [x] SubTask 8.2: `git commit` with a conventional commit message describing the network policy review fixes
  - [x] SubTask 8.3: `git push` to remote

# Task Dependencies

- [Task 7] depends on [Task 1], [Task 2], [Task 3], [Task 4], [Task 5], [Task 6]
- [Task 8] depends on [Task 7]
- [Tasks 1-6] are independent and can be done in parallel
