package camera

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/vision"
)

// ISAPIGuardProvider abstracts guard mode querying.
type ISAPIGuardProvider interface {
	GetMode() string
}

// ISAPIEventAlert represents the Hikvision/Hostone EventNotificationAlert XML schema.
type ISAPIEventAlert struct {
	XMLName          xml.Name `xml:"EventNotificationAlert"`
	IPAddress        string   `xml:"ipAddress"`
	PortNo           int      `xml:"portNo"`
	ChannelID        int      `xml:"channelID"`
	DateTime         string   `xml:"dateTime"`
	ActivePostCount  int      `xml:"activePostCount"`
	EventType        string   `xml:"eventType"`
	EventState       string   `xml:"eventState"`
	EventDescription string   `xml:"eventDescription"`
	ChannelName      string   `xml:"channelName"`
	TargetType       string   `xml:"targetType"`
	TargetAttrs      struct {
		TargetType string `xml:"targetType"`
	} `xml:"TargetAttrs"`
	DetectionRegionList []struct {
		RegionID   int    `xml:"regionID"`
		TargetType string `xml:"targetType"`
	} `xml:"DetectionRegionList>DetectionRegionEntry"`
}

// IsHuman returns true if the alert explicitly indicates a human target,
// or is an AI smart detection type.
func (a *ISAPIEventAlert) IsHuman() bool {
	if strings.EqualFold(a.TargetType, "human") {
		return true
	}
	if strings.EqualFold(a.TargetAttrs.TargetType, "human") {
		return true
	}
	for _, r := range a.DetectionRegionList {
		if strings.EqualFold(r.TargetType, "human") {
			return true
		}
	}
	et := strings.ToLower(a.EventType)
	return strings.Contains(et, "fielddetection") ||
		strings.Contains(et, "linedetection") ||
		strings.Contains(et, "facedetection") ||
		strings.Contains(et, "humandetection") ||
		strings.Contains(et, "regionentrance")
}

// CameraActivitySession tracks ongoing human activity detected on a camera
// so continuous detections are merged into one session rather than firing
// noisy repeated alerts.
type CameraActivitySession struct {
	SessionID   string
	FirstTs     int64
	LastAlert   time.Time
	LastPublish time.Time
	Count       int
}

// ISAPIAlertListener maintains persistent ISAPI alertStream connections to all
// Hikvision-compatible on-board AI cameras and merges events into the App notification pipeline.
type ISAPIAlertListener struct {
	registry      *Registry
	bus           *eventbus.Bus
	guardProvider ISAPIGuardProvider
	visionClient  *vision.Client

	mu        sync.Mutex
	active    map[uint]context.CancelFunc
	lastAlert map[uint]time.Time
	sessions  map[uint]*CameraActivitySession
	ctx       context.Context
	cancel    context.CancelFunc
}

// NewISAPIAlertListener creates a listener instance.
func NewISAPIAlertListener(reg *Registry, bus *eventbus.Bus, gp ISAPIGuardProvider, vc *vision.Client) *ISAPIAlertListener {
	ctx, cancel := context.WithCancel(context.Background())
	return &ISAPIAlertListener{
		registry:      reg,
		bus:           bus,
		guardProvider: gp,
		visionClient:  vc,
		active:        make(map[uint]context.CancelFunc),
		lastAlert:     make(map[uint]time.Time),
		sessions:      make(map[uint]*CameraActivitySession),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Start begins periodic camera synchronization and starts stream listeners.
func (l *ISAPIAlertListener) Start() {
	log.Printf("isapi_listener: starting camera hardware AI alert listener manager")
	l.syncCameras()
	go l.syncLoop()
}

// Stop shuts down all camera stream workers.
func (l *ISAPIAlertListener) Stop() {
	l.cancel()
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, cancel := range l.active {
		cancel()
		delete(l.active, id)
	}
	log.Printf("isapi_listener: stopped all camera alert stream workers")
}

func (l *ISAPIAlertListener) syncLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			l.syncCameras()
		}
	}
}

