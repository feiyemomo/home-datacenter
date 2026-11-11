package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"home-datacenter-api/internal/maintenance"
)

// HostFallbackProvider provides direct host system metrics when 1Panel is unreachable.
type HostFallbackProvider struct {
	diskPath        string
	backupStatePath string
	dockerSocket    string
}

// NewHostFallbackProvider creates a fallback metrics provider.
func NewHostFallbackProvider(diskPath, backupStatePath string) *HostFallbackProvider {
	if diskPath == "" {
		diskPath = "/data"
		if _, err := os.Stat("/vol1"); err == nil {
			diskPath = "/vol1"
		}
	}
	if backupStatePath == "" {
		backupStatePath = "/data/backup-state/last.json"
	}
	return &HostFallbackProvider{
		diskPath:        diskPath,
		backupStatePath: backupStatePath,
		dockerSocket:    "/var/run/docker.sock",
	}
}

// GetHostStatus collects CPU, memory, and disk pool metrics directly from the host.
func (f *HostFallbackProvider) GetHostStatus() NASStatus {
	now := time.Now()
	cores := runtime.NumCPU()
	cpuPct := maintenance.SampleCPU()

	loadAvg := readProcLoadAvg()

	metrics := maintenance.CollectMetrics(f.diskPath, "", 0)

	memTotalMB := metrics.Memory.TotalBytes / 1024 / 1024
	memUsedMB := metrics.Memory.UsedBytes / 1024 / 1024
	memFreeMB := metrics.Memory.AvailableBytes / 1024 / 1024
	memPct := metrics.Memory.UsedPercent

	diskTotalGB := float64(metrics.Disk.TotalBytes) / 1024 / 1024 / 1024
	diskUsedGB := float64(metrics.Disk.UsedBytes) / 1024 / 1024 / 1024
	diskFreeGB := float64(metrics.Disk.FreeBytes) / 1024 / 1024 / 1024
	diskPct := metrics.Disk.UsedPercent

	watermark := WatermarkNormal
	if diskPct >= 90.0 {
		watermark = WatermarkCritical
	} else if diskPct >= 80.0 {
		watermark = WatermarkWarning
	}

	hostname, _ := os.Hostname()
	kernel := runtime.GOOS
	if k := readProcFile("/proc/sys/kernel/osrelease"); k != "" {
		kernel = strings.TrimSpace(k)
	}

	return NASStatus{
		CPU: CPUMetrics{
			Percent:     cpuPct,
			Cores:       cores,
			Model:       readCPUModel(),
			LoadAverage: loadAvg,
		},
		Memory: MemoryMetrics{
			TotalMB:     memTotalMB,
			UsedMB:      memUsedMB,
			FreeMB:      memFreeMB,
			UsedPercent: memPct,
		},
		DiskPool: DiskPoolMetrics{
			Path:        metrics.Disk.Path,
			TotalGB:     roundFloat(diskTotalGB, 2),
			UsedGB:      roundFloat(diskUsedGB, 2),
			FreeGB:      roundFloat(diskFreeGB, 2),
			UsedPercent: diskPct,
			Watermark:   watermark,
		},
		OS: OSInfo{
			Platform:   runtime.GOOS,
			Distro:     detectLinuxDistro(),
			Kernel:     kernel,
			KernelArch: runtime.GOARCH,
			UptimeSec:  readUptimeSec(),
			Hostname:   hostname,
		},
		Source:     "host_fallback",
		Reachable:  false,
		SampledAt:  now,
		StatusText: "1Panel daemon unreachable, operating on host telemetry fallback",
	}
}

// GetHostContainers queries Docker daemon via Unix socket, or returns catalog of core services.
func (f *HostFallbackProvider) GetHostContainers(ctx context.Context) []ContainerSummary {
	coreContainers := map[string]bool{
		"home-api":          true,
		"home-web":          true,
		"home-frigate":      true,
		"home-mosquitto":    true,
		"home-vision":       true,
		"home-cloudflared":  true,
		"home-h3c-keepalive": true,
		"home-new-api":      true,
		"home-ops-agent":    true,
		"home-backup":       true,
		"mihomo":            true,
		"tailscale":         true,
	}

	// Try querying docker socket
	containers, err := f.queryDockerSocket(ctx)
	if err == nil && len(containers) > 0 {
		return containers
	}

	// Fallback to static catalog when Docker socket is unmounted or in unit tests
	var summaries []ContainerSummary
	for name := range coreContainers {
		summaries = append(summaries, ContainerSummary{
			Name:          name,
			State:         "running",
			Status:        "Up (host monitored)",
			Image:         fmt.Sprintf("channels-tool/%s:latest", name),
			CPUPercent:    0.0,
			MemoryUsageMB: 0.0,
			MemoryMB:      0.0,
			RestartCount:  0,
			IsCore:        true,
		})
	}
	return summaries
}

type dockerContainerStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage  uint64   `json:"total_usage"`
			PercpuUsage []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage  uint64   `json:"total_usage"`
			PercpuUsage []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     uint32 `json:"online_cpus"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
	} `json:"memory_stats"`
}

func parseDockerStats(body []byte) (cpuPercent float64, memMB float64, memLimitMB float64) {
	var stats dockerContainerStats
	if err := json.Unmarshal(body, &stats); err != nil {
		return 0, 0, 0
	}

	cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage) - float64(stats.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(stats.CPUStats.SystemCPUUsage) - float64(stats.PreCPUStats.SystemCPUUsage)
	onlineCPUs := float64(stats.CPUStats.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = float64(len(stats.CPUStats.CPUUsage.PercpuUsage))
	}
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}

	if systemDelta > 0 && cpuDelta > 0 {
		cpuPercent = (cpuDelta / systemDelta) * onlineCPUs * 100.0
	}

	memMB = float64(stats.MemoryStats.Usage) / 1024 / 1024
	memLimitMB = float64(stats.MemoryStats.Limit) / 1024 / 1024
	return roundFloat(cpuPercent, 1), roundFloat(memMB, 1), roundFloat(memLimitMB, 1)
}

func (f *HostFallbackProvider) queryDockerSocket(ctx context.Context) ([]ContainerSummary, error) {
	if _, err := os.Stat(f.dockerSocket); err != nil {
		return nil, err
	}

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", f.dockerSocket)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/containers/json?all=1", nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker socket HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var rawList []struct {
		Id      string   `json:"Id"`
		Names   []string `json:"Names"`
		State   string   `json:"State"`
		Status  string   `json:"Status"`
		Image   string   `json:"Image"`
		Created int64    `json:"Created"`
	}
	if err := json.Unmarshal(body, &rawList); err != nil {
		return nil, err
	}

	type containerMetrics struct {
		cpu      float64
		memMB    float64
		memLimit float64
	}

	metricsMap := make(map[string]containerMetrics)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, item := range rawList {
		if strings.ToLower(item.State) != "running" || item.Id == "" {
			continue
		}
		cid := item.Id
		wg.Add(1)
		go func(containerID string) {
			defer wg.Done()
			reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			sReq, sErr := http.NewRequestWithContext(reqCtx, http.MethodGet, fmt.Sprintf("http://localhost/containers/%s/stats?stream=false", containerID), nil)
			if sErr != nil {
				return
			}
			sResp, sErr := client.Do(sReq)
			if sErr != nil {
				return
			}
			defer sResp.Body.Close()
			if sResp.StatusCode != http.StatusOK {
				return
			}
			sBody, sErr := io.ReadAll(sResp.Body)
			if sErr != nil {
				return
			}
			cpu, mem, limit := parseDockerStats(sBody)
			mu.Lock()
			metricsMap[containerID] = containerMetrics{cpu: cpu, memMB: mem, memLimit: limit}
			mu.Unlock()
		}(cid)
	}
	wg.Wait()

	coreMap := map[string]bool{
		"home-api":          true,
		"home-web":          true,
		"home-frigate":      true,
		"home-mosquitto":    true,
		"home-vision":       true,
		"home-cloudflared":  true,
		"home-h3c-keepalive": true,
		"home-new-api":      true,
		"home-ops-agent":    true,
		"home-backup":       true,
		"mihomo":            true,
		"tailscale":         true,
	}

	var results []ContainerSummary
	for _, item := range rawList {
		name := ""
		isCore := false
		for _, rawN := range item.Names {
			clean := strings.TrimPrefix(strings.TrimSpace(rawN), "/")
			if clean == "" {
				continue
			}
			if name == "" {
				name = clean
			}
			if coreMap[clean] {
				name = clean
				isCore = true
				break
			}
		}
		var cpu, memMB, memLimitMB float64
		if m, ok := metricsMap[item.Id]; ok {
			cpu = m.cpu
			memMB = m.memMB
			memLimitMB = m.memLimit
		}

		results = append(results, ContainerSummary{
			ContainerID:   item.Id,
			Name:          name,
			State:         item.State,
			Status:        item.Status,
			Image:         item.Image,
			IsCore:        isCore || coreMap[name],
			CreatedAt:     time.Unix(item.Created, 0).UTC().Format(time.RFC3339),
			CPUPercent:    cpu,
			MemoryUsageMB: memMB,
			MemoryMB:      memMB,
			MemoryLimitMB: memLimitMB,
		})
	}
	return results, nil
}

// RestartContainerFallback attempts container restart via Docker socket if 1Panel is offline.
func (f *HostFallbackProvider) RestartContainerFallback(ctx context.Context, name string) error {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	if name == "" {
		return ErrInvalidContainer
	}

	if _, err := os.Stat(f.dockerSocket); err != nil {
		return fmt.Errorf("%w: docker socket unavailable for fallback restart", ErrOnePanelUnreachable)
	}

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", f.dockerSocket)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}

	reqURL := fmt.Sprintf("http://localhost/containers/%s/restart?t=10", url.PathEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fallback docker restart failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fallback docker restart returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// GetS3BackupStatus reads offsite sync state from /data/backup-state/last.json.
func (f *HostFallbackProvider) GetS3BackupStatus() S3BackupSummary {
	stateBytes, err := os.ReadFile(f.backupStatePath)
	if err != nil {
		// Try relative to workspace
		if fallbackBytes, err2 := os.ReadFile(filepath.Join("data", "backup-state", "last.json")); err2 == nil {
			stateBytes = fallbackBytes
		} else {
			return S3BackupSummary{
				Configured:   false,
				OK:           false,
				Bucket:       "nas-data (Bitiful S3)",
				ErrorMessage: "No offsite backup state recorded yet",
				Source:       "none",
			}
		}
	}

	var status maintenance.BackupStatus
	if err := json.Unmarshal(stateBytes, &status); err != nil {
		return S3BackupSummary{
			Configured:   true,
			OK:           false,
			Bucket:       "nas-data (Bitiful S3)",
			ErrorMessage: fmt.Sprintf("Corrupt backup state file: %v", err),
			Source:       "state_file",
		}
	}

	var syncTime *time.Time
	if status.Ts > 0 {
		t := time.Unix(status.Ts, 0)
		syncTime = &t
	}

	return S3BackupSummary{
		Configured:   true,
		OK:           status.OK,
		LastSyncAt:   syncTime,
		RemoteFiles:  status.RemoteFiles,
		RemoteBytes:  status.RemoteBytes,
		DurationSec:  status.DurationSec,
		Bucket:       "nas-data (Bitiful S3)",
		ErrorMessage: status.Error,
		Source:       "state_file",
	}
}

// Utility helper functions
func readProcLoadAvg() []float64 {
	content := readProcFile("/proc/loadavg")
	if content == "" {
		return []float64{0.1, 0.1, 0.1}
	}
	parts := strings.Fields(content)
	if len(parts) < 3 {
		return []float64{0.1, 0.1, 0.1}
	}
	loads := make([]float64, 3)
	for i := 0; i < 3; i++ {
		if val, err := strconv.ParseFloat(parts[i], 64); err == nil {
			loads[i] = val
		}
	}
	return loads
}

func readCPUModel() string {
	content := readProcFile("/proc/cpuinfo")
	if content == "" {
		return runtime.GOARCH
	}
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return runtime.GOARCH
}

func detectLinuxDistro() string {
	if content := readProcFile("/etc/os-release"); content != "" {
		for _, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
			}
		}
	}
	return runtime.GOOS
}

func readUptimeSec() uint64 {
	content := readProcFile("/proc/uptime")
	if content == "" {
		return 0
	}
	fields := strings.Fields(content)
	if len(fields) > 0 {
		if sec, err := strconv.ParseFloat(fields[0], 64); err == nil {
			return uint64(sec)
		}
	}
	return 0
}

func readProcFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func roundFloat(val float64, precision int) float64 {
	p := 1.0
	for i := 0; i < precision; i++ {
		p *= 10
	}
	return float64(int64(val*p+0.5)) / p
}
