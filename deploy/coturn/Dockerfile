# coturn Dockerfile — pinned to a recent alpine image.
FROM docker.m.daocloud.io/library/alpine:3.20
RUN apk add --no-cache ca-certificates tzdata coturn
COPY turnserver.conf /etc/coturn/turnserver.conf
EXPOSE 3478 3478/udp 5349 5349/udp 49160-49200/udp
ENTRYPOINT ["turnserver", "-c", "/etc/coturn/turnserver.conf"]