func (l *ISAPIAlertListener) syncCameras() {
	cams := l.registry.List()
	l.mu.Lock()
	defer l.mu.Unlock()

	activeIDs := make(map[uint]bool)
	for _, cam := range cams {
		if cam.Status == "offline" || cam.Host == "" {
			continue
		}
		activeIDs[cam.ID] = true
		if _, exists := l.active[cam.ID]; !exists {
			camCtx, camCancel := context.WithCancel(l.ctx)
			l.active[cam.ID] = camCancel
			go l.worker(camCtx, cam)
		}
	}

	// Clean up removed or offline cameras
	for id, cancel := range l.active {
		if !activeIDs[id] {
			cancel()
			delete(l.active, id)
			delete(l.lastAlert, id)
			delete(l.sessions, id)
		}
	}
}

func (l *ISAPIAlertListener) worker(ctx context.Context, cam model.Camera) {
	camName := cam.Name
	if camName == "" {
		camName = cam.Host
	}
	log.Printf("isapi_listener: [%s] worker started for %s:%d", camName, cam.Host, cam.ONVIFPort)
	defer log.Printf("isapi_listener: [%s] worker exiting", camName)

	backoff := 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		user, pass, err := l.registry.DecryptCredentials(&cam)
		if err != nil {
			log.Printf("isapi_listener: [%s] credentials decrypt error: %v", camName, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
				continue
			}
		}

		err = l.streamLoop(ctx, cam, user, pass)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("isapi_listener: [%s] alert stream disconnected (%v), retrying in %v...", camName, err, backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (l *ISAPIAlertListener) streamLoop(ctx context.Context, cam model.Camera, user, pass string) error {
	body, err := openISAPIAlertStream(ctx, cam.Host, cam.ONVIFPort, user, pass)
	if err != nil {
		return err
	}
	defer body.Close()

	camName := cam.Name
	if camName == "" {
		camName = cam.Host
	}
	log.Printf("isapi_listener: [%s] alertStream connected successfully!", camName)

	reader := bufio.NewReader(body)
	var chunkBuffer bytes.Buffer
	inXml := false

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}

		trimmed := strings.TrimSpace(line)

		// Boundary delimiter marks end of a chunk
		if strings.HasPrefix(trimmed, "--") {
			if chunkBuffer.Len() > 0 {
				data := chunkBuffer.Bytes()
				chunkBuffer.Reset()
				l.handleAlertXML(cam, data)
			}
			inXml = false
			continue
		}

		// Detect XML start
		if strings.HasPrefix(trimmed, "<EventNotificationAlert") || strings.HasPrefix(trimmed, "<?xml") {
			inXml = true
		}

		if inXml {
			chunkBuffer.WriteString(line)
		}
	}
}

