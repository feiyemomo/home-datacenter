package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"home-datacenter-api/internal/ops"
)

type probeResult struct {
	Acquired          bool   `json:"acquired"`
	Valid             bool   `json:"valid"`
	Renewed           bool   `json:"renewed"`
	Released          bool   `json:"released"`
	Active            bool   `json:"active"`
	Holder            string `json:"holder,omitempty"`
	Token             string `json:"token,omitempty"`
	FencingGeneration int64  `json:"fencing_generation"`
	ExpiresAtMs       int64  `json:"expires_at_ms,omitempty"`
	Error             string `json:"error,omitempty"`
}

func emit(res probeResult, exitCode int) {
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
	os.Exit(exitCode)
}

func main() {
	dbPath := flag.String("db", "", "Path to shared SQLite WAL database")
	action := flag.String("action", "acquire", "Action: acquire | renew | verify | release | inspect | hold")
	component := flag.String("component", "home-web", "Canonical or alias component name")
	holder := flag.String("holder", "go-core", "Lease holder identity")
	token := flag.String("token", "", "Lease token for renew/verify/release")
	gen := flag.Int64("gen", 0, "Fencing generation for verify")
	ttlMs := flag.Int64("ttl-ms", 5000, "Lease TTL in milliseconds")
	holdMs := flag.Int64("hold-ms", 500, "How long to sleep while holding lease in hold action")
	initSchema := flag.Bool("init-schema", false, "Initialize official v4 schema on target DB before action")
	flag.Parse()

	if *dbPath == "" {
		emit(probeResult{Error: "missing -db flag"}, 2)
	}

	dsn := *dbPath + "?_journal_mode=WAL&_busy_timeout=5000"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		emit(probeResult{Error: fmt.Sprintf("open sqlite failed: %v", err)}, 1)
	}
	if sqlDB, err := db.DB(); err == nil {
		defer sqlDB.Close()
	}

	if *initSchema {
		if err := ops.EnsureOpsSchema(db); err != nil {
			emit(probeResult{Error: fmt.Sprintf("ensure ops schema failed: %v", err)}, 1)
		}
	}

	mgr := ops.NewRecoveryLeaseManager(db)
	ttl := time.Duration(*ttlMs) * time.Millisecond

	switch *action {
	case "acquire":
		grant, err := mgr.AcquireLeaseGrant(*component, *holder, ttl)
		if err != nil {
			emit(probeResult{Acquired: false, Error: err.Error()}, 1)
		}
		emit(probeResult{
			Acquired:          true,
			Token:             grant.LeaseToken,
			Holder:            grant.Holder,
			FencingGeneration: grant.FencingGeneration,
			ExpiresAtMs:       grant.ExpiresAtMs,
		}, 0)

	case "hold":
		grant, err := mgr.AcquireLeaseGrant(*component, *holder, ttl)
		if err != nil {
			emit(probeResult{Acquired: false, Error: err.Error()}, 1)
		}
		// Print acquired immediately to stdout for test orchestrator, then sleep until killed or elapsed
		b, _ := json.Marshal(probeResult{
			Acquired:          true,
			Token:             grant.LeaseToken,
			Holder:            grant.Holder,
			FencingGeneration: grant.FencingGeneration,
			ExpiresAtMs:       grant.ExpiresAtMs,
		})
		fmt.Println(string(b))
		_ = os.Stdout.Sync()
		time.Sleep(time.Duration(*holdMs) * time.Millisecond)
		os.Exit(0)

	case "renew":
		expMs, err := mgr.RenewLease(*component, *token, ttl)
		if err != nil {
			emit(probeResult{Renewed: false, Error: err.Error()}, 1)
		}
		emit(probeResult{Renewed: true, Token: *token, ExpiresAtMs: expMs}, 0)

	case "verify":
		err := mgr.VerifyLeaseFencing(*component, *token, *gen, 0)
		if err != nil {
			emit(probeResult{Valid: false, Error: err.Error()}, 1)
		}
		emit(probeResult{Valid: true, Token: *token, FencingGeneration: *gen}, 0)

	case "release":
		err := mgr.ReleaseLease(*component, *token)
		if err != nil {
			emit(probeResult{Released: false, Error: err.Error()}, 1)
		}
		emit(probeResult{Released: true, Token: *token}, 0)

	case "inspect":
		active, h, err := mgr.IsLeaseActive(*component)
		if err != nil {
			emit(probeResult{Active: active, Holder: h, Error: err.Error()}, 1)
		}
		emit(probeResult{Active: active, Holder: h}, 0)

	default:
		emit(probeResult{Error: fmt.Sprintf("unknown action %q", *action)}, 2)
	}
}
