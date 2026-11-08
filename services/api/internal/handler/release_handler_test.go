package handler

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseHandler_Sha256sumFormatParsing(t *testing.T) {
	tempDir := t.TempDir()

	apkName := "app-debug-v1.18.13.apk"
	apkPath := filepath.Join(tempDir, apkName)
	if err := os.WriteFile(apkPath, []byte("fake-apk-binary-content"), 0644); err != nil {
		t.Fatalf("failed to create fake apk: %v", err)
	}

	// 1. Test when .sha256 file has standard sha256sum format: "<hash>  <filename>\n"
	expectedHash := "a8287f643c8c380b96ecdef4db029994a00b39f724310e372c11e9a867534ffe"
	shaContent := expectedHash + "  " + apkName + "\n"
	shaPath := filepath.Join(tempDir, apkName+".sha256")
	if err := os.WriteFile(shaPath, []byte(shaContent), 0644); err != nil {
		t.Fatalf("failed to create sha256 file: %v", err)
	}

	h := &ReleaseHandler{releasesDir: tempDir}
	latest, err := h.findLatest("debug")
	if err != nil {
		t.Fatalf("findLatest failed: %v", err)
	}

	if latest.SHA256 != expectedHash {
		t.Errorf("expected clean sha256 hash %q, got %q", expectedHash, latest.SHA256)
	}

	// 2. Test when .sha256 has bare hash with leading/trailing whitespace
	bareContent := "  " + expectedHash + " \n"
	if err := os.WriteFile(shaPath, []byte(bareContent), 0644); err != nil {
		t.Fatalf("failed to overwrite sha256 file: %v", err)
	}

	latestBare, err := h.findLatest("debug")
	if err != nil {
		t.Fatalf("findLatest failed: %v", err)
	}

	if latestBare.SHA256 != expectedHash {
		t.Errorf("expected bare sha256 hash %q, got %q", expectedHash, latestBare.SHA256)
	}
}
