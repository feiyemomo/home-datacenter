#!/bin/sh
# Quick sanity: did the patch land in the binary?
# Look for "30" appearing near "time.Second" in the hls package's
# data section. We can't recover Go source, but the literal "30" in
# the const should still show up as a printable string somewhere.
strings /usr/local/bin/go2rtc | grep -i "hls" | head -20
echo "---"
# Also look at the build's commit hash; if the build was cached
# the patch may have been skipped.
strings /usr/static/build.info 2>/dev/null
