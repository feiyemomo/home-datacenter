package audit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
)

var (
	// ErrRemoteConflict indicates an append-only WORM violation (hash/digest fork or sequence conflict).
	ErrRemoteConflict = errors.New("REMOTE_WORM_CONFLICT: remote store rejected conflicting history")
	// ErrRemoteUnavailable indicates a transient network, filesystem, or HTTP unavailability on the remote store.
	ErrRemoteUnavailable = errors.New("REMOTE_STORE_UNAVAILABLE: remote audit store unreachable")
)

// RemoteAuditStore abstracts an off-DB append-only / WORM replica (remote HTTP/object-store collector or isolated WORM mount).
type RemoteAuditStore interface {
	Type() string
	EndpointDisplay() string
	AppendEvents(ctx context.Context, streamID string, events []model.AgentAuditEvent) error
	SubmitCheckpoint(ctx context.Context, cp model.AgentAuditCheckpoint) error
	FetchTail(ctx context.Context, streamID string) (*RemoteTailState, error)
	FetchCheckpoints(ctx context.Context, streamID string) ([]model.AgentAuditCheckpoint, error)
	FetchEventsRange(ctx context.Context, streamID string, fromSeq, toSeq uint64) ([]model.AgentAuditEvent, error)
}

func sanitizeStreamName(streamID string) string {
	s := strings.TrimSpace(streamID)
	if s == "" {
		return model.AuditPrimaryStreamID
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// FileWORMRemoteStore stores append-only events and checkpoints in an isolated filesystem/WORM mount.
// Rewrites, truncations, or forks are detected fail-closed by verifying monotonic sequences and hash linkage on append.
type FileWORMRemoteStore struct {
	rootDir string
	mu      sync.Mutex
}

// NewFileWORMRemoteStore constructs a FileWORMRemoteStore backed by rootDir.
func NewFileWORMRemoteStore(rootDir string) (*FileWORMRemoteStore, error) {
	trimmed := strings.TrimSpace(rootDir)
	if trimmed == "" {
		return nil, fmt.Errorf("file-worm remote directory is empty")
	}
	if err := os.MkdirAll(trimmed, 0o700); err != nil {
		return nil, fmt.Errorf("%w: failed to initialize remote dir: %v", ErrRemoteUnavailable, err)
	}
	return &FileWORMRemoteStore{rootDir: trimmed}, nil
}

func (s *FileWORMRemoteStore) Type() string            { return "file-worm" }
func (s *FileWORMRemoteStore) EndpointDisplay() string { return redact.SensitiveString(s.rootDir) }

func (s *FileWORMRemoteStore) eventsPath(streamID string) string {
	return filepath.Join(s.rootDir, fmt.Sprintf("events-%s.jsonl", sanitizeStreamName(streamID)))
}

func (s *FileWORMRemoteStore) checkpointsPath(streamID string) string {
	return filepath.Join(s.rootDir, fmt.Sprintf("checkpoints-%s.jsonl", sanitizeStreamName(streamID)))
}

func (s *FileWORMRemoteStore) readEventsLocked(streamID string) ([]model.AgentAuditEvent, error) {
	path := s.eventsPath(streamID)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %v", ErrRemoteUnavailable, err)
	}
	defer f.Close()

	var out []model.AgentAuditEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev model.AgentAuditEvent
		if uErr := json.Unmarshal([]byte(line), &ev); uErr != nil {
			return nil, fmt.Errorf("corrupt WORM event record in %s: %w", path, uErr)
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRemoteUnavailable, err)
	}
	return out, nil
}

func (s *FileWORMRemoteStore) readCheckpointsLocked(streamID string) ([]model.AgentAuditCheckpoint, error) {
	path := s.checkpointsPath(streamID)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %v", ErrRemoteUnavailable, err)
	}
	defer f.Close()

	var out []model.AgentAuditCheckpoint
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var cp model.AgentAuditCheckpoint
		if uErr := json.Unmarshal([]byte(line), &cp); uErr != nil {
			return nil, fmt.Errorf("corrupt WORM checkpoint record in %s: %w", path, uErr)
		}
		out = append(out, cp)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRemoteUnavailable, err)
	}
	return out, nil
}

