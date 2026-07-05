// fix_camera_password — re-encrypt every camera's stored password
// with a single new value, then re-push the resulting RTSP URL to
// go2rtc so the live stream picks up the corrected credentials.
//
// Use case: the operator changed the camera password on the device
// (e.g. Hikvision "Haikangcam") and wants all stored cameras to
// point at the new password without going through the DELETE + POST
// dance in the dashboard.
//
// Run from d:\Projects\home-datacenter\services\api\scripts (same as
// create_device.go), with the same APP_CONFIG / config.local.yaml
// the API uses, so the JWT secret that derives the AES-GCM key is
// the real one.
//
//	go run fix_camera_password.go -new Haikangcam
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"time"

	"home-datacenter-api/internal/camera"
	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/database"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

func main() {
	newPass := flag.String("new", "Haikangcam", "new camera password to apply to every row")
	go2Base := flag.String("go2rtc", "http://home-go2rtc:1984", "go2rtc base URL")
	dbPath := flag.String("db", "", "override database path (defaults to config.database.path)")
	// -rename-missing-keys: scan cameras whose stream_name still
	// matches the legacy "cam_<id>" pattern and rewrite both the
	// DB row AND the go2rtc stream entry to the friendly name.
	// One-shot migration for the Bug1 fix — existing rows that
	// were registered before the stream-name-as-friendly-name
	// change would otherwise keep the synthetic key in go2rtc and
	// the dashboard would show a different name than the stream
	// list. Use after rebuilding home-api with the new code.
	renameMissingKeys := flag.Bool("rename-missing-keys", false, "rewrite legacy cam_<id> stream names to the friendly name on every camera")
	flag.Parse()

	if *newPass == "" {
		log.Fatal("new password must not be empty")
	}

	// Same config resolution as the API: APP_CONFIG env var, then
	// configs/config.local.yaml, then configs/config.yaml.
	cfgPath := os.Getenv("APP_CONFIG")
	if err := config.Load(cfgPath); err != nil {
		log.Fatalf("config: %v", err)
	}
	if config.AppConfig.JWT.Secret == "" {
		log.Fatal("jwt.secret missing — set JWT_SECRET env or fix config.local.yaml")
	}

	// Open the SAME database the API uses. Path comes from config
	// unless overridden on the command line (handy for local
	// Windows dev where the path is d:\Projects\home-datacenter\data\sqlite\app.db
	// but the Docker config points at /data/sqlite/app.db).
	db := config.AppConfig.Database.Path
	if *dbPath != "" {
		db = *dbPath
	}
	database.InitDB(db)

	box, err := utils.NewSecretBox(config.AppConfig.JWT.Secret)
	if err != nil {
		log.Fatalf("secret box: %v", err)
	}

	var cams []model.Camera
	if err := database.DB.Find(&cams).Error; err != nil {
		log.Fatalf("list cameras: %v", err)
	}
	if len(cams) == 0 {
		log.Println("no cameras to fix")
		return
	}

	go2 := camera.NewGo2RTCClient(*go2Base)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// -rename-missing-keys: one-shot Bug1 migration. Detect any
	// camera whose stream_name is the synthetic "cam_<id>" and
	// rewrite both the DB row and the live go2rtc stream to use
	// the friendly name. This is the "soft" path: existing
	// recordings (file paths) keep working because the
	// `recordings` table carries the stream_name that was active
	// at recording time, and the new stream_name is purely a
	// display + SDP lookup key.
	if *renameMissingKeys {
		renamed := 0
		for i := range cams {
			c := &cams[i]
			expected := fmt.Sprintf("cam_%d", c.ID)
			if c.StreamName != expected {
				continue
			}
			if c.Name == "" || c.Name == expected {
				log.Printf("cam %d: no friendly name to migrate to, skipping", c.ID)
				continue
			}
			// 1) drop the old synthetic stream from go2rtc
			if err := go2.RemoveStream(ctx, c.StreamName); err != nil {
				log.Printf("cam %d: remove %s from go2rtc: %v", c.ID, c.StreamName, err)
			}
			// 2) push the RTSP URL under the new friendly key
			eu, _ := c.Credentials["onvif_user"].(string)
			ep, _ := c.Credentials["onvif_pass"].(string)
			user, err := box.Decrypt(eu)
			if err != nil {
				log.Printf("cam %d: decrypt user: %v", c.ID, err)
				continue
			}
			pass, err := box.Decrypt(ep)
			if err != nil {
				log.Printf("cam %d: decrypt pass: %v", c.ID, err)
				continue
			}
			rtsp := fmt.Sprintf("rtsp://%s:%s@%s:%d/Streaming/Channels/%d#audio=0",
				user, pass, c.Host, c.RTSPPort, c.ChannelID)
			if err := go2.AddStream(ctx, c.Name, rtsp); err != nil {
				log.Printf("cam %d: add %s to go2rtc: %v", c.ID, c.Name, err)
				continue
			}
			// 3) update the DB row last, only after go2rtc has the
			//    new key. A crash before this point leaves the
			//    stream under the new name in go2rtc but the DB on
			//    the old one — the BootReplay re-injects it under
			//    the DB name and you end up with a duplicate.
			old := c.StreamName
			if err := database.DB.Model(c).Update("stream_name", c.Name).Error; err != nil {
				log.Printf("cam %d: db update stream_name: %v", c.ID, err)
				continue
			}
			c.StreamName = c.Name
			log.Printf("cam %d: stream_name %q → %q", c.ID, old, c.Name)
			renamed++
		}
		log.Printf("rename: %d camera(s) migrated to friendly-name stream key", renamed)
	}

	for i := range cams {
		c := &cams[i]
		if c.Credentials == nil {
			log.Printf("cam %d (%s): no credentials, skipping", c.ID, c.StreamName)
			continue
		}

		eu, _ := c.Credentials["onvif_user"].(string)
		ep, _ := c.Credentials["onvif_pass"].(string)
		user, err := box.Decrypt(eu)
		if err != nil {
			log.Printf("cam %d: decrypt user: %v", c.ID, err)
			continue
		}
		oldPass, err := box.Decrypt(ep)
		if err != nil {
			log.Printf("cam %d: decrypt pass: %v", c.ID, err)
			continue
		}

		// Box the new password with the same SecretBox the API uses.
		newEp, err := box.Encrypt(*newPass)
		if err != nil {
			log.Printf("cam %d: encrypt new pass: %v", c.ID, err)
			continue
		}
		c.Credentials["onvif_pass"] = newEp

		// Persist. We update only the credentials column so we don't
		// race with the HealthChecker rewriting status / last_seen_at.
		if err := database.DB.Model(c).Update("credentials", c.Credentials).Error; err != nil {
			log.Printf("cam %d: db update: %v", c.ID, err)
			continue
		}
		log.Printf("cam %d (%s): password updated (user=%q, was=%q, now=%q)",
			c.ID, c.StreamName, user, oldPass, *newPass)

		// Re-push the RTSP URL to go2rtc. The rtspURL helper bakes
		// in #audio=0 exactly like a fresh Register. We do NOT
		// add #video=H264 any more — the go2rtc image dropped
		// the server-side transcoder and we rely on HLS
		// passthrough of the camera's native codec (HEVC).
		rtsp := fmt.Sprintf("rtsp://%s:%s@%s:%d/Streaming/Channels/%d#audio=0",
			user, *newPass, c.Host, c.RTSPPort, c.ChannelID)
		log.Printf("cam %d: pushing %s", c.ID, redactPass(rtsp))
		if err := go2.AddStream(ctx, c.StreamName, rtsp); err != nil {
			log.Printf("cam %d: go2rtc add: %v", c.ID, err)
		} else {
			log.Printf("cam %d: go2rtc stream updated", c.ID)
		}
	}

	log.Println("done")
	log.Println("hint: restart home-api so BootReplay re-loads the corrected credentials into any cached state")
}

// redactPass masks the password component so the URL is safe to log.
func redactPass(rtsp string) string {
	u, err := url.Parse(rtsp)
	if err != nil {
		return rtsp
	}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), "REDACTED")
		}
	}
	return u.String()
}
