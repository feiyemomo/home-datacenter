package camera

import (
	"path/filepath"
	"testing"
)

func TestAIAlertSourceManager(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := GetGlobalAIAlertSourceManager(tmpDir)

	// Default should be camera
	if s := mgr.GetSource(); s != AISourceCamera {
		t.Fatalf("expected default %q, got %q", AISourceCamera, s)
	}

	if mgr.ShouldProcessFrigateAlert(1) {
		t.Errorf("frigate alert should be suppressed in camera mode")
	}
	if !mgr.ShouldProcessCameraAlert(1) {
		t.Errorf("camera alert should be allowed in camera mode")
	}

	// Switch to frigate
	if err := mgr.SetSource(AISourceFrigate); err != nil {
		t.Fatalf("set frigate failed: %v", err)
	}
	if !mgr.ShouldProcessFrigateAlert(1) {
		t.Errorf("frigate alert should be allowed in frigate mode")
	}
	if mgr.ShouldProcessCameraAlert(1) {
		t.Errorf("camera alert should be suppressed in frigate mode")
	}

	// Switch to both
	if err := mgr.SetSource(AISourceBoth); err != nil {
		t.Fatalf("set both failed: %v", err)
	}
	// First event should pass
	if !mgr.ShouldProcessCameraAlert(10) {
		t.Errorf("first camera alert in both mode should pass")
	}
	// Second immediate event for same camera from frigate should be suppressed
	if mgr.ShouldProcessFrigateAlert(10) {
		t.Errorf("immediate second event across source for same camera should be suppressed")
	}

	// Different camera should pass
	if !mgr.ShouldProcessFrigateAlert(20) {
		t.Errorf("event for another camera should pass")
	}

	// Persistence check
	mgr2 := &AIAlertSourceManager{configPath: filepath.Join(tmpDir, "ai_alert_source.json")}
	mgr2.load()
	if mgr2.GetSource() != AISourceBoth {
		t.Errorf("expected persisted %q, got %q", AISourceBoth, mgr2.GetSource())
	}
}
