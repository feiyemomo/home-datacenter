# Android App vs Web Dashboard — Feature Gap Analysis

> **Purpose**: Catalog features present in the Android app
> (`d:\Projects\Android\`) but missing or weaker in the web dashboard
> (`d:\Projects\home-datacenter\web\`), with implementation details
> to help prioritize dashboard补全 work.
>
> **Last updated**: 2026-07-20 (Android v1.6.8 vs dashboard at the
> same commit).

## TL;DR

The Android app currently leads the dashboard in three major areas:

1. **Recording playback** — the Android side has a full 24-hour
   continuous-play experience (ExoPlayer playlist + fisheye chip
   scroller + motion-range overlay + seek-snap + gestures + 5x
   speed + custom fullscreen). The dashboard's recording playback
   is a single-file `<video controls>` with no timeline, no motion
   markers, no gestures.
2. **Network adaptation** — Android auto-switches between LAN
   (`http://192.168.31.234:8088/`) and Cloudflare Tunnel
   (`https://api.feiyemomo.top/`) with 5-minute TTL + forced
   re-probe on network change. The dashboard has no equivalent
   (its `axios` client uses the current page origin).
3. **Dashboard cards** — Android has a weather card and a
   LAN/Remote path chip on the network-quality card; the dashboard
   has neither.

The dashboard already matches or exceeds the app on: admin user
management UI, MQTT debug page, responsive grid layouts (Tailwind
CSS), and live-video transport selection (auto/webrtc/hls dropdown).

---

## 1. Recording Playback (largest gap)

### 1.1 Per-day recording list

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | High | — |

**Description**: Recordings are grouped per LOCAL day. Each card
shows the date (large), month/year (small), weekday, recording
count, total duration, and a "今天" (Today) badge for the current
day. Tapping a card opens the 24-hour playback view for that date.

**Implementation**:
- File: `app/src/main/java/com/homedatacenter/app/ui/cameras/RecordingsDialog.kt` — `DayRecordingAdapter`, `DayRecording` data class
- Layout: `app/src/main/res/layout/item_day_recording.xml`
- Key logic:
  - Backend `GET /api/v1/cameras/:id/recordings` returns a flat list of recording segments.
  - Client-side `groupBy { it.start_time.toLocalDate() }` (Asia/Shanghai timezone).
  - Each `DayRecording` carries `dayStartCalendar`, `recordingCount`, `totalDurationSeconds`, `totalSizeBytes`.
  - Tap → `openDayForTimestamp(dayStart, initialTimestamp = null)`.

**Dashboard补全建议**: Add a `RecordingDayList` component in
`web/src/components/LiveVideo.tsx`. Frontend `groupBy` recordings
by local-date of `start_time`. Render as a card grid; clicking a
card switches to the `RecordingDayPlayer` sub-component (see 1.2).

---

### 1.2 24-hour continuous playback (ExoPlayer playlist)

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | **Very High** | — |

**Description**: All 60-second recording buckets of a day are
concatenated into a single ExoPlayer playlist. The user can drag
the SeekBar to any time in the 24-hour window and ExoPlayer
transparently seeks across clip boundaries.

**Implementation**:
- File: `RecordingsDialog.kt` — `playDayAsPlaylist(recs)`, `clipStartOffsets: LongArray`, `seekToMotionStart`, `openDayForTimestamp`
- Key logic:
  1. Filter recordings whose `start_time` falls in `[dayStart, dayStart + 24h)`, sort ascending.
  2. For each recording, build a `ProgressiveMediaSource` with a `DataSource.Factory` that injects the `home_token` cookie via `CookieManager`.
  3. `clipStartOffsets[i]` = cumulative milliseconds of clip `i` from `dayStart`. Used for binary search during SeekBar drag.
  4. `exoPlayer.setMediaSources(sources); prepare(); playWhenReady = true;`
  5. SeekBar drag → `binarySearch(clipStartOffsets)` → `exoPlayer.seekTo(clipIndex, offsetMs)`.
  6. `pendingAlertSeekMs` for alert-click seek: one-shot `Player.Listener` fires on `STATE_READY` and seeks to the requested timestamp.

**Dashboard补全建议**: Add a `RecordingDayPlayer` component. Two
implementation paths:
- **Ideal**: Use `hls.js` with a synthesized `MediaPlaylist` (each
  60s mp4 becomes a `#EXTINF:60` segment). hls.js handles gapless
  transitions.
- **Pragmatic**: Use `<video>` with `ended` event → load next mp4
  URL → maintain cumulative offset. Custom SeekBar uses
  `clipStartOffsets` for binary search.

---

### 1.3 Fisheye chip scroller

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | High | — |

**Description**: Horizontal scroller of motion-event chips.
Centered chip is full-size with `HH:mm` label; chips decay
linearly to 40% scale toward the edges. Chips with scale < 0.65
lose their text and become 3dp-wide colored ticks.

**Implementation**:
- File: `app/src/main/java/com/homedatacenter/app/ui/cameras/FisheyeChipScroller.kt` (177 lines, `HorizontalScrollView` subclass)
- Key constants: `fullScaleBandPx = 80`, `minScale = 0.4f`, `textThreshold = 0.65f`
- Key logic:
  - `onScrollChanged` → `applyFisheyeScales()` walks each child and applies `scaleX/Y` based on distance from viewport center.
  - **Pure visual transform** — no `requestLayout()` during scroll (perf-critical).
  - `updateContainerEdgePadding`: left/right padding = half viewport width so the first/last chips can scroll to center.
  - `scrollToCenterChip(index)`: `smoothScrollTo` 200ms animation.

**Dashboard补全建议**: Add a `<div className="overflow-x-auto">`
in `LiveVideo.tsx` below the video element. Render chips with
absolute positioning. Listen to `onScroll`, compute `dx` for each
chip, apply `transform: scale(...)` + `opacity`. Hide text when
scale < 0.65 and set width to 3px. Could use `framer-motion`'s
`useScroll` to simplify.

---

### 1.4 Motion-range overlay on SeekBar

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | High | — |

**Description**: Colored motion-range bars are drawn on top of the
SeekBar track. Each range is colored by tier (teal/amber/red) and
adjacent same-tier ranges within 30s are merged into one wide bar.

**Implementation**:
- File: `app/src/main/java/com/homedatacenter/app/ui/cameras/AlertRangeOverlay.kt` (247 lines, custom `View`)
- Tiers: `TIER_LOW=0 (teal #4DB6AC)`, `TIER_MID=1 (amber #FFB300)`, `TIER_HIGH=2 (merged with MID in v1.6.8)`, `TIER_ALERT=3 (red #EF5350)`
- Key logic:
  - `setTieredRanges(ranges)`: client-side merge with `consolidateGapMs = 30_000L`.
  - `onDraw`: `drawRect` for the bar + `drawCircle` for edge tick.
  - Data from `GET /api/v1/cameras/:id/motion-ranges?date=YYYY-MM-DD`.

**Dashboard补全建议**: Add an SVG or absolutely-positioned `<div>`
overlay above the SeekBar. Reuse the motion-ranges API. Replicate
the 30s same-tier merge logic in TypeScript. Use 4 colors matching
the Android palette.

---

### 1.5 SeekBar snap to motion-range edge

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | Medium | — |

**Description**: When the user releases the SeekBar within ±120s of
a motion range edge, the progress snaps to that edge.

**Implementation**:
- File: `RecordingsDialog.kt` — `snapProgressToRangeEdge(progressMs, radiusMs = 120_000L)`
- Key logic: Walk `motionRangesRelative`, find the nearest `startMs` or `endMs` within 120s of `progressMs`, call `seekTo` on it.

**Dashboard补全建议**: In the custom SeekBar's `onMouseUp` /
`onTouchEnd`, call a `snapToRangeEdge` function. Reuses the
motion-ranges array from 1.4.

---

### 1.6 Double-tap ±10s seek

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | Medium | — |

**Description**: Double-tap the left 40% of the player to rewind
10s; double-tap the right 40% to skip forward 10s. The middle 20%
is reserved. A `《` or `》` glyph blinks twice (alpha 0→1→0→1→0 over
~500ms) as feedback.

**Implementation**:
- File: `app/src/main/java/com/homedatacenter/app/util/PlayerGestureHelper.kt` (235 lines)
- Key logic:
  - `GestureDetector.SimpleOnGestureListener.onDoubleTap(e)`.
  - `view.width` × 0.4 = rewind zone, × 0.6 = forward zone.
  - `exoPlayer.seekTo(currentPos ± 10_000L)`.
  - `playSeekHintAnimation(direction)`: `ObjectAnimator` of alpha.

**Dashboard补全建议**: Attach `onDoubleClick` to the video
container. Compare `event.clientX - rect.left` against `rect.width × 0.4`. Use `video.currentTime += 10`. CSS keyframe animation for
the `《`/`》` glyph.

---

### 1.7 Long-press 5x speed

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | Medium | — |

**Description**: Long-press the player to temporarily switch to 5x
playback. Release to restore the previous speed.

**Implementation**:
- File: `PlayerGestureHelper.kt` — `onLongPress`
- Key logic:
  - On long-press: `savedSpeed = exoPlayer.playbackParameters.speed; exoPlayer.playbackParameters = PlaybackParameters(5.0f)`.
  - On `ACTION_UP`: `exoPlayer.playbackParameters = PlaybackParameters(savedSpeed)`.
  - Synchronizes with the speed slider via `onSpeedChangedBySlider`.

**Dashboard补全建议**: Listen to `onPointerDown` / `onPointerUp`
on the `<video>` container. Use a 500ms long-press threshold. Set
`video.playbackRate = 5` on long-press, restore on release.

---

### 1.8 Playback speed slider (0.5x–5.0x)

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Weak** (native `<video controls>` only goes 0.5–2x via right-click menu) |
| Severity | Medium | — |

**Description**: Horizontal `Slider` with discrete steps
0.5/1/1.5/2/2.5/3/3.5/4/4.5/5x. A `1x` label updates in real
time. Sits at top-center of the player in a dark glass panel.

**Implementation**:
- File: `dialog_recordings.xml` — `speedBarContainer` with `Slider` + `tvSpeedLabel`
- Logic: `Slider.OnChangeListener` → `exoPlayer.playbackParameters = PlaybackParameters(speed)` → `tvSpeedLabel.text = "${speed}x"`.

**Dashboard补全建议**: Add a `<input type="range" min="0.5" max="5" step="0.5">` or custom dropdown. Update `video.playbackRate`.

---

### 1.9 Glass-style center pause button (auto-hide 2s)

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | Low | — |

**Description**: 56dp circular glass-textured pause/play button at
the center of the player. Auto-hides 2s after the last tap.

**Implementation**:
- Layout: `dialog_recordings.xml` — `btnCenterPause` with `bg_glass_dark_panel` background
- Logic: `PlayerGestureHelper` — `Handler.postDelayed(hideRunnable, 2000)`. Any `onTouch` resets the timer.

**Dashboard补全建议**: Overlay a `<button>` with
`backdrop-filter: blur()` and semi-transparent background. Use
`setTimeout` + `mousemove` to control 2s auto-hide.

---

### 1.10 Custom fullscreen toggle

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Weak** (native `<video controls>` fullscreen) |
| Severity | Low | — |

**Description**: Custom fullscreen button (bottom-right corner,
40dp icon, transparent background, 0dp margin). Toggling
fullscreen also rotates the activity to landscape, hides system
bars via `WindowInsetsControllerCompat`, switches the player host
height to `MATCH_PARENT`, and synchronously shows/hides the
speed button / back button / controller.

**Implementation**:
- File: `app/src/main/java/com/homedatacenter/app/util/PlayerFullscreenHelper.kt` (612 lines)
- Key logic:
  - `toggleFullscreen` saves the host view's original height (including `MATCH_PARENT / -2` special values).
  - `applyFullscreenState`: `requestedOrientation = SCREEN_ORIENTATION_LANDSCAPE/PORTRAIT`, `setDecorFitsSystemWindows(false/true)`, `WindowInsetsControllerCompat.hide/show systemBars`.
  - `applyHeight` uses `hostView.post` to defer to the next frame (avoid rotation race) and `addOnLayoutChangeListener` for continuous correction.
  - `controllerSyncViews`: visibility follows the player controller.

**Dashboard补全建议**: Use the browser Fullscreen API
(`element.requestFullscreen()`). Listen for `fullscreenchange`,
and in fullscreen mode show a custom control bar (speed, back,
PTZ). CSS `position: fixed; inset: 0` to fill the viewport.

---

### 1.11 Alert click → recording seek

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Weak** (alert click currently goes to live view, not recording playback) |
| Severity | High | — |

**Description**: Clicking "查看录像" on an alert opens the
recording dialog and automatically seeks ExoPlayer to the alert's
timestamp.

**Implementation**:
- Files:
  - `app/src/main/java/com/homedatacenter/app/ui/alerts/AlertsFragment.kt` — `jumpToCameraAtTimestamp`
  - `app/src/main/java/com/homedatacenter/app/ui/cameras/AlertsDialog.kt` — `playAlert`
  - `RecordingsDialog.kt` — `pendingAlertSeekMs`
- Key logic:
  - `Intent` carries `EXTRA_INITIAL_TIMESTAMP` (ms) to `CameraDetailActivity`.
  - `RecordingsDialog(camera, container, initialTimestamp = ...)` opens the day containing the timestamp.
  - `pendingAlertSeekMs = initialTimestamp - dayStartMs`. One-shot `Player.Listener` fires on `STATE_READY` and calls `exoPlayer.seekTo(clipIndex, offsetMs)`.

**Dashboard补全建议**: Extend the alert-click URL to
`/cameras?camera=&time=&mode=recording`. In `LiveVideo.tsx`, when
`mode=recording`, switch to the `RecordingDayPlayer` (see 1.2)
and seek to `time`.

---

## 2. Camera Card & Detail Page

### 2.1 Standalone camera detail Activity

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | Medium | — |

**Description**: Tapping a camera card opens a dedicated Activity
hosting live video + PTZ + presets + settings (audio/recording/
codec/delete) + recordings entry + alerts entry.

**Implementation**:
- File: `app/src/main/java/com/homedatacenter/app/ui/cameras/CameraDetailActivity.kt` (1115 lines)
- Key logic:
  - `Intent` carries `EXTRA_CAMERA_JSON` and `EXTRA_INITIAL_TIMESTAMP`.
  - `startPlayback` → `startWebRtcStream` → onError → `startMp4Playback` → `preparePlayback` (HLS fallback on MP4 error).
  - `updateStreamStrategy badge`: shows current `webrtc`/`mp4`/`hls`.
  - `setupPtz` (hidden for non-admin), `setupPresets` (add/delete/goto), `setupSettings`, `setupWebRtcControls` (pause/mute/fullscreen).

**Dashboard补全建议**: Add a `/cameras/:id` route rendering a
`CameraDetailPage` component. Integrate `LiveVideo` + PTZ + presets
+ settings + tabs for recordings and alerts. Have `Cameras.tsx`
navigate to this route on card click.

---

### 2.2 WebRTC / MP4 / HLS three-tier fallback

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Weak** (no MP4 middle tier) |
| Severity | Medium | — |

**Description**: WebRTC failure → MP4 direct stream → HLS → give
up. UI badge updates with each transition.

**Implementation**:
- File: `CameraDetailActivity.kt` — `startWebRtcStream`, `startMp4Playback`, `preparePlayback`
- Key logic:
  - `WebRtcClient.Listener.onError` triggers `startMp4Playback`.
  - ExoPlayer `onPlayerError` triggers `preparePlayback(hlsUrl)`.
  - `updateStreamStrategy` updates the badge text.

**Dashboard补全建议**: In `LiveVideo.tsx` add an MP4 fallback tier
between `useWebRTCStream` and `HLSVideo`. When WebRTC fails, switch
to `<video src={mp4Url} />`. When MP4 fails, switch to HLS. Show a
badge indicating the active mode.

---

### 2.3 Large card with warm liquid-glass styling

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Weak** (smaller cards) |
| Severity | Low | — |

**Description**: Camera cards are 132dp tall with 192×108dp
thumbnails (2.25× the area of the previous 84dp card). Warm
liquid-glass background (`#F2FFFFFF` 95% warm white) with a peach
border (`#66FFD4B8`). Adaptive sizing for 7"/10" tablets via
`@dimen/camera_card_*`.

**Implementation**:
- File: `app/src/main/java/com/homedatacenter/app/ui/cameras/CameraCard.kt` (229 lines, Jetpack Compose)
- Key logic:
  - `CardBackground = Color(0xF2FFFFFF)`, `CardBorder = Color(0x66FFD4B8)`.
  - `dimensionResource(R.dimen.camera_card_height)` etc. for responsive sizing.
  - `inSampleSize = 2` (up from 4) for thumbnail bitmap decoding.
  - Online/offline pill + codec/audio badges.

**Dashboard补全建议**: Adjust `CamCard` CSS in `Cameras.tsx`:
enlarge the thumbnail area to 16:9 full card width, add padding,
use `backdrop-filter: blur()` + peach border to mimic the liquid
glass. Use Tailwind responsive classes for tablet sizing.

---

## 3. Dashboard Cards

### 3.1 Weather card

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | High | — |

**Description**: Dashboard top shows a card with temperature,
weather icon, and location ("宝鸡市陈仓区"). WMO weather code is
mapped to a Material icon.

**Implementation**:
- File: `app/src/main/java/com/homedatacenter/app/ui/dashboard/DashboardFragment.kt` — `loadWeather`, `weatherIconFor`
- Key logic:
  - `GET /api/v1/weather` (backend Open-Meteo proxy).
  - `weatherIconFor(wmoCode)`: WMO code → Material icon mapping.
  - Hard-coded location label.

**Dashboard补全建议**: Add a `WeatherCard` component at the top of
`web/src/pages/Dashboard.tsx`. Call `GET /api/v1/weather`. Use
`lucide-react` icons (Cloud/Sun/CloudRain/etc.) mapped from WMO
codes. Show the location label.

---

### 3.2 LAN/Remote path chip

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | Medium | — |

**Description**: The network-quality card displays a chip
indicating the current API path: green dot = LAN, amber dot =
remote (Cloudflare Tunnel).

**Implementation**:
- File: `DashboardFragment.kt` — `updateBackendPath`
- Key logic: `BaseUrlResolver.current` returns `LAN` or `REMOTE`. The chip color and label are updated in `refreshAll()` and the 5-second status poll.

**Dashboard补全建议**: Add a chip to the Network Quality card in
`Dashboard.tsx`. Determine the path from `window.location.hostname`
(`192.168.*` = LAN, otherwise = remote). Use an emerald/amber dot.

---

## 4. Network Adaptation (Android-only)

### 4.1 LAN / Cloudflare Tunnel auto-switching

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | High | — |

**Description**: At app startup, both LAN
(`http://192.168.31.234:8088/`) and remote
(`https://api.feiyemomo.top/`) are probed. LAN is preferred when
reachable (TTFB ~10ms vs 1.4s+ through the tunnel). Result is
cached for 5 minutes.

**Implementation**:
- File: `app/src/main/java/com/homedatacenter/app/util/BaseUrlResolver.kt` (353 lines)
- Key logic:
  - `LAN_URL`, `REMOTE_URL` constants.
  - `probeSync`: HTTP `GET /api/v1/system/status` < 500 = alive, with TCP socket fallback.
  - `probeLanOnStartup`: 1.5/4/9/16s exponential backoff retries.
  - `TTL_MS = 5 * 60 * 1000` (5-minute cache).
  - `onUrlChanged` callback lets `AppContainer` rebuild Retrofit.

**Dashboard补全建议**: Web is bound to the current page origin,
so true auto-switching is less applicable. However, you can add
fallback logic in `web/src/api/client.ts`: if the current origin
fails, try a backup origin stored in `localStorage`. Use a 5-minute
TTL. Optionally inject the backup URL via `<meta name="api-backup">`.

---

### 4.2 Network-change forced re-probe

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | Medium | — |

**Description**: WiFi ↔ cellular switch immediately triggers
`forceProbe()` without waiting for the 5-minute TTL.

**Implementation**:
- File: `app/src/main/java/com/homedatacenter/app/util/NetworkChangeMonitor.kt` (126 lines)
- Key logic:
  - `ConnectivityManager.registerNetworkCallback`.
  - `onAvailable / onLost / onCapabilitiesChanged` (with `NET_CAPABILITY_VALIDATED`) → `container.baseUrlResolver.forceProbe()`.
  - Registered in `HomeCenterApp.onCreate`.

**Dashboard补全建议**: Use `navigator.connection.onchange` or
`window.addEventListener('online' / 'offline')` to trigger
`forceProbe()` in `client.ts`.

---

### 4.3 5-minute TTL cache

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | Low | — |

**Description**: Successful probe results are cached for 5 minutes;
no re-probe within the TTL window.

**Implementation**: `BaseUrlResolver.kt` — `TTL_MS = 5 * 60 * 1000` + `lastProbeAt` timestamp.

**Dashboard补全建议**: Bundle with 4.1. Use
`localStorage.setItem('lastProbeAt', Date.now())` and check the
TTL before re-probing.

---

## 5. Live Video

### 5.1 MP4 fallback middle tier

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | Medium | — |

**Description**: When WebRTC fails, MP4 direct stream
(`ProgressiveMediaSource`) is tried before HLS.

**Implementation**:
- File: `CameraDetailActivity.kt` — `startMp4Playback`
- Key logic: `ProgressiveMediaSource.Factory` + the camera's MP4 endpoint.

**Dashboard补全建议**: See 2.2.

---

### 5.2 WebRTC client pre-warming

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Missing** |
| Severity | Low | — |

**Description**: WebRTC client is pre-warmed when entering the
camera list. Tapping a camera picks up the pre-warmed client,
reducing cold-start latency.

**Implementation**:
- Files:
  - `app/src/main/java/com/homedatacenter/app/util/WebRtcClient.kt` (528 lines)
  - `CameraDetailActivity.kt` — `ensureWebRtcClient`
- Key logic:
  - Native WebRTC (`org.webrtc`).
  - WHEP `POST /api/v1/cameras/{id}/webrtc` with `Content-Type: application/sdp`.
  - Non-trickle ICE (`GATHER_ONCE`).
  - `recvonly` audio+video transceiver.
  - `waitForIceGathering` 2s timeout.
  - `setVideoEnabled / setAudioEnabled` for local toggles.

**Dashboard补全建议**: In `Cameras.tsx`, expose a `WebRtcClientPool` via React Context. Pre-create `RTCPeerConnection` objects for all cameras on mount. `LiveVideo.tsx` borrows from the pool instead of cold-starting.

---

## 6. Alerts

### 6.1 Alert click → recording playback (not live)

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Weak** (jumps to live view) |
| Severity | High | — |

**Description**: Clicking "查看录像" on an alert opens the recording
playback view (not live) and seeks to the alert's timestamp.

**Implementation**: See 1.11.

**Dashboard补全建议**: See 1.11.

---

### 6.2 Multi-source thumbnail with downsampling

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Weak** |
| Severity | Low | — |

**Description**: Alert thumbnails try base64 inline data first, fall
back to `/alerts/:id/thumbnail` URL. Decoding uses
`inSampleSize=4` + `RGB_565` to reduce memory.

**Implementation**:
- Files: `app/src/main/java/com/homedatacenter/app/ui/alerts/AlertListAdapter.kt`, `AlertsDialog.kt`
- Key logic: `BitmapFactory.Options.inSampleSize=4`, `inPreferredConfig=RGB_565`.

**Dashboard补全建议**: In the alert `<img>` `onError`, fall back to
the base64 thumbnail if the alert object has a `thumbnail` field.
The browser handles decode downsampling automatically.

---

## 7. Other

### 7.1 Follow-system theme

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Weak** (only light/dark, no "system") |
| Severity | Medium | — |

**Description**: Theme supports three modes: light, dark, follow
system. "Follow system" listens to the system dark-mode setting.

**Implementation**:
- Files:
  - `app/src/main/java/com/homedatacenter/app/ui/settings/SettingsFragment.kt` (194 lines)
  - `app/src/main/java/com/homedatacenter/app/util/ThemeManager.kt` (32 lines)
- Key logic: `PrefsManager.THEME_LIGHT / DARK / FOLLOW_SYSTEM` → `AppCompatDelegate.setDefaultNightMode(MODE_NIGHT_NO / MODE_NIGHT_YES / MODE_NIGHT_FOLLOW_SYSTEM)`.

**Dashboard补全建议**: Extend `Theme = "light" | "dark"` to
`"light" | "dark" | "system"` in `web/src/hooks/useTheme.ts`. For
`"system"`, use
`window.matchMedia('(prefers-color-scheme: dark)')` and listen for
changes. Update the theme toggle in `Layout.tsx` to a dropdown
with three options.

---

### 7.2 Camera list column count adaptation

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Already has** |
| Severity | — | — |

**Description**: Phones use 1 column; 7" tablets use 2; 10" tablets
use 2. Driven by `@integer/camera_list_column_count` resource
qualifier (`sw600dp` / `sw936dp`).

**Implementation**:
- File: `app/src/main/java/com/homedatacenter/app/ui/cameras/CamerasFragment.kt`
- Key logic: `GridLayoutManager(context, resources.getInteger(R.integer.camera_list_column_count))`.

**Dashboard status**: `Cameras.tsx` already uses Tailwind CSS grid
responsive classes (`grid-cols-1 md:grid-cols-2 lg:grid-cols-3`).
No补全 needed.

---

### 7.3 Admin FAB for camera registration

| | Android | Dashboard |
|---|---|---|
| Status | **Has** | **Already has** |
| Severity | — | — |

**Description**: Admin users see a FAB on the camera list for
registering a new camera.

**Implementation**: `CamerasFragment.kt` shows the FAB based on
`prefsManager.isAdmin`.

**Dashboard status**: `Cameras.tsx` shows a "Register camera"
button for admin users. No补全 needed.

---

## Summary Matrix

| # | Feature | Android | Dashboard | Severity |
|---|---|---|---|---|
| 1.1 | Per-day recording list | Has | Missing | High |
| 1.2 | 24h continuous playback | Has | Missing | **Very High** |
| 1.3 | Fisheye chip scroller | Has | Missing | High |
| 1.4 | Motion-range SeekBar overlay | Has | Missing | High |
| 1.5 | SeekBar snap to motion edge | Has | Missing | Medium |
| 1.6 | Double-tap ±10s | Has | Missing | Medium |
| 1.7 | Long-press 5x speed | Has | Missing | Medium |
| 1.8 | Speed slider 0.5–5x | Has | Weak (≤2x native) | Medium |
| 1.9 | Glass center pause button | Has | Missing | Low |
| 1.10 | Custom fullscreen toggle | Has | Weak (native) | Low |
| 1.11 | Alert click → recording seek | Has | Weak (jumps to live) | High |
| 2.1 | Standalone camera detail Activity | Has | Missing | Medium |
| 2.2 | WebRTC/MP4/HLS three-tier fallback | Has | Weak (no MP4) | Medium |
| 2.3 | Large liquid-glass camera card | Has | Weak | Low |
| 3.1 | Weather card | Has | **Missing** | High |
| 3.2 | LAN/Remote path chip | Has | Missing | Medium |
| 4.1 | LAN/Tunnel auto-switching | Has | **Missing** | High |
| 4.2 | Network-change forced re-probe | Has | Missing | Medium |
| 4.3 | 5-minute TTL cache | Has | Missing | Low |
| 5.1 | MP4 fallback middle tier | Has | Missing | Medium |
| 5.2 | WebRTC client pre-warming | Has | Missing | Low |
| 6.1 | Alert click → recording playback | Has | Weak (jumps to live) | High |
| 6.2 | Multi-source thumbnail downsampling | Has | Weak | Low |
| 7.1 | Follow-system theme | Has | Weak (no "system") | Medium |
| 7.2 | Camera list column adaptation | Has | Already has | — |
| 7.3 | Admin camera registration FAB | Has | Already has | — |

---

## Recommended Priority for Dashboard补全

Ordered by user value divided by implementation cost:

1. **Weather card** (3.1) — lowest cost, calls existing `/api/v1/weather`.
2. **Alert click → recording playback** (1.11 / 6.1) — highest value; users currently can't review past events from an alert.
3. **Per-day recording list** (1.1) — foundational UI; frontend `groupBy` is straightforward.
4. **Follow-system theme** (7.1) — `useTheme.ts` extension + `matchMedia`.
5. **24h continuous playback** (1.2) — highest cost but highest value. Recommend `hls.js` with synthesized MediaPlaylist.

Advanced recording-playback features (fisheye chips, motion-range
overlay, gestures, custom fullscreen) can be added in later
iterations once the foundation (1.1 + 1.2) is in place.

---

## Key File Index

### Android (reference implementations)

- `app/src/main/java/com/homedatacenter/app/ui/cameras/RecordingsDialog.kt` (~1450 lines)
- `app/src/main/java/com/homedatacenter/app/ui/cameras/FisheyeChipScroller.kt` (177 lines)
- `app/src/main/java/com/homedatacenter/app/ui/cameras/AlertRangeOverlay.kt` (247 lines)
- `app/src/main/java/com/homedatacenter/app/ui/cameras/CameraCard.kt` (229 lines)
- `app/src/main/java/com/homedatacenter/app/ui/cameras/CameraDetailActivity.kt` (1115 lines)
- `app/src/main/java/com/homedatacenter/app/ui/cameras/CamerasFragment.kt`
- `app/src/main/java/com/homedatacenter/app/ui/cameras/AlertsDialog.kt` (453 lines)
- `app/src/main/java/com/homedatacenter/app/ui/dashboard/DashboardFragment.kt` (760 lines)
- `app/src/main/java/com/homedatacenter/app/ui/alerts/AlertsFragment.kt` (167 lines)
- `app/src/main/java/com/homedatacenter/app/ui/settings/SettingsFragment.kt` (194 lines)
- `app/src/main/java/com/homedatacenter/app/util/BaseUrlResolver.kt` (353 lines)
- `app/src/main/java/com/homedatacenter/app/util/NetworkChangeMonitor.kt` (126 lines)
- `app/src/main/java/com/homedatacenter/app/util/WebRtcClient.kt` (528 lines)
- `app/src/main/java/com/homedatacenter/app/util/PlayerGestureHelper.kt` (235 lines)
- `app/src/main/java/com/homedatacenter/app/util/PlayerFullscreenHelper.kt` (612 lines)
- `app/src/main/java/com/homedatacenter/app/util/ThemeManager.kt` (32 lines)

### Dashboard (web — files to modify)

- `web/src/App.tsx` (128 lines) — add routes for camera detail / recording
- `web/src/pages/Cameras.tsx` (309 lines) — refactor to detail route
- `web/src/components/LiveVideo.tsx` (~1000 lines) — add `RecordingDayList`, `RecordingDayPlayer`, motion-range overlay, fisheye chips, gestures
- `web/src/pages/Dashboard.tsx` (652 lines) — add weather card, path chip
- `web/src/pages/Network.tsx` (397 lines)
- `web/src/pages/Profile.tsx` (260 lines) — extend theme dropdown
- `web/src/pages/Devices.tsx` (396 lines)
- `web/src/pages/Users.tsx` (611 lines)
- `web/src/hooks/useTheme.ts` (87 lines) — extend with "system" option
- `web/src/hooks/useWebRTCStream.ts` (342 lines) — add MP4 fallback tier
- `web/src/components/Layout.tsx` (245 lines) — theme toggle UI
- `web/src/api/client.ts` — add backup-origin fallback
- `web/src/api/camera.ts` (210 lines)
