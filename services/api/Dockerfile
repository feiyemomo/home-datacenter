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
# recording segments into 60s clips on the fly (stream copy, no
# re-encoding — sub-second for ~24MB of data).
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

EXPOSE 8080

CMD ["/server"]