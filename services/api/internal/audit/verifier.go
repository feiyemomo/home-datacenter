package audit

import (
	"fmt"
	"strings"

	"home-datacenter-api/internal/model"
)

const localNASLimitationNote = "Verified append-only SQLite triggers, SHA-256 hash chain, and external-key HMAC-SHA256 checkpoints. Local NAS hosts without external WORM storage remain exposed to privileged host/disk rollback outside remote backup sync."

var requiredAuditTriggers = []string{
	"trg_agent_audit_events_no_update",
	"trg_agent_audit_events_no_delete",
	"trg_agent_audit_checkpoints_no_update",
	"trg_agent_audit_checkpoints_no_delete",
}

// VerifyTriggers checks that append-only SQLite triggers remain installed on audit tables.
func (e *AgentAuditEngine) VerifyTriggers() (bool, string) {
	if e == nil || e.db == nil {
		return false, "audit database unavailable"
	}
	var names []string
	if err := e.db.Raw("SELECT name FROM sqlite_master WHERE type='trigger'").Scan(&names).Error; err != nil {
		return false, fmt.Sprintf("failed to inspect sqlite triggers: %v", err)
	}
	present := make(map[string]bool, len(names))
	for _, n := range names {
		present[n] = true
	}
	for _, req := range requiredAuditTriggers {
		if !present[req] {
			return false, fmt.Sprintf("missing required append-only trigger: %s", req)
		}
	}
	return true, ""
}