func (s *FileWORMRemoteStore) AppendEvents(ctx context.Context, streamID string, events []model.AgentAuditEvent) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if len(events) == 0 {
		return nil
	}
	if strings.TrimSpace(streamID) == "" {
		streamID = model.AuditPrimaryStreamID
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.rootDir, 0o700); err != nil {
		return fmt.Errorf("%w: %v", ErrRemoteUnavailable, err)
	}

	existing, err := s.readEventsLocked(streamID)
	if err != nil {
		return err
	}

	bySeq := make(map[uint64]model.AgentAuditEvent, len(existing))
	var tailSeq uint64
	tailHash := model.AuditGenesisHash
	for _, ev := range existing {
		bySeq[ev.Seq] = ev
		if ev.Seq > tailSeq {
			tailSeq = ev.Seq
			tailHash = ev.EntryHash
		}
	}

	var toAppend []model.AgentAuditEvent
	for _, ev := range events {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if prev, exists := bySeq[ev.Seq]; exists {
			if prev.EntryHash != ev.EntryHash || prev.AuditID != ev.AuditID {
				return fmt.Errorf("%w: seq=%d existing hash %s != incoming %s", ErrRemoteConflict, ev.Seq, prev.EntryHash, ev.EntryHash)
			}
			continue // idempotent retry
		}
		if ev.Seq != tailSeq+1 {
			return fmt.Errorf("%w: non-contiguous remote append expected seq=%d got=%d", ErrRemoteConflict, tailSeq+1, ev.Seq)
		}
		if ev.PreviousHash != tailHash {
			return fmt.Errorf("%w: previous_hash mismatch at seq=%d", ErrRemoteConflict, ev.Seq)
		}
		toAppend = append(toAppend, ev)
		bySeq[ev.Seq] = ev
		tailSeq = ev.Seq
		tailHash = ev.EntryHash
	}
	if len(toAppend) == 0 {
		return nil
	}

	f, err := os.OpenFile(s.eventsPath(streamID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRemoteUnavailable, err)
	}
	defer f.Close()

	for _, ev := range toAppend {
		raw, mErr := json.Marshal(ev)
		if mErr != nil {
			return mErr
		}
		if _, wErr := f.Write(append(raw, '\n')); wErr != nil {
			return fmt.Errorf("%w: %v", ErrRemoteUnavailable, wErr)
		}
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("%w: %v", ErrRemoteUnavailable, err)
	}
	return nil
}

func (s *FileWORMRemoteStore) SubmitCheckpoint(ctx context.Context, cp model.AgentAuditCheckpoint) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	streamID := strings.TrimSpace(cp.StreamID)
	if streamID == "" {
		streamID = model.AuditPrimaryStreamID
		cp.StreamID = streamID
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.rootDir, 0o700); err != nil {
		return fmt.Errorf("%w: %v", ErrRemoteUnavailable, err)
	}

	existing, err := s.readCheckpointsLocked(streamID)
	if err != nil {
		return err
	}
	prevDigest := model.AuditGenesisHash
	var maxToSeq uint64
	for _, old := range existing {
		if old.CheckpointID == cp.CheckpointID {
			if old.ChainDigest != cp.ChainDigest || old.TailEntryHash != cp.TailEntryHash || old.ToSeq != cp.ToSeq {
				return fmt.Errorf("%w: checkpoint %s diverges from stored WORM record", ErrRemoteConflict, cp.CheckpointID)
			}
			return nil // idempotent
		}
		if old.ToSeq > maxToSeq {
			maxToSeq = old.ToSeq
			prevDigest = old.ChainDigest
		}
	}
	if cp.ToSeq <= maxToSeq {
		return fmt.Errorf("%w: checkpoint to_seq=%d regresses behind stored WORM to_seq=%d", ErrRemoteConflict, cp.ToSeq, maxToSeq)
	}
	if cp.PrevChainDigest != prevDigest {
		return fmt.Errorf("%w: checkpoint %s prev_chain_digest mismatch", ErrRemoteConflict, cp.CheckpointID)
	}

	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.checkpointsPath(streamID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRemoteUnavailable, err)
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("%w: %v", ErrRemoteUnavailable, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("%w: %v", ErrRemoteUnavailable, err)
	}
	return nil
}

