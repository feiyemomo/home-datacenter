package handler

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

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
}

// NewReleaseHandler creates a handler that serves APK files from
// the given directory. The directory must exist (or be created on
// first deploy); missing directory is not a fatal error — the
// endpoints will return 404 with a clear message.
func NewReleaseHandler(releasesDir string) *ReleaseHandler {
	return &ReleaseHandler{releasesDir: releasesDir}
}

// apkFile is one entry in the releases directory after parsing.
type apkFile struct {
	Path        string // absolute path on disk
	VersionName string // e.g. "1.6.10"
	VersionCode int    // e.g. 53 (parsed from versionName as 1*10000 + 6*100 + 10)
	SizeBytes   int64
	FileName    string // e.g. "app-debug-v1.6.10.apk"
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
//	    "download_url": "/api/v1/release/latest/apk",
//	    "file_name": "app-debug-v1.6.10.apk",
//	    "size_bytes": 93543219,
//	    "release_notes": ""
//	  }
//	}
//
// The Android client compares version_code against its own
// PackageInfo.versionCode to decide whether to prompt the user.
// download_url is a RELATIVE path — the client prepends its
// resolved base URL (LAN or Cloudflare Tunnel) so the same endpoint
// works in both environments.
func (h *ReleaseHandler) Latest(c *gin.Context) {
	apk, err := h.findLatest()
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "no releases available: "+err.Error())
		return
	}

	utils.Success(c, gin.H{
		"version_name":  apk.VersionName,
		"version_code":  apk.VersionCode,
		"download_url":  "/api/v1/release/latest/apk",
		"file_name":     apk.FileName,
		"size_bytes":    apk.SizeBytes,
		"release_notes": "",
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
	apk, err := h.findLatest()
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "no releases available: "+err.Error())
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
// from filenames matching the convention "app-debug-vX.Y.Z.apk",
// and returns the one with the highest version_code. Returns an
// error if the directory doesn't exist, is empty, or contains no
// matching files.
func (h *ReleaseHandler) findLatest() (*apkFile, error) {
	if h.releasesDir == "" {
		return nil, os.ErrNotExist
	}

	entries, err := os.ReadDir(h.releasesDir)
	if err != nil {
		return nil, err
	}

	var apks []apkFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "app-debug-v") || !strings.HasSuffix(name, ".apk") {
			continue
		}
		// Strip "app-debug-v" prefix and ".apk" suffix → "1.6.10"
		verStr := strings.TrimSuffix(strings.TrimPrefix(name, "app-debug-v"), ".apk")
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

	return &apks[0], nil
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
