package handler

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// ReleaseHandler serves the latest Android APK build metadata and
// the APK file itself. The Android app's in-app updater calls
// GET /api/v1/release/latest to discover new versions, then streams
// the APK via GET /api/v1/release/latest/apk.
//
// Both endpoints are JWT-protected (registered under the /api/v1
// group with JWTAuth middleware in cmd/main.go). Anonymous clients
// cannot enumerate or download APKs.
//
// Directory layout (configured via config.releases_dir, default
// /data/releases which maps to ./data/releases on the host in
// compose.yaml):
//
//	/data/releases/
//	  app-debug-v1.6.10.apk
//	  app-debug-v1.6.9.apk
//	  app-debug-v1.6.8.apk
//	  ...
//
// File naming convention MUST match push-apk.ps1:
//
//	app-debug-v{MAJOR}.{MINOR}.{PATCH}.apk
//
// The handler scans the directory, parses the version from each
// filename, and returns the highest one. This means publishing a
// new release is just `scp app-debug-v1.6.11.apk nas:/.../data/releases/`
// — no database row, no config file edit, no service restart.
type ReleaseHandler struct {
	releasesDir string
	db          *gorm.DB
}

// NewReleaseHandler creates a handler that serves APK files from
// the given directory. The directory must exist (or be created on
// first deploy); missing directory is not a fatal error — the
// endpoints will return 404 with a clear message.
func NewReleaseHandler(releasesDir string, db *gorm.DB) *ReleaseHandler {
	return &ReleaseHandler{releasesDir: releasesDir, db: db}
}

// isUserAdmin checks if the current authenticated user has the admin role.
func (h *ReleaseHandler) isUserAdmin(c *gin.Context) bool {
	if h.db == nil {
		return false
	}
	raw, ok := c.Get("user_id")
	if !ok {
		return false
	}
	uid, ok := raw.(uint)
	if !ok {
		return false
	}
	var u model.User
	if err := h.db.Select("id, is_admin").First(&u, uid).Error; err != nil {
		return false
	}
	return u.IsAdmin
}

// apkFile is one entry in the releases directory after parsing.
type apkFile struct {
	Path         string // absolute path on disk
	Flavor       string // "debug" or "release"
	VersionName  string // e.g. "1.6.10"
	VersionCode  int    // e.g. 53 (parsed from versionName as 1*10000 + 6*100 + 10)
	SizeBytes    int64
	FileName     string // e.g. "app-debug-v1.6.10.apk"
	ReleaseNotes string // contents of release-notes-v1.6.10.txt (empty if absent)
}

// Latest returns metadata about the highest-version APK in the
// releases directory.
//
//	Route: GET /api/v1/release/latest
//
// Response shape (wrapped in utils.Success -> ApiResponse):
//
//	{
//	  "code": 0,
//	  "message": "success",
//	  "data": {
//	    "version_name": "1.6.10",
//	    "version_code": 53,
//	    "download_url": "/api/v1/release/latest/apk?flavor=release",
//	    "file_name": "app-release-v1.6.10.apk",
//	    "flavor": "release",
//	    "size_bytes": 93543219,
//	    "release_notes": ""
//	  }
//	}
//
// 普通用户只能接收 release 版本的推送；admin 用户可以接收 debug 与 release 两种版本。
func (h *ReleaseHandler) Latest(c *gin.Context) {
	flavor := strings.TrimSpace(c.Query("flavor"))
	isAdmin := h.isUserAdmin(c)

	// 普通用户只接受 release 版本；admin 用户接受两种版本
	if !isAdmin {
		flavor = "release"
	}

	apk, err := h.findLatest(flavor)
	if err != nil {
		log.Printf("[handler] no %s releases available: %v", flavor, err)
		utils.Fail(c, http.StatusNotFound, "no releases available")
		return
	}

	utils.Success(c, gin.H{
		"version_name":  apk.VersionName,
		"version_code":  apk.VersionCode,
		"download_url":  "/api/v1/release/latest/apk?flavor=" + apk.Flavor,
		"file_name":     apk.FileName,
		"flavor":        apk.Flavor,
		"size_bytes":    apk.SizeBytes,
		"release_notes": apk.ReleaseNotes,
	})
}

// Download streams the latest APK file to the client. Uses
// c.File() which sets Content-Type, Content-Length, and supports
// HTTP Range requests for resumable downloads — important on
// flaky cellular where a 90MB APK download may get interrupted.
//
//	Route: GET /api/v1/release/latest/apk
//
// Content-Type is application/vnd.android.package-archive (the
// official APK MIME type). Some older browsers fall back to
// application/octet-stream which also works — Android's
// PackageInstaller accepts both.
func (h *ReleaseHandler) Download(c *gin.Context) {
	flavor := strings.TrimSpace(c.Query("flavor"))
	isAdmin := h.isUserAdmin(c)

	// 普通用户只接受 release 版本
	if !isAdmin {
		flavor = "release"
	}

	apk, err := h.findLatest(flavor)
	if err != nil {
		log.Printf("[handler] no %s releases available: %v", flavor, err)
		utils.Fail(c, http.StatusNotFound, "no releases available")
		return
	}

	// Content-Disposition: attachment forces a download rather than
	// attempting inline display (which would just show binary garbage
	// in the browser). The filename lets the browser save it with a
	// meaningful name.
	c.Header("Content-Disposition", "attachment; filename=\""+apk.FileName+"\"")
	c.Header("Content-Type", "application/vnd.android.package-archive")
	c.File(apk.Path)
}

