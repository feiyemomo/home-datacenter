package camera

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

// DaguCloudRecording represents a cloud recording clip stored on Dagu cloud.
type DaguCloudRecording struct {
	Filename        string `json:"filename"`
	StartUnix       int64  `json:"start_unix"`
	EndUnix         int64  `json:"end_unix"`
	DurationSeconds int    `json:"duration_seconds"`
	EventType       int    `json:"event_type"`
	SizeBytes       int64  `json:"size_bytes"`
	URL             string `json:"url"`
}

var (
	daguRecRegex = regexp.MustCompile(`/video/(\d{10})_(\d{3})_(\d+)`)
	cstZone      = time.FixedZone("CST", 8*3600) // China Standard Time (UTC+8)
)

// Cache for day recordings to avoid frequent re-scraping of past closed days: dateStr -> []DaguCloudRecording
var (
	daguDayCacheMu sync.RWMutex
	daguDayCache   = make(map[string][]DaguCloudRecording)
)

// FetchDayRecordings retrieves the list of recording clips for a specific day (format: YYYYMMDD) from Dagu cloud.
func (m *DaguManager) FetchDayRecordings(ctx context.Context, sn string, dateStr string) ([]DaguCloudRecording, error) {
	if len(sn) < 2 {
		return nil, fmt.Errorf("invalid sn: %s", sn)
	}

	todayStr := time.Now().In(cstZone).Format("20060102")
	isPastDay := dateStr < todayStr

	if isPastDay {
		daguDayCacheMu.RLock()
		if cached, ok := daguDayCache[sn+":"+dateStr]; ok {
			daguDayCacheMu.RUnlock()
			return cached, nil
		}
		daguDayCacheMu.RUnlock()
	}

	prefixChar := string(sn[len(sn)-2])
	url := fmt.Sprintf("http://cloud.daguiot.com:7738/%s/15/%s/%s/video/", prefixChar, dateStr, sn)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create req: %w", err)
	}

	client := m.httpClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get day video list %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // No recordings for this day
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http error %d from %s", resp.StatusCode, url)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	matches := daguRecRegex.FindAllStringSubmatch(string(bodyBytes), -1)
	if len(matches) == 0 {
		return nil, nil
	}

	var recs []DaguCloudRecording
	seen := make(map[string]bool)

	for _, m := range matches {
		if len(m) < 4 {
			continue
		}
		filename := fmt.Sprintf("%s_%s_%s", m[1], m[2], m[3])
		if seen[filename] {
			continue
		}
		seen[filename] = true

		rawUnix, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		// Dagu cloud recording filenames embed timestamp in camera local time (+8h / 28800s ahead of UTC).
		// Normalize to true UTC Unix timestamp so API RFC3339 and clients display accurate local recording time.
		startUnix := rawUnix - 8*3600
		dur, _ := strconv.Atoi(m[2])
		if dur <= 0 {
			dur = 10
		}
		eventType, _ := strconv.Atoi(m[3])

		fileURL := fmt.Sprintf("http://cloud.daguiot.com:7738/%s/15/%s/%s/video/%s", prefixChar, dateStr, sn, filename)
		approxSize := int64(dur) * 25000 // ~200kbps

		recs = append(recs, DaguCloudRecording{
			Filename:        filename,
			StartUnix:       startUnix,
			EndUnix:         startUnix + int64(dur),
			DurationSeconds: dur,
			EventType:       eventType,
			SizeBytes:       approxSize,
			URL:             fileURL,
		})
	}

	if isPastDay && len(recs) > 0 {
		daguDayCacheMu.Lock()
		daguDayCache[sn+":"+dateStr] = recs
		daguDayCacheMu.Unlock()
	}

	return recs, nil
}

