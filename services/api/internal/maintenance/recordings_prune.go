package maintenance

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	_ "github.com/glebarez/go-sqlite"
)

var (
	dateDirRegex = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	hourDirRegex = regexp.MustCompile(`^\d{2}$`)
)

// PruneResult summarizes the outcome of an incremental FIFO prune operation.
type PruneResult struct {
	ReclaimedBytes uint64   `json:"reclaimed_bytes"`
	PrunedHours    int      `json:"pruned_hours"`
	PrunedDirs     []string `json:"pruned_dirs"`
}

// PruneOldestRecordings iteratively removes the oldest hour directories under root
// until targetReclaim bytes have been freed or maxHours have been removed.
// If frigateDBPath is provided, matching rows in frigate.db's recordings table are also removed.
func PruneOldestRecordings(root string, frigateDBPath string, targetReclaim uint64, maxHours int) (PruneResult, error) {
	result := PruneResult{}
	if root == "" || maxHours <= 0 {
		return result, nil
	}

	dateEntries, err := os.ReadDir(root)
	if err != nil {
		return result, fmt.Errorf("read recordings root %q: %w", root, err)
	}

	var dateDirs []string
	for _, de := range dateEntries {
		if de.IsDir() && dateDirRegex.MatchString(de.Name()) {
			dateDirs = append(dateDirs, de.Name())
		}
	}
	sort.Strings(dateDirs)

	if len(dateDirs) == 0 {
		return result, nil
	}

	// Prepare optional SQLite connection to Frigate database
	var fdb *sql.DB
	if frigateDBPath != "" {
		if _, err := os.Stat(frigateDBPath); err == nil {
			if dbConn, err := sql.Open("sqlite", frigateDBPath); err == nil {
				fdb = dbConn
				defer fdb.Close()
			}
		}
	}

	for _, dateDir := range dateDirs {
		datePath := filepath.Join(root, dateDir)
		hourEntries, err := os.ReadDir(datePath)
		if err != nil {
			continue
		}

		var hourDirs []string
		for _, he := range hourEntries {
			if he.IsDir() && hourDirRegex.MatchString(he.Name()) {
				hourDirs = append(hourDirs, he.Name())
			}
		}
		sort.Strings(hourDirs)

		for _, hourDir := range hourDirs {
			hourPath := filepath.Join(datePath, hourDir)

			// Calculate physical disk bytes occupied by this hour directory
			sz, _, _ := dirSize(hourPath)

			// Remove the hour directory tree
			if err := os.RemoveAll(hourPath); err != nil {
				log.Printf("maintenance: failed to remove oldest hour %q: %v", hourPath, err)
				continue
			}

			result.ReclaimedBytes += sz
			result.PrunedHours++
			result.PrunedDirs = append(result.PrunedDirs, filepath.ToSlash(filepath.Join(dateDir, hourDir)))

			// Clean up matching records in Frigate DB so UI and timeline stay in sync
			if fdb != nil {
				patternSlash := "%/" + dateDir + "/" + hourDir + "/%"
				patternBackslash := "%\\" + dateDir + "\\" + hourDir + "\\%"
				if _, err := fdb.Exec("DELETE FROM recordings WHERE path LIKE ? OR path LIKE ?", patternSlash, patternBackslash); err != nil {
					log.Printf("maintenance: frigate db prune for %s/%s failed: %v", dateDir, hourDir, err)
				}
			}

			if (targetReclaim > 0 && result.ReclaimedBytes >= targetReclaim) || result.PrunedHours >= maxHours {
				break
			}
		}

		// If all hour subdirectories in datePath were removed, delete the empty date directory
		if remaining, err := os.ReadDir(datePath); err == nil && len(remaining) == 0 {
			_ = os.Remove(datePath)
		}

		if (targetReclaim > 0 && result.ReclaimedBytes >= targetReclaim) || result.PrunedHours >= maxHours {
			break
		}
	}

	return result, nil
}