// findLatest scans the releases directory, parses version numbers
// from filenames matching the convention "app-{flavor}-vX.Y.Z.apk",
// and returns the one with the highest version_code matching targetFlavor.
func (h *ReleaseHandler) findLatest(targetFlavor string) (*apkFile, error) {
	apks, err := h.listAll(targetFlavor)
	if err != nil {
		return nil, err
	}
	latest := &apks[0]

	// Read optional release notes from a sibling text file named
	// release-notes-v{version}.txt. Missing file = empty string.
	notesPath := filepath.Join(h.releasesDir, "release-notes-v"+latest.VersionName+".txt")
	notes, _ := os.ReadFile(notesPath)
	latest.ReleaseNotes = string(notes)
	return latest, nil
}

// listAll scans the releases directory and returns every APK matching
// the "app-{flavor}-vX.Y.Z.apk" convention (flavor in {debug, release}),
// filtered optionally by targetFlavor, sorted by version_code descending (newest first).
func (h *ReleaseHandler) listAll(targetFlavor string) ([]apkFile, error) {
	if h.releasesDir == "" {
		return nil, os.ErrNotExist
	}

	entries, err := os.ReadDir(h.releasesDir)
	if err != nil {
		return nil, err
	}

	const (
		debugPrefix   = "app-debug-v"
		releasePrefix = "app-release-v"
	)

	var apks []apkFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".apk") {
			continue
		}
		// Strip the recognized prefix and ".apk" suffix → "1.6.10"
		var verStr string
		var flavor string
		switch {
		case strings.HasPrefix(name, debugPrefix):
			flavor = "debug"
			verStr = strings.TrimSuffix(strings.TrimPrefix(name, debugPrefix), ".apk")
		case strings.HasPrefix(name, releasePrefix):
			flavor = "release"
			verStr = strings.TrimSuffix(strings.TrimPrefix(name, releasePrefix), ".apk")
		default:
			continue
		}
		if targetFlavor != "" && targetFlavor != "all" && flavor != targetFlavor {
			continue
		}
		code, ok := parseVersionCode(verStr)
		if !ok {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		apks = append(apks, apkFile{
			Path:        filepath.Join(h.releasesDir, name),
			Flavor:      flavor,
			VersionName: verStr,
			VersionCode: code,
			SizeBytes:   info.Size(),
			FileName:    name,
		})
	}

	if len(apks) == 0 {
		return nil, os.ErrNotExist
	}

	// Sort by version_code descending; first element is the latest.
	sort.Slice(apks, func(i, j int) bool {
		return apks[i].VersionCode > apks[j].VersionCode
	})

	return apks, nil
}

// CleanupOldReleases deletes all but the newest `keep` APK files in
// the releases directory, along with their sibling release-notes
// files. Returns the number of APKs removed. Safe to call repeatedly:
// it is a no-op when there are already ≤ keep releases.
func (h *ReleaseHandler) CleanupOldReleases(keep int) (int, error) {
	if h.releasesDir == "" || keep <= 0 {
		return 0, nil
	}
	apks, err := h.listAll("")
	if err != nil {
		// os.ErrNotExist (empty dir) is not an error worth logging.
		if err == os.ErrNotExist {
			return 0, nil
		}
		return 0, err
	}
	if len(apks) <= keep {
		return 0, nil
	}
	removed := 0
	for _, apk := range apks[keep:] {
		if err := os.Remove(apk.Path); err != nil {
			log.Printf("[handler] release cleanup: remove %s failed: %v", apk.Path, err)
			continue
		}
		// Also remove the sibling release-notes file if present.
		notesPath := filepath.Join(h.releasesDir, "release-notes-v"+apk.VersionName+".txt")
		_ = os.Remove(notesPath)
		removed++
	}
	if removed > 0 {
		log.Printf("[handler] release cleanup: removed %d old release(s), keeping newest %d", removed, keep)
	}
	return removed, nil
}

// parseVersionCode converts "1.6.10" to 10000 + 6*100 + 10 = 10610.
// Returns false if the string isn't in MAJOR.MINOR.PATCH form with
// numeric components. The exact formula doesn't matter — it just
// needs to be monotonic so higher versions sort higher.
func parseVersionCode(s string) (int, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	patch, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return major*10000 + minor*100 + patch, true
}
