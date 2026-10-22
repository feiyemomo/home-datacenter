package maintenance

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/glebarez/go-sqlite"
)

func TestPruneOldestRecordings(t *testing.T) {
	tempDir := t.TempDir()
	recordingsRoot := filepath.Join(tempDir, "recordings")

	// Set up fake recording hours:
	// 2026-10-18/13/cam1/01.00.mp4
	// 2026-10-18/14/cam1/01.00.mp4
	// 2026-10-19/00/cam1/01.00.mp4
	createSegment := func(date, hour string, sizeBytes int) string {
		dir := filepath.Join(recordingsRoot, date, hour, "cam1")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir failed: %v", err)
		}
		filePath := filepath.Join(dir, "01.00.mp4")
		data := make([]byte, sizeBytes)
		if err := os.WriteFile(filePath, data, 0644); err != nil {
			t.Fatalf("write file failed: %v", err)
		}
		return filePath
	}

	p1 := createSegment("2026-10-18", "13", 1000)
	p2 := createSegment("2026-10-18", "14", 1000)
	p3 := createSegment("2026-10-19", "00", 1000)

	// Create fake frigate.db
	dbPath := filepath.Join(tempDir, "frigate.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE recordings (id VARCHAR(30) PRIMARY KEY, path VARCHAR(255))`)
	if err != nil {
		t.Fatalf("create table failed: %v", err)
	}
	_, _ = db.Exec("INSERT INTO recordings VALUES ('r1', ?)", p1)
	_, _ = db.Exec("INSERT INTO recordings VALUES ('r2', ?)", p2)
	_, _ = db.Exec("INSERT INTO recordings VALUES ('r3', ?)", p3)

	// Test 1: Prune targetReclaim = 1500 bytes (should prune hours 13 and 14)
	res, err := PruneOldestRecordings(recordingsRoot, dbPath, 1500, 10)
	if err != nil {
		t.Fatalf("prune failed: %v", err)
	}

	if res.PrunedHours != 2 {
		t.Errorf("expected 2 pruned hours, got %d", res.PrunedHours)
	}
	if res.ReclaimedBytes < 2000 {
		t.Errorf("expected at least 2000 reclaimed bytes, got %d", res.ReclaimedBytes)
	}

	// 2026-10-18/13 and 14 should no longer exist
	if _, err := os.Stat(filepath.Join(recordingsRoot, "2026-10-18", "13")); !os.IsNotExist(err) {
		t.Errorf("expected 2026-10-18/13 to be deleted")
	}
	if _, err := os.Stat(filepath.Join(recordingsRoot, "2026-10-18", "14")); !os.IsNotExist(err) {
		t.Errorf("expected 2026-10-18/14 to be deleted")
	}
	// The 2026-10-18 directory itself should be cleaned up because all its hours were deleted
	if _, err := os.Stat(filepath.Join(recordingsRoot, "2026-10-18")); !os.IsNotExist(err) {
		t.Errorf("expected empty date dir 2026-10-18 to be deleted")
	}

	// 2026-10-19/00 must still exist!
	if _, err := os.Stat(filepath.Join(recordingsRoot, "2026-10-19", "00")); err != nil {
		t.Errorf("expected 2026-10-19/00 to still exist")
	}

	// Verify DB cleanup
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM recordings WHERE path LIKE '%2026-10-18%'").Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 recordings for 2026-10-18 in DB, got %d", count)
	}
	_ = db.QueryRow("SELECT COUNT(*) FROM recordings WHERE path LIKE '%2026-10-19%'").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 recording for 2026-10-19 in DB, got %d", count)
	}
}