// VerifyChain validates sequential continuity, hash-chain integrity, HMAC signatures, and checkpoints.
func (e *AgentAuditEngine) VerifyChain(req VerifyRequest) VerifyResult {
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		streamID = model.AuditPrimaryStreamID
	}
	res := VerifyResult{
		StreamID:       streamID,
		LimitationNote: localNASLimitationNote,
		VerifiedAt:     FormatRFC3339Ms(e.nowUTC()),
	}

	if e == nil || e.db == nil {
		res.IntegrityStatus = IntegrityUnverifiable
		res.FailureReason = "audit database unavailable"
		return res
	}

	_, ring, err := ResolveAuditHMACKeyring()
	if err != nil {
		res.IntegrityStatus = IntegrityHMACInvalid
		res.FailureReason = fmt.Sprintf("audit keyring unavailable: %v", err)
		return res
	}

	triggersOK, trigReason := e.VerifyTriggers()
	res.TriggersIntact = triggersOK
	if req.VerifyTriggers && !triggersOK {
		res.IntegrityStatus = IntegrityTriggerMissing
		res.FailureReason = trigReason
		return res
	}

	// Load DB checkpoints and optional external disk checkpoints for truncation detection.
	var dbCheckpoints []model.AgentAuditCheckpoint
	if err := e.db.Where("stream_id = ?", streamID).Order("to_seq ASC").Find(&dbCheckpoints).Error; err != nil {
		res.IntegrityStatus = IntegrityUnverifiable
		res.FailureReason = fmt.Sprintf("failed to query checkpoints: %v", err)
		return res
	}
	extCheckpoints, extErr := e.loadExternalCheckpoints(streamID)
	if extErr != nil {
		res.IntegrityStatus = IntegrityUnverifiable
		res.FailureReason = fmt.Sprintf("failed to inspect external checkpoint file: %v", extErr)
		return res
	}

	var latestCP *model.AgentAuditCheckpoint
	for i := range dbCheckpoints {
		cp := dbCheckpoints[i]
		if latestCP == nil || cp.ToSeq >= latestCP.ToSeq {
			copyCP := cp
			latestCP = &copyCP
		}
	}
	for i := range extCheckpoints {
		cp := extCheckpoints[i]
		if latestCP == nil || cp.ToSeq >= latestCP.ToSeq {
			copyCP := cp
			latestCP = &copyCP
		}
	}
	if latestCP != nil {
		res.LastCheckpointID = latestCP.CheckpointID
		res.LastCheckpointSeq = latestCP.ToSeq
	}

	// Query events ordered strictly by seq.
	q := e.db.Where("stream_id = ?", streamID).Order("seq ASC")
	if req.FromSeq > 0 {
		q = q.Where("seq >= ?", req.FromSeq)
	}
	if req.ToSeq > 0 {
		q = q.Where("seq <= ?", req.ToSeq)
	}
	var events []model.AgentAuditEvent
	if err := q.Find(&events).Error; err != nil {
		res.IntegrityStatus = IntegrityUnverifiable
		res.FailureReason = fmt.Sprintf("failed to load audit events: %v", err)
		return res
	}

	// Honest non-empty guarantee: never return Valid=true when count == 0.
	if len(events) == 0 {
		var runCount int64
		_ = e.db.Model(&model.AgentAuditRun{}).Count(&runCount).Error
		if latestCP != nil || runCount > 0 {
			res.IntegrityStatus = IntegrityTruncatedTail
			res.FailureReason = "audit event table is empty despite existing runs or checkpoints"
		} else {
			res.IntegrityStatus = IntegrityUnverifiable
			res.FailureReason = "no audit events present to verify"
		}
		return res
	}

	expectedPrevHash := model.AuditGenesisHash
	expectedSeq := uint64(1)
	if req.FromSeq > 1 {
		var prev model.AgentAuditEvent
		if err := e.db.Where("stream_id = ? AND seq = ?", streamID, req.FromSeq-1).First(&prev).Error; err != nil {
			res.IntegrityStatus = IntegritySeqGap
			res.BrokenAtSeq = req.FromSeq - 1
			res.FailureReason = fmt.Sprintf("missing predecessor seq %d before range start %d", req.FromSeq-1, req.FromSeq)
			return res
		}
		expectedPrevHash = prev.EntryHash
		expectedSeq = req.FromSeq
	}

	keyUsedSet := make(map[string]bool)
	bySeq := make(map[uint64]model.AgentAuditEvent, len(events))
	res.HeadSeq = events[0].Seq

	for _, ev := range events {
		if ev.Seq != expectedSeq {
			res.IntegrityStatus = IntegritySeqGap
			res.BrokenAtSeq = expectedSeq
			res.FailureReason = fmt.Sprintf("expected seq %d, got seq %d (deleted or skipped row)", expectedSeq, ev.Seq)
			return res
		}
		if ev.PreviousHash != expectedPrevHash {
			res.IntegrityStatus = IntegrityHashMismatch
			res.BrokenAtSeq = ev.Seq
			res.FailureReason = fmt.Sprintf("previous_hash mismatch at seq=%d", ev.Seq)
			return res
		}
		computed, hErr := ComputeEventEntryHash(&ev)
		if hErr != nil || computed != ev.EntryHash {
			res.IntegrityStatus = IntegrityHashMismatch
			res.BrokenAtSeq = ev.Seq
			res.FailureReason = fmt.Sprintf("entry_hash tamper detected at seq=%d", ev.Seq)
			return res
		}
		kid, macErr := VerifyEventHMAC(&ev, ring)
		if macErr != nil {
			res.IntegrityStatus = IntegrityHMACInvalid
			res.BrokenAtSeq = ev.Seq
			res.FailureReason = fmt.Sprintf("HMAC verification failed at seq=%d: %v", ev.Seq, macErr)
			return res
		}
		keyUsedSet[kid] = true
		expectedPrevHash = ev.EntryHash
		expectedSeq++
		res.CheckedEvents++
		res.TailSeq = ev.Seq
		res.TailEntryHash = ev.EntryHash
		bySeq[ev.Seq] = ev
	}

	for kid := range keyUsedSet {
		res.KeyIDsUsed = append(res.KeyIDsUsed, kid)
	}

	// Verify checkpoints and tail truncation when verifying full stream or checkpoints requested.
	if req.VerifyCheckpoints {
		allCheckpoints := append([]model.AgentAuditCheckpoint{}, dbCheckpoints...)
		allCheckpoints = append(allCheckpoints, extCheckpoints...)

		prevChainDigest := model.AuditGenesisHash
		// Track DB checkpoint chain digest order separately from external file mirror.
		for _, cp := range dbCheckpoints {
			if err := VerifyCheckpointHMAC(&cp, ring); err != nil {
				res.IntegrityStatus = IntegrityHMACInvalid
				res.BrokenAtSeq = cp.ToSeq
				res.FailureReason = fmt.Sprintf("checkpoint %s HMAC invalid: %v", cp.CheckpointID, err)
				return res
			}
			expectedDigest := ComputeCheckpointChainDigest(prevChainDigest, cp.TailEntryHash, cp.EventCount)
			if cp.PrevChainDigest != prevChainDigest || cp.ChainDigest != expectedDigest {
				res.IntegrityStatus = IntegrityHashMismatch
				res.BrokenAtSeq = cp.ToSeq
				res.FailureReason = fmt.Sprintf("checkpoint %s chain_digest mismatch", cp.CheckpointID)
				return res
			}
			prevChainDigest = cp.ChainDigest
			res.CheckedCheckpoints++
		}

		for _, cp := range allCheckpoints {
			if err := VerifyCheckpointHMAC(&cp, ring); err != nil {
				res.IntegrityStatus = IntegrityHMACInvalid
				res.BrokenAtSeq = cp.ToSeq
				res.FailureReason = fmt.Sprintf("external checkpoint %s HMAC invalid: %v", cp.CheckpointID, err)
				return res
			}
			if res.TailSeq < cp.ToSeq && req.ToSeq == 0 {
				res.IntegrityStatus = IntegrityTruncatedTail
				res.BrokenAtSeq = res.TailSeq + 1
				res.FailureReason = fmt.Sprintf("tail truncation detected: DB tail_seq=%d < checkpoint to_seq=%d (%s)", res.TailSeq, cp.ToSeq, cp.CheckpointID)
				return res
			}
			if req.FromSeq <= cp.FromSeq && (req.ToSeq == 0 || req.ToSeq >= cp.ToSeq) {
				if headEv, ok := bySeq[cp.FromSeq]; ok && headEv.EntryHash != cp.HeadEntryHash {
					res.IntegrityStatus = IntegrityTruncatedTail
					res.BrokenAtSeq = cp.FromSeq
					res.FailureReason = fmt.Sprintf("head hash mismatch against checkpoint %s at seq=%d", cp.CheckpointID, cp.FromSeq)
					return res
				}
				if tailEv, ok := bySeq[cp.ToSeq]; ok && tailEv.EntryHash != cp.TailEntryHash {
					res.IntegrityStatus = IntegrityTruncatedTail
					res.BrokenAtSeq = cp.ToSeq
					res.FailureReason = fmt.Sprintf("tail hash mismatch against checkpoint %s at seq=%d", cp.CheckpointID, cp.ToSeq)
					return res
				}
			}
		}
	}

	if !triggersOK {
		res.Valid = false
		res.IntegrityStatus = IntegrityTriggerMissing
		res.FailureReason = trigReason
		return res
	}

	res.Valid = true
	if res.CheckedCheckpoints > 0 || res.LastCheckpointID != "" {
		res.IntegrityStatus = IntegrityVerifiedAgainstCheckpoint
	} else {
		res.IntegrityStatus = IntegrityChainIntactLocalOnly
	}
	return res
}

// VerifyRunChain verifies the primary stream and ensures all events of a given run_id match computed hashes.
func (e *AgentAuditEngine) VerifyRunChain(runID string, householdID uint) VerifyResult {
	res := e.VerifyChain(VerifyRequest{
		StreamID:          model.AuditPrimaryStreamID,
		VerifyCheckpoints: true,
		VerifyTriggers:    true,
	})
	if !res.Valid {
		return res
	}
	var count int64
	if e != nil && e.db != nil {
		_ = e.db.Model(&model.AgentAuditEvent{}).
			Where("run_id = ? AND household_id = ?", runID, householdID).
			Count(&count).Error
	}
	if count == 0 {
		res.Valid = false
		res.IntegrityStatus = IntegrityUnverifiable
		res.FailureReason = "run has no verified audit events"
	}
	return res
}
