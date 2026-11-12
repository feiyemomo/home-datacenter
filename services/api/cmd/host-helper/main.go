package main

import (
	"context"
	"encoding/hex"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"home-datacenter-api/internal/hostsec"
)

func main() {
	var (
		listenAddr   = flag.String("listen", "", "TCP listen address (e.g., 127.0.0.1:8088)")
		socketPath   = flag.String("socket", "", "UNIX domain socket path (e.g., /run/home-datacenter/host-bridge.sock)")
		hostRoot     = flag.String("host-root", "/", "Host root filesystem path")
		snapshotRoot = flag.String("snapshot-root", "/var/lib/home-datacenter/snapshots", "Snapshot storage root directory")
		keyID        = flag.String("key-id", "bridge-default-key", "HMAC Key ID")
		hmacKeyHex   = flag.String("hmac-key-hex", "", "Hex-encoded Bridge HMAC Key (min 32 bytes)")
		grantKeyID   = flag.String("grant-key-id", "", "Grant Authority Key ID")
		grantKeyHex  = flag.String("grant-hmac-key-hex", "", "Hex-encoded Grant HMAC Key (independent from bridge key)")
		requireGrant = flag.Bool("require-grant", false, "Require cryptographically signed HostExecutionGrant for mutating operations")
		requireFence = flag.Bool("require-fencing", true, "Require strictly monotonic fencing generation")
	)
	flag.Parse()

	// Environment variable fallback
	if *listenAddr == "" && *socketPath == "" {
		if ep := strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_ENDPOINT")); ep != "" {
			if strings.HasPrefix(ep, "unix://") {
				*socketPath = strings.TrimPrefix(ep, "unix://")
			} else if strings.HasPrefix(ep, "http://") {
				*listenAddr = strings.TrimPrefix(ep, "http://")
			} else {
				*listenAddr = ep
			}
		} else {
			*socketPath = "/run/home-datacenter/host-bridge.sock"
		}
	}
	if *hmacKeyHex == "" {
		*hmacKeyHex = strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_HMAC_KEY"))
	}
	if *grantKeyHex == "" {
		*grantKeyHex = strings.TrimSpace(os.Getenv("OPS_HOST_GRANT_HMAC_KEY"))
	}
	if *grantKeyID == "" {
		*grantKeyID = strings.TrimSpace(os.Getenv("OPS_HOST_GRANT_KEY_ID"))
	}
	if strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_REQUIRE_GRANT")) == "1" {
		*requireGrant = true
	}

	var hmacKey []byte
	var err error
	if len(*hmacKeyHex) == 64 {
		hmacKey, err = hex.DecodeString(*hmacKeyHex)
		if err != nil {
			log.Fatalf("invalid hex for bridge hmac key: %v", err)
		}
	} else if len(*hmacKeyHex) >= 32 {
		hmacKey = []byte(*hmacKeyHex)
	} else {
		log.Fatalf("bridge HMAC key must be provided via -hmac-key-hex or OPS_HOST_BRIDGE_HMAC_KEY (min 32 bytes)")
	}

	var grantHMACKey []byte
	if *grantKeyHex != "" {
		if len(*grantKeyHex) == 64 {
			grantHMACKey, err = hex.DecodeString(*grantKeyHex)
			if err != nil {
				log.Fatalf("invalid hex for grant hmac key: %v", err)
			}
		} else if len(*grantKeyHex) >= 32 {
			grantHMACKey = []byte(*grantKeyHex)
		} else {
			log.Fatalf("grant HMAC key must be at least 32 bytes")
		}
	}

	cfg := hostsec.HostBridgeServerConfig{
		HostRoot:       *hostRoot,
		SnapshotRoot:   *snapshotRoot,
		KeyID:          *keyID,
		HMACKey:        hmacKey,
		RequireFencing: *requireFence,
		RequireGrant:   *requireGrant,
		GrantKeyID:     *grantKeyID,
		GrantHMACKey:   grantHMACKey,
	}

	handler, err := hostsec.NewHostBridgeHTTPHandler(cfg)
	if err != nil {
		log.Fatalf("failed to initialize host bridge handler: %v", err)
	}

	server := &http.Server{
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	var listener net.Listener
	if *socketPath != "" {
		sockDir := filepath.Dir(*socketPath)
		if err := os.MkdirAll(sockDir, 0755); err != nil {
			log.Fatalf("create socket directory %q: %v", sockDir, err)
		}
		_ = os.Remove(*socketPath)
		listener, err = net.Listen("unix", *socketPath)
		if err != nil {
			log.Fatalf("listen unix %q: %v", *socketPath, err)
		}
		_ = os.Chmod(*socketPath, 0660)
		defer os.Remove(*socketPath)
		log.Printf("Home Datacenter Host Helper listening on unix:%s", *socketPath)
	} else {
		listener, err = net.Listen("tcp", *listenAddr)
		if err != nil {
			log.Fatalf("listen tcp %q: %v", *listenAddr, err)
		}
		log.Printf("Home Datacenter Host Helper listening on http://%s", *listenAddr)
	}

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-stopChan
	log.Println("Shutting down Host Helper daemon gracefully...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	log.Println("Host Helper stopped.")
}
