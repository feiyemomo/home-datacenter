FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

RUN go build -o server .

FROM alpine:latest

WORKDIR /

COPY --from=builder /app/server .

EXPOSE 8080

CMD ["/server"]