package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
)

type originStreamTip struct {
	lastSeq  uint64
	lastHash string
}

// IngestOfflineEvents validates, deduplicates, verifies signatures/hashes/sequence-continuity,
// and merges offline Python edge-agent audit events into the authoritative core hash chain.
func (e *AgentAuditEngine) IngestOfflineEvents(ctx context.Context, events []OfflineSyncEvent, requestID, clientIP string, householdID uint) (*OfflineSyncResult, error) {
	if e == nil || e.db == nil {
		return nil, fmt.Errorf("%w: audit database unavailable", ErrAuditCommitFailed)
	}
	if householdID == 0 {
		householdID = 1
	}

	result := &OfflineSyncResult{
		AcceptedUUIDs: make([]string, 0, len(events)),
	}
	if len(events) == 0 {
		result.ChainStatus = IntegrityChainIntactLocalOnly
		return result, nil
	}

	kid, ring, kErr := ResolveAuditHMACKeyring()
	if kErr != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuditCommitFailed, kErr)
	}
	secret := ring[kid]

	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	var insertedEvents []*model.AgentAuditEvent
	err := e.runImmediate(func(conn *sql.Conn) error {
		var lastSeq uint64
		var lastHash string
		row := conn.QueryRowContext(ctx,
			`SELECT seq, entry_hash FROM agent_audit_events WHERE stream_id = ? ORDER BY seq DESC LIMIT 1`,
			model.AuditPrimaryStreamID,
		)
		if sErr := row.Scan(&lastSeq, &lastHash); sErr != nil && sErr != sql.ErrNoRows {
			return sErr
		}
		if lastHash == "" {
			lastHash = model.AuditGenesisHash
		}

		streamTips := make(map[string]originStreamTip)
		syncedBatch := make([]map[string]interface{}, 0, len(events))
		var tailOriginHash string

		recordReject := func(uuid, code, reason string) {
			result.Rejected = append(result.Rejected, OfflineSyncRejection{
				EventUUID: uuid,
				ErrorCode: code,
				Reason:    reason,
			})
			result.IntegrityAlerts = append(result.IntegrityAlerts, fmt.Sprintf("[%s] %s: %s", code, uuid, reason))
		}

		for _, item := range events {
			rawUUID := strings.TrimSpace(item.EventUUID)
			rawAuditID := strings.TrimSpace(item.AuditID)
			uuid := defaultStr(rawUUID, rawAuditID)
			auditID := defaultStr(rawAuditID, rawUUID)
			if uuid == "" {
				recordReject("", "MISSING_ID", "missing event_uuid/audit_id")
				continue
			}

			// Idempotency lookup across both primary audit table and legacy structured log table.
			var existingOriginHash string
			idErr := conn.QueryRowContext(ctx,
				`SELECT origin_entry_hash FROM agent_audit_events WHERE audit_id = ? OR audit_id = ? LIMIT 1`,
				auditID, uuid,
			).Scan(&existingOriginHash)
			if idErr == nil {
				incomingHash := strings.TrimSpace(item.EntryHash)
				if incomingHash != "" && existingOriginHash != "" && incomingHash != existingOriginHash {
					recordReject(uuid, "DUPLICATE_ID_CONFLICT", "audit_id already exists with different origin_entry_hash (possible replay tamper)")
					continue
				}
				result.AcceptedUUIDs = append(result.AcceptedUUIDs, uuid)
				continue
			} else if idErr != sql.ErrNoRows {
				return idErr
			}

			var legacyCount int64
			if err := conn.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM structured_ops_audit_logs WHERE event_uuid = ? OR event_uuid = ?`,
				uuid, auditID,
			).Scan(&legacyCount); err != nil {
				return err
			}
			if legacyCount > 0 {
				result.AcceptedUUIDs = append(result.AcceptedUUIDs, uuid)
				continue
			}

			// Fail-closed cryptographic proof requirement: never ingest unsigned/unchained offline events.
			originHash := strings.TrimSpace(item.EntryHash)
			if item.OfflineSeq == 0 || originHash == "" || strings.TrimSpace(item.HMACSignature) == "" {
				recordReject(uuid, "MISSING_CRYPTO_PROOF", "offline audit event missing required offline_seq, entry_hash, or hmac_signature")
				continue
			}

			ts := parseOfflineTimestamp(item.ExecutedAt, item.ExecutedAtMs, e.nowUTC())
			tsMs := ts.UnixMilli()
			if item.ExecutedAtMs > 0 {
				tsMs = item.ExecutedAtMs
				ts = time.UnixMilli(tsMs).UTC().Truncate(time.Millisecond)
			}

			itemHouseholdID := item.HouseholdID
			if itemHouseholdID == 0 {
				itemHouseholdID = householdID
			}
			originStream := defaultStr(strings.TrimSpace(item.OriginStreamID), "offline-agent")
			policyVer := defaultStr(strings.TrimSpace(item.PolicyVersion), PolicyVersion)

			rawTarget := defaultStr(strings.TrimSpace(item.Target), strings.TrimSpace(item.ContainerName))
			component := defaultStr(item.Component, item.ContainerName)
			action := defaultStr(item.Action, "container.emergency_restart")
			eventType := defaultStr(item.EventType, model.AuditEventExecutionCompleted)
			actorType := defaultStr(item.ActorType, "offline_agent")
			actorID := defaultStr(item.ActorID, defaultStr(item.Holder, "home-ops-agent"))
			trig := defaultStr(item.TriggerSource, "python_offline")
			decision := defaultStr(item.Decision, model.AuditDecisionAllow)
			riskLevel := defaultStr(item.RiskLevel, model.AuditRiskHigh)
			runID := defaultStr(item.RunID, "run-offline-"+uuid)
			corrID := defaultStr(item.CorrelationID, defaultStr(requestID, "corr-"+uuid))
			proposalID := strings.TrimSpace(item.ProposalID)
			approvalID := strings.TrimSpace(item.ApprovalID)
			operationID := strings.TrimSpace(item.OperationID)
			resLabel := "SUCCESS"
			if item.Success == 0 {
				resLabel = "FAILED"
			}

			// Verify origin entry_hash and HMAC against exact incoming signed payload before Core re-redaction.
			localPrevHash := defaultStr(strings.TrimSpace(item.LocalPrevHash), model.AuditGenesisHash)
			verifyCopy := model.AgentAuditEvent{
				StreamID:           originStream,
				Seq:                item.OfflineSeq,
				AuditID:            auditID,
				RunID:              runID,
				ParentEventID:      strings.TrimSpace(item.ParentEventID),
				CorrelationID:      corrID,
				HouseholdID:        itemHouseholdID,
				TriggerSource:      trig,
				ActorType:          actorType,
				ActorID:            actorID,
				EventType:          eventType,
				Action:             action,
				Target:             rawTarget,
				Component:          component,
				Decision:           decision,
				RiskLevel:          riskLevel,
				PolicyVersion:      policyVer,
				ProposalID:         proposalID,
				ApprovalID:         approvalID,
				OperationID:        operationID,
				FencingGeneration:  item.FencingGeneration,
				BeforeState:        item.BeforeState,
				AfterState:         item.AfterState,
				ParametersDigest:   strings.TrimSpace(item.ParametersDigest),
				ParametersRedacted: item.ParametersRedacted,
				Result:             resLabel,
				ErrorCode:          strings.TrimSpace(item.ErrorMessage),
				DurationMs:         item.DurationMs,
				Evidence:           item.Evidence,
				CreatedAt:          ts,
				CreatedAtMs:        tsMs,
				IsOffline:          true,
				PreviousHash:       localPrevHash,
			}
			expectedOriginHash, hErr := ComputeEventEntryHash(&verifyCopy)
			if hErr != nil || expectedOriginHash != originHash {
				recordReject(uuid, "HASH_MISMATCH", fmt.Sprintf("offline entry_hash mismatch (got %s, expected %s)", originHash, expectedOriginHash))
				continue
			}
			if _, mErr := VerifyOfflineOriginHMACWithKID(item, ring); mErr != nil {
				recordReject(uuid, "HMAC_INVALID", fmt.Sprintf("offline HMAC verification failed: %v", mErr))
				continue
			}

			// Cross-device / fork duplicate sequence detection on (origin_stream_id, origin_seq).
			var existingSeqAuditID string
			seqDupErr := conn.QueryRowContext(ctx,
				`SELECT audit_id FROM agent_audit_events WHERE origin_stream_id = ? AND origin_seq = ? LIMIT 1`,
				originStream, item.OfflineSeq,
			).Scan(&existingSeqAuditID)
			if seqDupErr == nil {
				recordReject(uuid, "DUPLICATE_SEQ_CONFLICT", fmt.Sprintf("origin_stream_id=%s origin_seq=%d already occupied by %s", originStream, item.OfflineSeq, existingSeqAuditID))
				continue
			} else if seqDupErr != sql.ErrNoRows {
				return seqDupErr
			}

			tip, loaded := streamTips[originStream]
			if !loaded {
				var dbTipSeq uint64
				var dbTipHash string
				tipErr := conn.QueryRowContext(ctx,
					`SELECT origin_seq, origin_entry_hash FROM agent_audit_events WHERE origin_stream_id = ? AND origin_seq > 0 ORDER BY origin_seq DESC LIMIT 1`,
					originStream,
				).Scan(&dbTipSeq, &dbTipHash)
				if tipErr != nil && tipErr != sql.ErrNoRows {
					return tipErr
				}
				tip = originStreamTip{lastSeq: dbTipSeq, lastHash: dbTipHash}
				streamTips[originStream] = tip
			}

			if tip.lastSeq == 0 {
				if item.OfflineSeq != 1 {
					recordReject(uuid, "SEQ_GAP", fmt.Sprintf("expected initial offline_seq=1 for stream %s, got %d (missing preceding offline events)", originStream, item.OfflineSeq))
					continue
				}
				if localPrevHash != model.AuditGenesisHash {
					recordReject(uuid, "CHAIN_FORK", fmt.Sprintf("initial offline event for stream %s must link to genesis hash", originStream))
					continue
				}
			} else {
				if item.OfflineSeq <= tip.lastSeq {
					recordReject(uuid, "DUPLICATE_SEQ_CONFLICT", fmt.Sprintf("non-monotonic offline_seq=%d <= tip=%d on stream %s", item.OfflineSeq, tip.lastSeq, originStream))
					continue
				}
				if item.OfflineSeq > tip.lastSeq+1 {
					recordReject(uuid, "SEQ_GAP", fmt.Sprintf("missing offline event(s) on stream %s: expected offline_seq=%d, got %d", originStream, tip.lastSeq+1, item.OfflineSeq))
					continue
				}
				if localPrevHash != tip.lastHash {
					recordReject(uuid, "CHAIN_FORK", fmt.Sprintf("local_prev_hash mismatch on stream %s at offline_seq=%d", originStream, item.OfflineSeq))
					continue
				}
			}

			target := redact.SensitiveString(rawTarget)
			ev := &model.AgentAuditEvent{
				StreamID:           model.AuditPrimaryStreamID,
				AuditID:            auditID,
				RunID:              runID,
				ParentEventID:      strings.TrimSpace(item.ParentEventID),
				CorrelationID:      corrID,
				HouseholdID:        itemHouseholdID,
				TriggerSource:      trig,
				ActorType:          actorType,
				ActorID:            actorID,
				EventType:          eventType,
				Action:             action,
				Target:             target,
				Component:          component,
				Decision:           decision,
				RiskLevel:          riskLevel,
				PolicyVersion:      policyVer,
				ProposalID:         proposalID,
				ApprovalID:         approvalID,
				OperationID:        operationID,
				FencingGeneration:  item.FencingGeneration,
				BeforeState:        CapAndRedactText(item.BeforeState, MaxEvidenceBytes),
				AfterState:         CapAndRedactText(item.AfterState, MaxEvidenceBytes),
				ParametersDigest:   strings.TrimSpace(item.ParametersDigest),
				ParametersRedacted: CapAndRedactText(item.ParametersRedacted, MaxEvidenceBytes),
				Result:             resLabel,
				ErrorCode:          CapAndRedactText(item.ErrorMessage, 256),
				DurationMs:         item.DurationMs,
				Evidence:           CapAndRedactText(item.Evidence, MaxEvidenceBytes),
				CreatedAt:          ts,
				CreatedAtMs:        tsMs,
				IsOffline:          true,
				Synced:             true,
				OriginStreamID:     originStream,
				OriginSeq:          item.OfflineSeq,
				OriginEntryHash:    originHash,
				KeyID:              kid,
			}

			lastSeq++
			ev.Seq = lastSeq
			ev.PreviousHash = lastHash
			entryHash, cErr := ComputeEventEntryHash(ev)
			if cErr != nil {
				return cErr
			}
			ev.EntryHash = entryHash
			ev.HMACSignature = ComputeEventHMAC(kid, secret, ev.StreamID, ev.Seq, ev.EntryHash, ev.CreatedAtMs)
			lastHash = ev.EntryHash
			tailOriginHash = originHash
			streamTips[originStream] = originStreamTip{lastSeq: item.OfflineSeq, lastHash: originHash}

			if _, execErr := conn.ExecContext(ctx, `
				INSERT INTO agent_audit_events (
					stream_id, seq, audit_id, run_id, parent_event_id, correlation_id, household_id,
					trigger_source, actor_type, actor_id, event_type, action, target, component,
					decision, risk_level, policy_version, proposal_id, approval_id, operation_id,
					fencing_generation, before_state, after_state, parameters_digest, parameters_redacted,
					result, error_code, duration_ms, evidence, created_at, created_at_ms, is_offline, synced,
					origin_stream_id, origin_seq, origin_entry_hash, key_id, previous_hash, entry_hash, hmac_signature
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ev.StreamID, ev.Seq, ev.AuditID, ev.RunID, ev.ParentEventID, ev.CorrelationID, ev.HouseholdID,
				ev.TriggerSource, ev.ActorType, ev.ActorID, ev.EventType, ev.Action, ev.Target, ev.Component,
				ev.Decision, ev.RiskLevel, ev.PolicyVersion, ev.ProposalID, ev.ApprovalID, ev.OperationID,
				ev.FencingGeneration, ev.BeforeState, ev.AfterState, ev.ParametersDigest, ev.ParametersRedacted,
				ev.Result, ev.ErrorCode, ev.DurationMs, ev.Evidence, ev.CreatedAt.UTC(), ev.CreatedAtMs, 1, 1,
				ev.OriginStreamID, ev.OriginSeq, ev.OriginEntryHash, ev.KeyID, ev.PreviousHash, ev.EntryHash, ev.HMACSignature,
			); execErr != nil {
				return execErr
			}

			finalStatus := ""
			switch ev.EventType {
			case model.AuditEventExecutionCompleted, model.AuditEventRecoveryVerified:
				finalStatus = model.AuditRunStatusCompleted
				if item.Success == 0 {
					finalStatus = model.AuditRunStatusFailed
				}
			case model.AuditEventExecutionFailed:
				finalStatus = model.AuditRunStatusFailed
			case model.AuditEventExecutionUnknown:
				finalStatus = model.AuditRunStatusExecutionUnknown
			}
			if upErr := upsertRunProgressConn(conn, ev, finalStatus, item.ErrorMessage); upErr != nil {
				return upErr
			}

			// Mirror into StructuredOpsAuditLog & SystemLog compatibly inside the same transaction.
			execResultJSON, _ := json.Marshal(map[string]interface{}{
				"success":            item.Success == 1,
				"exit_code":          item.ExitCode,
				"audit_id":           ev.AuditID,
				"run_id":             ev.RunID,
				"fencing_generation": ev.FencingGeneration,
			})
			if _, legErr := conn.ExecContext(ctx, `
				INSERT OR IGNORE INTO structured_ops_audit_logs (
					event_uuid, household_id, request_id, actor_type, actor_id, actor_name,
					client_ip, target_type, target_id, action, proposal_id, approver,
					execution_result, rollback_result, is_offline_action, created_at
				) VALUES (?, ?, ?, ?, 0, ?, ?, 'container', ?, ?, ?, ?, ?, '', 1, ?)`,
				uuid, itemHouseholdID, defaultStr(requestID, corrID), actorType, actorID,
				clientIP, target, action, proposalID, approvalID, string(execResultJSON), ts.UTC(),
			); legErr != nil {
				return legErr
			}

			sysPayload, _ := json.Marshal(map[string]interface{}{
				"event_uuid": uuid,
				"run_id":     ev.RunID,
				"exit_code":  item.ExitCode,
			})
			sysMsg := redact.SensitiveString(fmt.Sprintf("【离线应急自愈】目标 %s 执行 %s (状态: %v, run_id: %s)", target, action, item.Success == 1, ev.RunID))
			if _, sysErr := conn.ExecContext(ctx, `
				INSERT INTO system_logs (household_id, ts, event_type, level, source, context, message, payload)
				VALUES (?, ?, 'ops.container.offline_restart', ?, 'ops_agent_offline', ?, ?, ?)`,
				itemHouseholdID, ts.Unix(), model.LevelWarning, target, sysMsg, string(sysPayload),
			); sysErr != nil {
				// Ignore only if legacy system_logs table was not migrated in minimal unit DBs.
				if !strings.Contains(strings.ToLower(sysErr.Error()), "no such table") {
					return sysErr
				}
			}

			insertedEvents = append(insertedEvents, ev)
			result.AcceptedUUIDs = append(result.AcceptedUUIDs, uuid)
			result.SyncedCount++
			syncedBatch = append(syncedBatch, map[string]interface{}{
				"uuid":          uuid,
				"origin_stream": originStream,
				"origin_seq":    item.OfflineSeq,
				"core_seq":      ev.Seq,
			})
		}

		// Anchor synced offline batch into core stream with explicit OFFLINE_BATCH_ANCHORED milestone.
		if result.SyncedCount > 0 {
			now := e.nowUTC()
			nowMs := now.UnixMilli()
			anchorEvidence := SerializeEvidence(map[string]interface{}{
				"synced_count":     result.SyncedCount,
				"rejected_count":   len(result.Rejected),
				"tail_origin_hash": tailOriginHash,
				"items":            syncedBatch,
			})
			anchorEv := &model.AgentAuditEvent{
				StreamID:      model.AuditPrimaryStreamID,
				Seq:           lastSeq + 1,
				AuditID:       fmt.Sprintf("aud-anchor-%d-%s", nowMs, generateRandomHex(4)),
				RunID:         fmt.Sprintf("run-sync-%d-%s", nowMs, generateRandomHex(4)),
				CorrelationID: defaultStr(requestID, fmt.Sprintf("corr-sync-%d", nowMs)),
				HouseholdID:   householdID,
				TriggerSource: "offline_sync",
				ActorType:     "internal_service",
				ActorID:       "home-ops-agent",
				EventType:     model.AuditEventRecoveryVerified,
				Action:        "audit.offline_batch_anchored",
				Target:        "offline-agent",
				Component:     "audit-sync",
				Decision:      model.AuditDecisionAllow,
				RiskLevel:     model.AuditRiskMedium,
				PolicyVersion: PolicyVersion,
				Result:        "SUCCESS",
				Evidence:      anchorEvidence,
				CreatedAt:     now,
				CreatedAtMs:   nowMs,
				IsOffline:     false,
				Synced:        true,
				KeyID:         kid,
				PreviousHash:  lastHash,
			}
			if len(result.Rejected) > 0 {
				anchorEv.Result = "PARTIAL_REJECTED"
				anchorEv.ErrorCode = fmt.Sprintf("%d_OFFLINE_EVENTS_REJECTED", len(result.Rejected))
			}
			aHash, aErr := ComputeEventEntryHash(anchorEv)
			if aErr != nil {
				return aErr
			}
			anchorEv.EntryHash = aHash
			anchorEv.HMACSignature = ComputeEventHMAC(kid, secret, anchorEv.StreamID, anchorEv.Seq, anchorEv.EntryHash, anchorEv.CreatedAtMs)

			if _, execErr := conn.ExecContext(ctx, `
				INSERT INTO agent_audit_events (
					stream_id, seq, audit_id, run_id, parent_event_id, correlation_id, household_id,
					trigger_source, actor_type, actor_id, event_type, action, target, component,
					decision, risk_level, policy_version, proposal_id, approval_id, operation_id,
					fencing_generation, before_state, after_state, parameters_digest, parameters_redacted,
					result, error_code, duration_ms, evidence, created_at, created_at_ms, is_offline, synced,
					origin_stream_id, origin_seq, origin_entry_hash, key_id, previous_hash, entry_hash, hmac_signature
				) VALUES (?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', 0, '', '', '', '', ?, ?, 0, ?, ?, ?, 0, 1, '', 0, '', ?, ?, ?, ?)`,
				anchorEv.StreamID, anchorEv.Seq, anchorEv.AuditID, anchorEv.RunID, anchorEv.CorrelationID, anchorEv.HouseholdID,
				anchorEv.TriggerSource, anchorEv.ActorType, anchorEv.ActorID, anchorEv.EventType, anchorEv.Action, anchorEv.Target, anchorEv.Component,
				anchorEv.Decision, anchorEv.RiskLevel, anchorEv.PolicyVersion, anchorEv.Result, anchorEv.ErrorCode, anchorEv.Evidence,
				anchorEv.CreatedAt.UTC(), anchorEv.CreatedAtMs, anchorEv.KeyID, anchorEv.PreviousHash, anchorEv.EntryHash, anchorEv.HMACSignature,
			); execErr != nil {
				return execErr
			}
			if upErr := upsertRunProgressConn(conn, anchorEv, model.AuditRunStatusCompleted, fmt.Sprintf("Anchored %d offline audit events (%d rejected)", result.SyncedCount, len(result.Rejected))); upErr != nil {
				return upErr
			}
			insertedEvents = append(insertedEvents, anchorEv)
			result.AnchorSeq = anchorEv.Seq
		}
		return nil
	})
	if err != nil {
		e.recordFailureMetric()
		return result, fmt.Errorf("%w: offline sync transaction failed: %v", ErrAuditCommitFailed, err)
	}

	result.ChainStatus = determineOfflineChainStatus(result.Rejected)
	if len(result.Rejected) > 0 {
		e.recordFailureMetric()
		if e.bus != nil {
			if rawPayload, mErr := json.Marshal(map[string]interface{}{
				"rejected_count": len(result.Rejected),
				"chain_status":   result.ChainStatus,
				"alerts":         result.IntegrityAlerts,
			}); mErr == nil {
				e.bus.Publish(eventbus.Event{
					Topic:     eventbus.TopicAuditEventRecorded,
					Source:    eventbus.SourceOps,
					Severity:  eventbus.SeverityCritical,
					Payload:   rawPayload,
					Timestamp: e.nowUTC(),
				})
			}
		}
	}

	for _, ev := range insertedEvents {
		_ = e.appendArchiveEvent(ev)
	}
	return result, nil
}

func determineOfflineChainStatus(rejected []OfflineSyncRejection) string {
	if len(rejected) == 0 {
		return IntegrityChainIntactLocalOnly
	}
	for _, r := range rejected {
		if r.ErrorCode == "CHAIN_FORK" || r.ErrorCode == "DUPLICATE_SEQ_CONFLICT" || r.ErrorCode == "DUPLICATE_ID_CONFLICT" {
			return IntegrityChainFork
		}
	}
	for _, r := range rejected {
		if r.ErrorCode == "SEQ_GAP" {
			return IntegritySeqGap
		}
	}
	for _, r := range rejected {
		if r.ErrorCode == "HMAC_INVALID" {
			return IntegrityHMACInvalid
		}
	}
	for _, r := range rejected {
		if r.ErrorCode == "HASH_MISMATCH" {
			return IntegrityHashMismatch
		}
	}
	return "PARTIAL_REJECTED"
}

func parseOfflineTimestamp(raw string, ms int64, fallback time.Time) time.Time {
	if ms > 0 {
		return time.UnixMilli(ms).UTC().Truncate(time.Millisecond)
	}
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return t.UTC().Truncate(time.Millisecond)
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t.UTC().Truncate(time.Millisecond)
		}
	}
	return fallback
}
