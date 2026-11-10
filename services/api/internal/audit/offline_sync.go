package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
)

// IngestOfflineEvents validates, deduplicates, verifies signatures/hashes, and merges offline
// Python edge-agent audit events into the authoritative core hash chain and legacy tables.
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

		syncedBatch := make([]map[string]interface{}, 0, len(events))
		var tailOriginHash string

		for _, item := range events {
			uuid := strings.TrimSpace(item.EventUUID)
			if uuid == "" {
				uuid = strings.TrimSpace(item.AuditID)
			}
			if uuid == "" {
				result.Rejected = append(result.Rejected, OfflineSyncRejection{
					EventUUID: "",
					Reason:    "missing event_uuid/audit_id",
				})
				continue
			}

			// Idempotency lookup across both primary audit table and legacy structured log table.
			var existingCount int64
			if err := conn.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM agent_audit_events WHERE audit_id = ?`, uuid,
			).Scan(&existingCount); err != nil {
				return err
			}
			if existingCount == 0 {
				if err := conn.QueryRowContext(ctx,
					`SELECT COUNT(*) FROM structured_ops_audit_logs WHERE event_uuid = ?`, uuid,
				).Scan(&existingCount); err != nil {
					return err
				}
			}
			if existingCount > 0 {
				result.AcceptedUUIDs = append(result.AcceptedUUIDs, uuid)
				continue
			}

			ts := parseOfflineTimestamp(item.ExecutedAt, item.ExecutedAtMs, e.nowUTC())
			tsMs := ts.UnixMilli()
			if item.ExecutedAtMs > 0 {
				tsMs = item.ExecutedAtMs
				ts = time.UnixMilli(tsMs).UTC().Truncate(time.Millisecond)
			}

			target := redact.SensitiveString(defaultStr(item.Target, item.ContainerName))
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
			auditID := defaultStr(item.AuditID, uuid)
			resLabel := "SUCCESS"
			if item.Success == 0 {
				resLabel = "FAILED"
			}

			ev := &model.AgentAuditEvent{
				StreamID:           model.AuditPrimaryStreamID,
				AuditID:            auditID,
				RunID:              runID,
				ParentEventID:      strings.TrimSpace(item.ParentEventID),
				CorrelationID:      corrID,
				HouseholdID:        householdID,
				TriggerSource:      trig,
				ActorType:          actorType,
				ActorID:            actorID,
				EventType:          eventType,
				Action:             action,
				Target:             target,
				Component:          component,
				Decision:           decision,
				RiskLevel:          riskLevel,
				PolicyVersion:      PolicyVersion,
				OperationID:        strings.TrimSpace(item.OperationID),
				FencingGeneration:  item.FencingGeneration,
				BeforeState:        CapAndRedactText(item.BeforeState, MaxEvidenceBytes),
				AfterState:         CapAndRedactText(item.AfterState, MaxEvidenceBytes),
				ParametersDigest:   strings.TrimSpace(item.ParametersDigest),
				ParametersRedacted: CapAndRedactText(item.ParametersRedacted, MaxEvidenceBytes),
				Result:             resLabel,
				ErrorCode:          CapAndRedactText(item.ErrorMessage, 256),
				Evidence:           CapAndRedactText(item.Evidence, MaxEvidenceBytes),
				CreatedAt:          ts,
				CreatedAtMs:        tsMs,
				IsOffline:          true,
				Synced:             true,
				OriginStreamID:     "offline-agent",
				OriginSeq:          item.OfflineSeq,
				OriginEntryHash:    strings.TrimSpace(item.EntryHash),
			}

			// If Python included v5 cryptographic proof (entry_hash or offline_seq > 0), enforce both hash & HMAC.
			if ev.OriginEntryHash != "" || item.OfflineSeq > 0 {
				if ev.OriginEntryHash == "" {
					result.Rejected = append(result.Rejected, OfflineSyncRejection{
						EventUUID: uuid,
						Reason:    "offline v5 event missing entry_hash",
					})
					continue
				}
				if strings.TrimSpace(item.HMACSignature) == "" {
					result.Rejected = append(result.Rejected, OfflineSyncRejection{
						EventUUID: uuid,
						Reason:    "offline v5 event missing required hmac_signature",
					})
					continue
				}
				verifyCopy := *ev
				verifyCopy.PreviousHash = defaultStr(strings.TrimSpace(item.LocalPrevHash), model.AuditGenesisHash)
				expectedOriginHash, hErr := ComputeEventEntryHash(&verifyCopy)
				if hErr != nil || expectedOriginHash != ev.OriginEntryHash {
					result.Rejected = append(result.Rejected, OfflineSyncRejection{
						EventUUID: uuid,
						Reason:    fmt.Sprintf("offline entry_hash mismatch (got %s, expected %s)", ev.OriginEntryHash, expectedOriginHash),
					})
					continue
				}
				if mErr := VerifyOfflineOriginHMAC(item, ring); mErr != nil {
					result.Rejected = append(result.Rejected, OfflineSyncRejection{
						EventUUID: uuid,
						Reason:    fmt.Sprintf("offline HMAC verification failed: %v", mErr),
					})
					continue
				}
				tailOriginHash = ev.OriginEntryHash
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

			if _, execErr := conn.ExecContext(ctx, `
				INSERT INTO agent_audit_events (
					stream_id, seq, audit_id, run_id, parent_event_id, correlation_id, household_id,
					trigger_source, actor_type, actor_id, event_type, action, target, component,
					decision, risk_level, policy_version, proposal_id, approval_id, operation_id,
					fencing_generation, before_state, after_state, parameters_digest, parameters_redacted,
					result, error_code, duration_ms, evidence, created_at, created_at_ms, is_offline, synced,
					origin_stream_id, origin_seq, origin_entry_hash, previous_hash, entry_hash, hmac_signature
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ev.StreamID, ev.Seq, ev.AuditID, ev.RunID, ev.ParentEventID, ev.CorrelationID, ev.HouseholdID,
				ev.TriggerSource, ev.ActorType, ev.ActorID, ev.EventType, ev.Action, ev.Target, ev.Component,
				ev.Decision, ev.RiskLevel, ev.PolicyVersion, ev.ProposalID, ev.ApprovalID, ev.OperationID,
				ev.FencingGeneration, ev.BeforeState, ev.AfterState, ev.ParametersDigest, ev.ParametersRedacted,
				ev.Result, ev.ErrorCode, ev.DurationMs, ev.Evidence, ev.CreatedAt.UTC(), ev.CreatedAtMs, 1, 1,
				ev.OriginStreamID, ev.OriginSeq, ev.OriginEntryHash, ev.PreviousHash, ev.EntryHash, ev.HMACSignature,
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
				) VALUES (?, ?, ?, ?, 0, ?, ?, 'container', ?, ?, '', '', ?, '', 1, ?)`,
				uuid, householdID, defaultStr(requestID, corrID), actorType, actorID,
				clientIP, target, action, string(execResultJSON), ts.UTC(),
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
				householdID, ts.Unix(), model.LevelWarning, target, sysMsg, string(sysPayload),
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
				"uuid":       uuid,
				"origin_seq": item.OfflineSeq,
				"core_seq":   ev.Seq,
			})
		}

		// Anchor synced offline batch into core stream with explicit OFFLINE_BATCH_ANCHORED milestone.
		if result.SyncedCount > 0 {
			now := e.nowUTC()
			nowMs := now.UnixMilli()
			anchorEvidence := SerializeEvidence(map[string]interface{}{
				"synced_count":     result.SyncedCount,
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
				PreviousHash:  lastHash,
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
					origin_stream_id, origin_seq, origin_entry_hash, previous_hash, entry_hash, hmac_signature
				) VALUES (?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', 0, '', '', '', '', ?, '', 0, ?, ?, ?, 0, 1, '', 0, '', ?, ?, ?)`,
				anchorEv.StreamID, anchorEv.Seq, anchorEv.AuditID, anchorEv.RunID, anchorEv.CorrelationID, anchorEv.HouseholdID,
				anchorEv.TriggerSource, anchorEv.ActorType, anchorEv.ActorID, anchorEv.EventType, anchorEv.Action, anchorEv.Target, anchorEv.Component,
				anchorEv.Decision, anchorEv.RiskLevel, anchorEv.PolicyVersion, anchorEv.Result, anchorEv.Evidence,
				anchorEv.CreatedAt.UTC(), anchorEv.CreatedAtMs, anchorEv.PreviousHash, anchorEv.EntryHash, anchorEv.HMACSignature,
			); execErr != nil {
				return execErr
			}
			if upErr := upsertRunProgressConn(conn, anchorEv, model.AuditRunStatusCompleted, fmt.Sprintf("Anchored %d offline audit events", result.SyncedCount)); upErr != nil {
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

	for _, ev := range insertedEvents {
		_ = e.appendArchiveEvent(ev)
	}
	return result, nil
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
