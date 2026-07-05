#!/bin/sh
# Disassemble the go2rtc binary and look at the time.AfterFunc
# call site for the keepalive argument. Go inlines the keepalive
# const into the MOV immediate that gets passed to the runtime
# timer. We want to see the actual byte sequence at the call
# site, not random matches elsewhere.
set -eu
apk add --no-cache binutils 2>/dev/null || true
# Dump the .text section and search for the AfterFunc call pattern.
# The keepalive nanosecond value is loaded via a 64-bit MOV imm
# into a register, then passed to the function. The two candidates
# are 0x12a05f200 (5e9) and 0x6fc23ac00 (3e10).
objdump -d /usr/local/bin/go2rtc 2>/dev/null > /tmp/dis.txt || \
    objdump -d /tmp/go2rtc.bin > /tmp/dis.txt
echo "=== movabs with 5e9 (0x12a05f200) ==="
grep -B1 -A1 "0x12a05f200" /tmp/dis.txt | head -40
echo ""
echo "=== movabs with 3e10 (0x6fc23ac00) ==="
grep -B1 -A1 "0x6fc23ac00" /tmp/dis.txt | head -40
echo ""
echo "=== count of each ==="
echo "5e9:  $(grep -c '0x12a05f200' /tmp/dis.txt)"
echo "3e10: $(grep -c '0x6fc23ac00' /tmp/dis.txt)"
