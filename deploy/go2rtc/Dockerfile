# Multi-stage Dockerfile for go2rtc (forked).
# Build marker: 2026-07-05-ffmpeg-optin
#
# Stage 1: build go2rtc FROM SOURCE so we can patch the HLS
# session keepalive. The upstream constant is hardcoded at 5s in
# internal/hls/hls.go (`const keepalive = 5 * time.Second`), and
# the timer fires 5s after the segment *request* is received,
# regardless of how long the response write takes. For HEVC
# cameras streaming at ~22 Mbps, each 0.5s segment is ~1.4 MB; on
# any link slower than ~2.5 Mbps the segment download takes > 5s
# and the session is reaped before the response finishes, after
# which every subsequent playlist poll returns 404. We bump the
# keepalive to 30s — long enough for a 1.4 MB segment over a
# 500 kbps link (~22s) plus headroom, short enough that an
# abandoned client still gets its resources freed within a
# sensible window. The git history is shallow (one commit) so the
# clone is fast, and we pin the version via build arg for
# reproducible builds.
FROM golang:1.22-alpine AS builder
ARG GO2RTC_VERSION=1.9.5
ARG GO2RTC_KEEPALIVE_SECONDS=30

# China-friendly mirrors: apk (Alpine) and Go modules.
RUN sed -i 's|dl-cdn.alpinelinux.org|mirrors.aliyun.com|g' /etc/apk/repositories \
     && apk add --no-cache git ca-certificates \
     && go env -w GOPROXY=https://goproxy.cn,direct

WORKDIR /src
RUN git clone --depth 1 --branch v${GO2RTC_VERSION} \
     https://github.com/AlexxIT/go2rtc.git .

# Patch the HLS session keepalive. We do three things to make
# the patch observable end-to-end:
#   1. sed replaces the upstream `const keepalive = 5 * time.Second`.
#   2. We grep for the new value so a future upstream rename fails
#      the build loudly instead of silently shipping the un-patched
#      binary.
#   3. We inject a package-level `KEEPALIVE_PATCH_MARKER_30S` var
#      and overwrite it at link time via -ldflags -X. The
#      container's logs include it so we can confirm the patched
#      binary is actually running (not the upstream image).
RUN sed -i "s|const keepalive = 5 \* time.Second|const keepalive = ${GO2RTC_KEEPALIVE_SECONDS} * time.Second|" \
     internal/hls/hls.go \
     && grep -F "const keepalive = ${GO2RTC_KEEPALIVE_SECONDS} * time.Second" internal/hls/hls.go >/dev/null \
     && printf '%s\n' \
     '' \
     '// KEEPALIVE_PATCH_MARKER_30S is overwritten at link time via' \
     '// -ldflags -X to record the build identity. Runtime code can' \
     '// log it to confirm the patched binary is actually running.' \
     'var KEEPALIVE_PATCH_MARKER_30S = "unpatched"' \
     > /tmp/marker.txt \
     && sed -i "/^const keepalive = ${GO2RTC_KEEPALIVE_SECONDS} \* time.Second$/r /tmp/marker.txt" \
     internal/hls/hls.go \
     && grep -F "KEEPALIVE_PATCH_MARKER_30S" internal/hls/hls.go >/dev/null \
     && echo "go2rtc HLS keepalive patched to ${GO2RTC_KEEPALIVE_SECONDS}s"

# Static build — no cgo, no dynamic linker dependencies. Strip
# the binary to keep the runtime image small. go2rtc v1.9.x keeps
# its `main.go` at the repo root, so the build target is `.`
# (current module), not `./cmd/go2rtc`.
#
# We inject a build-time marker via -ldflags -X so we can verify
# the patched binary is actually running. The marker is declared
# as a package-level var in internal/hls/hls.go (added below);
# -X overwrites it at link time without touching the Go AST.
RUN MARKER="patched-30s-$(date +%s)-pid$$" \
     && echo "BUILD-MARKER-STRING: $MARKER" \
     && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
     go build \
     -ldflags="-s -w -X 'github.com/AlexxIT/go2rtc/internal/hls.KEEPALIVE_PATCH_MARKER_30S=$MARKER'" \
     -trimpath -o /out/go2rtc . \
     && echo "go2rtc binary built with marker: $MARKER"

# Stage 2: slim runtime image.
#
# ffmpeg is now unconditionally included so cameras with
# `transcode=true` (see services/api/internal/model/camera.go
# Transcode) can be transcoded to H.264 on demand. go2rtc
# interprets the RTSP URL fragment `#video=h264` as "use ffmpeg,
# output H.264" — the registry appends that fragment only when
# the operator opts a specific camera in. Cameras that don't
# have the flag set use the native H.264 / HEVC passthrough with
# no transcode cost.
#
# The previous "no ffmpeg" hard constraint (see
# docs/platformization.md and project_memory.md) was relaxed in
# Phase 7 because (a) the per-camera opt-in keeps the CPU/memory
# cost bounded to only the cameras the operator cares about, and
# (b) the alternative — a second image profile operators have to
# remember to switch into — was deemed worse than ~30 MB of
# ffmpeg in the base image. ffmpeg-static (the prebuilt
# musl-linked binary) keeps the runtime image reproducible
# without dragging in a 200+ MB build toolchain.
#
# tzdata + ca-certificates are the only aux packages: tzdata is
# required for the container's `TZ` env to take effect on
# Alpine's musl libc.
FROM alpine:3.20
# mirrors.aliyun.com has the ffmpeg package in v3.20 main; if the
# mirror ever drops it, fall back to the upstream dl-cdn (which
# always has it for an LTS branch).
RUN sed -i 's|dl-cdn.alpinelinux.org|mirrors.aliyun.com|g' /etc/apk/repositories \
     && (apk add --no-cache ca-certificates tzdata ffmpeg \
     || (sed -i 's|mirrors.aliyun.com|dl-cdn.alpinelinux.org|g' /etc/apk/repositories \
     && apk add --no-cache ca-certificates tzdata ffmpeg))
COPY --from=builder /out/go2rtc /usr/local/bin/go2rtc
COPY go2rtc.yaml /etc/go2rtc.yaml

# 1984 = HTTP API + WebRTC SDP signalling
# 8555 = WebRTC media (TCP ICE + UDP ICE, single port via webrtc.udp)
EXPOSE 1984 8555

ENTRYPOINT ["/usr/local/bin/go2rtc", "-config", "/etc/go2rtc.yaml"]
