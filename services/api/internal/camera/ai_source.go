package camera

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"home-datacenter-api/internal/config"
)

const (
	AISourceCamera  = "camera"  // 仅使用摄像头自带硬件 AI 识别
	AISourceFrigate = "frigate" // 仅使用 Frigate 软件检测
	AISourceBoth    = "both"    // 双路融合互备（带跨源智能去重）
)

// AIAlertSourceManager coordinates and controls AI detection alert sources
// to prevent duplicate notifications between camera hardware AI and Frigate AI.
type AIAlertSourceManager struct {
	mu             sync.RWMutex
	source         string
	motionFallback bool
	configPath     string

	recentMu     sync.Mutex
	recentAlerts map[uint]time.Time
}

type AISourceInfo struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Active      bool   `json:"active"`
}

type AISourceStatus struct {
	CurrentSource    string         `json:"current_source"`
	MotionFallback   bool           `json:"motion_fallback"`
	AvailableSources []AISourceInfo `json:"available_sources"`
	UpdatedAt        string         `json:"updated_at"`
}

var (
	globalSourceMgr *AIAlertSourceManager
	sourceOnce      sync.Once
)

// GetGlobalAIAlertSourceManager returns the shared singleton instance.
func GetGlobalAIAlertSourceManager(dataDir string) *AIAlertSourceManager {
	sourceOnce.Do(func() {
		cfgFile := filepath.Join(dataDir, "ai_alert_source.json")
		globalSourceMgr = &AIAlertSourceManager{
			source:       AISourceCamera, // 默认优先摄像头自带硬件 AI
			configPath:   cfgFile,
			recentAlerts: make(map[uint]time.Time),
		}
		globalSourceMgr.load()
	})
	return globalSourceMgr
}

func (m *AIAlertSourceManager) load() {
	if m.configPath == "" {
		return
	}
	data, err := os.ReadFile(m.configPath)
	if err != nil {
		return
	}
	var stored struct {
		Source         string `json:"source"`
		MotionFallback bool   `json:"motion_fallback"`
	}
	if err := json.Unmarshal(data, &stored); err == nil {
		if isValidAISource(stored.Source) {
			m.source = stored.Source
		}
		m.motionFallback = stored.MotionFallback
		log.Printf("ai_source: loaded persisted source mode: %q (motion_fallback=%v)", m.source, m.motionFallback)
	}
}

func (m *AIAlertSourceManager) save() {
	if m.configPath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(m.configPath), 0755)
	payload, _ := json.MarshalIndent(map[string]any{
		"source":          m.source,
		"motion_fallback": m.motionFallback,
		"updated_at":      time.Now().Format(time.RFC3339),
	}, "", "  ")
	_ = os.WriteFile(m.configPath, payload, 0644)
}

func isValidAISource(s string) bool {
	return s == AISourceCamera || s == AISourceFrigate || s == AISourceBoth
}

// GetSource returns the currently active AI alert source mode ("camera" | "frigate" | "both").
func (m *AIAlertSourceManager) GetSource() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.source == "" {
		return AISourceCamera
	}
	return m.source
}

// SetSource updates the AI alert source mode and persists it to disk.
func (m *AIAlertSourceManager) SetSource(source string) error {
	if !isValidAISource(source) {
		return fmt.Errorf("invalid AI alert source: %q (allowed: camera, frigate, both)", source)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.source = source
	m.save()
	log.Printf("ai_source: switched AI alert source mode to %q", source)
	return nil
}

// GetMotionFallback returns true if motion events without explicit human labels
// should be verified via Vision AI rather than directly dropped.
func (m *AIAlertSourceManager) GetMotionFallback() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.motionFallback
}

// SetMotionFallback toggles the motion fallback verification mode and persists it.
func (m *AIAlertSourceManager) SetMotionFallback(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.motionFallback = enabled
	m.save()
	log.Printf("ai_source: switched motion fallback mode to %v", enabled)
}

// ShouldProcessFrigateAlert checks if a Frigate AI event should produce alerts.
func (m *AIAlertSourceManager) ShouldProcessFrigateAlert(cameraID uint) bool {
	src := m.GetSource()
	if src == AISourceCamera {
		// Suppressed: operator chose camera-hardware-only
		return false
	}
	if src == AISourceFrigate {
		return true
	}
	// "both" mode: cross-source deduplication window of 6 seconds
	return m.checkCrossSourceDedup(cameraID)
}

// ShouldProcessCameraAlert checks if a camera hardware ISAPI event should produce alerts.
func (m *AIAlertSourceManager) ShouldProcessCameraAlert(cameraID uint) bool {
	src := m.GetSource()
	if src == AISourceFrigate {
		// Suppressed: operator chose frigate-only
		return false
	}
	if src == AISourceCamera {
		return true
	}
	// "both" mode: cross-source deduplication window of 6 seconds
	return m.checkCrossSourceDedup(cameraID)
}

func (m *AIAlertSourceManager) checkCrossSourceDedup(cameraID uint) bool {
	m.recentMu.Lock()
	defer m.recentMu.Unlock()

	crossDedupSec := 6
	if config.AppConfig != nil && config.AppConfig.Notify.CrossSourceDedupSeconds > 0 {
		crossDedupSec = config.AppConfig.Notify.CrossSourceDedupSeconds
	}

	now := time.Now()
	if last, exists := m.recentAlerts[cameraID]; exists {
		if now.Sub(last) < time.Duration(crossDedupSec)*time.Second {
			// Suppressed duplicate across sources
			return false
		}
	}
	m.recentAlerts[cameraID] = now
	return true
}

// GetStatus returns the descriptive status structure for API consumers.
func (m *AIAlertSourceManager) GetStatus() AISourceStatus {
	cur := m.GetSource()
	fallback := m.GetMotionFallback()
	return AISourceStatus{
		CurrentSource:  cur,
		MotionFallback: fallback,
		AvailableSources: []AISourceInfo{
			{
				Key:         AISourceCamera,
				Name:        "摄像头自带硬件AI (推荐)",
				Description: "直接由摄像头自身芯片识别人形，0% NAS CPU开销，响应快，无重复",
				Active:      cur == AISourceCamera,
			},
			{
				Key:         AISourceFrigate,
				Name:        "Frigate NVR 软件检测",
				Description: "由NAS主机运行OpenVINO检测模型，支持精细绘制防区",
				Active:      cur == AISourceFrigate,
			},
			{
				Key:         AISourceBoth,
				Name:        "双路冗余互备 (智能去重)",
				Description: "同时接收摄像头硬件与Frigate事件，6秒内同相机自动合并去重",
				Active:      cur == AISourceBoth,
			},
		},
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
}
