package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"

	"gorm.io/gorm"
)

func buildSecurityBoundaryNote(remoteConfigured, remoteReachable, remoteVerified, extAsymConfigured, extAsymVerified bool) string {
	switch {
	case remoteConfigured && remoteVerified && extAsymConfigured && extAsymVerified:
		return "Remote append-only replica and external Ed25519 checkpoint signatures verified. Boundary: prevents undetected fnOS SQLite rollback, tail truncation, or silent history rewrite; a live fnOS root attacker with runtime HMAC env access can still forge pre-replication entries, which external divergence checks catch."
	case remoteConfigured && remoteVerified:
		return "Remote append-only replica verified against local ledger. Boundary: detects fnOS SQLite rollback and tail truncation; without external Ed25519 checkpoint keys, keep the remote store credentials and host strictly isolated from fnOS root."
	case remoteConfigured && !remoteReachable:
		return "Remote replica is configured but currently unreachable; local integrity cannot prove immunity against fnOS host rollback until remote verification completes."
	default:
		return "WARNING (fnOS root-attacker boundary): Local-only NAS storage shares disk and HMAC env secrets with the host OS. A fnOS root attacker can roll back SQLite or forge local entries; configure OPS_AUDIT_REMOTE_* (WORM/HTTP) and OPS_AUDIT_CHECKPOINT_ED25519_* for independent verification."
	}
}

func (e *AgentAuditEngine) activeExternalSignerAndVerifier() (ExternalCheckpointSigner, ExternalCheckpointVerifier) {
	signer := e.externalSigner
	verifier := e.externalVerifier
	if signer == nil && verifier == nil {
		if envSigner, envVerifier, err := ResolveExternalCheckpointKeys(); err == nil {
			signer = envSigner
			verifier = envVerifier
		}
	}
	if verifier == nil && signer != nil {
		if v, ok := signer.(ExternalCheckpointVerifier); ok {
			verifier = v
		}
	}
	return signer, verifier
}

func (e *AgentAuditEngine) saveRemoteSyncState(state *model.AgentAuditRemoteSyncState) error {
	if e == nil || e.db == nil {
		return ErrAuditCommitFailed
	}
	if state == nil {
		return nil
	}
	if state.StreamID == "" {
		state.StreamID = model.AuditPrimaryStreamID
	}
	state.LastError = redact.SensitiveString(state.LastError)
	state.UpdatedAtMs = e.nowUTC().UnixMilli()
	if err := e.db.Exec(`
		INSERT INTO agent_audit_remote_sync_states (
			stream_id, remote_store_type, remote_endpoint,
			last_synced_seq, last_synced_entry_hash,
			last_synced_checkpoint_id, last_synced_checkpoint_seq,
			last_sync_at_ms, last_error, consecutive_failures,
			sync_status, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(stream_id) DO UPDATE SET
			remote_store_type = excluded.remote_store_type,
			remote_endpoint = excluded.remote_endpoint,
			last_synced_seq = CASE WHEN excluded.last_synced_seq > agent_audit_remote_sync_states.last_synced_seq THEN excluded.last_synced_seq ELSE agent_audit_remote_sync_states.last_synced_seq END,
			last_synced_entry_hash = CASE WHEN excluded.last_synced_entry_hash <> '' THEN excluded.last_synced_entry_hash ELSE agent_audit_remote_sync_states.last_synced_entry_hash END,
			last_synced_checkpoint_id = CASE WHEN excluded.last_synced_checkpoint_id <> '' THEN excluded.last_synced_checkpoint_id ELSE agent_audit_remote_sync_states.last_synced_checkpoint_id END,
			last_synced_checkpoint_seq = CASE WHEN excluded.last_synced_checkpoint_seq > agent_audit_remote_sync_states.last_synced_checkpoint_seq THEN excluded.last_synced_checkpoint_seq ELSE agent_audit_remote_sync_states.last_synced_checkpoint_seq END,
			last_sync_at_ms = CASE WHEN excluded.last_sync_at_ms > 0 THEN excluded.last_sync_at_ms ELSE agent_audit_remote_sync_states.last_sync_at_ms END,
			last_error = excluded.last_error,
			consecutive_failures = excluded.consecutive_failures,
			sync_status = excluded.sync_status,
			updated_at_ms = excluded.updated_at_ms`,
		state.StreamID, state.RemoteStoreType, state.RemoteEndpoint,
		state.LastSyncedSeq, state.LastSyncedEntryHash,
		state.LastSyncedCheckpointID, state.LastSyncedCheckpointSeq,
		state.LastSyncAtMs, state.LastError, state.ConsecutiveFailures,
		state.SyncStatus, state.UpdatedAtMs,
	).Error; err != nil {
		msg := redact.SensitiveString(fmt.Sprintf("persist remote sync state failed: %v", err))
		e.recordFailureMetric()
		e.publishRemoteAlert("ops.audit.remote_sync_state_persist_failed", msg)
		return fmt.Errorf("%w: %s", ErrAuditCommitFailed, msg)
	}
	return nil
}

