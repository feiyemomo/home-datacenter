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
RUN apk add --no-cache ffmpeg

COPY --from=builder /app/server .
COPY --from=builder /app/configs ./configs

EXPOSE 8080

CMD ["/server"]