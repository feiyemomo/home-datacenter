//go:build linux

package maintenance

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// CPU/memory usage readers for Linux. Inside the Docker container
// /proc/stat and /proc/meminfo expose the HOST kernel's counters (the
// container shares the host kernel and /proc is not namespaced for
// these files), so the returned values are host-wide — exactly what a
// whole-box monitor wants.

// memUsage returns total and available memory in bytes from
// /proc/meminfo. "available" maps to MemAvailable (present since
// kernel 3.14), which is what free(1) reports and is a better "can I
// safely keep running" signal than MemFree (which ignores reclaimable
// page cache).
func memUsage() (total, available uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total, _ = parseMemLine(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			available, _ = parseMemLine(line)
		}
		if total > 0 && available > 0 {
			break
		}
	}
	if sc.Err() != nil {
		return 0, 0, sc.Err()
	}
	if total == 0 {
		return 0, 0, os.ErrNotExist
	}
	return total, available, nil
}

// parseMemLine parses "MemTotal:       16384000 kB" → bytes.
func parseMemLine(line string) (uint64, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, false
	}
	v, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, false
	}
	// The unit is always kB in /proc/meminfo.
	return v * 1024, true
}

// cpuUsage returns the aggregate CPU time in jiffies (total and idle)
// from /proc/stat. The caller computes the delta between two samples
// to get a usage percentage. The first line is the per-CPU aggregate:
//
//	cpu  user nice system idle iowait irq softirq steal guest guest_nice
//
// idle = field 4; total = sum of fields 1..8 (excluding guest/guest_nice
// which are already counted in user/nice).
func cpuUsage() (total, idle uint64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			break
		}
		var vals [8]uint64
		for i := 1; i <= 8; i++ {
			vals[i-1], _ = strconv.ParseUint(fields[i], 10, 64)
		}
		// vals = user nice system idle iowait irq softirq steal
		idle = vals[3] + vals[4] // idle + iowait
		total = 0
		for _, v := range vals {
			total += v
		}
		return total, idle, nil
	}
	if sc.Err() != nil {
		return 0, 0, sc.Err()
	}
	return 0, 0, os.ErrNotExist
}