// GetRemoteSyncState returns the persisted replication cursor for streamID.
func (e *AgentAuditEngine) GetRemoteSyncState(streamID string) (*model.AgentAuditRemoteSyncState, error) {
	if e == nil || e.db == nil {
		return nil, ErrAuditCommitFailed
	}
	if strings.TrimSpace(streamID) == "" {
		streamID = model.AuditPrimaryStreamID
	}
	var st model.AgentAuditRemoteSyncState
	err := e.db.Where("stream_id = ?", streamID).First(&st).Error
	if err != nil {
		storeType := "none"
		endpoint := ""
		status := RemoteStatusLocalOnly
		if e.remoteStore != nil {
			storeType = e.remoteStore.Type()
			endpoint = e.remoteStore.EndpointDisplay()
			status = RemoteStatusPendingSync
		}
		return &model.AgentAuditRemoteSyncState{
			StreamID:        streamID,
			RemoteStoreType: storeType,
			RemoteEndpoint:  endpoint,
			SyncStatus:      status,
		}, nil
	}
	if st.SyncStatus == "" || st.SyncStatus == "DISABLED" {
		if e.remoteStore == nil {
			st.SyncStatus = RemoteStatusLocalOnly
		} else {
			st.SyncStatus = RemoteStatusPendingSync
		}
	}
	return &st, nil
}

func (e *AgentAuditEngine) publishRemoteAlert(topic string, msg string) {
	if e == nil || e.bus == nil {
		return
	}
	raw, err := json.Marshal(map[string]interface{}{
		"stream_id": model.AuditPrimaryStreamID,
		"message":   redact.SensitiveString(msg),
	})
	if err != nil {
		return
	}
	e.bus.Publish(eventbus.Event{
		Topic:     topic,
		Source:    eventbus.SourceOps,
		Severity:  eventbus.SeverityCritical,
		Payload:   raw,
		Timestamp: e.nowUTC(),
	})
}

