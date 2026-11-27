package camera

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestDaguEncryptAndLogin(t *testing.T) {
	if testing.Short() || os.Getenv("DAGU_LIVE_TEST") == "" {
		t.Skip("skipping live Dagu cloud test; set DAGU_LIVE_TEST=1 to run")
	}
	mgr := NewDaguManager("15291472514", "wm10050817")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token, err := mgr.GetToken(ctx)
	if err != nil {
		t.Fatalf("GetToken failed: %v", err)
	}
	if token == "" {
		t.Fatal("empty token received")
	}
	t.Logf("Got token successfully: %s...", token[:20])
}

func TestDaguAcquireStreamAndRelease(t *testing.T) {
	if testing.Short() || os.Getenv("DAGU_LIVE_TEST") == "" {
		t.Skip("skipping live Dagu cloud stream test; set DAGU_LIVE_TEST=1 to run")
	}
	mgr := NewDaguManager("15291472514", "wm10050817")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sn := "6912200023929742"
	addr, err := mgr.AcquireStream(ctx, sn, 1)
	if err != nil {
		t.Fatalf("AcquireStream failed: %v", err)
	}
	t.Logf("Acquired stream address: %s", addr)
	if addr == "" {
		t.Fatal("expected non-empty stream address")
	}

	// Release stream
	mgr.ReleaseStream(sn)
}
