# Multi-stage Dockerfile for go2rtc.
#
# Stage 1: fetch the official linux/amd64 binary from GitHub
# Releases. We pin the version via build arg so reproducible
# builds are possible.
FROM docker.m.daocloud.io/library/alpine:3.20 AS fetch
ARG GO2RTC_VERSION=1.9.5
RUN apk add --no-cache curl ca-certificates \
 && curl -fsSL -o /usr/local/bin/go2rtc \
      "https://github.com/AlexxIT/go2rtc/releases/download/v${GO2RTC_VERSION}/go2rtc_linux_amd64" \
 && chmod +x /usr/local/bin/go2rtc \
 && /usr/local/bin/go2rtc --version

# Stage 2: slim runtime image.
FROM docker.m.daocloud.io/library/alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=fetch /usr/local/bin/go2rtc /usr/local/bin/go2rtc
COPY go2rtc.yaml /etc/go2rtc.yaml

# 1984 = HTTP API + WebRTC SDP signalling
# 1985 = WebRTC media (UDP) — not used today; reserved for direct
#        media path if you stop going through Cloudflare Tunnel.
EXPOSE 1984 1985

ENTRYPOINT ["/usr/local/bin/go2rtc", "-config", "/etc/go2rtc.yaml"]