// SyncRemote pushes un-replicated local audit events and signed checkpoints to the remote append-only store.
// Before pushing, it validates remote tail consistency to avoid overwriting or masking local DB rollback/forks.
func (e *AgentAuditEngine) SyncRemote(ctx context.Context, streamID string) RemoteDiffReport {
	if strings.TrimSpace(streamID) == "" {
		streamID = model.AuditPrimaryStreamID
	}
	if e == nil || e.db == nil || e.remoteStore == nil {
		return e.CompareWithRemote(ctx, streamID)
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
	}

	e.remoteMu.Lock()
	defer e.remoteMu.Unlock()

	prevState, prevErr := e.GetRemoteSyncState(streamID)
	failures := 0
	if prevErr == nil && prevState != nil {
		failures = prevState.ConsecutiveFailures
	}

	reportErr := func(status string, err error) RemoteDiffReport {
		errMsg := redact.SensitiveString(err.Error())
		e.recordFailureMetric()
		if saveErr := e.saveRemoteSyncState(&model.AgentAuditRemoteSyncState{
			StreamID:            streamID,
			RemoteStoreType:     e.remoteStore.Type(),
			RemoteEndpoint:      e.remoteStore.EndpointDisplay(),
			LastError:           errMsg,
			ConsecutiveFailures: failures + 1,
			SyncStatus:          status,
		}); saveErr != nil {
			errMsg = fmt.Sprintf("%s; %s", errMsg, redact.SensitiveString(saveErr.Error()))
		}
		e.publishRemoteAlert("ops.audit.remote_sync_failed", errMsg)
		rep := e.CompareWithRemote(ctx, streamID)
		if rep.Error == "" {
			rep.Error = errMsg
		} else if !strings.Contains(rep.Error, errMsg) {
			rep.Error = fmt.Sprintf("%s; %s", rep.Error, errMsg)
		}
		if rep.Status == "" || rep.Status == RemoteStatusInSync || rep.Status == RemoteStatusLocalOnly {
			rep.Status = status
		}
		return rep
	}

	remoteTail, err := e.remoteStore.FetchTail(ctx, streamID)
	if err != nil {
		return reportErr(RemoteStatusUnreachable, err)
	}

	var localTail model.AgentAuditEvent
	if qErr := e.db.Where("stream_id = ?", streamID).Order("seq DESC").First(&localTail).Error; qErr != nil && !errors.Is(qErr, gorm.ErrRecordNotFound) {
		return reportErr(RemoteStatusUnreachable, qErr)
	}

	// Abort replication immediately if remote has progressed beyond local DB (rollback/truncation)
	// or if local history at remote's tail seq diverges from remote hash.
	if remoteTail.TailSeq > localTail.Seq {
		err := fmt.Errorf("local DB rollback/truncation detected before sync: local_tail=%d < remote_tail=%d", localTail.Seq, remoteTail.TailSeq)
		return reportErr(RemoteStatusDBRollback, err)
	}
	if remoteTail.TailSeq > 0 {
		var localAtRemote model.AgentAuditEvent
		if qErr := e.db.Where("stream_id = ? AND seq = ?", streamID, remoteTail.TailSeq).First(&localAtRemote).Error; qErr != nil {
			return reportErr(RemoteStatusDBRollback, fmt.Errorf("missing local event at remote tail seq=%d", remoteTail.TailSeq))
		}
		if localAtRemote.EntryHash != remoteTail.TailEntryHash {
			return reportErr(RemoteStatusChainDiverged, fmt.Errorf("local hash at seq=%d (%s) diverges from remote tail (%s)", remoteTail.TailSeq, localAtRemote.EntryHash, remoteTail.TailEntryHash))
		}
	}

	const batchSize = 200
	cursorSeq := remoteTail.TailSeq
	lastSyncedHash := remoteTail.TailEntryHash
	for {
		var batch []model.AgentAuditEvent
		if qErr := e.db.Where("stream_id = ? AND seq > ?", streamID, cursorSeq).
			Order("seq ASC").Limit(batchSize).Find(&batch).Error; qErr != nil {
			return reportErr(RemoteStatusUnreachable, qErr)
		}
		if len(batch) == 0 {
			break
		}
		if aErr := e.remoteStore.AppendEvents(ctx, streamID, batch); aErr != nil {
			status := RemoteStatusUnreachable
			if errors.Is(aErr, ErrRemoteConflict) {
				status = RemoteStatusChainDiverged
			}
			return reportErr(status, aErr)
		}
		last := batch[len(batch)-1]
		cursorSeq = last.Seq
		lastSyncedHash = last.EntryHash
		if len(batch) < batchSize {
			break
		}
	}

	var unsyncedCPs []model.AgentAuditCheckpoint
	if qErr := e.db.Where("stream_id = ? AND to_seq > ?", streamID, remoteTail.LastCheckpointSeq).
		Order("to_seq ASC").Find(&unsyncedCPs).Error; qErr != nil {
		return reportErr(RemoteStatusUnreachable, qErr)
	}
	lastCPID := remoteTail.LastCheckpointID
	lastCPSeq := remoteTail.LastCheckpointSeq
	for _, cp := range unsyncedCPs {
		if sErr := e.remoteStore.SubmitCheckpoint(ctx, cp); sErr != nil {
			status := RemoteStatusUnreachable
			if errors.Is(sErr, ErrRemoteConflict) {
				status = RemoteStatusCheckpointMismatch
			}
			return reportErr(status, sErr)
		}
		lastCPID = cp.CheckpointID
		lastCPSeq = cp.ToSeq
	}

	if saveErr := e.saveRemoteSyncState(&model.AgentAuditRemoteSyncState{
		StreamID:                streamID,
		RemoteStoreType:         e.remoteStore.Type(),
		RemoteEndpoint:          e.remoteStore.EndpointDisplay(),
		LastSyncedSeq:           cursorSeq,
		LastSyncedEntryHash:     lastSyncedHash,
		LastSyncedCheckpointID:  lastCPID,
		LastSyncedCheckpointSeq: lastCPSeq,
		LastSyncAtMs:            e.nowUTC().UnixMilli(),
		LastError:               "",
		ConsecutiveFailures:     0,
		SyncStatus:              RemoteStatusInSync,
	}); saveErr != nil {
		saveMsg := redact.SensitiveString(saveErr.Error())
		e.publishRemoteAlert("ops.audit.remote_sync_failed", saveMsg)
		rep := e.CompareWithRemote(ctx, streamID)
		rep.Error = saveMsg
		if rep.Status == "" || rep.Status == RemoteStatusInSync {
			rep.Status = RemoteStatusPendingSync
		}
		return rep
	}

	return e.CompareWithRemote(ctx, streamID)
}

