package hostsec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HostActionType enumerates the strictly typed host operations permitted across
// RestrictedHostExecutionBridge. Arbitrary shell, binary names, and arbitrary file writes are forbidden.
type HostActionType string

const (
	HostActionAptCachePolicy HostActionType = "apt.cache_policy"
	HostActionAptUpgrade     HostActionType = "apt.upgrade"
	HostActionAptDowngrade   HostActionType = "apt.downgrade"
	HostActionDpkgQuery      HostActionType = "dpkg.query"
	HostActionSSHDTest       HostActionType = "ssh.config_test"
	HostActionSSHDRestart    HostActionType = "ssh.service_restart"
	HostActionRestoreConfig  HostActionType = "ssh.restore_config"
	HostActionWatchdogArm    HostActionType = "ssh.watchdog_arm"
	HostActionWatchdogDisarm HostActionType = "ssh.watchdog_disarm"
)

const (
	bridgeActionHTTPPath       = "/v1/host/actions"
	bridgeHeaderKeyID          = "X-Bridge-Key-Id"
	bridgeHeaderTimestamp      = "X-Bridge-Timestamp"
	bridgeHeaderNonce          = "X-Bridge-Nonce"
	bridgeHeaderSignature      = "X-Bridge-Signature"
	minBridgeHMACKeyBytes      = 32
	defaultBridgeReplayWindow  = 60 * time.Second
	maxBridgeRequestBodyBytes  = 64 * 1024
	maxBridgeNonceCacheEntries = 4096
	defaultBridgeResourceScope = "host-os"
	maxBridgeWatchdogTimeoutMs = 15 * 60 * 1000
)

var (
	ErrBridgeAuthenticationFailed = errors.New("host execution bridge authentication failed")
	ErrBridgeActionRejected       = errors.New("host execution bridge rejected untyped or disallowed action")
	ErrBridgeBindingInvalid       = errors.New("host execution bridge binding validation failed")

	reSafeSnapshotID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
)

// HostExecutionBinding cryptographically binds every host action invocation to an
// approved proposal, approval digest/ID, operation ID, baseline hash, and fencing generation.
type HostExecutionBinding struct {
	ProposalID        string `json:"proposal_id"`
	ApprovalID        string `json:"approval_id"`
	OperationID       string `json:"operation_id"`
	ResourceScope     string `json:"resource_scope,omitempty"`
	BaselineHash      string `json:"baseline_hash"`
	FencingGeneration int64  `json:"fencing_generation"`
	HouseholdID       uint   `json:"household_id,omitempty"`
	ActorID           string `json:"actor_id,omitempty"`
}

// ResolveResourceScope returns the normalized fencing resource scope (default "host-os").
func (b HostExecutionBinding) ResolveResourceScope() string {
	s := strings.TrimSpace(b.ResourceScope)
	if s == "" {
		return defaultBridgeResourceScope
	}
	return s
}

// Validate checks that required binding fields are present and non-spoofed.
func (b HostExecutionBinding) Validate(requireFencing bool) error {
	if strings.TrimSpace(b.ProposalID) == "" {
		return fmt.Errorf("%w: proposal_id is required", ErrBridgeBindingInvalid)
	}
	if strings.TrimSpace(b.ApprovalID) == "" {
		return fmt.Errorf("%w: approval_id is required", ErrBridgeBindingInvalid)
	}
	if strings.TrimSpace(b.OperationID) == "" {
		return fmt.Errorf("%w: operation_id is required", ErrBridgeBindingInvalid)
	}
	base := strings.TrimSpace(b.BaselineHash)
	if base == "" || !strings.HasPrefix(base, "host-v1:") {
		return fmt.Errorf("%w: valid host-v1 baseline_hash is required (got %q)", ErrBridgeBindingInvalid, b.BaselineHash)
	}
	if b.FencingGeneration < 0 || (requireFencing && b.FencingGeneration <= 0) {
		return fmt.Errorf("%w: valid fencing_generation (> 0 when fenced) is required (got %d)", ErrBridgeBindingInvalid, b.FencingGeneration)
	}
	return nil
}

// HostActionRequest is the strictly typed payload sent over RestrictedHostExecutionBridge.
type HostActionRequest struct {
	Action            HostActionType       `json:"action"`
	Binding           HostExecutionBinding `json:"binding"`
	Grant             *HostExecutionGrant  `json:"grant,omitempty"`
	Packages          []string             `json:"packages,omitempty"`
	TargetVersions    map[string]string    `json:"target_versions,omitempty"`
	SnapshotID        string               `json:"snapshot_id,omitempty"`
	GuardID           string               `json:"guard_id,omitempty"`
	WatchdogTimeoutMs int64                `json:"watchdog_timeout_ms,omitempty"`
}

