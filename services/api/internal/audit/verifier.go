package audit

import (
	"context"
	"fmt"
	"sort"
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
		StreamID:         streamID,
		SecurityBoundary: buildSecurityBoundaryNote(e != nil && e.remoteStore != nil, false, false, false, false),
		LimitationNote:   localNASLimitationNote,
		VerifiedAt:       FormatRFC3339Ms(e.nowUTC()),
	}

	if e == nil || e.db == nil {
		res.IntegrityStatus = IntegrityUnverifiable
		res.FailureReason = "audit database unavailable"
		return res
	}

	verifyRemote := e.remoteStore != nil && (req.VerifyRemote == nil || *req.VerifyRemote)
	_, extVerifier := e.activeExternalSignerAndVerifier()
	if e.remoteStore != nil {
		res.RemoteStoreConfigured = true
		res.RemoteStoreType = e.remoteStore.Type()
	}
	res.SecurityBoundary = buildSecurityBoundaryNote(e.remoteStore != nil, false, false, extVerifier != nil, false)

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
		if verifyRemote {
			rep := e.CompareWithRemote(context.Background(), streamID)
			res.RemoteSyncStatus = rep.Status
			res.RemoteTailSeq = rep.RemoteTailSeq
			res.RemoteTailHash = rep.RemoteTailHash
			res.SecurityBoundary = buildSecurityBoundaryNote(true, rep.Reachable, false, extVerifier != nil, rep.ExternalSigVerified)
			if rep.Status == RemoteStatusDBRollback || rep.Status == RemoteStatusTailTruncated || rep.RemoteTailSeq > 0 {
				res.IntegrityStatus = IntegrityDBRollbackDetected
				res.FailureReason = fmt.Sprintf("whole-DB rollback/truncation detected against remote store (%s): %s", rep.StoreType, strings.Join(rep.Discrepancies, "; "))
				return res
			}
		}
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
		if ev.KeyID != "" && ev.KeyID != kid {
			res.IntegrityStatus = IntegrityHMACInvalid
			res.BrokenAtSeq = ev.Seq
			res.FailureReason = fmt.Sprintf("key_id mismatch at seq=%d: column=%q signature=%q", ev.Seq, ev.KeyID, kid)
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
	sort.Strings(res.KeyIDsUsed)

	var dbMaxSeq uint64
	_ = e.db.Model(&model.AgentAuditEvent{}).Where("stream_id = ?", streamID).Select("COALESCE(MAX(seq), 0)").Scan(&dbMaxSeq).Error

	// Verify checkpoints and tail truncation when verifying full stream or checkpoints requested.
	if req.VerifyCheckpoints {
		prevChainDigest := model.AuditGenesisHash
		dbCPByID := make(map[string]model.AgentAuditCheckpoint, len(dbCheckpoints))
		allDBExtSigsOK := extVerifier != nil && len(dbCheckpoints) > 0
		for _, cp := range dbCheckpoints {
			if err := VerifyCheckpointHMAC(&cp, ring); err != nil {
				res.IntegrityStatus = IntegrityHMACInvalid
				res.BrokenAtSeq = cp.ToSeq
				res.FailureReason = fmt.Sprintf("checkpoint %s HMAC invalid: %v", cp.CheckpointID, err)
				return res
			}
			if extVerifier != nil {
				if err := extVerifier.VerifyCheckpointExternal(&cp); err != nil {
					res.IntegrityStatus = IntegrityHMACInvalid
					res.BrokenAtSeq = cp.ToSeq
					res.FailureReason = fmt.Sprintf("checkpoint %s external asymmetric signature invalid: %v", cp.CheckpointID, err)
					return res
				}
			}
			expectedDigest := ComputeCheckpointChainDigest(prevChainDigest, cp.TailEntryHash, cp.EventCount)
			if cp.PrevChainDigest != prevChainDigest || cp.ChainDigest != expectedDigest {
				res.IntegrityStatus = IntegrityHashMismatch
				res.BrokenAtSeq = cp.ToSeq
				res.FailureReason = fmt.Sprintf("checkpoint %s chain_digest mismatch", cp.CheckpointID)
				return res
			}
			prevChainDigest = cp.ChainDigest
			dbCPByID[cp.CheckpointID] = cp
			res.CheckedCheckpoints++
		}
		if allDBExtSigsOK {
			res.ExternalSigVerified = true
		}

		// Verify external checkpoints chain continuity and cross-check against DB checkpoints.
		extPrevDigest := model.AuditGenesisHash
		for _, cp := range extCheckpoints {
			if err := VerifyCheckpointHMAC(&cp, ring); err != nil {
				res.IntegrityStatus = IntegrityHMACInvalid
				res.BrokenAtSeq = cp.ToSeq
				res.FailureReason = fmt.Sprintf("external checkpoint %s HMAC invalid: %v", cp.CheckpointID, err)
				return res
			}
			if extVerifier != nil {
				if err := extVerifier.VerifyCheckpointExternal(&cp); err != nil {
					res.IntegrityStatus = IntegrityHMACInvalid
					res.BrokenAtSeq = cp.ToSeq
					res.FailureReason = fmt.Sprintf("external checkpoint %s asymmetric signature invalid: %v", cp.CheckpointID, err)
					return res
				}
			}
			expectedExtDigest := ComputeCheckpointChainDigest(extPrevDigest, cp.TailEntryHash, cp.EventCount)
			if cp.PrevChainDigest != extPrevDigest || cp.ChainDigest != expectedExtDigest {
				res.IntegrityStatus = IntegrityHashMismatch
				res.BrokenAtSeq = cp.ToSeq
				res.FailureReason = fmt.Sprintf("external checkpoint %s chain_digest mismatch", cp.CheckpointID)
				return res
			}
			extPrevDigest = cp.ChainDigest
			if dbCP, exists := dbCPByID[cp.CheckpointID]; exists {
				if dbCP.ChainDigest != cp.ChainDigest || dbCP.TailEntryHash != cp.TailEntryHash || dbCP.ToSeq != cp.ToSeq {
					res.IntegrityStatus = IntegrityHashMismatch
					res.BrokenAtSeq = cp.ToSeq
					res.FailureReason = fmt.Sprintf("DB checkpoint %s diverges from external checkpoint log", cp.CheckpointID)
					return res
				}
			} else if len(dbCheckpoints) > 0 && cp.ToSeq <= dbMaxSeq {
				res.IntegrityStatus = IntegrityTruncatedTail
				res.BrokenAtSeq = cp.ToSeq
				res.FailureReason = fmt.Sprintf("external checkpoint %s missing from DB checkpoints table", cp.CheckpointID)
				return res
			}
		}

		allCheckpoints := append([]model.AgentAuditCheckpoint{}, dbCheckpoints...)
		allCheckpoints = append(allCheckpoints, extCheckpoints...)
		for _, cp := range allCheckpoints {
			if dbMaxSeq < cp.ToSeq {
				res.IntegrityStatus = IntegrityTruncatedTail
				res.BrokenAtSeq = dbMaxSeq + 1
				res.FailureReason = fmt.Sprintf("tail truncation detected: DB tail_seq=%d < checkpoint to_seq=%d (%s)", dbMaxSeq, cp.ToSeq, cp.CheckpointID)
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

	if verifyRemote {
		rep := e.CompareWithRemote(context.Background(), streamID)
		res.RemoteStoreConfigured = rep.Configured
		res.RemoteStoreType = rep.StoreType
		res.RemoteSyncStatus = rep.Status
		res.RemoteTailSeq = rep.RemoteTailSeq
		res.RemoteTailHash = rep.RemoteTailHash
		if rep.ExternalSigVerified {
			res.ExternalSigVerified = true
		}
		res.SecurityBoundary = buildSecurityBoundaryNote(
			rep.Configured,
			rep.Reachable,
			rep.Status == RemoteStatusInSync,
			extVerifier != nil,
			res.ExternalSigVerified,
		)
		switch rep.Status {
		case RemoteStatusDBRollback:
			res.Valid = false
			res.IntegrityStatus = IntegrityDBRollbackDetected
			res.FailureReason = strings.Join(rep.Discrepancies, "; ")
			return res
		case RemoteStatusChainDiverged:
			res.Valid = false
			res.IntegrityStatus = IntegrityChainDiverged
			res.FailureReason = strings.Join(rep.Discrepancies, "; ")
			return res
		case RemoteStatusTailTruncated:
			res.Valid = false
			res.IntegrityStatus = IntegrityTruncatedTail
			res.FailureReason = strings.Join(rep.Discrepancies, "; ")
			return res
		case RemoteStatusCheckpointMismatch:
			res.Valid = false
			res.IntegrityStatus = IntegrityCheckpointMismatch
			res.FailureReason = strings.Join(rep.Discrepancies, "; ")
			return res
		}
	}

	res.Valid = true
	if res.RemoteStoreConfigured && res.RemoteSyncStatus == RemoteStatusInSync {
		res.IntegrityStatus = IntegrityVerifiedAgainstRemote
	} else if res.CheckedCheckpoints > 0 || res.LastCheckpointID != "" {
		res.IntegrityStatus = IntegrityVerifiedAgainstCheckpoint
	} else {
		res.IntegrityStatus = IntegrityChainIntactLocalOnly
	}
	return res
}

// VerifyRunChain verifies triggers, hashes, predecessor links, HMACs, and covering checkpoints for a household-scoped run_id.
func (e *AgentAuditEngine) VerifyRunChain(runID string, householdID uint) VerifyResult {
	res := VerifyResult{
		StreamID:       model.AuditPrimaryStreamID,
		LimitationNote: localNASLimitationNote,
		VerifiedAt:     FormatRFC3339Ms(e.nowUTC()),
	}
	if e == nil || e.db == nil || strings.TrimSpace(runID) == "" || householdID == 0 {
		res.IntegrityStatus = IntegrityUnverifiable
		res.FailureReason = "audit database or run scope unavailable"
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
	if !triggersOK {
		res.IntegrityStatus = IntegrityTriggerMissing
		res.FailureReason = trigReason
		return res
	}

	var events []model.AgentAuditEvent
	if err := e.db.Where("run_id = ? AND household_id = ?", strings.TrimSpace(runID), householdID).
		Order("seq ASC").Find(&events).Error; err != nil {
		res.IntegrityStatus = IntegrityUnverifiable
		res.FailureReason = fmt.Sprintf("failed to load run events: %v", err)
		return res
	}
	if len(events) == 0 {
		res.IntegrityStatus = IntegrityUnverifiable
		res.FailureReason = "run has no verified audit events"
		return res
	}

	keyUsedSet := make(map[string]bool)
	res.HeadSeq = events[0].Seq
	for _, ev := range events {
		expectedPrev := model.AuditGenesisHash
		if ev.Seq > 1 {
			var prevHash string
			if err := e.db.Model(&model.AgentAuditEvent{}).
				Where("stream_id = ? AND seq = ?", ev.StreamID, ev.Seq-1).
				Select("entry_hash").Scan(&prevHash).Error; err != nil || prevHash == "" {
				res.IntegrityStatus = IntegritySeqGap
				res.BrokenAtSeq = ev.Seq - 1
				res.FailureReason = fmt.Sprintf("missing predecessor seq %d", ev.Seq-1)
				return res
			}
			expectedPrev = prevHash
		}
		if ev.PreviousHash != expectedPrev {
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
		if macErr != nil || (ev.KeyID != "" && ev.KeyID != kid) {
			res.IntegrityStatus = IntegrityHMACInvalid
			res.BrokenAtSeq = ev.Seq
			res.FailureReason = fmt.Sprintf("HMAC verification failed at seq=%d", ev.Seq)
			return res
		}
		keyUsedSet[kid] = true
		res.CheckedEvents++
		res.TailSeq = ev.Seq
		res.TailEntryHash = ev.EntryHash
	}
	for kid := range keyUsedSet {
		res.KeyIDsUsed = append(res.KeyIDsUsed, kid)
	}
	sort.Strings(res.KeyIDsUsed)

	_, extVerifier := e.activeExternalSignerAndVerifier()
	if e.remoteStore != nil {
		res.RemoteStoreConfigured = true
		res.RemoteStoreType = e.remoteStore.Type()
	}

	var cps []model.AgentAuditCheckpoint
	_ = e.db.Where("stream_id = ? AND to_seq >= ?", model.AuditPrimaryStreamID, res.TailSeq).
		Order("to_seq ASC").Find(&cps).Error
	allExtSigsOK := extVerifier != nil && len(cps) > 0
	for _, cp := range cps {
		if err := VerifyCheckpointHMAC(&cp, ring); err != nil {
			res.IntegrityStatus = IntegrityHMACInvalid
			res.BrokenAtSeq = cp.ToSeq
			res.FailureReason = fmt.Sprintf("checkpoint %s HMAC invalid", cp.CheckpointID)
			return res
		}
		if extVerifier != nil {
			if err := extVerifier.VerifyCheckpointExternal(&cp); err != nil {
				res.IntegrityStatus = IntegrityHMACInvalid
				res.BrokenAtSeq = cp.ToSeq
				res.FailureReason = fmt.Sprintf("checkpoint %s external asymmetric signature invalid", cp.CheckpointID)
				return res
			}
		}
		res.CheckedCheckpoints++
		res.LastCheckpointID = cp.CheckpointID
		res.LastCheckpointSeq = cp.ToSeq
	}
	res.ExternalSigVerified = allExtSigsOK
	res.SecurityBoundary = buildSecurityBoundaryNote(res.RemoteStoreConfigured, false, false, extVerifier != nil, res.ExternalSigVerified)
	res.Valid = true
	if res.CheckedCheckpoints > 0 {
		res.IntegrityStatus = IntegrityVerifiedAgainstCheckpoint
	} else {
		res.IntegrityStatus = IntegrityChainIntactLocalOnly
	}
	return res
}