// SyncRemoteStrict executes SyncRemote and returns an explicit error on remote unavailability,
// persistence error, rollback/truncation, chain divergence, or strict-remote misconfiguration.
func (e *AgentAuditEngine) SyncRemoteStrict(ctx context.Context, streamID string) (RemoteDiffReport, error) {
	if e == nil || e.db == nil {
		return RemoteDiffReport{StoreType: "none", Status: RemoteStatusLocalOnly, Error: ErrAuditCommitFailed.Error()}, ErrAuditCommitFailed
	}
	if e.remoteStore == nil {
		rep := e.CompareWithRemote(ctx, streamID)
		if e.strictRemote {
			return rep, fmt.Errorf("%w: remote audit store is not configured", ErrAuditRemoteRequired)
		}
		return rep, nil
	}
	rep := e.SyncRemote(ctx, streamID)
	if rep.Error != "" || (rep.Status != RemoteStatusInSync && rep.Status != RemoteStatusLocalOnly) {
		return rep, fmt.Errorf("%w: status=%s: %s", ErrAuditSyncFailed, rep.Status, rep.Error)
	}
	return rep, nil
}

// CompareWithRemote inspects local SQLite vs external remote WORM/HTTP store to detect
// tail truncation, whole-DB rollback, chain divergence, checkpoint mismatch, and external Ed25519 signature validity.
func (e *AgentAuditEngine) CompareWithRemote(ctx context.Context, streamID string) RemoteDiffReport {
	if strings.TrimSpace(streamID) == "" {
		streamID = model.AuditPrimaryStreamID
	}
	rep := RemoteDiffReport{
		StoreType: "none",
		Status:    RemoteStatusLocalOnly,
		CheckedAt: FormatRFC3339Ms(e.nowUTC()),
	}
	if e == nil || e.db == nil {
		rep.Error = "audit database unavailable"
		return rep
	}

	var localTail model.AgentAuditEvent
	if qErr := e.db.Where("stream_id = ?", streamID).Order("seq DESC").First(&localTail).Error; qErr != nil && !errors.Is(qErr, gorm.ErrRecordNotFound) {
		rep.Error = redact.SensitiveString(qErr.Error())
		return rep
	}
	rep.LocalTailSeq = localTail.Seq
	rep.LocalTailHash = localTail.EntryHash

	var localCPs []model.AgentAuditCheckpoint
	if qErr := e.db.Where("stream_id = ?", streamID).Order("to_seq ASC").Find(&localCPs).Error; qErr != nil {
		rep.Error = redact.SensitiveString(qErr.Error())
		return rep
	}
	localCPByID := make(map[string]model.AgentAuditCheckpoint, len(localCPs))
	for _, cp := range localCPs {
		localCPByID[cp.CheckpointID] = cp
		if cp.ToSeq >= rep.LocalCheckpointSeq {
			rep.LocalCheckpointSeq = cp.ToSeq
		}
	}

	if e.remoteStore == nil {
		return rep
	}
	rep.Configured = true
	rep.StoreType = e.remoteStore.Type()
	rep.Endpoint = e.remoteStore.EndpointDisplay()

	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}

	remoteTail, err := e.remoteStore.FetchTail(ctx, streamID)
	if err != nil {
		rep.Reachable = false
		rep.Status = RemoteStatusUnreachable
		rep.Error = redact.SensitiveString(err.Error())
		return rep
	}
	remoteCPs, cpErr := e.remoteStore.FetchCheckpoints(ctx, streamID)
	if cpErr != nil {
		rep.Reachable = false
		rep.Status = RemoteStatusUnreachable
		rep.Error = redact.SensitiveString(cpErr.Error())
		return rep
	}

	rep.Reachable = true
	rep.RemoteTailSeq = remoteTail.TailSeq
	rep.RemoteTailHash = remoteTail.TailEntryHash
	rep.RemoteCheckpointID = remoteTail.LastCheckpointID
	rep.RemoteCheckpointSeq = remoteTail.LastCheckpointSeq
	rep.RemoteChainDigest = remoteTail.LastChainDigest

	// Verify external Ed25519 asymmetric checkpoint signatures when configured.
	_, extVerifier := e.activeExternalSignerAndVerifier()
	if extVerifier != nil && len(remoteCPs) > 0 {
		allValid := true
		for i := range remoteCPs {
			cp := remoteCPs[i]
			if vErr := extVerifier.VerifyCheckpointExternal(&cp); vErr != nil {
				allValid = false
				rep.Discrepancies = append(rep.Discrepancies, fmt.Sprintf("external signature check failed on remote checkpoint %s: %v", cp.CheckpointID, vErr))
			}
		}
		rep.ExternalSigVerified = allValid
	}

	// Verify checkpoint chain digest linkage & cross-check remote checkpoints vs local DB checkpoints.
	prevDigest := model.AuditGenesisHash
	for _, rcp := range remoteCPs {
		expectedDigest := ComputeCheckpointChainDigest(prevDigest, rcp.TailEntryHash, rcp.EventCount)
		if rcp.PrevChainDigest != prevDigest || rcp.ChainDigest != expectedDigest {
			rep.Discrepancies = append(rep.Discrepancies, fmt.Sprintf("remote checkpoint %s chain_digest broken", rcp.CheckpointID))
		}
		prevDigest = rcp.ChainDigest
		if lcp, ok := localCPByID[rcp.CheckpointID]; ok {
			if lcp.ChainDigest != rcp.ChainDigest || lcp.TailEntryHash != rcp.TailEntryHash || lcp.ToSeq != rcp.ToSeq {
				rep.Discrepancies = append(rep.Discrepancies, fmt.Sprintf("checkpoint %s diverges between local DB and remote store", rcp.CheckpointID))
			}
		} else if rcp.ToSeq <= rep.LocalTailSeq {
			rep.Discrepancies = append(rep.Discrepancies, fmt.Sprintf("remote checkpoint %s (to_seq=%d) missing from local DB", rcp.CheckpointID, rcp.ToSeq))
		}
		if rcp.ToSeq > rep.RemoteCheckpointSeq {
			rep.RemoteCheckpointSeq = rcp.ToSeq
			rep.RemoteCheckpointID = rcp.CheckpointID
			rep.RemoteChainDigest = rcp.ChainDigest
		}
	}

	// Detect whole-DB rollback and tail truncation.
	rollbackDetected := false
	tailTruncated := false
	chainDiverged := false

	if remoteTail.TailSeq > rep.LocalTailSeq {
		if rep.LocalTailSeq == 0 {
			rollbackDetected = true
			rep.Discrepancies = append(rep.Discrepancies, fmt.Sprintf("local DB empty while remote store holds tail seq=%d", remoteTail.TailSeq))
		} else {
			// Check whether overlapping prefix matches or was rolled back + rewritten.
			remotePrefix, rErr := e.remoteStore.FetchEventsRange(ctx, streamID, rep.LocalTailSeq, rep.LocalTailSeq)
			if rErr == nil && len(remotePrefix) == 1 && remotePrefix[0].EntryHash != rep.LocalTailHash {
				rollbackDetected = true
				chainDiverged = true
				rep.Discrepancies = append(rep.Discrepancies, fmt.Sprintf("DB rollback + fork at seq=%d: local hash %s != remote %s", rep.LocalTailSeq, rep.LocalTailHash, remotePrefix[0].EntryHash))
			} else {
				tailTruncated = true
				rep.Discrepancies = append(rep.Discrepancies, fmt.Sprintf("local tail truncated: local seq=%d < remote seq=%d", rep.LocalTailSeq, remoteTail.TailSeq))
			}
		}
	} else if rep.RemoteCheckpointSeq > rep.LocalTailSeq {
		tailTruncated = true
		rep.Discrepancies = append(rep.Discrepancies, fmt.Sprintf("local tail seq=%d < remote checkpoint to_seq=%d", rep.LocalTailSeq, rep.RemoteCheckpointSeq))
	} else if remoteTail.TailSeq > 0 {
		var localAtRemote model.AgentAuditEvent
		if qErr := e.db.Where("stream_id = ? AND seq = ?", streamID, remoteTail.TailSeq).First(&localAtRemote).Error; qErr != nil {
			rollbackDetected = true
			rep.Discrepancies = append(rep.Discrepancies, fmt.Sprintf("missing local event for remote tail seq=%d", remoteTail.TailSeq))
		} else if localAtRemote.EntryHash != remoteTail.TailEntryHash {
			chainDiverged = true
			rep.Discrepancies = append(rep.Discrepancies, fmt.Sprintf("chain diverged at seq=%d: local hash %s != remote hash %s", remoteTail.TailSeq, localAtRemote.EntryHash, remoteTail.TailEntryHash))
		}
	}

	switch {
	case rollbackDetected:
		rep.Status = RemoteStatusDBRollback
	case chainDiverged:
		rep.Status = RemoteStatusChainDiverged
	case tailTruncated:
		rep.Status = RemoteStatusTailTruncated
	case len(rep.Discrepancies) > 0:
		rep.Status = RemoteStatusCheckpointMismatch
	case rep.LocalTailSeq > remoteTail.TailSeq:
		rep.Status = RemoteStatusPendingSync
		rep.PendingEvents = rep.LocalTailSeq - remoteTail.TailSeq
	default:
		rep.Status = RemoteStatusInSync
	}

	return rep
}