func (l *ISAPIAlertListener) handleAlertXML(cam model.Camera, data []byte) {
	var alert ISAPIEventAlert
	if err := xml.Unmarshal(data, &alert); err != nil {
		return
	}

	// Filter out harmless keepalives and inactive states
	if alert.EventState != "active" {
		return
	}
	et := strings.ToLower(alert.EventType)
	if et == "videoloss" || et == "heartbeat" {
		return
	}

	// Check AI alert source policy (e.g. if set to "frigate", suppress camera hardware alert)
	mgr := GetGlobalAIAlertSourceManager("")
	if mgr != nil && !mgr.ShouldProcessCameraAlert(cam.ID) {
		log.Printf("isapi_listener: [%s] hardware alert suppressed by AI alert source policy (mode=%s)", cam.Name, mgr.GetSource())
		return
	}

	motionFallback := mgr != nil && mgr.GetMotionFallback()
	isHuman := alert.IsHuman()
	if !isHuman {
		// Strict mode (default): drop plain motion without targetType=human.
		// Compatibility mode (Plan B): if motion fallback is enabled, accept active VMD / motion events
		// so cameras whose firmware doesn't tag human attributes (e.g. yard / backyard) won't miss alerts.
		if !motionFallback {
			return
		}
		if !strings.Contains(et, "vmd") && !strings.Contains(et, "motion") && !strings.Contains(et, "alarm") && !strings.Contains(et, "detect") {
			return
		}
	}

	now := time.Now()

	// Retrieve notify settings from config (with default fallbacks)
	dedupSec := 5
	sessionWinSec := 90
	heartbeatSec := 30
	if config.AppConfig != nil {
		if config.AppConfig.Notify.DedupSeconds > 0 {
			dedupSec = config.AppConfig.Notify.DedupSeconds
		}
		if config.AppConfig.Notify.SessionWindowSeconds > 0 {
			sessionWinSec = config.AppConfig.Notify.SessionWindowSeconds
		}
		if config.AppConfig.Notify.HeartbeatSeconds > 0 {
			heartbeatSec = config.AppConfig.Notify.HeartbeatSeconds
		}
	}

	// Per-camera alert deduplication and activity session de-bounce
	l.mu.Lock()
	last := l.lastAlert[cam.ID]
	if now.Sub(last) < time.Duration(dedupSec)*time.Second {
		l.mu.Unlock()
		return
	}
	l.lastAlert[cam.ID] = now

	sess := l.sessions[cam.ID]
	isUpdate := false
	sessionID := ""
	firstTs := now.Unix()
	alertCount := 1

	if sess != nil && now.Sub(sess.LastAlert) < time.Duration(sessionWinSec)*time.Second {
		// Ongoing activity session
		sess.Count++
		sess.LastAlert = now
		sessionID = sess.SessionID
		firstTs = sess.FirstTs
		alertCount = sess.Count

		if now.Sub(sess.LastPublish) < time.Duration(heartbeatSec)*time.Second {
			// Sub-heartbeat burst: silently increment count without broadcasting
			l.mu.Unlock()
			return
		}

		// Heartbeat due: publish as in-session update
		isUpdate = true
		sess.LastPublish = now
	} else {
		// New activity session (first report)
		sessionID = fmt.Sprintf("sess-%d-%d", cam.ID, now.UnixMilli())
		l.sessions[cam.ID] = &CameraActivitySession{
			SessionID:   sessionID,
			FirstTs:     firstTs,
			LastAlert:   now,
			LastPublish: now,
			Count:       1,
		}
	}
	l.mu.Unlock()

	guardMode := model.GuardModeArmedAway
	if l.guardProvider != nil {
		guardMode = l.guardProvider.GetMode()
	}

	// 撤防模式免打扰：不产生手机告警
	if guardMode == model.GuardModeDisarmed {
		log.Printf("isapi_listener: [%s] hardware AI alert suppressed in disarmed mode: event=%s", cam.Name, alert.EventType)
		return
	}

	isIntrusion := (guardMode == model.GuardModeArmedAway)
	label := "person"

	slug := l.registry.FrigateSlugUnique(&cam)
	eventID := fmt.Sprintf("isapi-%d-%d", cam.ID, now.UnixMilli())

	desc := "摄像头自带AI人物识别"
	if isIntrusion {
		desc = "离家布防异常入侵人员"
	} else if !isHuman {
		desc = "摄像头移动侦测 (兼容模式)"
	}

	payloadMap := map[string]any{
		"id":           eventID,
		"event_id":     eventID,
		"camera_id":    cam.ID,
		"camera_slug":  slug,
		"camera_name":  cam.Name,
		"type":         "detection",
		"label":        label,
		"confidence":   0.96,
		"is_intrusion": isIntrusion,
		"name":         desc,
		"persons":      []string{"人员活动"},
		"muted":        false,
		"has_snapshot": true,
		"has_clip":     true,
		"start_time":   float64(now.Unix()),
		"end_time":     float64(now.Unix() + 10),
		"ts":           now.Unix(),
		"is_update":    isUpdate,
		"count":        alertCount,
		"first_ts":     firstTs,
		"session_id":   sessionID,
	}

	payload, _ := json.Marshal(payloadMap)

	log.Printf("isapi_listener: [ALERT] %s (id=%d) triggered camera AI %s detection: intrusion=%v update=%v count=%d session=%s",
		cam.Name, cam.ID, label, isIntrusion, isUpdate, alertCount, sessionID)

	if l.bus != nil {
		// Publish camera.person_recognized (handled by AlertKeepAliveService -> showPersonRecognizedNotification)
		severity := eventbus.SeverityCritical
		if isUpdate {
			severity = eventbus.SeverityInfo
		}
		l.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicCameraPersonRecognized,
			Source:   eventbus.SourceCamera,
			Severity: severity,
			Payload:  payload,
		})

		// Also publish camera.motion for general timeline tracking and dashboard live detection.
		// Mark muted=true so notification handlers (e.g. showSecurityAlertNotification) do not
		// fire duplicate push notifications for the same event, while log/subscriber and web dashboard
		// continue recording motion.
		motionPayloadMap := make(map[string]any, len(payloadMap))
		for k, v := range payloadMap {
			motionPayloadMap[k] = v
		}
		motionPayloadMap["muted"] = true
		motionPayload, _ := json.Marshal(motionPayloadMap)

		l.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicCameraMotion,
			Source:   eventbus.SourceCamera,
			Severity: eventbus.SeverityInfo,
			Payload:  motionPayload,
		})
	}

	// Capture snapshot and persist alert asynchronously
	go l.captureAndPersistAlert(cam, eventID, slug, label, isIntrusion, now.Unix())
}

