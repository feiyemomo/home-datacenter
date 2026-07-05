#!/bin/sh
# Pull the binary out of the running container and inspect the
# nanosecond values that time.AfterFunc's argument would compile
# to. Go's compiler folds `5 * time.Second` (=5e9) and
# `30 * time.Second` (=3e10) into a single int64 literal at the
# use site. So we look for both 0x12a05f200 (5e9) and 0x6fc23ac00
# (3e10) as raw little-endian byte sequences in the .text/.rodata
# sections. Whichever appears in the timer call site is the
# effective keepalive.
set -eu
out=/tmp/go2rtc.bin
docker cp home-go2rtc:/usr/local/bin/go2rtc "$out" 2>/dev/null || \
  cp /usr/local/bin/go2rtc "$out"
ls -la "$out"
echo "md5: $(md5sum "$out" | awk '{print $1}')"
echo "--- looking for keepalive nanosecond constants ---"
echo "5e9 ns  (5s)   = 0x12a05f200  bytes: 00 5f a0 02 01 00 00 00"
echo "3e10 ns (30s)  = 0x6fc23ac00  bytes: 00 3a c2 6f 00 00 00 00"
python3 - <<'PY'
import struct
needle_5  = struct.pack("<Q", 5_000_000_000)
needle_30 = struct.pack("<Q", 30_000_000_000)
data = open("/tmp/go2rtc.bin","rb").read()
def find_all(needle):
    out, i = [], 0
    while True:
        j = data.find(needle, i)
        if j < 0: break
        out.append(j)
        i = j + 1
    return out
hits5  = find_all(needle_5)
hits30 = find_all(needle_30)
print(f"5e9  occurrences: {len(hits5)}")
print(f"3e10 occurrences: {len(hits30)}")
# Print a small window around the first few hits so we can see
# what's near them.
for label, hits in (("5e9", hits5), ("3e10", hits30)):
    for h in hits[:5]:
        lo = max(0, h - 16)
        hi = min(len(data), h + 24)
        snippet = data[lo:hi]
        # Filter: only count if the surrounding bytes look like
        # the keepalive reference (i.e. the literal is the
        # argument to a function or method call). We do a
        # lightweight heuristic: the bytes before the literal are
        # typically a 1-2 byte LEA / MOV immediate sequence; we
        # just print the snippet and let the human eyeball it.
        print(f"  {label} @ 0x{h:x}: {snippet.hex()}")
PY