// RecordKeyRotationIfNeeded checks whether the active HMAC key_id differs from the latest recorded event's key_id
// and emits an immutable AUDIT_KEY_ROTATED lifecycle event when a rotation occurred.
func (e *AgentAuditEngine) RecordKeyRotationIfNeeded(ctx context.Context) (*model.AgentAuditEvent, error) {
	if e == nil || e.db == nil {
		return nil, nil
	}
	activeKID, ring, err := ResolveAuditHMACKeyring()
	if err != nil || activeKID == "" {
		return nil, err
	}

	var prevKID string
	_ = e.db.Model(&model.AgentAuditEvent{}).
		Where("stream_id = ? AND key_id <> ''", model.AuditPrimaryStreamID).
		Order("seq DESC").Limit(1).
		Select("key_id").Scan(&prevKID).Error

	if prevKID == "" || prevKID == activeKID {
		return nil, nil
	}

	var configuredKIDs []string
	for k := range ring {
		configuredKIDs = append(configuredKIDs, k)
	}
	sort.Strings(configuredKIDs)

	if ctx == nil {
		ctx = context.Background()
	}
	return e.EmitEvent(ctx, EmitEventInput{
		HouseholdID:   1,
		TriggerSource: "audit_keyring",
		ActorType:     "system",
		ActorID:       "audit-engine",
		EventType:     model.AuditEventAuditKeyRotated,
		Action:        "audit.key_rotated",
		Target:        model.AuditPrimaryStreamID,
		Component:     "audit-engine",
		Decision:      model.AuditDecisionAllow,
		RiskLevel:     model.AuditRiskHigh,
		BeforeState:   map[string]string{"active_key_id": prevKID},
		AfterState:    map[string]string{"active_key_id": activeKID},
		Parameters:    map[string]interface{}{"configured_key_ids": configuredKIDs},
		Result:        "SUCCESS",
		FinalRunStatus: model.AuditRunStatusCompleted,
		RunSummary:    fmt.Sprintf("Audit HMAC key rotated from %s to %s", prevKID, activeKID),
	})
}

