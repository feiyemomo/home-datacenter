//go:build ignore

// reset_camera_credentials — overwrite every camera's stored
// onvif_user / onvif_pass with a known-good value, sealed with the
// SecretBox the API uses.
//
// Use case: rows were sealed with an older, since-rotated key and can
// no longer be decrypted ("cipher: message authentication failed"),
// which makes BootReplay skip them and leaves Frigate with zero
// cameras. Because AES-GCM is authenticated there is no way to recover
// the plaintext — the only repair is to write the credentials again.
//
// The credentials are written with utils.NewAppSecretBox (primary =
// ENCRYPTION_KEY, legacy = jwt.secret), i.e. exactly what the API
// reads with, so the rows are guaranteed to open afterwards.
//
// The tool only touches the `credentials` column; go2rtc streams and
// the Frigate config are refreshed by the API's BootReplay on the next
// restart (the deploy step restarts home-api right after this runs).
//
// Run from services/api (so the module resolves) with the same
// APP_CONFIG the API uses:
//
//	go run scripts/reset_camera_credentials.go -user admin -pass <PW>
//	go run scripts/reset_camera_credentials.go -user admin -pass <PW> -id 10
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/database"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

func main() {
	user := flag.String("user", "admin", "camera username to store")
	pass := flag.String("pass", "", "camera password to store (required)")
	camID := flag.Uint("id", 0, "only reset this camera id (0 = all)")
	dbPath := flag.String("db", "", "override database path (defaults to config.database.path)")
	dryRun := flag.Bool("dry-run", false, "show what would change without writing")
	flag.Parse()

	if *pass == "" {
		log.Fatal("missing -pass <password>")
	}

	cfgPath := os.Getenv("APP_CONFIG")
	if err := config.Load(cfgPath); err != nil {
		log.Fatalf("config: %v", err)
	}

	db := config.AppConfig.Database.Path
	if *dbPath != "" {
		db = *dbPath
	}
	database.InitDB(db)

	box, err := utils.NewAppSecretBox(config.AppConfig.Security.EncryptionKey, config.AppConfig.JWT.Secret)
	if err != nil {
		log.Fatalf("secret box: %v", err)
	}

	var cams []model.Camera
	q := database.DB
	if *camID != 0 {
		q = q.Where("id = ?", *camID)
	}
	if err := q.Find(&cams).Error; err != nil {
		log.Fatalf("list cameras: %v", err)
	}
	if len(cams) == 0 {
		log.Println("no cameras matched")
		return
	}

	newUser, err := box.Encrypt(*user)
	if err != nil {
		log.Fatalf("encrypt user: %v", err)
	}
	newPass, err := box.Encrypt(*pass)
	if err != nil {
		log.Fatalf("encrypt pass: %v", err)
	}

	fixed, skipped := 0, 0
	for i := range cams {
		c := &cams[i]

		// Skip cameras whose stored credentials already decrypt to the
		// target values — avoids needless writes (and a fresh nonce) on
		// every deploy.
		if c.Credentials != nil {
			eu, _ := c.Credentials["onvif_user"].(string)
			ep, _ := c.Credentials["onvif_pass"].(string)
			u, uerr := box.Decrypt(eu)
			p, perr := box.Decrypt(ep)
			if uerr == nil && perr == nil && u == *user && p == *pass {
				log.Printf("cam %d (%s): already correct, skipping", c.ID, c.Name)
				skipped++
				continue
			}
		}

		if *dryRun {
			log.Printf("cam %d (%s): WOULD reset credentials", c.ID, c.Name)
			fixed++
			continue
		}

		creds := c.Credentials
		if creds == nil {
			creds = model.JSON{}
		}
		creds["onvif_user"] = newUser
		creds["onvif_pass"] = newPass

		if err := database.DB.Model(c).Update("credentials", creds).Error; err != nil {
			log.Printf("cam %d (%s): update failed: %v", c.ID, c.Name, err)
			continue
		}
		log.Printf("cam %d (%s): credentials reset (user=%s)", c.ID, c.Name, *user)
		fixed++
	}

	fmt.Printf("done: %d reset, %d skipped\n", fixed, skipped)
	if !*dryRun && fixed > 0 {
		log.Println("hint: restart home-api so BootReplay re-pushes streams + Frigate config")
	}
}