// ListRecordings retrieves cloud recordings for a time window [after, before] (unix timestamps).
func (m *DaguManager) ListRecordings(ctx context.Context, sn string, after, before int64) ([]DaguCloudRecording, error) {
	if before <= 0 {
		before = time.Now().Unix()
	}
	if after <= 0 {
		after = time.Now().AddDate(0, 0, -15).Unix()
	}
	if before < after {
		before, after = after, before
	}

	startDate := time.Unix(after, 0).In(cstZone)
	endDate := time.Unix(before, 0).In(cstZone)

	// Clamp to 15 days max
	oldestAllowed := time.Now().In(cstZone).AddDate(0, 0, -15)
	if startDate.Before(oldestAllowed) {
		startDate = oldestAllowed
	}

	var allRecs []DaguCloudRecording
	// Iterate through each day in the range
	for d := startDate; !d.After(endDate); d = d.AddDate(0, 0, 1) {
		dateStr := d.Format("20060102")
		dayRecs, err := m.FetchDayRecordings(ctx, sn, dateStr)
		if err != nil {
			log.Printf("[dagu] failed to fetch recordings for %s (%s): %v", sn, dateStr, err)
			continue
		}
		for _, r := range dayRecs {
			if r.StartUnix >= after && r.StartUnix <= before {
				allRecs = append(allRecs, r)
			}
		}
	}

	// Sort newest first
	sort.Slice(allRecs, func(i, j int) bool {
		return allRecs[i].StartUnix > allRecs[j].StartUnix
	})

	return allRecs, nil
}

// GetRecordingCacheDir returns the directory used to cache downloaded recordings.
func GetRecordingCacheDir(camID uint) string {
	base := "/data/recordings/.transcode-cache/dagu"
	if _, err := os.Stat("/data/recordings"); os.IsNotExist(err) {
		base = filepath.Join(os.TempDir(), "dagu_cache")
	}
	return filepath.Join(base, strconv.FormatUint(uint64(camID), 10))
}

// GetRecordingPathOrDownload returns the local file path of the recording clip,
// downloading and caching it from the Dagu cloud if necessary.
func (m *DaguManager) GetRecordingPathOrDownload(ctx context.Context, sn string, camID uint, startUnix int64) (string, error) {
	cacheDir := GetRecordingCacheDir(camID)
	cacheFile := filepath.Join(cacheDir, fmt.Sprintf("%d.mp4", startUnix))

	if fi, err := os.Stat(cacheFile); err == nil && fi.Size() > 0 {
		return cacheFile, nil
	}

	// Try candidate dates since Dagu server may store clips by UTC or CST date
	datesToTry := []string{
		time.Unix(startUnix, 0).UTC().Format("20060102"),
		time.Unix(startUnix, 0).In(cstZone).Format("20060102"),
		time.Unix(startUnix, 0).AddDate(0, 0, -1).UTC().Format("20060102"),
		time.Unix(startUnix, 0).AddDate(0, 0, 1).UTC().Format("20060102"),
	}

	seen := make(map[string]bool)
	var targetRec *DaguCloudRecording
	for _, d := range datesToTry {
		if seen[d] {
			continue
		}
		seen[d] = true
		recs, err := m.FetchDayRecordings(ctx, sn, d)
		if err != nil {
			continue
		}
		for i := range recs {
			if recs[i].StartUnix == startUnix {
				targetRec = &recs[i]
				break
			}
		}
		if targetRec != nil {
			break
		}
		// Fallback to fuzzy match within 10s if exact timestamp differs slightly
		for i := range recs {
			diff := recs[i].StartUnix - startUnix
			if diff >= -10 && diff <= 10 {
				targetRec = &recs[i]
				break
			}
		}
		if targetRec != nil {
			break
		}
	}

	if targetRec == nil {
		return "", fmt.Errorf("recording with timestamp %d not found in candidate dates", startUnix)
	}

	// Download to a temporary file in the cache directory
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetRec.URL, nil)
	if err != nil {
		return "", fmt.Errorf("create download req: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download recording from %s: %w", targetRec.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	tmpFile := cacheFile + ".tmp"
	f, err := os.Create(tmpFile)
	if err != nil {
		return "", fmt.Errorf("create tmp file: %w", err)
	}

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmpFile)
		return "", fmt.Errorf("write tmp file: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpFile, cacheFile); err != nil {
		return "", fmt.Errorf("rename to cache file: %w", err)
	}

	log.Printf("[dagu] successfully cached recording %d (%s) -> %s", startUnix, targetRec.Filename, cacheFile)
	return cacheFile, nil
}