// GetKeyStatus returns non-secret HMAC key lifecycle metadata, asymmetric checkpoint public key metadata,
// rotation history counts, and the explicit fnOS root-attacker boundary statement.
func (e *AgentAuditEngine) GetKeyStatus() AuditKeyStatus {
	activeKID, ring, err := ResolveAuditHMACKeyring()
	status := AuditKeyStatus{
		ConfiguredKeyIDs: []string{},
		HistoricalKeyIDs: []string{},
	}
	if err == nil {
		status.ActiveKeyID = activeKID
		for k := range ring {
			status.ConfiguredKeyIDs = append(status.ConfiguredKeyIDs, k)
		}
		sort.Strings(status.ConfiguredKeyIDs)
	}

	signer, verifier := e.activeExternalSignerAndVerifier()
	if verifier != nil {
		status.ExternalSigAlg = verifier.Algorithm()
		status.ExternalKeyID = verifier.KeyID()
		status.ExternalPublicKey = verifier.PublicKeyHex()
	} else if signer != nil {
		status.ExternalSigAlg = signer.Algorithm()
		status.ExternalKeyID = signer.KeyID()
		status.ExternalPublicKey = signer.PublicKeyHex()
	}

	if e != nil && e.remoteStore != nil {
		status.RemoteStoreConfigured = true
		status.RemoteStoreType = e.remoteStore.Type()
	}

	if e != nil && e.db != nil {
		histSet := make(map[string]bool)
		var eventKIDs []string
		_ = e.db.Model(&model.AgentAuditEvent{}).
			Where("key_id <> ''").
			Distinct("key_id").
			Pluck("key_id", &eventKIDs).Error
		for _, k := range eventKIDs {
			histSet[k] = true
		}
		var cpKIDs []string
		_ = e.db.Model(&model.AgentAuditCheckpoint{}).
			Where("key_id <> ''").
			Distinct("key_id").
			Pluck("key_id", &cpKIDs).Error
		for _, k := range cpKIDs {
			histSet[k] = true
		}
		for k := range histSet {
			status.HistoricalKeyIDs = append(status.HistoricalKeyIDs, k)
		}
		sort.Strings(status.HistoricalKeyIDs)

		var rotCount int64
		_ = e.db.Model(&model.AgentAuditEvent{}).
			Where("event_type = ?", model.AuditEventAuditKeyRotated).
			Count(&rotCount).Error
		status.RotationCount = rotCount

		var lastRotMs int64
		_ = e.db.Model(&model.AgentAuditEvent{}).
			Where("event_type = ?", model.AuditEventAuditKeyRotated).
			Order("seq DESC").Limit(1).
			Select("created_at_ms").Scan(&lastRotMs).Error
		if lastRotMs > 0 {
			status.LastRotationAt = FormatRFC3339Ms(time.UnixMilli(lastRotMs).UTC())
		}
	}

	status.SecurityBoundaryNote = buildSecurityBoundaryNote(
		status.RemoteStoreConfigured,
		false,
		false,
		status.ExternalSigAlg != "",
		false,
	)
	if e != nil && e.remoteStore != nil {
		rep := e.CompareWithRemote(context.Background(), model.AuditPrimaryStreamID)
		status.SecurityBoundaryNote = buildSecurityBoundaryNote(
			true,
			rep.Reachable,
			rep.Status == RemoteStatusInSync || rep.Status == RemoteStatusPendingSync,
			status.ExternalSigAlg != "",
			rep.ExternalSigVerified,
		)
	}

	return status
}
