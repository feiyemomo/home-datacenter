FROM golang:1.26-alpine AS builder

WORKDIR /app

# Configure Go proxy for faster downloads in China
ENV GOPROXY=https://goproxy.cn,https://goproxy.io,direct
ENV GOSUMDB=off

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

RUN go build -o server ./cmd

FROM alpine:latest

WORKDIR /

# ffmpeg is used by PlayRecording to concatenate Frigate's 10s
# recording segments into 60s clips and transcode HEVC→H.264 on the
# fly (v1.8.25). Alpine's ffmpeg bundles libx264 + aac, so no extra
# packages are needed for the browser-compatible h264/aac output.
# tzdata is required so the TZ=Asia/Shanghai env var (set in
# compose.yaml) actually takes effect. Without tzdata, Alpine's
# musl libc has no timezone database — `date` returns UTC and
# Go's time.LoadLocation("Asia/Shanghai") fails, falling back to
# time.Local (UTC). This caused the "8-hour time offset" bug:
# RecordingSegmentsForMinute used .In(loc) with loc=UTC instead
# of Asia/Shanghai, so it looked up recording directories 8 hours
# behind the real LOCAL date/hour layout that Frigate (with tzdata
# baked into its Debian-based image) had created.
RUN apk add --no-cache ffmpeg tzdata

COPY --from=builder /app/server .
COPY --from=builder /app/configs ./configs

# Run as a non-root user (UID 1000) for defense in depth. The
# binary and configs are world-readable by default (go build
# produces 0755, COPY preserves source perms), so the app user
# can execute /server and read /configs without extra chmod.
#
# /data is the mount point for bind volumes (sqlite, recordings).
# We create it here and chown it so the directory exists with
# correct ownership even before the bind mount is attached. Note:
# for bind mounts the host directory's permissions take precedence
# — ensure ./data/sqlite and ./data/recordings on the host are
# writable by UID 1000.
RUN adduser -D -u 1000 app \
    && mkdir -p /data \
    && chown app:app /data

USER app

EXPOSE 8080

CMD ["/server"]
