FROM docker.m.daocloud.io/library/golang:1.26-alpine AS builder

WORKDIR /app

# Configure Go proxy for faster downloads in China
ENV GOPROXY=https://goproxy.cn,https://goproxy.io,direct
ENV GOSUMDB=off

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

RUN go build -o server ./cmd

FROM docker.m.daocloud.io/library/alpine:latest

WORKDIR /

COPY --from=builder /app/server .
COPY --from=builder /app/configs ./configs

EXPOSE 8080

CMD ["/server"]