func (l *ISAPIAlertListener) captureAndPersistAlert(cam model.Camera, eventID, slug, label string, isIntrusion bool, ts int64) {
	user, pass, err := l.registry.DecryptCredentials(&cam)
	if err != nil {
		return
	}

	port := cam.ONVIFPort
	if port <= 0 {
		port = 80
	}
	picURL := fmt.Sprintf("http://%s:%d/ISAPI/Streaming/channels/102/picture", cam.Host, port)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	imgBytes, _ := fetchISAPIPicture(ctx, picURL, user, pass)

	var snapshotRelPath string
	var thumbB64 string
	hasSnapshot := false

	if len(imgBytes) > 0 {
		hasSnapshot = true
		thumbB64 = base64.StdEncoding.EncodeToString(imgBytes)

		// Save snapshot to disk under /data/snapshots/<eventID>.jpg
		snapshotDir := "/data/snapshots"
		if err := os.MkdirAll(snapshotDir, 0755); err == nil {
			snapFile := filepath.Join(snapshotDir, fmt.Sprintf("%s.jpg", eventID))
			if err := os.WriteFile(snapFile, imgBytes, 0644); err == nil {
				snapshotRelPath = snapFile
			}
		}
	}

	// Persist to camera_alerts table
	if l.registry != nil && l.registry.DB != nil {
		alertRec := model.CameraAlert{
			ID:           eventID,
			CameraID:     cam.ID,
			CameraSlug:   slug,
			CameraName:   cam.Name,
			Label:        label,
			Confidence:   0.96,
			StartTime:    float64(ts),
			EndTime:      float64(ts + 10),
			HasClip:      true,
			HasSnapshot:  hasSnapshot,
			SnapshotPath: snapshotRelPath,
			Thumbnail:    thumbB64,
			Source:       "camera",
			IsIntrusion:  isIntrusion,
			CreatedAt:    time.Unix(ts, 0),
		}
		if err := l.registry.DB.Create(&alertRec).Error; err != nil {
			log.Printf("isapi_listener: failed to persist camera alert: %v", err)
		} else {
			log.Printf("isapi_listener: persisted camera alert %s (has_snapshot=%v)", eventID, hasSnapshot)
		}
	}

	// Optional face recognition enrichment if armed_home and visionClient is available
	guardMode := model.GuardModeArmedAway
	if l.guardProvider != nil {
		guardMode = l.guardProvider.GetMode()
	}
	if !isIntrusion && guardMode == model.GuardModeArmedHome && l.visionClient != nil && len(imgBytes) > 0 {
		l.runFaceEnrichment(cam, eventID, slug, ts, imgBytes)
	}
}