// HostActionResult captures structured output from a bridge-mediated typed host action.
type HostActionResult struct {
	Success   bool   `json:"success"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	ExitError string `json:"exit_error,omitempty"`
}

// HostBridgeInfo describes the active host execution bridge posture.
type HostBridgeInfo struct {
	Type          string `json:"type"`
	Endpoint      string `json:"endpoint,omitempty"`
	Authenticated bool   `json:"authenticated"`
	KeyID         string `json:"key_id,omitempty"`
	Fenced        bool   `json:"fenced"`
	RequireGrant  bool   `json:"require_grant,omitempty"`
}

// RestrictedHostExecutionBridge enforces a strict, authenticated, typed RPC boundary
// between a containerized API process and host-namespace privileged execution.
type RestrictedHostExecutionBridge interface {
	BridgeInfo() HostBridgeInfo
	VerifyBinding(ctx context.Context, binding HostExecutionBinding) error
	ExecuteTypedAction(ctx context.Context, req HostActionRequest) (HostActionResult, error)
}

// IsAllowedHostAction returns true iff action is in the closed typed enum.
func IsAllowedHostAction(action HostActionType) bool {
	switch action {
	case HostActionAptCachePolicy,
		HostActionAptUpgrade,
		HostActionAptDowngrade,
		HostActionDpkgQuery,
		HostActionSSHDTest,
		HostActionSSHDRestart,
		HostActionRestoreConfig,
		HostActionWatchdogArm,
		HostActionWatchdogDisarm:
		return true
	default:
		return false
	}
}

// ValidateHostActionRequest strictly validates action enum, binding, whitelisted Debian
// packages, pinned version syntax, and snapshot restore bounds.
func ValidateHostActionRequest(req HostActionRequest, allowedPkgs map[string]bool, requireFencing bool) error {
	if !IsAllowedHostAction(req.Action) {
		return fmt.Errorf("%w: %q", ErrBridgeActionRejected, req.Action)
	}
	if err := req.Binding.Validate(requireFencing); err != nil {
		return err
	}
	if len(allowedPkgs) == 0 {
		allowedPkgs = AllowedHostPackages
	}

	switch req.Action {
	case HostActionAptCachePolicy, HostActionAptUpgrade, HostActionAptDowngrade, HostActionDpkgQuery:
		if len(req.Packages) == 0 {
			return fmt.Errorf("%w: packages list required for action %s", ErrInvalidPackageName, req.Action)
		}
		for _, rawPkg := range req.Packages {
			pkg := strings.TrimSpace(rawPkg)
			if err := ValidatePackageName(pkg); err != nil {
				return err
			}
			if !allowedPkgs[pkg] {
				return fmt.Errorf("%w: %s", ErrPackageNotWhitelisted, pkg)
			}
			if ver, ok := req.TargetVersions[pkg]; ok {
				if err := ValidatePackageVersion(ver); err != nil {
					return err
				}
			}
		}
		for pkg, ver := range req.TargetVersions {
			if err := ValidatePackageName(pkg); err != nil {
				return err
			}
			if !allowedPkgs[pkg] {
				return fmt.Errorf("%w: %s", ErrPackageNotWhitelisted, pkg)
			}
			if err := ValidatePackageVersion(ver); err != nil {
				return err
			}
		}
		if req.Action == HostActionAptDowngrade {
			for _, rawPkg := range req.Packages {
				if strings.TrimSpace(req.TargetVersions[strings.TrimSpace(rawPkg)]) == "" {
					return fmt.Errorf("%w: downgrade requires pinned target version for %s", ErrInvalidPackageVersion, rawPkg)
				}
			}
		}
	case HostActionRestoreConfig:
		if !reSafeSnapshotID.MatchString(strings.TrimSpace(req.SnapshotID)) {
			return fmt.Errorf("%w: invalid snapshot_id %q for ssh.restore_config", ErrBridgeActionRejected, req.SnapshotID)
		}
	case HostActionWatchdogArm:
		if !reSafeSnapshotID.MatchString(strings.TrimSpace(req.GuardID)) {
			return fmt.Errorf("%w: invalid guard_id %q for ssh.watchdog_arm", ErrBridgeActionRejected, req.GuardID)
		}
		if !reSafeSnapshotID.MatchString(strings.TrimSpace(req.SnapshotID)) {
			return fmt.Errorf("%w: invalid snapshot_id %q for ssh.watchdog_arm", ErrBridgeActionRejected, req.SnapshotID)
		}
		if req.WatchdogTimeoutMs <= 0 || req.WatchdogTimeoutMs > maxBridgeWatchdogTimeoutMs {
			return fmt.Errorf("%w: invalid watchdog_timeout_ms %d for ssh.watchdog_arm", ErrBridgeActionRejected, req.WatchdogTimeoutMs)
		}
		if len(req.Packages) > 0 || len(req.TargetVersions) > 0 {
			return fmt.Errorf("%w: unexpected package parameters for %s", ErrBridgeActionRejected, req.Action)
		}
	case HostActionWatchdogDisarm:
		if !reSafeSnapshotID.MatchString(strings.TrimSpace(req.GuardID)) {
			return fmt.Errorf("%w: invalid guard_id %q for ssh.watchdog_disarm", ErrBridgeActionRejected, req.GuardID)
		}
		if len(req.Packages) > 0 || len(req.TargetVersions) > 0 || strings.TrimSpace(req.SnapshotID) != "" {
			return fmt.Errorf("%w: unexpected parameters for %s", ErrBridgeActionRejected, req.Action)
		}
	case HostActionSSHDTest, HostActionSSHDRestart:
		if len(req.Packages) > 0 || len(req.TargetVersions) > 0 || strings.TrimSpace(req.SnapshotID) != "" || strings.TrimSpace(req.GuardID) != "" {
			return fmt.Errorf("%w: unexpected parameters for %s", ErrBridgeActionRejected, req.Action)
		}
	}
	return nil
}

// HostBridgeConfig configures the HMAC-authenticated Unix socket or loopback/TLS bridge client.
type HostBridgeConfig struct {
	Endpoint       string // "unix:///run/home-datacenter/host-bridge.sock", "http://127.0.0.1:...", or "https://..."
	KeyID          string
	HMACKey        []byte
	RequireFencing bool
	RequireGrant   bool
	Timeout        time.Duration
	AllowedPkgs    map[string]bool
	HTTPClient     *http.Client // optional override for tests
}

// HMACHostExecutionBridge is an authenticated client that sends signed HostActionRequest payloads
// over a Unix domain socket or authenticated loopback/TLS HTTP endpoint.
type HMACHostExecutionBridge struct {
	cfg        HostBridgeConfig
	client     *http.Client
	requestURL string
}

// NewHMACHostExecutionBridge validates bridge transport and HMAC credentials (fail-closed).
func NewHMACHostExecutionBridge(cfg HostBridgeConfig) (*HMACHostExecutionBridge, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("%w: bridge endpoint is empty", ErrContainerHostExecutionBoundary)
	}
	if len(cfg.HMACKey) < minBridgeHMACKeyBytes {
		return nil, fmt.Errorf("%w: bridge HMAC key must be at least %d bytes", ErrBridgeAuthenticationFailed, minBridgeHMACKeyBytes)
	}
	keyID := strings.TrimSpace(cfg.KeyID)
	if keyID == "" {
		keyID = "bridge-k1"
	}
	cfg.KeyID = keyID
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultUpdateTimeout
	}
	if len(cfg.AllowedPkgs) == 0 {
		cfg.AllowedPkgs = AllowedHostPackages
	}

	var client *http.Client
	var reqURL string
	if strings.HasPrefix(endpoint, "unix://") {
		sockPath := strings.TrimPrefix(endpoint, "unix://")
		if strings.TrimSpace(sockPath) == "" {
			return nil, fmt.Errorf("%w: empty unix socket path", ErrContainerHostExecutionBoundary)
		}
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", sockPath)
			},
		}
		client = &http.Client{Transport: tr, Timeout: cfg.Timeout}
		reqURL = "http://localhost" + bridgeActionHTTPPath
	} else {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("%w: invalid bridge endpoint %q", ErrContainerHostExecutionBoundary, endpoint)
		}
		host := u.Hostname()
		isLoopback := host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
		if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback) {
			return nil, fmt.Errorf("%w: non-TLS non-loopback bridge endpoint %q is forbidden", ErrBridgeAuthenticationFailed, endpoint)
		}
		client = cfg.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: cfg.Timeout}
		}
		reqURL = strings.TrimRight(endpoint, "/") + bridgeActionHTTPPath
	}

	return &HMACHostExecutionBridge{
		cfg:        cfg,
		client:     client,
		requestURL: reqURL,
	}, nil
}

func isRequireGrantDefaultTrue() bool {
	env := strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_REQUIRE_GRANT"))
	return env != "0" && !strings.EqualFold(env, "false")
}

// NewHMACHostExecutionBridgeFromEnv constructs an authenticated bridge if configured in env,
// otherwise returns nil so containerized mutations fail closed with ErrContainerHostExecutionBoundary.
func NewHMACHostExecutionBridgeFromEnv() *HMACHostExecutionBridge {
	endpoint := strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_ENDPOINT"))
	if endpoint == "" {
		if sock := strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_SOCKET")); sock != "" {
			if strings.HasPrefix(sock, "unix://") {
				endpoint = sock
			} else {
				endpoint = "unix://" + sock
			}
		} else if u := strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_URL")); u != "" {
			endpoint = u
		}
	}
	if endpoint == "" {
		return nil
	}
	key := []byte(strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_HMAC_KEY")))
	if len(key) < minBridgeHMACKeyBytes {
		return nil
	}
	keyID := strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_KEY_ID"))
	b, err := NewHMACHostExecutionBridge(HostBridgeConfig{
		Endpoint:       endpoint,
		KeyID:          keyID,
		HMACKey:        key,
		RequireFencing: strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_REQUIRE_FENCING")) == "1",
		RequireGrant:   isRequireGrantDefaultTrue(),
	})
	if err != nil {
		return nil
	}
	return b
}

func (b *HMACHostExecutionBridge) BridgeInfo() HostBridgeInfo {
	return HostBridgeInfo{
		Type:          "hmac-restricted-bridge",
		Endpoint:      b.cfg.Endpoint,
		Authenticated: len(b.cfg.HMACKey) >= minBridgeHMACKeyBytes,
		KeyID:         b.cfg.KeyID,
		Fenced:        b.cfg.RequireFencing,
		RequireGrant:  b.cfg.RequireGrant,
	}
}

func (b *HMACHostExecutionBridge) VerifyBinding(_ context.Context, binding HostExecutionBinding) error {
	if len(b.cfg.HMACKey) < minBridgeHMACKeyBytes {
		return fmt.Errorf("%w: missing or weak bridge HMAC key", ErrBridgeAuthenticationFailed)
	}
	return binding.Validate(b.cfg.RequireFencing)
}

func (b *HMACHostExecutionBridge) ExecuteTypedAction(ctx context.Context, req HostActionRequest) (HostActionResult, error) {
	if len(b.cfg.HMACKey) < minBridgeHMACKeyBytes {
		return HostActionResult{}, fmt.Errorf("%w: missing bridge HMAC key", ErrBridgeAuthenticationFailed)
	}
	if err := ValidateHostActionRequest(req, b.cfg.AllowedPkgs, b.cfg.RequireFencing); err != nil {
		return HostActionResult{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return HostActionResult{}, err
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return HostActionResult{}, err
	}
	ts := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	nonce := hex.EncodeToString(nonceBytes)
	sig := computeBridgeRequestSignature(b.cfg.HMACKey, http.MethodPost, bridgeActionHTTPPath, ts, nonce, body)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.requestURL, bytes.NewReader(body))
	if err != nil {
		return HostActionResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(bridgeHeaderKeyID, b.cfg.KeyID)
	httpReq.Header.Set(bridgeHeaderTimestamp, ts)
	httpReq.Header.Set(bridgeHeaderNonce, nonce)
	httpReq.Header.Set(bridgeHeaderSignature, "hmac-sha256="+sig)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return HostActionResult{}, fmt.Errorf("%w: %v", ErrContainerHostExecutionBoundary, err)
	}
	defer resp.Body.Close()

	respBytes, rErr := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if rErr != nil {
		return HostActionResult{}, rErr
	}

	var out struct {
		HostActionResult
		Error string `json:"error,omitempty"`
	}
	if decErr := json.Unmarshal(respBytes, &out); decErr != nil {
		return HostActionResult{}, fmt.Errorf("bridge response decode error (status=%d): %w", resp.StatusCode, decErr)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return out.HostActionResult, fmt.Errorf("%w: %s", ErrBridgeAuthenticationFailed, out.Error)
	}
	if resp.StatusCode >= 400 {
		msg := out.Error
		if msg == "" {
			msg = out.ExitError
		}
		if msg == "" {
			msg = fmt.Sprintf("bridge HTTP %d", resp.StatusCode)
		}
		return out.HostActionResult, errors.New(msg)
	}
	if out.ExitError != "" {
		return out.HostActionResult, errors.New(out.ExitError)
	}
	return out.HostActionResult, nil
}

func computeBridgeRequestSignature(key []byte, method, path, ts, nonce string, body []byte) string {
	bodySum := sha256.Sum256(body)
	canonical := method + "\n" + path + "\n" + ts + "\n" + nonce + "\n" + hex.EncodeToString(bodySum[:])
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// HostBridgeServerConfig configures the host-side restricted execution bridge handler.
type HostBridgeServerConfig struct {
	HostRoot        string
	SnapshotRoot    string
	NonceDir        string
	FencingDir      string
	GrantDir        string
	WatchdogDir     string
	WatchdogBackend ExternalWatchdogBackend
	KeyID           string
	HMACKey         []byte
	RequireFencing  bool
	RequireGrant    bool
	GrantKeyID      string
	GrantHMACKey    []byte
	GrantPublicKey  ed25519.PublicKey
	Runner          HostExecRunner
	AllowedPkgs     map[string]bool
	ReplayWindow    time.Duration
}

type bridgeFencingWatermark struct {
	ResourceScope  string `json:"resource_scope"`
	LastGeneration int64  `json:"last_generation"`
	ProposalID     string `json:"proposal_id"`
	OperationID    string `json:"operation_id"`
	UpdatedAtMs    int64  `json:"updated_at_ms"`
}

func parseBridgeFencingWatermark(data []byte) (bridgeFencingWatermark, error) {
	var wm bridgeFencingWatermark
	if err := json.Unmarshal(data, &wm); err != nil {
		return bridgeFencingWatermark{}, fmt.Errorf("corrupted fencing watermark JSON: %w", err)
	}
	if wm.LastGeneration <= 0 || strings.TrimSpace(wm.ResourceScope) == "" ||
		strings.TrimSpace(wm.ProposalID) == "" || strings.TrimSpace(wm.OperationID) == "" {
		return bridgeFencingWatermark{}, errors.New("corrupted fencing watermark: incomplete or non-positive generation")
	}
	return wm, nil
}

func initBridgeNonceDir(nonceDir string, replayWindow time.Duration) (map[string]int64, error) {
	seenNonces := make(map[string]int64)
	if nonceDir == "" {
		return seenNonces, nil
	}
	if err := os.MkdirAll(nonceDir, 0700); err != nil {
		return nil, fmt.Errorf("create bridge nonce dir %q: %w", nonceDir, err)
	}
	unlock, err := acquireCrossProcessFileLock(filepath.Join(nonceDir, ".nonce.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock bridge nonce dir %q: %w", nonceDir, err)
	}
	var unlocked bool
	defer func() {
		if !unlocked {
			_ = unlock()
		}
	}()

	entries, err := os.ReadDir(nonceDir)
	if err != nil {
		return nil, fmt.Errorf("read bridge nonce dir %q: %w", nonceDir, err)
	}
	nowMs := time.Now().UTC().UnixMilli()
	skewMs := replayWindow.Milliseconds()
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".ts") {
			continue
		}
		p := filepath.Join(nonceDir, ent.Name())
		data, rErr := os.ReadFile(p)
		if rErr != nil {
			return nil, fmt.Errorf("read bridge nonce file %q: %w", p, rErr)
		}
		tsMs, pErr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if pErr != nil {
			return nil, fmt.Errorf("corrupted bridge nonce file %q: %w", p, pErr)
		}
		if tsMs < nowMs-skewMs {
			if remErr := removeFileFailClosed(p); remErr != nil {
				return nil, fmt.Errorf("prune expired bridge nonce file %q: %w", p, remErr)
			}
			continue
		}
		nonceKey := strings.TrimSuffix(ent.Name(), ".ts")
		seenNonces[nonceKey] = tsMs
	}
	unlocked = true
	if uErr := unlock(); uErr != nil {
		return nil, fmt.Errorf("unlock bridge nonce dir %q: %w", nonceDir, uErr)
	}
	return seenNonces, nil
}

func initBridgeFencingDir(fencingDir string) error {
	if fencingDir == "" {
		return nil
	}
	if err := os.MkdirAll(fencingDir, 0700); err != nil {
		return fmt.Errorf("create bridge fencing dir %q: %w", fencingDir, err)
	}
	unlock, err := acquireCrossProcessFileLock(filepath.Join(fencingDir, ".fencing.lock"))
	if err != nil {
		return fmt.Errorf("lock bridge fencing dir %q: %w", fencingDir, err)
	}
	var unlocked bool
	defer func() {
		if !unlocked {
			_ = unlock()
		}
	}()

	entries, err := os.ReadDir(fencingDir)
	if err != nil {
		return fmt.Errorf("read bridge fencing dir %q: %w", fencingDir, err)
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".gen") {
			continue
		}
		p := filepath.Join(fencingDir, ent.Name())
		data, rErr := os.ReadFile(p)
		if rErr != nil {
			return fmt.Errorf("read bridge fencing file %q: %w", p, rErr)
		}
		if _, pErr := parseBridgeFencingWatermark(data); pErr != nil {
			return fmt.Errorf("corrupted bridge fencing file %q: %w", p, pErr)
		}
	}
	unlocked = true
	if uErr := unlock(); uErr != nil {
		return fmt.Errorf("unlock bridge fencing dir %q: %w", fencingDir, uErr)
	}
	return nil
}

func verifyAndPersistBridgeNonce(
	nonceMu *sync.Mutex,
	seenNonces map[string]int64,
	nonceDir string,
	reqNonce string,
	tsMs int64,
	nowMs int64,
	skewMs int64,
) (httpCode int, err error) {
	nonceMu.Lock()
	defer nonceMu.Unlock()

	if nonceDir != "" {
		unlock, lErr := acquireCrossProcessFileLock(filepath.Join(nonceDir, ".nonce.lock"))
		if lErr != nil {
			return http.StatusInternalServerError, fmt.Errorf("bridge nonce lock failed: %w", lErr)
		}
		defer func() {
			if unlock != nil {
				_ = unlock()
			}
		}()
		for k, exp := range seenNonces {
			if exp < nowMs-skewMs {
				if remErr := removeFileFailClosed(filepath.Join(nonceDir, k+".ts")); remErr != nil {
					return http.StatusInternalServerError, fmt.Errorf("bridge nonce cleanup failed: %w", remErr)
				}
				delete(seenNonces, k)
			}
		}
		nonceSum := sha256.Sum256([]byte(reqNonce))
		nonceKey := hex.EncodeToString(nonceSum[:])
		if _, dup := seenNonces[nonceKey]; dup {
			return http.StatusUnauthorized, errors.New("bridge nonce replayed")
		}
		noncePath := filepath.Join(nonceDir, nonceKey+".ts")
		if info, statErr := os.Stat(noncePath); statErr == nil {
			if info.IsDir() {
				return http.StatusInternalServerError, fmt.Errorf("corrupted nonce path %q is a directory", noncePath)
			}
			return http.StatusUnauthorized, errors.New("bridge nonce replayed")
		} else if !os.IsNotExist(statErr) {
			return http.StatusInternalServerError, fmt.Errorf("bridge nonce stat failed: %w", statErr)
		}
		if len(seenNonces) >= maxBridgeNonceCacheEntries {
			return http.StatusUnauthorized, errors.New("bridge nonce cache saturated")
		}
		if wErr := writeFileAtomicFailClosed(noncePath, []byte(strconv.FormatInt(tsMs, 10)), 0600); wErr != nil {
			return http.StatusInternalServerError, fmt.Errorf("bridge nonce persistence failed: %w", wErr)
		}
		seenNonces[nonceKey] = tsMs
		uErr := unlock()
		unlock = nil
		if uErr != nil {
			return http.StatusInternalServerError, fmt.Errorf("bridge nonce unlock failed: %w", uErr)
		}
		return 0, nil
	}

	for k, exp := range seenNonces {
		if exp < nowMs-skewMs {
			delete(seenNonces, k)
		}
	}
	nonceSum := sha256.Sum256([]byte(reqNonce))
	nonceKey := hex.EncodeToString(nonceSum[:])
	if _, dup := seenNonces[nonceKey]; dup || len(seenNonces) >= maxBridgeNonceCacheEntries {
		return http.StatusUnauthorized, errors.New("bridge nonce replayed or cache saturated")
	}
	seenNonces[nonceKey] = tsMs
	return 0, nil
}

func verifyAndAdvanceBridgeFencing(
	fencingMu *sync.Mutex,
	memWatermarks map[string]bridgeFencingWatermark,
	fencingDir string,
	binding HostExecutionBinding,
) (httpCode int, err error) {
	fencingMu.Lock()
	defer fencingMu.Unlock()

	scope := binding.ResolveResourceScope()
	scopeSum := sha256.Sum256([]byte(scope))
	scopeKey := hex.EncodeToString(scopeSum[:])

	var unlock func() error
	if fencingDir != "" {
		var lErr error
		unlock, lErr = acquireCrossProcessFileLock(filepath.Join(fencingDir, ".fencing.lock"))
		if lErr != nil {
			return http.StatusInternalServerError, fmt.Errorf("bridge fencing lock failed: %w", lErr)
		}
		defer func() {
			if unlock != nil {
				_ = unlock()
			}
		}()
	}

	var prev bridgeFencingWatermark
	var hasPrev bool
	if fencingDir != "" {
		genPath := filepath.Join(fencingDir, scopeKey+".gen")
		data, rErr := os.ReadFile(genPath)
		if rErr == nil {
			wm, pErr := parseBridgeFencingWatermark(data)
			if pErr != nil {
				return http.StatusInternalServerError, fmt.Errorf("corrupted fencing watermark for scope %s: %w", scope, pErr)
			}
			prev = wm
			hasPrev = true
		} else if !os.IsNotExist(rErr) {
			return http.StatusInternalServerError, fmt.Errorf("read fencing watermark for scope %s: %w", scope, rErr)
		}
	} else {
		prev, hasPrev = memWatermarks[scopeKey]
	}

	if hasPrev {
		if binding.FencingGeneration < prev.LastGeneration {
			return http.StatusBadRequest, fmt.Errorf(
				"stale or non-monotonic fencing generation %d < %d for scope %s (proposal %s)",
				binding.FencingGeneration, prev.LastGeneration, scope, binding.ProposalID,
			)
		}
		if binding.FencingGeneration == prev.LastGeneration &&
			(binding.ProposalID != prev.ProposalID || binding.OperationID != prev.OperationID) {
			return http.StatusBadRequest, fmt.Errorf(
				"stale or non-monotonic fencing generation %d == %d already claimed for scope %s by proposal %s operation %s",
				binding.FencingGeneration, prev.LastGeneration, scope, prev.ProposalID, prev.OperationID,
			)
		}
	}

	next := bridgeFencingWatermark{
		ResourceScope:  scope,
		LastGeneration: binding.FencingGeneration,
		ProposalID:     strings.TrimSpace(binding.ProposalID),
		OperationID:    strings.TrimSpace(binding.OperationID),
		UpdatedAtMs:    time.Now().UTC().UnixMilli(),
	}
	if fencingDir != "" {
		encoded, mErr := json.Marshal(next)
		if mErr != nil {
			return http.StatusInternalServerError, mErr
		}
		genPath := filepath.Join(fencingDir, scopeKey+".gen")
		if wErr := writeFileAtomicFailClosed(genPath, encoded, 0600); wErr != nil {
			return http.StatusInternalServerError, fmt.Errorf("persist fencing watermark for scope %s: %w", scope, wErr)
		}
		uErr := unlock()
		unlock = nil
		if uErr != nil {
			return http.StatusInternalServerError, fmt.Errorf("bridge fencing unlock failed: %w", uErr)
		}
	}
	memWatermarks[scopeKey] = next
	return 0, nil
}

func isGrantAuthRejection(err error) bool {
	return errors.Is(err, ErrExecutionGrantRequired) ||
		errors.Is(err, ErrExecutionGrantInvalid) ||
		errors.Is(err, ErrExecutionGrantExpired) ||
		errors.Is(err, ErrExecutionGrantRevoked) ||
		errors.Is(err, ErrExecutionGrantConsumed) ||
		errors.Is(err, ErrExecutionGrantMismatch) ||
		errors.Is(err, ErrExecutionGrantKeyConflict)
}

func handleBridgeWatchdogAction(ctx context.Context, cfg HostBridgeServerConfig, req HostActionRequest) (HostActionResult, error) {
	if strings.TrimSpace(cfg.SnapshotRoot) == "" {
		return HostActionResult{}, errors.New("snapshotRoot is required for host bridge watchdog")
	}
	watchDir := strings.TrimSpace(cfg.WatchdogDir)
	if watchDir == "" {
		watchDir = filepath.Join(filepath.Clean(cfg.SnapshotRoot), "ssh-watchdog")
	}
	guard := NewPersistentHostSSHGuard(watchDir, cfg.HostRoot, cfg.SnapshotRoot, cfg.Runner, nil)
	guard.DisableLocalTimer = true
	if cfg.WatchdogBackend != nil {
		guard.ExternalWatchdog = cfg.WatchdogBackend
	} else {
		guard.ExternalWatchdog = NewDefaultHostWatchdogBackend(watchDir)
	}

	switch req.Action {
	case HostActionWatchdogArm:
		snapDir := filepath.Join(filepath.Clean(cfg.SnapshotRoot), strings.TrimSpace(req.SnapshotID))
		if _, err := os.Stat(filepath.Join(snapDir, "manifest.json")); err != nil {
			return HostActionResult{}, fmt.Errorf("host watchdog snapshot %s manifest missing: %w", req.SnapshotID, err)
		}
		timeout := time.Duration(req.WatchdogTimeoutMs) * time.Millisecond
		gID, err := guard.armWithGuardID(ctx, strings.TrimSpace(req.GuardID), time.Now().UTC(), snapDir, timeout, req.Binding, nil, nil)
		if err != nil {
			return HostActionResult{}, err
		}
		return HostActionResult{Stdout: "armed=" + gID}, nil
	case HostActionWatchdogDisarm:
		if err := guard.Disarm(strings.TrimSpace(req.GuardID)); err != nil {
			return HostActionResult{}, err
		}
		return HostActionResult{Stdout: "disarmed=" + strings.TrimSpace(req.GuardID)}, nil
	default:
		return HostActionResult{}, fmt.Errorf("%w: unsupported watchdog action %s", ErrBridgeActionRejected, req.Action)
	}
}

// NewHostBridgeHTTPHandler creates an authenticated, fail-closed host bridge server handler
// that verifies HMAC signatures, anti-replay nonces, monotonic resource-scoped fencing,
// independent HostExecutionGrant capability, platform policy (fnOS fail-closed),
// closed package whitelist, and binding fields, and translates ONLY typed actions to argv.
func NewHostBridgeHTTPHandler(cfg HostBridgeServerConfig) (http.Handler, error) {
	if len(cfg.HMACKey) < minBridgeHMACKeyBytes {
		return nil, fmt.Errorf("%w: server HMAC key must be at least %d bytes", ErrBridgeAuthenticationFailed, minBridgeHMACKeyBytes)
	}
	if strings.TrimSpace(cfg.KeyID) == "" {
		cfg.KeyID = "bridge-k1"
	}
	if cfg.Runner == nil {
		cfg.Runner = realHostExecRunner{}
	}
	if len(cfg.AllowedPkgs) == 0 {
		cfg.AllowedPkgs = AllowedHostPackages
	}
	if cfg.ReplayWindow <= 0 {
		cfg.ReplayWindow = defaultBridgeReplayWindow
	}
	if cfg.NonceDir == "" && cfg.SnapshotRoot != "" {
		cfg.NonceDir = filepath.Join(cfg.SnapshotRoot, "bridge-nonces")
	}
	if cfg.FencingDir == "" && cfg.SnapshotRoot != "" {
		cfg.FencingDir = filepath.Join(cfg.SnapshotRoot, "bridge-fencing")
	}
	if cfg.GrantDir == "" && cfg.SnapshotRoot != "" {
		cfg.GrantDir = filepath.Join(cfg.SnapshotRoot, "bridge-grants")
	}
	if cfg.WatchdogDir == "" && cfg.SnapshotRoot != "" {
		cfg.WatchdogDir = filepath.Join(cfg.SnapshotRoot, "ssh-watchdog")
	}

	if len(cfg.GrantHMACKey) > 0 && hmac.Equal(cfg.GrantHMACKey, cfg.HMACKey) {
		return nil, ErrExecutionGrantKeyConflict
	}

	var grantAuthority *HostGrantAuthority
	var grantStore *PersistentGrantStore
	if cfg.RequireGrant || len(cfg.GrantHMACKey) > 0 || len(cfg.GrantPublicKey) > 0 {
		var gaErr error
		grantAuthority, gaErr = NewHostGrantAuthority(HostGrantAuthorityConfig{
			KeyID:         cfg.GrantKeyID,
			HMACKey:       cfg.GrantHMACKey,
			PublicKey:     cfg.GrantPublicKey,
			BridgeHMACKey: cfg.HMACKey,
		})
		if gaErr != nil {
			return nil, fmt.Errorf("configure bridge grant authority: %w", gaErr)
		}
		var gsErr error
		grantStore, gsErr = NewPersistentGrantStore(cfg.GrantDir)
		if gsErr != nil {
			return nil, fmt.Errorf("configure bridge persistent grant store: %w", gsErr)
		}
	}

	detector := NewPlatformDetector(cfg.HostRoot)
	var nonceMu sync.Mutex
	seenNonces, nErr := initBridgeNonceDir(cfg.NonceDir, cfg.ReplayWindow)
	if nErr != nil {
		return nil, nErr
	}

	var fencingMu sync.Mutex
	memWatermarks := make(map[string]bridgeFencingWatermark)
	if fErr := initBridgeFencingDir(cfg.FencingDir); fErr != nil {
		return nil, fErr
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		if r.Method != http.MethodPost || r.URL.Path != bridgeActionHTTPPath {
			writeJSON(http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBridgeRequestBodyBytes+1))
		if err != nil {
			writeJSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if len(body) > maxBridgeRequestBodyBytes {
			writeJSON(http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}

		// 1. Verify HMAC key_id, timestamp replay window, HMAC-SHA256 signature, and persistent anti-replay nonce.
		reqKeyID := strings.TrimSpace(r.Header.Get(bridgeHeaderKeyID))
		reqTS := strings.TrimSpace(r.Header.Get(bridgeHeaderTimestamp))
		reqNonce := strings.TrimSpace(r.Header.Get(bridgeHeaderNonce))
		reqSig := strings.TrimPrefix(strings.TrimSpace(r.Header.Get(bridgeHeaderSignature)), "hmac-sha256=")
		if reqKeyID != cfg.KeyID || reqTS == "" || reqNonce == "" || reqSig == "" {
			writeJSON(http.StatusUnauthorized, map[string]string{"error": "missing or mismatched bridge authentication headers"})
			return
		}
		tsMs, tsErr := strconv.ParseInt(reqTS, 10, 64)
		if tsErr != nil {
			writeJSON(http.StatusUnauthorized, map[string]string{"error": "invalid bridge timestamp"})
			return
		}
		nowMs := time.Now().UTC().UnixMilli()
		skewMs := cfg.ReplayWindow.Milliseconds()
		if tsMs < nowMs-skewMs || tsMs > nowMs+skewMs {
			writeJSON(http.StatusUnauthorized, map[string]string{"error": "bridge timestamp outside replay window"})
			return
		}
		expectedSig := computeBridgeRequestSignature(cfg.HMACKey, r.Method, r.URL.Path, reqTS, reqNonce, body)
		gotSigBytes, hexErr := hex.DecodeString(reqSig)
		expSigBytes, _ := hex.DecodeString(expectedSig)
		if hexErr != nil || !hmac.Equal(gotSigBytes, expSigBytes) {
			writeJSON(http.StatusUnauthorized, map[string]string{"error": "invalid bridge HMAC signature"})
			return
		}
		if nCode, nErr := verifyAndPersistBridgeNonce(&nonceMu, seenNonces, cfg.NonceDir, reqNonce, tsMs, nowMs, skewMs); nErr != nil {
			writeJSON(nCode, map[string]string{"error": nErr.Error()})
			return
		}

		// 2. Decode strictly without unknown fields (prevent arbitrary shell/command/args injection)
		var req HostActionRequest
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(http.StatusBadRequest, map[string]string{"error": "invalid bridge payload: " + err.Error()})
			return
		}
		if err := ValidateHostActionRequest(req, cfg.AllowedPkgs, cfg.RequireFencing); err != nil {
			writeJSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		// Pre-verify independent HostExecutionGrant capability BEFORE mutating fencing state
		// so forged/tampered grants cannot poison resource-scoped fencing watermarks.
		hasGrantPolicy := grantAuthority != nil || cfg.RequireGrant || req.Grant != nil
		if hasGrantPolicy {
			if grantAuthority == nil || grantStore == nil {
				writeJSON(http.StatusForbidden, map[string]string{"error": "independent grant verifier not configured on bridge"})
				return
			}
			if gErr := grantStore.CheckGrantUsable(grantAuthority, cfg.HMACKey, req.Grant, req, cfg.RequireFencing, time.Now().UTC()); gErr != nil {
				code := http.StatusInternalServerError
				if isGrantAuthRejection(gErr) {
					code = http.StatusForbidden
				}
				writeJSON(code, map[string]string{"error": gErr.Error()})
				return
			}
		}

		// Enforce resource-scoped monotonic fencing generation across processes/restarts
		if req.Binding.FencingGeneration > 0 || cfg.RequireFencing {
			if fCode, fErr := verifyAndAdvanceBridgeFencing(&fencingMu, memWatermarks, cfg.FencingDir, req.Binding); fErr != nil {
				writeJSON(fCode, map[string]string{"error": fErr.Error()})
				return
			}
		}

		// 3. Independent local host platform policy enforcement (fnOS strictly read-only, no bypass)
		// Enforced BEFORE watchdog arm/disarm and BEFORE typed command dispatch.
		plat := detector.Detect(r.Context())
		if polErr := plat.HostUpdatePolicyError(); polErr != nil {
			writeJSON(http.StatusConflict, map[string]string{"error": polErr.Error()})
			return
		}

		// Persist single-use grant consumption after fencing and platform checks pass
		if hasGrantPolicy {
			if gErr := grantStore.VerifyAndConsume(grantAuthority, cfg.HMACKey, req.Grant, req, cfg.RequireFencing, time.Now().UTC()); gErr != nil {
				code := http.StatusInternalServerError
				if isGrantAuthRejection(gErr) {
					code = http.StatusForbidden
				}
				writeJSON(code, map[string]string{"error": gErr.Error()})
				return
			}
		}

		if req.Action == HostActionWatchdogArm || req.Action == HostActionWatchdogDisarm {
			res, wErr := handleBridgeWatchdogAction(r.Context(), cfg, req)
			if wErr != nil {
				res.Success = false
				res.ExitError = wErr.Error()
				writeJSON(http.StatusInternalServerError, map[string]any{
					"success":    res.Success,
					"exit_error": res.ExitError,
					"error":      wErr.Error(),
				})
				return
			}
			res.Success = true
			writeJSON(http.StatusOK, res)
			return
		}

		// 4. Execute strictly typed action (never arbitrary argv or shell)
		res, execErr := dispatchTypedBridgeAction(r.Context(), cfg.Runner, detector, cfg.SnapshotRoot, req)
		if execErr != nil {
			res.Success = false
			res.ExitError = execErr.Error()
			writeJSON(http.StatusInternalServerError, map[string]any{
				"success":    res.Success,
				"stdout":     res.Stdout,
				"stderr":     res.Stderr,
				"exit_error": res.ExitError,
				"error":      execErr.Error(),
			})
			return
		}
		res.Success = true
		writeJSON(http.StatusOK, res)
	}), nil
}

func dispatchTypedBridgeAction(
	ctx context.Context,
	runner HostExecRunner,
	detector *PlatformDetector,
	snapshotRoot string,
	req HostActionRequest,
) (HostActionResult, error) {
	switch req.Action {
	case HostActionAptCachePolicy:
		if len(req.Packages) != 1 {
			return HostActionResult{}, fmt.Errorf("%w: apt.cache_policy requires exactly 1 package", ErrBridgeActionRejected)
		}
		out, errOut, err := runner.Run(ctx, []string{"LC_ALL=C"}, "apt-cache", "policy", strings.TrimSpace(req.Packages[0]))
		return HostActionResult{Stdout: out, Stderr: errOut}, err

	case HostActionAptUpgrade:
		args := []string{
			"install", "-y", "--only-upgrade",
			"-o", "Acquire::AllowInsecureRepositories=false",
			"-o", "Acquire::AllowDowngradeToInsecureRepositories=false",
			"-o", "Dpkg::Options::=--force-confdef",
			"-o", "Dpkg::Options::=--force-confold",
			"--",
		}
		for _, rawPkg := range req.Packages {
			pkg := strings.TrimSpace(rawPkg)
			if ver := strings.TrimSpace(req.TargetVersions[pkg]); ver != "" {
				args = append(args, pkg+"="+ver)
			} else {
				args = append(args, pkg)
			}
		}
		out, errOut, err := runner.Run(ctx, []string{"DEBIAN_FRONTEND=noninteractive", "LC_ALL=C"}, "apt-get", args...)
		return HostActionResult{Stdout: out, Stderr: errOut}, err

	case HostActionAptDowngrade:
		args := []string{
			"install", "-y", "--allow-downgrades",
			"-o", "Acquire::AllowInsecureRepositories=false",
			"--",
		}
		for _, rawPkg := range req.Packages {
			pkg := strings.TrimSpace(rawPkg)
			ver := strings.TrimSpace(req.TargetVersions[pkg])
			args = append(args, pkg+"="+ver)
		}
		out, errOut, err := runner.Run(ctx, []string{"DEBIAN_FRONTEND=noninteractive", "LC_ALL=C"}, "apt-get", args...)
		return HostActionResult{Stdout: out, Stderr: errOut}, err

	case HostActionDpkgQuery:
		if len(req.Packages) != 1 {
			return HostActionResult{}, fmt.Errorf("%w: dpkg.query requires exactly 1 package", ErrBridgeActionRejected)
		}
		out, errOut, err := runner.Run(ctx, []string{"LC_ALL=C"}, "dpkg-query", "-W", "-f=${Status}\t${Version}", strings.TrimSpace(req.Packages[0]))
		return HostActionResult{Stdout: out, Stderr: errOut}, err

	case HostActionSSHDTest:
		out, errOut, err := runner.Run(ctx, nil, "sshd", "-t")
		return HostActionResult{Stdout: out, Stderr: errOut}, err

	case HostActionSSHDRestart:
		out, errOut, err := runner.Run(ctx, nil, "systemctl", "restart", "ssh")
		if err != nil {
			out2, errOut2, err2 := runner.Run(ctx, nil, "systemctl", "restart", "sshd")
			if err2 == nil {
				return HostActionResult{Stdout: out2, Stderr: errOut2}, nil
			}
			return HostActionResult{Stdout: out + out2, Stderr: errOut + "\n" + errOut2}, fmt.Errorf("%v; %v", err, err2)
		}
		return HostActionResult{Stdout: out, Stderr: errOut}, nil

	case HostActionRestoreConfig:
		return restoreSSHConfigsFromSnapshotID(detector, snapshotRoot, req.SnapshotID)

	default:
		return HostActionResult{}, fmt.Errorf("%w: %s", ErrBridgeActionRejected, req.Action)
	}
}

// isAllowedRestoreRelPath restricts config restoration strictly to SSH configuration files.
func isAllowedRestoreRelPath(rel string) bool {
	slash := filepath.ToSlash(filepath.Clean(strings.TrimSpace(rel)))
	if strings.HasPrefix(slash, "..") || strings.HasPrefix(slash, "/") {
		return false
	}
	if slash == "etc/ssh/sshd_config" {
		return true
	}
	if strings.HasPrefix(slash, "etc/ssh/sshd_config.d/") && !strings.Contains(strings.TrimPrefix(slash, "etc/ssh/sshd_config.d/"), "/") {
		return true
	}
	return false
}

func restoreSSHConfigsFromSnapshotID(detector *PlatformDetector, snapshotRoot, snapshotID string) (HostActionResult, error) {
	if !reSafeSnapshotID.MatchString(strings.TrimSpace(snapshotID)) {
		return HostActionResult{}, fmt.Errorf("%w: invalid snapshot_id", ErrBridgeActionRejected)
	}
	if strings.TrimSpace(snapshotRoot) == "" {
		return HostActionResult{}, errors.New("snapshotRoot is not configured on bridge")
	}
	snapDir := filepath.Join(filepath.Clean(snapshotRoot), snapshotID)
	manifestBytes, err := os.ReadFile(filepath.Join(snapDir, "manifest.json"))
	if err != nil {
		return HostActionResult{}, fmt.Errorf("read snapshot manifest: %w", err)
	}
	var snap HostUpdateSnapshot
	if err := json.Unmarshal(manifestBytes, &snap); err != nil {
		return HostActionResult{}, fmt.Errorf("parse snapshot manifest: %w", err)
	}
	var restored []string
	for rel, backupPath := range snap.CopiedFiles {
		if !isAllowedRestoreRelPath(rel) {
			continue
		}
		cleanBackup := filepath.Clean(backupPath)
		if !strings.HasPrefix(cleanBackup, snapDir+string(filepath.Separator)) && cleanBackup != snapDir {
			return HostActionResult{}, fmt.Errorf("%w: snapshot backup path %q escapes snapshot directory", ErrBridgeActionRejected, backupPath)
		}
		data, rErr := os.ReadFile(cleanBackup)
		if rErr != nil {
			return HostActionResult{}, rErr
		}
		dst := detector.resolvePath(rel)
		if mkErr := os.MkdirAll(filepath.Dir(dst), 0755); mkErr != nil {
			return HostActionResult{}, mkErr
		}
		if wErr := os.WriteFile(dst, data, 0644); wErr != nil {
			return HostActionResult{}, wErr
		}
		restored = append(restored, rel)
	}
	return HostActionResult{Stdout: "restored=" + strings.Join(restored, ",")}, nil
}
