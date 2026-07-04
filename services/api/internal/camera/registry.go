package camera

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// Registry is the camera CRUD + go2rtc sync boundary. It does NOT
// expose HTTP — the handler layer calls it.
type Registry struct {
	DB  *gorm.DB
	Go2 *Go2RTCClient
	Box *utils.SecretBox
}

func NewRegistry(db *gorm.DB, g *Go2RTCClient, box *utils.SecretBox) *Registry {
	return &Registry{DB: db, Go2: g, Box: box}
}

// RegisterInput is the wire format for POST /api/v1/cameras.
// The handler is responsible for binding it; the service is
// responsible for sanitising defaults and persisting it.
type RegisterInput struct {
	Name         string
	Vendor       string
	Host         string
	ONVIFPort    int
	RTSPPort     int
	ChannelID    int
	Username     string
	Password     string
	PTZ          bool
	Audio        bool
	Motion       bool
	ProfileToken string // optional; blank → controller picks the first profile
}

// Register inserts a Camera row, then asks go2rtc to start pulling
// the RTSP stream under the name "cam_<id>". If go2rtc is down we
// still keep the DB row (so the operator can see & retry) but bubble
// up the error so the handler can return 502.
func (r *Registry) Register(ctx context.Context, in RegisterInput) (*model.Camera, error) {
	if in.RTSPPort == 0 {
		in.RTSPPort = 554
	}
	if in.ONVIFPort == 0 {
		in.ONVIFPort = 80
	}
	if in.ChannelID == 0 {
		in.ChannelID = 1
	}

	creds, err := r.boxCredentials(in.Username, in.Password)
	if err != nil {
		return nil, err
	}

	cam := &model.Camera{
		Type:      "camera",
		Name:      in.Name,
		Vendor:    in.Vendor,
		Host:      in.Host,
		ONVIFPort: in.ONVIFPort,
		RTSPPort:  in.RTSPPort,
		ChannelID: in.ChannelID,
		Status:    "unknown",
		Capabilities: model.JSON{
			"ptz":    in.PTZ,
			"audio":  in.Audio,
			"motion": in.Motion,
		},
		Credentials: creds,
		Meta:        model.JSON{"onvif_profile": in.ProfileToken},
	}

	if err := r.DB.Create(cam).Error; err != nil {
		return nil, err
	}

	cam.StreamName = fmt.Sprintf("cam_%d", cam.ID)

	rtspURL := r.rtspURL(cam, in.Username, in.Password)
	if err := r.Go2.AddStream(ctx, cam.StreamName, rtspURL); err != nil {
		// Roll back the DB row so the system doesn't claim a stream
		// that go2rtc doesn't have. Keep the original error.
		_ = r.DB.Delete(cam).Error
		return nil, fmt.Errorf("go2rtc add stream: %w", err)
	}

	if err := r.DB.Model(cam).Updates(map[string]any{
		"stream_name": cam.StreamName,
		"updated_at":  time.Now(),
	}).Error; err != nil {
		return nil, err
	}
	return cam, nil
}

// Unregister removes the row and asks go2rtc to drop the stream.
// go2rtc errors are logged but not returned — the DB is the source
// of truth and we don't want a half-deleted camera.
func (r *Registry) Unregister(ctx context.Context, id uint) error {
	var cam model.Camera
	if err := r.DB.First(&cam, id).Error; err != nil {
		return err
	}
	if cam.StreamName != "" {
		_ = r.Go2.RemoveStream(ctx, cam.StreamName)
	}
	return r.DB.Delete(&cam).Error
}

func (r *Registry) Get(id uint) (*model.Camera, error) {
	var c model.Camera
	if err := r.DB.First(&c, id).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *Registry) List() []model.Camera {
	var cs []model.Camera
	r.DB.Find(&cs)
	return cs
}

// UpdateStatus is called by the HealthChecker after each probe.
// Keeping it in the Registry means the persistence path is the same
// whether the caller is the background loop or a manual webhook.
func (r *Registry) UpdateStatus(id uint, status string, seen *time.Time) {
	updates := map[string]any{"status": status, "updated_at": time.Now()}
	if seen != nil {
		updates["last_seen_at"] = seen
	}
	r.DB.Model(&model.Camera{}).Where("id = ?", id).Updates(updates)
}

// BootReplay re-registers every existing camera with go2rtc. Call
// this from main.go after the go2rtc container has had a moment to
// come up. A failure on one camera must not stop the others.
func (r *Registry) BootReplay(ctx context.Context) error {
	for _, c := range r.List() {
		if c.StreamName == "" {
			continue
		}
		u, p, err := r.DecryptCredentials(&c)
		if err != nil {
			continue
		}
		_ = r.Go2.AddStream(ctx, c.StreamName, r.rtspURL(&c, u, p))
	}
	return nil
}

// --- credential helpers (also used by the ONVIF controller) ---

// rtspURL builds the canonical Hikvision-style URL:
//
//	rtsp://<user>:<pass>@<host>:<port>/Streaming/Channels/<channel>
//
// Most Dahua / Uniview / Ezviz devices accept the same shape; for
// vendors that diverge (Reolink, TP-Link) we add per-vendor paths
// later. For now this is the 90% case.
func (r *Registry) rtspURL(cam *model.Camera, user, pass string) string {
	return fmt.Sprintf("rtsp://%s:%s@%s:%d/Streaming/Channels/%d",
		user, pass, cam.Host, cam.RTSPPort, cam.ChannelID)
}

// boxCredentials encrypts the user/pass pair and packages them into
// a JSON blob the model stores as a single TEXT column.
func (r *Registry) boxCredentials(user, pass string) (model.JSON, error) {
	eu, err := r.Box.Encrypt(user)
	if err != nil {
		return nil, err
	}
	ep, err := r.Box.Encrypt(pass)
	if err != nil {
		return nil, err
	}
	return model.JSON{"onvif_user": eu, "onvif_pass": ep}, nil
}

// DecryptCredentials returns the plaintext user/pass for the
// supplied camera. The caller is expected to use them in-process
// and not log or persist them.
func (r *Registry) DecryptCredentials(c *model.Camera) (user, pass string, err error) {
	if c.Credentials == nil {
		return "", "", fmt.Errorf("camera %d: no credentials", c.ID)
	}
	eu, _ := c.Credentials["onvif_user"].(string)
	ep, _ := c.Credentials["onvif_pass"].(string)
	if user, err = r.Box.Decrypt(eu); err != nil {
		return "", "", err
	}
	if pass, err = r.Box.Decrypt(ep); err != nil {
		return "", "", err
	}
	return user, pass, nil
}

// MarshalStreamConfig is a small helper for the handler layer: it
// returns a JSON-safe struct describing the URLs the front-end
// should hit for live view.
type StreamConfig struct {
	StreamName string `json:"stream_name"`
	WebRTC     string `json:"webrtc_url"`
	HLS        string `json:"hls_url"`
}

func (r *Registry) StreamConfig(c *model.Camera) StreamConfig {
	return StreamConfig{
		StreamName: c.StreamName,
		WebRTC:     r.Go2.WebRTCURL(c.StreamName),
		HLS:        r.Go2.HLSURL(c.StreamName),
	}
}

// itoa is a tiny convenience so callers don't need to import strconv
// just to format a probe target. Kept private; package users can keep
// using strconv.
func itoa(i int) string { return strconv.Itoa(i) }
