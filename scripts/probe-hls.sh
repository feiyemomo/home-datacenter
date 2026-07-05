#!/bin/sh
# Probe the HLS media-playlist repeatedly. If the keepalive patch
# landed, sessions should survive >5s of idle; if not, they 404
# after 5s of no segment request.
for i in 1 2 3 4 5 6 7 8; do
  printf "t=%s  " "$(date +%H:%M:%S)"
  wget -q -S -O /dev/null \
    "http://localhost:1984/api/hls/playlist.m3u8?id=2vV8XUZy" 2>&1 \
    | grep -E "^  HTTP|^  Content-Length" | tr '\n' ' '
  printf "\n"
  sleep 6
done
