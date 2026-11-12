package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"home-datacenter-api/internal/hostsec"
)

func main() {
	var (
		listenAddr     = flag.String("listen", "", "TCP listen address (e.g., 127.0.0.1:8088)")
		socketPath     = flag.String("socket", "", "UNIX domain socket path (e.g., /run/home-datacenter/host-bridge.sock)")
		socketUID      = flag.Int("socket-uid", -1, "Socket owner UID (-1 to leave unchanged)")
		socketGID      = flag.Int("socket-gid", -1, "Socket owner GID (-1 to leave unchanged)")
		hostRoot       = flag.String("host-root", "/", "Host root filesystem path")
		snapshotRoot   = flag.String("snapshot-root", "/var/lib/home-datacenter/snapshots", "Snapshot storage root directory")
		keyID          = flag.String("key-id", "bridge-default-key", "HMAC Key ID")
		hmacKeyHex     = flag.String("hmac-key-hex", "", "Hex-encoded Bridge HMAC Key (min 32 bytes)")
		grantKeyID     = flag.String("grant-key-id", "", "Grant Authority Key ID")
		grantKeyHex    = flag.String("grant-hmac-key-hex", "", "Hex-encoded Grant HMAC Key (independent from bridge key)")
		grantPubKeyHex = flag.String("grant-public-key-hex", "", "Hex-encoded Ed25519 Public Key for grant verification (asymmetric mode)")
		requireGrant   = flag.Bool("require-grant", true, "Require cryptographically signed HostExecutionGrant for mutating operations (fail-closed)")
		requireFence   = flag.Bool("require-fencing", true, "Require strictly monotonic fencing generation")
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
	if *grantPubKeyHex == "" {
		*grantPubKeyHex = strings.TrimSpace(os.Getenv("OPS_HOST_GRANT_PUBLIC_KEY"))
	}
	if *grantKeyID == "" {
		*grantKeyID = strings.TrimSpace(os.Getenv("OPS_HOST_GRANT_KEY_ID"))
	}
	if envRG := strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_REQUIRE_GRANT")); envRG == "0" || strings.EqualFold(envRG, "false") {
		*requireGrant = false
	}
	if *socketUID < 0 {
		if uStr := strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_SOCKET_UID")); uStr != "" {
			if u, err := strconv.Atoi(uStr); err == nil {
				*socketUID = u
			}
		}
	}
	if *socketGID < 0 {
		if gStr := strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_SOCKET_GID")); gStr != "" {
			if g, err := strconv.Atoi(gStr); err == nil {
				*socketGID = g
			}
		}
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

	var grantPubKey ed25519.PublicKey
	if *grantPubKeyHex != "" {
		pkBytes, pkErr := hex.DecodeString(*grantPubKeyHex)
		if pkErr != nil || len(pkBytes) != ed25519.PublicKeySize {
			log.Fatalf("invalid hex for grant public key (must be %d bytes): %v", ed25519.PublicKeySize, pkErr)
		}
		grantPubKey = ed25519.PublicKey(pkBytes)
	}

	if *requireGrant && len(grantHMACKey) == 0 && len(grantPubKey) == 0 {
		log.Fatalf("FAIL-CLOSED: require-grant is enabled by default, but no independent grant verification key was provided (-grant-hmac-key-hex or -grant-public-key-hex)")
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
		GrantPublicKey: grantPubKey,
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
		sockPathClean := filepath.Clean(*socketPath)
		sockDir := filepath.Dir(sockPathClean)
		if err := os.MkdirAll(sockDir, 0750); err != nil {
			log.Fatalf("create socket directory %q: %v", sockDir, err)
		}
		// Security: verify sockDir is not a symlink
		if dirInfo, dErr := os.Lstat(sockDir); dErr == nil {
			if dirInfo.Mode()&os.ModeSymlink != 0 {
				log.Fatalf("SECURITY VIOLATION: socket directory %q is a symlink", sockDir)
			}
		}

		// Security: inspect existing file at socketPath with Lstat
		if fi, sErr := os.Lstat(sockPathClean); sErr == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				log.Fatalf("SECURITY VIOLATION: socket path %q is a symlink, refusing to remove or bind", sockPathClean)
			}
			if fi.Mode().Type()&os.ModeSocket == 0 {
				log.Fatalf("SECURITY VIOLATION: socket path %q already exists and is not a socket, refusing to remove or bind", sockPathClean)
			}
			if err := os.Remove(sockPathClean); err != nil {
				log.Fatalf("failed to remove stale socket %q: %v", sockPathClean, err)
			}
		}

		restoreUmask := setSocketUmask()
		listener, err = net.Listen("unix", sockPathClean)
		restoreUmask()
		if err != nil {
			log.Fatalf("listen unix %q: %v", sockPathClean, err)
		}
		if err := os.Chmod(sockPathClean, 0660); err != nil {
			log.Fatalf("chmod 0660 %q: %v", sockPathClean, err)
		}
		if *socketUID >= 0 || *socketGID >= 0 {
			uid := *socketUID
			gid := *socketGID
			if uid < 0 {
				uid = os.Getuid()
			}
			if gid < 0 {
				gid = os.Getgid()
			}
			if err := os.Chown(sockPathClean, uid, gid); err != nil {
				log.Printf("warning: chown %d:%d %q: %v", uid, gid, sockPathClean, err)
			}
		}
		defer os.Remove(sockPathClean)
		log.Printf("Home Datacenter Host Helper listening on unix:%s (mode 0660)", sockPathClean)
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
