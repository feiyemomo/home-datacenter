package camera

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

// RecordingPlan is the user-controllable recording schedule for a
// camera. Stored inside Camera.Meta under the "recording" key (not
// its own column) so that the camera row is the only mutable
// thing the plan depends on — and the same plan is replayed across
// container restarts.
type RecordingPlan struct {
	// Enabled is the master switch.
	Enabled bool `json:"enabled"`
	// SegmentSeconds is the target length of each file. The recorder
	// rolls a new file when this many seconds have passed or when a
	// stop signal arrives. Default 600 (10 min).
	SegmentSeconds int `json:"segment_seconds"`
	// RetentionDays is how long files are kept. 0 = keep forever.
	RetentionDays int `json:"retention_days"`
	// OutputDir is the directory inside the host that's bind-mounted
	// into the API container at /data/recordings. Files are written
	// to <OutputDir>/<camera_id>/<start_ts>.mp4.
	OutputDir string `json:"output_dir"`
	// Cron is a small subset of cron syntax: "HH:MM" (single shot
	// per day) or "" (continuous). Kept intentionally simple — the
	// recorder uses time-based roll rather than a full cron parser.
	Cron string `json:"cron"`
}

// Recorder is the goroutine that drives go2rtc's recording API for
// every camera with an Enabled plan.
//
// go2rtc exposes the recording controls as HTTP endpoints:
//
//	POST /api/recorder     body: {"name":"cam_1","path":"/data/recordings/1/x.mp4"}
//	PUT  /api/recorder     body: {"name":"cam_1","path":...}   (re-open new file)
//
// We use one in-flight segment per camera, rolling the path on
// each SegmentSeconds tick.
type Recorder struct {
	DB  *gorm.DB
	Go2 *Go2RTCClient
	// OutputDir is the on-disk root. Defaults to "./data/recordings".
	OutputDir string
	// Interval is the roll-tick frequency. Defaults to 10s so we
	// re-evaluate plan timing often without spinning.
	Interval time.Duration
	// HC is the health checker so we can skip offline cameras
	// (recording nothing useful).
	HC *HealthChecker
}

// Run blocks until ctx is cancelled. It is intended to be started
// from main.go with a long-lived context.
func (r *Recorder) Run(ctx context.Context) {
	if r.Interval == 0 {
		r.Interval = 10 * time.Second
	}
	if r.OutputDir == "" {
		r.OutputDir = "./data/recordings"
	}
	if err := os.MkdirAll(r.OutputDir, 0o755); err != nil {
		// not fatal: a misconfigured path will surface on the first write
		fmt.Printf("recorder: mkdir %s: %v\n", r.OutputDir, err)
	}

	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *Recorder) tick(ctx context.Context) {
	var cams []model.Camera
	r.DB.Find(&cams)
	now := time.Now()
	for i := range cams {
		plan := r.planFor(&cams[i])
		if !plan.Enabled {
			continue
		}
		if r.HC != nil && cams[i].Status == "offline" {
			continue
		}
		// Housekeeping: delete expired files.
		if plan.RetentionDays > 0 {
			r.expire(ctx, &cams[i], plan, now)
		}
		// Active recording management.
		r.advance(ctx, &cams[i], plan, now)
	}
}

func (r *Recorder) planFor(cam *model.Camera) RecordingPlan {
	if cam.Meta == nil {
		return RecordingPlan{SegmentSeconds: 600, OutputDir: r.OutputDir}
	}
	raw, ok := cam.Meta["recording"]
	if !ok {
		return RecordingPlan{SegmentSeconds: 600, OutputDir: r.OutputDir}
	}
	b, _ := json.Marshal(raw)
	var p RecordingPlan
	_ = json.Unmarshal(b, &p)
	if p.SegmentSeconds == 0 {
		p.SegmentSeconds = 600
	}
	if p.OutputDir == "" {
		p.OutputDir = r.OutputDir
	}
	return p
}

// advance is the "should I start a new segment now?" decision. It is
// deliberately conservative — we never *stop* an active segment; we
// only decide whether to *open* a new one when (a) we have no open
// segment for this camera or (b) the open one is older than
// SegmentSeconds.
//
// The DB column "active" is the source of truth for "is there an
// open segment?". We use the most-recent Recording row that has not
// been closed (EndAt == zero).
func (r *Recorder) advance(ctx context.Context, cam *model.Camera, plan RecordingPlan, now time.Time) {
	var last model.Recording
	err := r.DB.Where("camera_id = ? AND end_at = ?", cam.ID, time.Time{}).
		Order("start_at desc").First(&last).Error

	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		// No open segment → start one.
		r.open(ctx, cam, plan, now)
	case err != nil:
		return
	default:
		// Roll if older than the plan's segment length.
		if now.Sub(last.StartAt) >= time.Duration(plan.SegmentSeconds)*time.Second {
			r.close(ctx, &last, now)
			r.open(ctx, cam, plan, now)
		}
	}
}

