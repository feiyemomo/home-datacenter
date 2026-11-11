package ops

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

func setupAuditDB(t *testing.T) *gorm.DB {
	t.Helper()
	cleanup := audit.SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "audit-sync-test-hmac-key-at-least-32-bytes-long",
	})
	t.Cleanup(cleanup)

	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	if err := db.AutoMigrate(&model.StructuredOpsAuditLog{}, &model.SystemLog{}, &model.OpsProposal{}); err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}
	return db
}

func makeSignedSyncItem(t *testing.T, seq uint64, uuid, container, prevHash string, tsMs int64) OfflineSyncItem {
	t.Helper()
	ev := model.AgentAuditEvent{
		StreamID:      "offline-agent",
		Seq:           seq,
		AuditID:       uuid,
		RunID:         "run-offline-" + uuid,
		CorrelationID: "corr-" + uuid,
		HouseholdID:   1,
		TriggerSource: "python_offline",
		ActorType:     "offline_agent",
		ActorID:       "home-ops-agent",
		EventType:     model.AuditEventExecutionCompleted,
		Action:        "docker.restart",
		Target:        container,
		Component:     container,
		Decision:      model.AuditDecisionAllow,
		RiskLevel:     model.AuditRiskHigh,
		PolicyVersion: audit.PolicyVersion,
		Result:        "SUCCESS",
		CreatedAt:     time.UnixMilli(tsMs).UTC(),
		CreatedAtMs:   tsMs,
		IsOffline:     true,
		PreviousHash:  prevHash,
	}
	h, err := audit.ComputeEventEntryHash(&ev)
	if err != nil {
		t.Fatalf("ComputeEventEntryHash failed: %v", err)
	}
	sig := audit.ComputeEventHMAC("k1", "audit-sync-test-hmac-key-at-least-32-bytes-long", "offline-agent", seq, h, tsMs)
	return OfflineSyncItem{
		EventUUID:      uuid,
		AuditID:        uuid,
		RunID:          ev.RunID,
		CorrelationID:  ev.CorrelationID,
		TriggerSource:  ev.TriggerSource,
		ActorType:      ev.ActorType,
		ActorID:        ev.ActorID,
		EventType:      ev.EventType,
		ContainerName:  container,
		Component:      container,
		Target:         container,
		Action:         ev.Action,
		Decision:       ev.Decision,
		RiskLevel:      ev.RiskLevel,
		Success:        1,
		ExecutedAt:     audit.FormatRFC3339Ms(ev.CreatedAt),
		ExecutedAtMs:   tsMs,
		OriginStreamID: "offline-agent",
		OfflineSeq:     seq,
		LocalPrevHash:  prevHash,
		EntryHash:      h,
		KeyID:          "k1",
		HMACSignature:  sig,
	}
}

func TestSyncOfflineAuditLogs_IngestionAndDeduplication(t *testing.T) {
	db := setupAuditDB(t)
	bus := eventbus.New()
	handler := NewOpsHandler(nil, nil, db, bus, nil)

	r := gin.New()
	rg := r.Group("/api/v1/ops")
	handler.RegisterFSMRoutes(rg)

	validToken := "audit-sync-test-token-at-least-32-chars-long"
	t.Setenv("INTERNAL_OPS_TOKEN", validToken)

	nowMs := time.Now().UTC().UnixMilli()
	item1 := makeSignedSyncItem(t, 1, "offline-event-uuid-001", "home-web", model.AuditGenesisHash, nowMs)
	item2 := makeSignedSyncItem(t, 2, "offline-event-uuid-002", "home-api", item1.EntryHash, nowMs+10)
	events := []OfflineSyncItem{item1, item2}

	reqBody, _ := json.Marshal(OfflineSyncRequest{Events: events})

	// 1. Initial ingestion batch
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/audit/offline-sync", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", validToken)
	req.RemoteAddr = "127.0.0.1:12345"
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for offline audit sync, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int `json:"code"`
		Data struct {
			SyncedCount int `json:"synced_count"`
			TotalCount  int `json:"total_count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse sync response: %v", err)
	}
	if resp.Data.SyncedCount != 2 {
		t.Fatalf("expected 2 synced events, got %d", resp.Data.SyncedCount)
	}

	// Verify database rows exist in StructuredOpsAuditLog
	var count int64
	db.Model(&model.StructuredOpsAuditLog{}).Count(&count)
	if count != 2 {
		t.Fatalf("expected 2 rows in StructuredOpsAuditLog, got %d", count)
	}

	// 2. Re-send identical batch (idempotency deduplication check)
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/audit/offline-sync", bytes.NewReader(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Internal-Secret", validToken)
	req2.RemoteAddr = "127.0.0.1:12345"
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for second sync, got %d", w2.Code)
	}

	var resp2 struct {
		Code int `json:"code"`
		Data struct {
			SyncedCount int `json:"synced_count"`
			TotalCount  int `json:"total_count"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	if resp2.Data.SyncedCount != 0 {
		t.Fatalf("expected 0 new synced events on duplicate submission, got %d", resp2.Data.SyncedCount)
	}

	// Verify count is still exactly 2
	db.Model(&model.StructuredOpsAuditLog{}).Count(&count)
	if count != 2 {
		t.Fatalf("expected count to remain 2 after duplicate submission, got %d", count)
	}
}
