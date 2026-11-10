package audit

import (
	"bufio"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"home-datacenter-api/internal/model"
)

func generateRandomHex(bytesLen int) string {
	b := make([]byte, bytesLen)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (e *AgentAuditEngine) ensureArchiveDir() error {
	if strings.TrimSpace(e.archiveDir) == "" {
		return nil
	}
	return os.MkdirAll(e.archiveDir, 0o700)
}

func (e *AgentAuditEngine) appendJSONLFile(filePath string, payload interface{}) error {
	if strings.TrimSpace(filePath) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return err
	}
	line, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (e *AgentAuditEngine) appendArchiveEvent(ev *model.AgentAuditEvent) error {
	if strings.TrimSpace(e.archiveDir) == "" || ev == nil {
		return nil
	}
	day := ev.CreatedAt.UTC().Format("2006-01-02")
	path := filepath.Join(e.archiveDir, fmt.Sprintf("audit-%s.jsonl", day))
	return e.appendJSONLFile(path, ev)
}

func (e *AgentAuditEngine) appendEmergencyFallback(record map[string]interface{}) error {
	if strings.TrimSpace(e.archiveDir) == "" {
		return fmt.Errorf("archive_dir not configured for emergency audit fallback")
	}
	path := filepath.Join(e.archiveDir, "emergency-audit.jsonl")
	return e.appendJSONLFile(path, record)
}

// CreateCheckpoint anchors the latest uncheckpointed slice of streamID into SQLite and external JSONL.
func (e *AgentAuditEngine) CreateCheckpoint(streamID string) (*model.AgentAuditCheckpoint, error) {
	if e == nil || e.db == nil {
		return nil, ErrAuditCommitFailed
	}
	if strings.TrimSpace(streamID) == "" {
		streamID = model.AuditPrimaryStreamID
	}

	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	return e.createCheckpointLocked(streamID)
}

func (e *AgentAuditEngine) createCheckpointLocked(streamID string) (*model.AgentAuditCheckpoint, error) {
	kid, ring, err := ResolveAuditHMACKeyring()
	if err != nil {
		return nil, err
	}
	secret := ring[kid]
	now := e.nowUTC()
	nowMs := now.UnixMilli()

	var created *model.AgentAuditCheckpoint
	err = e.runImmediate(func(conn *sql.Conn) error {
		return e.createCheckpointInConn(conn, streamID, kid, secret, now, nowMs, &created)
	})
	if err != nil {
		return nil, err
	}

	if created != nil && strings.TrimSpace(e.archiveDir) != "" {
		cpPath := filepath.Join(e.archiveDir, "checkpoints.jsonl")
		if err := e.appendJSONLFile(cpPath, created); err != nil {
			return created, err
		}
	}
	return created, nil
}

func (e *AgentAuditEngine) createCheckpointInConn(
	conn *sql.Conn,
	streamID string,
	kid string,
	secret string,
	now time.Time,
	nowMs int64,
	out **model.AgentAuditCheckpoint,
) error {
	var lastCPToSeq uint64
	var prevDigest string
	row := conn.QueryRowContext(e.bgCtx(),
		`SELECT to_seq, chain_digest FROM agent_audit_checkpoints WHERE stream_id = ? ORDER BY to_seq DESC LIMIT 1`,
		streamID,
	)
	if scanErr := row.Scan(&lastCPToSeq, &prevDigest); scanErr != nil && scanErr != sql.ErrNoRows {
		return scanErr
	}
	if prevDigest == "" {
		prevDigest = model.AuditGenesisHash
	}

	var tailSeq uint64
	var tailHash string
	rowTail := conn.QueryRowContext(e.bgCtx(),
		`SELECT seq, entry_hash FROM agent_audit_events WHERE stream_id = ? ORDER BY seq DESC LIMIT 1`,
		streamID,
	)
	if scanErr := rowTail.Scan(&tailSeq, &tailHash); scanErr != nil {
		if scanErr == sql.ErrNoRows || tailSeq == 0 || tailSeq <= lastCPToSeq {
			return nil
		}
		return scanErr
	}
	if tailSeq <= lastCPToSeq {
		return nil
	}

	fromSeq := lastCPToSeq + 1
	var headHash string
	var eventCount int64
	if err := conn.QueryRowContext(e.bgCtx(),
		`SELECT entry_hash FROM agent_audit_events WHERE stream_id = ? AND seq = ? LIMIT 1`,
		streamID, fromSeq,
	).Scan(&headHash); err != nil {
		return err
	}
	if err := conn.QueryRowContext(e.bgCtx(),
		`SELECT COUNT(*) FROM agent_audit_events WHERE stream_id = ? AND seq >= ? AND seq <= ?`,
		streamID, fromSeq, tailSeq,
	).Scan(&eventCount); err != nil {
		return err
	}

	archiveFile := ""
	if strings.TrimSpace(e.archiveDir) != "" {
		archiveFile = filepath.Join(e.archiveDir, fmt.Sprintf("audit-%s.jsonl", now.UTC().Format("2006-01-02")))
	}

	cp := &model.AgentAuditCheckpoint{
		CheckpointID:    fmt.Sprintf("chk-%d-%s", nowMs, generateRandomHex(4)),
		StreamID:        streamID,
		FromSeq:         fromSeq,
		ToSeq:           tailSeq,
		EventCount:      eventCount,
		HeadEntryHash:   headHash,
		TailEntryHash:   tailHash,
		ChainDigest:     ComputeCheckpointChainDigest(prevDigest, tailHash, eventCount),
		PrevChainDigest: prevDigest,
		KeyID:           kid,
		ArchiveFile:     archiveFile,
		CreatedAt:       now,
		CreatedAtMs:     nowMs,
	}
	cp.HMACSignature = ComputeCheckpointHMAC(kid, secret, cp)

	if _, err := conn.ExecContext(e.bgCtx(), `
		INSERT INTO agent_audit_checkpoints (
			checkpoint_id, stream_id, from_seq, to_seq, event_count,
			head_entry_hash, tail_entry_hash, chain_digest, prev_chain_digest,
			key_id, hmac_signature, archive_file, created_at, created_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cp.CheckpointID, cp.StreamID, cp.FromSeq, cp.ToSeq, cp.EventCount,
		cp.HeadEntryHash, cp.TailEntryHash, cp.ChainDigest, cp.PrevChainDigest,
		cp.KeyID, cp.HMACSignature, cp.ArchiveFile, cp.CreatedAt.UTC(), cp.CreatedAtMs,
	); err != nil {
		return err
	}
	*out = cp
	return nil
}

func (e *AgentAuditEngine) loadExternalCheckpoints(streamID string) ([]model.AgentAuditCheckpoint, error) {
	if strings.TrimSpace(e.archiveDir) == "" {
		return nil, nil
	}
	cpPath := filepath.Join(e.archiveDir, "checkpoints.jsonl")
	f, err := os.Open(cpPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []model.AgentAuditCheckpoint
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var cp model.AgentAuditCheckpoint
		if uErr := json.Unmarshal([]byte(line), &cp); uErr != nil {
			return nil, fmt.Errorf("corrupt checkpoint line in %s: %w", cpPath, uErr)
		}
		if streamID == "" || cp.StreamID == streamID {
			out = append(out, cp)
		}
	}
	return out, scanner.Err()
}
