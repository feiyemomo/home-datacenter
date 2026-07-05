FROM golang:1.22-alpine
ARG GO2RTC_KEEPALIVE_SECONDS=30

# China-friendly mirrors: apk (Alpine) and Go modules.
RUN sed -i 's|dl-cdn.alpinelinux.org|mirrors.aliyun.com|g' /etc/apk/repositories \
    && apk add --no-cache git ca-certificates \
    && go env -w GOPROXY=https://goproxy.cn,direct

WORKDIR /src
# Copy already-patched source from build-host/go2rtc on the host.
COPY go2rtc/ /src/

RUN echo "=== check source ===" \
    && grep -n "const keepalive" internal/hls/hls.go \
    && echo "=== build ===" \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath -o /out/go2rtc . \
    && ls -la /out/go2rtc \
    && md5sum /out/go2rtc