func (s *FileWORMRemoteStore) FetchTail(ctx context.Context, streamID string) (*RemoteTailState, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if strings.TrimSpace(streamID) == "" {
		streamID = model.AuditPrimaryStreamID
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	events, err := s.readEventsLocked(streamID)
	if err != nil {
		return nil, err
	}
	cps, err := s.readCheckpointsLocked(streamID)
	if err != nil {
		return nil, err
	}

	tail := &RemoteTailState{
		StreamID:   streamID,
		EventCount: int64(len(events)),
	}
	for _, ev := range events {
		if ev.Seq >= tail.TailSeq {
			tail.TailSeq = ev.Seq
			tail.TailEntryHash = ev.EntryHash
			tail.UpdatedAt = FormatRFC3339Ms(ev.CreatedAt)
		}
	}
	for _, cp := range cps {
		if cp.ToSeq >= tail.LastCheckpointSeq {
			tail.LastCheckpointSeq = cp.ToSeq
			tail.LastCheckpointID = cp.CheckpointID
			tail.LastChainDigest = cp.ChainDigest
		}
	}
	return tail, nil
}

func (s *FileWORMRemoteStore) FetchCheckpoints(ctx context.Context, streamID string) ([]model.AgentAuditCheckpoint, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if strings.TrimSpace(streamID) == "" {
		streamID = model.AuditPrimaryStreamID
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readCheckpointsLocked(streamID)
}

func (s *FileWORMRemoteStore) FetchEventsRange(ctx context.Context, streamID string, fromSeq, toSeq uint64) ([]model.AgentAuditEvent, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if strings.TrimSpace(streamID) == "" {
		streamID = model.AuditPrimaryStreamID
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := s.readEventsLocked(streamID)
	if err != nil {
		return nil, err
	}
	var out []model.AgentAuditEvent
	for _, ev := range all {
		if (fromSeq == 0 || ev.Seq >= fromSeq) && (toSeq == 0 || ev.Seq <= toSeq) {
			out = append(out, ev)
		}
	}
	return out, nil
}

// HTTPRemoteAuditStore pushes append-only events and signed checkpoints to an external HTTP/object-store gateway.
type HTTPRemoteAuditStore struct {
	baseURL     string
	displayURL  string
	bearerToken string
	client      *http.Client
}

// NewHTTPRemoteAuditStore creates an HTTP-backed append-only remote audit store client.
func NewHTTPRemoteAuditStore(baseURL, bearerToken string, timeout time.Duration) (*HTTPRemoteAuditStore, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil, fmt.Errorf("remote HTTP audit store URL is empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid remote audit store URL")
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""

	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	tok := strings.TrimSpace(bearerToken)
	if tok != "" {
		redact.RegisterRuntimeSecret(tok)
	}
	return &HTTPRemoteAuditStore{
		baseURL:     trimmed,
		displayURL:  redact.SensitiveString(u.String()),
		bearerToken: tok,
		client:      &http.Client{Timeout: timeout},
	}, nil
}

func (h *HTTPRemoteAuditStore) Type() string            { return "http-append" }
func (h *HTTPRemoteAuditStore) EndpointDisplay() string { return h.displayURL }

func (h *HTTPRemoteAuditStore) doJSON(ctx context.Context, method, path string, reqBody interface{}, respOut interface{}) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var bodyReader io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrRemoteUnavailable, redact.SensitiveString(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	if h.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+h.bearerToken)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrRemoteUnavailable, redact.SensitiveString(err.Error()))
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: %s", ErrRemoteConflict, redact.SensitiveString(string(respBytes)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: HTTP %d: %s", ErrRemoteUnavailable, resp.StatusCode, redact.SensitiveString(string(respBytes)))
	}
	if respOut != nil && len(bytes.TrimSpace(respBytes)) > 0 {
		if uErr := json.Unmarshal(respBytes, respOut); uErr != nil {
			return fmt.Errorf("invalid JSON response from remote audit store: %w", uErr)
		}
	}
	return nil
}

