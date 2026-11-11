package ops

import (
	"time"
)

// WatermarkLevel represents storage capacity watermark thresholds.
type WatermarkLevel string

const (
	WatermarkNormal   WatermarkLevel = "normal"
	WatermarkWarning  WatermarkLevel = "warning"
	WatermarkCritical WatermarkLevel = "critical"
)

// CPUMetrics contains host CPU performance telemetry.
type CPUMetrics struct {
	Percent     float64   `json:"percent"`
	Cores       int       `json:"cores"`
	Model       string    `json:"model,omitempty"`
	LoadAverage []float64 `json:"load_average,omitempty"` // [1m, 5m, 15m]
}

// MemoryMetrics contains system memory utilization telemetry.
type MemoryMetrics struct {
	TotalMB     uint64  `json:"total_mb"`
	UsedMB      uint64  `json:"used_mb"`
	FreeMB      uint64  `json:"free_mb"`
	UsedPercent float64 `json:"used_percent"`
}

// DiskPoolMetrics captures primary storage pool usage and watermarks.
type DiskPoolMetrics struct {
	Path        string         `json:"path"`
	TotalGB     float64        `json:"total_gb"`
	UsedGB      float64        `json:"used_gb"`
	FreeGB      float64        `json:"free_gb"`
	UsedPercent float64        `json:"used_percent"`
	Watermark   WatermarkLevel `json:"watermark"` // normal (<80%), warning (80-90%), critical (>90%)
}

// OSInfo provides host platform and kernel details.
type OSInfo struct {
	Platform     string `json:"platform"`
	Distro       string `json:"distro"`
	Kernel       string `json:"kernel"`
	KernelArch   string `json:"kernel_arch"`
	UptimeSec    uint64 `json:"uptime_sec"`
	Hostname     string `json:"hostname,omitempty"`
	PanelVersion string `json:"panel_version,omitempty"`
}

// NASStatus is the aggregated telemetry returned by GET /api/v1/ops/nas/status.
type NASStatus struct {
	CPU        CPUMetrics      `json:"cpu"`
	Memory     MemoryMetrics   `json:"memory"`
	DiskPool   DiskPoolMetrics `json:"disk_pool"`
	OS         OSInfo          `json:"os"`
	Source     string          `json:"source"` // "1panel" or "host_fallback"
	Reachable  bool            `json:"reachable"`
	SampledAt  time.Time       `json:"sampled_at"`
	StatusText string          `json:"status_text"`
}

// ContainerSummary describes a Docker container's health and resource consumption.
type ContainerSummary struct {
	ContainerID   string  `json:"container_id,omitempty"`
	Name          string  `json:"name"`
	State         string  `json:"state"`  // "running", "exited", "created", etc.
	Status        string  `json:"status"` // human-readable status, e.g. "Up 2 days"
	Image         string  `json:"image"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryUsageMB float64 `json:"memory_usage_mb"`
	MemoryMB      float64 `json:"memory_mb"` // compatibility field matching Android OpsViewModel.kt
	MemoryLimitMB float64 `json:"memory_limit_mb"`
	RestartCount  int     `json:"restart_count"`
	IsCore        bool    `json:"is_core"`
	CreatedAt     string  `json:"created_at,omitempty"`
}

// ContainerRestartResponse is returned after initiating a container restart.
type ContainerRestartResponse struct {
	Name        string    `json:"name"`
	Success     bool      `json:"success"`
	Message     string    `json:"message"`
	Operator    string    `json:"operator"`
	RestartedAt time.Time `json:"restarted_at"`
}

// S3BackupSummary describes offsite backup health.
type S3BackupSummary struct {
	Configured   bool       `json:"configured"`
	OK           bool       `json:"ok"`
	LastSyncAt   *time.Time `json:"last_sync_at,omitempty"`
	RemoteFiles  int64      `json:"remote_files"`
	RemoteBytes  int64      `json:"remote_bytes"`
	DurationSec  float64    `json:"duration_sec,omitempty"`
	Bucket       string     `json:"bucket,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	Source       string     `json:"source"` // "state_file", "1panel", "none"
}

// SecuritySummary aggregates WAF statistics and offsite backup status.
type SecuritySummary struct {
	TodayWAFBlockedCount int64           `json:"today_waf_blocked_count"`
	TotalBlockedHits     int64           `json:"total_blocked_hits"`
	ActiveBannedIPs      int             `json:"active_banned_ips"`
	LastBlockedAt        *time.Time      `json:"last_blocked_at,omitempty"`
	S3Backup             S3BackupSummary `json:"s3_backup"`
	Source               string          `json:"source"`
	SampledAt            time.Time       `json:"sampled_at"`
}

// 1Panel API Internal DTO Structures
type onePanelResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

type onePanelNodeMetrics struct {
	Load1              float64   `json:"load1"`
	Load5              float64   `json:"load5"`
	Load15             float64   `json:"load15"`
	LoadUsagePercent   float64   `json:"loadUsagePercent"`
	CPUUsedPercent     float64   `json:"cpuUsedPercent"`
	CPUUsed            float64   `json:"cpuUsed"`
	CPUTotal           int       `json:"cpuTotal"`
	CPUModel           string    `json:"cpuModel"`
	CPUDetailedPercent []float64 `json:"cpuDetailedPercent"`
	MemoryTotal        uint64    `json:"memoryTotal"`
	MemoryUsed         uint64    `json:"memoryUsed"`
	MemoryAvailable    uint64    `json:"memoryAvailable"`
	MemoryFree         uint64    `json:"memoryFree"`
	MemoryUsedPercent  float64   `json:"memoryUsedPercent"`
	Uptime             uint64    `json:"uptime"`
}

type onePanelOSInfo struct {
	OS            string `json:"os"`
	Platform      string `json:"platform"`
	PlatformVer   string `json:"platformFamily"`
	KernelArch    string `json:"kernelArch"`
	KernelVersion string `json:"kernelVersion"`
	PrettyDistro  string `json:"prettyDistro"`
	DiskSize      uint64 `json:"diskSize"`
	Hostname      string `json:"hostname"`
}

type onePanelContainerItem struct {
	ContainerID string  `json:"containerID"`
	Name        string  `json:"name"`
	State       string  `json:"state"`
	Status      string  `json:"status"`
	Image       string  `json:"image"`
	CPUPerc     float64 `json:"cpuPerc"`
	MemPerc     float64 `json:"memPerc"`
	MemUsage    uint64  `json:"memUsage"`
	MemLimit    uint64  `json:"memLimit"`
	Created     string  `json:"created"`
}

type onePanelContainerPage struct {
	Items []onePanelContainerItem `json:"items"`
	Total int                     `json:"total"`
}

type onePanelStatsItem struct {
	ContainerID   string  `json:"containerID"`
	CPUPercent    float64 `json:"cpuPercent"`
	MemoryUsage   uint64  `json:"memoryUsage"`   // bytes
	MemoryLimit   uint64  `json:"memoryLimit"`   // bytes
	MemoryPercent float64 `json:"memoryPercent"`
}