func (l *ISAPIAlertListener) runFaceEnrichment(cam model.Camera, eventID, slug string, ts int64, imgBytes []byte) {
	analyzeCtx, aCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer aCancel()

	res, err := l.visionClient.Analyze(analyzeCtx, vision.AnalyzeRequest{
		ImageBase64: base64.StdEncoding.EncodeToString(imgBytes),
		DetectFace:  true,
		DetectPose:  false,
	})
	if err != nil || res == nil || len(res.Data.MatchedPersons) == 0 {
		return
	}

	matchedName := res.Data.MatchedPersons[0]
	log.Printf("isapi_listener: camera %s enriched with face recognition: %s", cam.Name, matchedName)

	enrichedPayload, _ := json.Marshal(map[string]any{
		"id":           eventID,
		"event_id":     eventID,
		"camera_id":    cam.ID,
		"camera_slug":  slug,
		"camera_name":  cam.Name,
		"persons":      res.Data.MatchedPersons,
		"name":         matchedName,
		"is_intrusion": false,
		"ts":           ts,
		"start_time":   float64(ts),
		"has_snapshot": true,
	})

	l.bus.Publish(eventbus.Event{
		Topic:    eventbus.TopicCameraPersonRecognized,
		Source:   eventbus.SourceSystem,
		Severity: eventbus.SeverityInfo,
		Payload:  enrichedPayload,
	})
}

func openISAPIAlertStream(ctx context.Context, host string, port int, user, pass string) (io.ReadCloser, error) {
	if port <= 0 {
		port = 80
	}
	target := fmt.Sprintf("http://%s:%d/ISAPI/Event/notification/alertStream", host, port)
	client := &http.Client{Timeout: 0}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(user, pass)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return resp.Body, nil
	}
	if resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		return nil, fmt.Errorf("alertStream returned HTTP %d", resp.StatusCode)
	}

	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()

	digestHeader := buildDigestAuthHeader(http.MethodGet, "/ISAPI/Event/notification/alertStream", user, pass, challenge)
	if digestHeader == "" {
		return nil, fmt.Errorf("failed to build digest auth header")
	}

	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req2.Header.Set("Authorization", digestHeader)

	resp2, err := client.Do(req2)
	if err != nil {
		return nil, err
	}
	if resp2.StatusCode != http.StatusOK {
		resp2.Body.Close()
		return nil, fmt.Errorf("alertStream digest auth returned HTTP %d", resp2.StatusCode)
	}
	return resp2.Body, nil
}

func fetchISAPIPicture(ctx context.Context, targetURL, user, pass string) ([]byte, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(user, pass)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		path := targetPath(targetURL)
		digest := buildDigestAuthHeader(http.MethodGet, path, user, pass, challenge)
		if digest != "" {
			req2, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
			if err == nil {
				req2.Header.Set("Authorization", digest)
				resp2, err := client.Do(req2)
				if err == nil {
					defer resp2.Body.Close()
					if resp2.StatusCode == http.StatusOK {
						return io.ReadAll(resp2.Body)
					}
				}
			}
		}
	}
	if resp.StatusCode == http.StatusOK {
		return io.ReadAll(resp.Body)
	}
	return nil, fmt.Errorf("picture HTTP %d", resp.StatusCode)
}
