package camera

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

)

// CloudArchiveClient communicates with Alist WebDAV to browse and fetch
// surveillance video recordings archived to Quark Cloud Drive (v1.8.35).
type CloudArchiveClient struct {
	BaseURL    string // e.g. "http://home-alist:5244/dav"
	Username   string
	Password   string
	RemotePath string // e.g. "quark/Surveillance/Recordings"
	CacheDir   string // e.g. "/data/recordings/.cloud-cache"
	client     *http.Client
}

// NewCloudArchiveClient creates a client using environment variables or defaults.
func NewCloudArchiveClient() *CloudArchiveClient {
	baseURL := strings.TrimRight(os.Getenv("ALIST_WEBDAV_URL"), "/")
	if baseURL == "" {
		baseURL = "http://home-alist:5244/dav"
	}
	user := os.Getenv("ALIST_WEBDAV_USER")
	if user == "" {
		user = "admin"
	}
	pass := os.Getenv("ALIST_WEBDAV_PASS")
	if pass == "" {
		pass = "wm10050817"
	}
	remPath := strings.Trim(os.Getenv("ALIST_RECORDINGS_PATH"), "/")
	if remPath == "" {
		remPath = "quark/Surveillance/Recordings"
	}
	cacheDir := os.Getenv("CLOUD_CACHE_DIR")
	if cacheDir == "" {
		cacheDir = "/data/recordings/.cloud-cache"
	}

	return &CloudArchiveClient{
		BaseURL:    baseURL,
		Username:   user,
		Password:   pass,
		RemotePath: remPath,
		CacheDir:   cacheDir,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// WebdavItem represents a single file or directory returned by PROPFIND.
type WebdavItem struct {
	Name  string
	Href  string
	IsDir bool
	Size  int64
}

// WebDAV XML multi-status schema for parsing PROPFIND responses.
type propfindResponse struct {
	XMLName   xml.Name `xml:"multistatus"`
	Responses []struct {
		Href     string `xml:"href"`
		Propstat struct {
			Prop struct {
				DisplayName  string `xml:"displayname"`
				ResourceType struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"resourcetype"`
				ContentLength int64 `xml:"getcontentlength"`
			} `xml:"prop"`
			Status string `xml:"status"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// propfind executes a Depth: 1 PROPFIND on the given subpath under BaseURL.
func (c *CloudArchiveClient) propfind(ctx context.Context, subPath string) ([]WebdavItem, error) {
	cleanPath := strings.Trim(subPath, "/")
	reqURL := fmt.Sprintf("%s/%s", c.BaseURL, cleanPath)

	req, err := http.NewRequestWithContext(ctx, "PROPFIND", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.Username, c.Password)
	req.Header.Set("Depth", "1")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, os.ErrNotExist
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("webdav status %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var parsed propfindResponse
	if err := xml.Unmarshal(bodyBytes, &parsed); err != nil {
		return nil, fmt.Errorf("parse propfind xml: %w", err)
	}

	var items []WebdavItem
	for _, r := range parsed.Responses {
		decodedHref, err := url.PathUnescape(r.Href)
		if err != nil {
			decodedHref = r.Href
		}
		trimmedHref := strings.TrimRight(decodedHref, "/")
		parts := strings.Split(trimmedHref, "/")
		name := parts[len(parts)-1]
		if r.Propstat.Prop.DisplayName != "" {
			name = r.Propstat.Prop.DisplayName
		}

		// Skip self (the directory itself being probed)
		if strings.HasSuffix(trimmedHref, cleanPath) {
			continue
		}

		isDir := r.Propstat.Prop.ResourceType.Collection != nil
		items = append(items, WebdavItem{
			Name:  name,
			Href:  r.Href,
			IsDir: isDir,
			Size:  r.Propstat.Prop.ContentLength,
		})
	}
	return items, nil
}

// ListRecordingMinutesFromCloud scans Quark Cloud Drive via Alist WebDAV
// for recording minutes within [afterUnix, beforeUnix], skipping minutes
// that already exist locally.
func (c *CloudArchiveClient) ListRecordingMinutesFromCloud(ctx context.Context, slug string, afterUnix, beforeUnix int64, localBuckets []RecordingMinute) ([]RecordingMinute, error) {
	if c == nil {
		return nil, nil
	}

	localSet := make(map[int64]bool, len(localBuckets))
	for _, b := range localBuckets {
		localSet[b.StartUnix] = true
	}

	// 1. List date directories in cloud archive
	dateEntries, err := c.propfind(ctx, c.RemotePath)
	if err != nil {
		return nil, err
	}

	type bucket struct {
		startUnix int64
		endUnix   int64
		count     int
	}
	buckets := make(map[int64]*bucket)

	for _, de := range dateEntries {
		if !de.IsDir {
			continue
		}
		dateStr := de.Name
		dateStart, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}
		dateStartUnix := dateStart.Unix()
		dateEndUnix := dateStartUnix + 24*3600
		if afterUnix > 0 && dateEndUnix < afterUnix {
			continue
		}
		if beforeUnix > 0 && dateStartUnix > beforeUnix {
			continue
		}

		// 2. List hour directories for this date
		hourEntries, err := c.propfind(ctx, fmt.Sprintf("%s/%s", c.RemotePath, dateStr))
		if err != nil {
			continue
		}

		for _, he := range hourEntries {
			if !he.IsDir {
				continue
			}
			hour, err := strconv.Atoi(he.Name)
			if err != nil || hour < 0 || hour > 23 {
				continue
			}
			hourStart := time.Date(dateStart.Year(), dateStart.Month(),
				dateStart.Day(), hour, 0, 0, 0, time.UTC).Unix()
			if afterUnix > 0 && hourStart+3600 < afterUnix {
				continue
			}
			if beforeUnix > 0 && hourStart > beforeUnix {
				continue
			}

			// 3. List segments for this camera slug
			slugPath := fmt.Sprintf("%s/%s/%s/%s", c.RemotePath, dateStr, he.Name, slug)
			segEntries, err := c.propfind(ctx, slugPath)
			if err != nil {
				continue
			}

			for _, se := range segEntries {
				if se.IsDir || !strings.HasSuffix(se.Name, ".mp4") {
					continue
				}
				stem := strings.TrimSuffix(se.Name, ".mp4")
				parts := strings.SplitN(stem, ".", 2)
				if len(parts) != 2 {
					continue
				}
				min, err1 := strconv.Atoi(parts[0])
				sec, err2 := strconv.Atoi(parts[1])
				if err1 != nil || err2 != nil || min < 0 || min > 59 || sec < 0 || sec > 59 {
					continue
				}

				segStartUnix := hourStart + int64(min)*60 + int64(sec)
				if afterUnix > 0 && segStartUnix < afterUnix {
					continue
				}
				if beforeUnix > 0 && segStartUnix > beforeUnix {
					continue
				}

				minuteStart := (segStartUnix / 60) * 60
				// If already present in local disk buckets, local takes precedence
				if localSet[minuteStart] {
					continue
				}

				b, ok := buckets[minuteStart]
				if !ok {
					b = &bucket{startUnix: minuteStart, endUnix: segStartUnix + 10, count: 1}
					buckets[minuteStart] = b
				} else {
					b.count++
					end := segStartUnix + 10
					if end > b.endUnix {
						b.endUnix = end
					}
				}
			}
		}
	}

	out := make([]RecordingMinute, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, RecordingMinute{
			StartUnix:    b.startUnix,
			EndUnix:      b.endUnix,
			SegmentCount: b.count,
			Storage:      "cloud",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartUnix > out[j].StartUnix })
	return out, nil
}

// FetchMinuteSegments downloads the segments of a requested minute from Quark WebDAV
// into the local .cloud-cache directory and returns the local file paths.
func (c *CloudArchiveClient) FetchMinuteSegments(ctx context.Context, slug string, minuteStart int64) ([]string, error) {
	if c == nil {
		return nil, os.ErrNotExist
	}

	t := time.Unix(minuteStart, 0).UTC()
	dateStr := t.Format("2006-01-02")
	hourStr := t.Format("15")
	minStr := t.Format("04")

	cacheHourDir := filepath.Join(c.CacheDir, slug, dateStr, hourStr)
	if err := os.MkdirAll(cacheHourDir, 0755); err != nil {
		return nil, fmt.Errorf("create cloud cache dir: %w", err)
	}

	// Check if already in local cache
	existingEntries, _ := os.ReadDir(cacheHourDir)
	var cachedPaths []string
	for _, e := range existingEntries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), minStr+".") && strings.HasSuffix(e.Name(), ".mp4") {
			cachedPaths = append(cachedPaths, filepath.Join(cacheHourDir, e.Name()))
		}
	}
	if len(cachedPaths) > 0 {
		sort.Strings(cachedPaths)
		return cachedPaths, nil
	}

	// Query WebDAV for segments in this hour
	remoteSlugPath := fmt.Sprintf("%s/%s/%s/%s", c.RemotePath, dateStr, hourStr, slug)
	items, err := c.propfind(ctx, remoteSlugPath)
	if err != nil {
		return nil, err
	}

	var toDownload []WebdavItem
	for _, item := range items {
		if !item.IsDir && strings.HasPrefix(item.Name, minStr+".") && strings.HasSuffix(item.Name, ".mp4") {
			toDownload = append(toDownload, item)
		}
	}

	if len(toDownload) == 0 {
		return nil, os.ErrNotExist
	}

	var downloadedPaths []string
	for _, item := range toDownload {
		dstPath := filepath.Join(cacheHourDir, item.Name)
		// Download file
		downloadURL := fmt.Sprintf("%s/%s/%s", c.BaseURL, remoteSlugPath, item.Name)
		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
		if err != nil {
			continue
		}
		req.SetBasicAuth(c.Username, c.Password)

		resp, err := c.client.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}

		tmpFile := dstPath + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
		f, err := os.Create(tmpFile)
		if err != nil {
			resp.Body.Close()
			continue
		}
		_, copyErr := io.Copy(f, resp.Body)
		f.Close()
		resp.Body.Close()

		if copyErr != nil {
			os.Remove(tmpFile)
			continue
		}
		if err := os.Rename(tmpFile, dstPath); err != nil {
			os.Remove(tmpFile)
			continue
		}
		downloadedPaths = append(downloadedPaths, dstPath)
	}

	if len(downloadedPaths) == 0 {
		return nil, fmt.Errorf("failed to download cloud segments")
	}

	sort.Strings(downloadedPaths)
	log.Printf("[cloud-archive] successfully fetched %d segments from Quark for %s minute %s", len(downloadedPaths), slug, t.Format("2006-01-02 15:04"))
	return downloadedPaths, nil
}