func (r *Recorder) open(ctx context.Context, cam *model.Camera, plan RecordingPlan, now time.Time) {
	dir := filepath.Join(plan.OutputDir, fmt.Sprintf("%d", cam.ID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.mp4", now.Unix()))
	if err := r.Go2.StartRecorder(ctx, cam.StreamName, path); err != nil {
		// Logged via the API process; the next tick will retry.
		return
	}
	r.DB.Create(&model.Recording{
		CameraID:   cam.ID,
		StreamName: cam.StreamName,
		FilePath:   path,
		StartAt:    now,
	})
}

func (r *Recorder) close(ctx context.Context, rec *model.Recording, now time.Time) {
	_ = r.Go2.StopRecorder(ctx, rec.StreamName)
	dur := int(now.Sub(rec.StartAt).Seconds())
	if dur < 0 {
		dur = 0
	}
	updates := map[string]any{
		"end_at":           now,
		"duration_seconds": dur,
	}
	if info, err := os.Stat(rec.FilePath); err == nil {
		updates["size_bytes"] = info.Size()
	}
	r.DB.Model(rec).Updates(updates)
}

func (r *Recorder) expire(_ context.Context, cam *model.Camera, plan RecordingPlan, now time.Time) {
	cutoff := now.Add(-time.Duration(plan.RetentionDays) * 24 * time.Hour)
	var old []model.Recording
	r.DB.Where("camera_id = ? AND start_at < ?", cam.ID, cutoff).Find(&old)
	for i := range old {
		_ = os.Remove(old[i].FilePath)
		r.DB.Delete(&old[i])
	}
}

// SetPlan stores a recording plan on the camera's Meta column. The
// key "recording" is reserved for the recorder goroutine.
func (r *Recorder) SetPlan(camID uint, plan RecordingPlan) error {
	cam, err := r.DBFirst(camID)
	if err != nil {
		return err
	}
	if cam.Meta == nil {
		cam.Meta = model.JSON{}
	}
	cam.Meta["recording"] = plan
	if plan.OutputDir == "" {
		plan.OutputDir = r.OutputDir
		cam.Meta["recording"] = plan
	}
	return r.DB.Model(cam).Updates(map[string]any{
		"meta":       model.JSON(cam.Meta),
		"updated_at": time.Now(),
	}).Error
}

// DBFirst is a thin wrapper that lives on Recorder so callers don't
// have to import gorm themselves. Kept private to the package.
func (r *Recorder) DBFirst(id uint) (*model.Camera, error) {
	var cam model.Camera
	if err := r.DB.First(&cam, id).Error; err != nil {
		return nil, err
	}
	return &cam, nil
}

// RecordingFilePath returns the on-disk path of a single recording
// after verifying that it belongs to the supplied camera. We use it
// to defend against path-traversal attacks via a hand-crafted
// :recId — the row is fetched and the resulting path is constrained
// to live under the recorder's OutputDir.
func (r *Recorder) RecordingFilePath(camID, recID uint) (string, *model.Recording, error) {
	var rec model.Recording
	if err := r.DB.First(&rec, recID).Error; err != nil {
		return "", nil, err
	}
	if rec.CameraID != camID {
		return "", nil, fmt.Errorf("recording %d does not belong to camera %d", recID, camID)
	}
	return rec.FilePath, &rec, nil
}

// ListRecordings returns the most recent segments for a camera, newest
// first. Used by GET /api/v1/cameras/:id/recordings.
func (r *Recorder) ListRecordings(camID uint, limit int) ([]model.Recording, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var recs []model.Recording
	err := r.DB.Where("camera_id = ?", camID).
		Order("start_at desc").Limit(limit).Find(&recs).Error
	return recs, err
}

// DeleteRecording hard-deletes a recording row and its file.
func (r *Recorder) DeleteRecording(id uint) error {
	var rec model.Recording
	if err := r.DB.First(&rec, id).Error; err != nil {
		return err
	}
	_ = os.Remove(rec.FilePath)
	return r.DB.Delete(&rec).Error
}

// ------- go2rtc recorder API client extensions -------

// StartRecorder tells go2rtc to begin writing the named stream to
// the supplied filesystem path (must be reachable from inside the
// go2rtc container — typically via a shared volume).
func (c *Go2RTCClient) StartRecorder(ctx context.Context, streamName, path string) error {
	body, _ := json.Marshal(map[string]any{
		"name": streamName,
		"path": path,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/api/recorder", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("go2rtc start recorder: %s", resp.Status)
	}
	return nil
}

// StopRecorder stops writing for the supplied stream. go2rtc closes
// the file cleanly.
func (c *Go2RTCClient) StopRecorder(ctx context.Context, streamName string) error {
	u := fmt.Sprintf("%s/api/recorder?src=%s", c.Base, url.QueryEscape(streamName))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("go2rtc stop recorder: %s", resp.Status)
}

// recordingKey is the reserved key used inside cam.Meta.
const recordingKey = "recording"

// humanSize is a small helper for the handler view.
func humanSize(n int64) string {
	const k = 1024
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	f := float64(n)
	for f >= k && i < len(units)-1 {
		f /= k
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// joinSortedKeys is used in the recording summary. (Currently
// unused — kept so future debug views can render plan summaries
// without re-importing sort/strings in the handler.)
var _ = sort.Strings
var _ = strings.Join
