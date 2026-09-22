package maintenance

import (
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type MetricsSnapshot struct {
	CPU            CPUMetrics        `json:"cpu"`
	Memory         MemoryMetrics     `json:"memory"`
	Disk           DiskMetrics       `json:"disk"`
	Recordings     RecordingsMetrics `json:"recordings"`
	TranscodeCache CacheMetrics      `json:"transcode_cache"`
}

type CPUMetrics struct {
	Percent float64 `json:"percent"`
	Cores   int     `json:"cores"`
}

type MemoryMetrics struct {
	TotalBytes     uint64  `json:"total_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}

type DiskMetrics struct {
	Path        string  `json:"path"`
	TotalBytes  uint64  `json:"total_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

type RecordingsMetrics struct {
	SizeBytes    uint64  `json:"size_bytes"`
	QuotaBytes   uint64  `json:"quota_bytes"`
	QuotaPercent float64 `json:"quota_percent"`
	QuotaActive  bool    `json:"quota_active"`
}

type CacheMetrics struct {
	SizeBytes uint64 `json:"size_bytes"`
	FileCount int    `json:"file_count"`
}

var (
	metricsMu       sync.RWMutex
	lastCPUTotal    uint64
	lastCPUIdle     uint64
	lastCPUPct      float64
	lastCPUSampleAt time.Time
)

// SampleCPU computes the CPU usage delta since the last call.
func SampleCPU() float64 {
	tot, idle, err := cpuUsage()
	if err != nil {
		return 0
	}

	metricsMu.Lock()
	defer metricsMu.Unlock()

	if lastCPUTotal == 0 || time.Since(lastCPUSampleAt) > 30*time.Second {
		lastCPUTotal = tot
		lastCPUIdle = idle
		lastCPUSampleAt = time.Now()
		return lastCPUPct
	}

	dTotal := tot - lastCPUTotal
	dIdle := idle - lastCPUIdle
	lastCPUTotal = tot
	lastCPUIdle = idle
	lastCPUSampleAt = time.Now()

	if dTotal > 0 {
		dUsed := dTotal - dIdle
		pct := math.Round((float64(dUsed)/float64(dTotal))*1000) / 10
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		lastCPUPct = pct
	}
	return lastCPUPct
}

// CollectMetrics aggregates host and datacenter storage telemetry.
func CollectMetrics(diskPath string, recordingsDir string, quotaBytes uint64) MetricsSnapshot {
	cpuPct := SampleCPU()
	cores := runtime.NumCPU()

	var mem MemoryMetrics
	if tot, avail, err := memUsage(); err == nil && tot > 0 {
		var used uint64
		if tot >= avail {
			used = tot - avail
		}
		mem = MemoryMetrics{
			TotalBytes:     tot,
			AvailableBytes: avail,
			UsedBytes:      used,
			UsedPercent:    math.Round((float64(used)/float64(tot))*1000) / 10,
		}
	}

	if diskPath == "" {
		diskPath = "/data"
	}
	var disk DiskMetrics
	disk.Path = diskPath
	if tot, avail, err := diskUsage(diskPath); err == nil && tot > 0 {
		var used uint64
		if tot >= avail {
			used = tot - avail
		}
		disk = DiskMetrics{
			Path:        diskPath,
			TotalBytes:  tot,
			FreeBytes:   avail,
			UsedBytes:   used,
			UsedPercent: math.Round((float64(used)/float64(tot))*1000) / 10,
		}
	}

	var rec RecordingsMetrics
	rec.QuotaBytes = quotaBytes
	if recordingsDir != "" {
		if sz, _, err := dirSize(recordingsDir); err == nil {
			rec.SizeBytes = sz
			if quotaBytes > 0 {
				pct := math.Round((float64(sz)/float64(quotaBytes))*1000) / 10
				rec.QuotaPercent = pct
				rec.QuotaActive = sz >= quotaBytes
			}
		}
	}

	var cache CacheMetrics
	if recordingsDir != "" {
		cacheDir := filepath.Join(recordingsDir, ".transcode-cache")
		if sz, cnt, err := dirSize(cacheDir); err == nil {
			cache.SizeBytes = sz
			cache.FileCount = cnt
		}
	}

	return MetricsSnapshot{
		CPU:            CPUMetrics{Percent: cpuPct, Cores: cores},
		Memory:         mem,
		Disk:           disk,
		Recordings:     rec,
		TranscodeCache: cache,
	}
}

// CleanTranscodeCache purges all files under <recordingsDir>/.transcode-cache.
func CleanTranscodeCache(recordingsDir string) (uint64, int, error) {
	if recordingsDir == "" {
		return 0, 0, nil
	}
	cacheDir := filepath.Join(recordingsDir, ".transcode-cache")
	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		return 0, 0, nil
	}

	var reclaimed uint64
	var deleted int

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return 0, 0, err
	}

	for _, entry := range entries {
		subPath := filepath.Join(cacheDir, entry.Name())
		if entry.IsDir() {
			// Subdirectories are camera IDs (e.g. .transcode-cache/1/...)
			files, fErr := os.ReadDir(subPath)
			if fErr != nil {
				continue
			}
			for _, file := range files {
				filePath := filepath.Join(subPath, file.Name())
				if info, iErr := file.Info(); iErr == nil {
					reclaimed += uint64(info.Size())
				}
				if rErr := os.Remove(filePath); rErr == nil {
					deleted++
				} else {
					log.Printf("clean-cache: remove file %s failed: %v", filePath, rErr)
				}
			}
		} else {
			if info, iErr := entry.Info(); iErr == nil {
				reclaimed += uint64(info.Size())
			}
			if rErr := os.Remove(subPath); rErr == nil {
				deleted++
			}
		}
	}

	return reclaimed, deleted, nil
}