func (h *HTTPRemoteAuditStore) AppendEvents(ctx context.Context, streamID string, events []model.AgentAuditEvent) error {
	if len(events) == 0 {
		return nil
	}
	if strings.TrimSpace(streamID) == "" {
		streamID = model.AuditPrimaryStreamID
	}
	payload := struct {
		StreamID string                  `json:"stream_id"`
		Events   []model.AgentAuditEvent `json:"events"`
	}{
		StreamID: streamID,
		Events:   events,
	}
	return h.doJSON(ctx, http.MethodPost, "/events", payload, nil)
}

func (h *HTTPRemoteAuditStore) SubmitCheckpoint(ctx context.Context, cp model.AgentAuditCheckpoint) error {
	return h.doJSON(ctx, http.MethodPost, "/checkpoints", cp, nil)
}

func (h *HTTPRemoteAuditStore) FetchTail(ctx context.Context, streamID string) (*RemoteTailState, error) {
	if strings.TrimSpace(streamID) == "" {
		streamID = model.AuditPrimaryStreamID
	}
	var out RemoteTailState
	if err := h.doJSON(ctx, http.MethodGet, "/tail?stream_id="+url.QueryEscape(streamID), nil, &out); err != nil {
		return nil, err
	}
	if out.StreamID == "" {
		out.StreamID = streamID
	}
	return &out, nil
}

func (h *HTTPRemoteAuditStore) FetchCheckpoints(ctx context.Context, streamID string) ([]model.AgentAuditCheckpoint, error) {
	if strings.TrimSpace(streamID) == "" {
		streamID = model.AuditPrimaryStreamID
	}
	var out []model.AgentAuditCheckpoint
	if err := h.doJSON(ctx, http.MethodGet, "/checkpoints?stream_id="+url.QueryEscape(streamID), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (h *HTTPRemoteAuditStore) FetchEventsRange(ctx context.Context, streamID string, fromSeq, toSeq uint64) ([]model.AgentAuditEvent, error) {
	if strings.TrimSpace(streamID) == "" {
		streamID = model.AuditPrimaryStreamID
	}
	q := fmt.Sprintf("/events?stream_id=%s&from_seq=%d&to_seq=%d", url.QueryEscape(streamID), fromSeq, toSeq)
	var out []model.AgentAuditEvent
	if err := h.doJSON(ctx, http.MethodGet, q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveRemoteAuditStoreFromEnv constructs an optional RemoteAuditStore from OPS_AUDIT_REMOTE_* environment variables.
func ResolveRemoteAuditStoreFromEnv() (RemoteAuditStore, error) {
	remoteType := strings.ToLower(strings.TrimSpace(os.Getenv("OPS_AUDIT_REMOTE_TYPE")))
	remoteDir := strings.TrimSpace(os.Getenv("OPS_AUDIT_REMOTE_DIR"))
	remoteURL := strings.TrimSpace(os.Getenv("OPS_AUDIT_REMOTE_URL"))
	remoteToken := strings.TrimSpace(os.Getenv("OPS_AUDIT_REMOTE_TOKEN"))
	if remoteToken != "" {
		redact.RegisterRuntimeSecret(remoteToken)
	}

	timeout := 3 * time.Second
	if rawSec := strings.TrimSpace(os.Getenv("OPS_AUDIT_REMOTE_TIMEOUT_SEC")); rawSec != "" {
		if sec, err := strconv.Atoi(rawSec); err == nil && sec > 0 {
			timeout = time.Duration(sec) * time.Second
		}
	}

	if remoteType == "" {
		if remoteURL != "" {
			remoteType = "http-append"
		} else if remoteDir != "" {
			remoteType = "file-worm"
		}
	}

	switch remoteType {
	case "", "none", "disabled":
		return nil, nil
	case "file-worm", "worm", "file":
		return NewFileWORMRemoteStore(remoteDir)
	case "http-append", "http", "s3-proxy":
		return NewHTTPRemoteAuditStore(remoteURL, remoteToken, timeout)
	default:
		return nil, fmt.Errorf("unsupported OPS_AUDIT_REMOTE_TYPE: %q", remoteType)
	}
}
