package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
)

const (
	maxQueryPageSize = 100
	maxExportRows    = 10000
)

// QueryRuns returns household-scoped paginated AgentAuditRuns matching filters.
func (e *AgentAuditEngine) QueryRuns(q RunQuery) ([]model.AgentAuditRun, int64, error) {
	if e == nil || e.db == nil {
		return nil, 0, ErrAuditCommitFailed
	}
	if q.HouseholdID == 0 {
		return nil, 0, fmt.Errorf("household_id is strictly required for audit queries")
	}

	page := q.Page
	if page < 1 {
		page = 1
	}
	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > maxQueryPageSize {
		pageSize = maxQueryPageSize
	}

	tx := e.db.Model(&model.AgentAuditRun{}).Where("household_id = ?", q.HouseholdID)
	if v := strings.TrimSpace(q.TriggerSource); v != "" {
		tx = tx.Where("trigger_source = ?", v)
	}
	if v := strings.TrimSpace(q.Component); v != "" {
		tx = tx.Where("component = ?", v)
	}
	if v := strings.TrimSpace(q.Action); v != "" {
		tx = tx.Where("action = ?", v)
	}
	if v := strings.TrimSpace(q.Status); v != "" {
		tx = tx.Where("status = ?", v)
	}
	if v := strings.TrimSpace(q.RiskLevel); v != "" {
		tx = tx.Where("risk_level = ?", strings.ToUpper(v))
	}
	if v := strings.TrimSpace(q.ActorID); v != "" {
		tx = tx.Where("actor_id = ?", v)
	}
	if v := strings.TrimSpace(q.ProposalID); v != "" {
		tx = tx.Where("proposal_id = ?", v)
	}
	if v := strings.TrimSpace(q.ApprovalID); v != "" {
		tx = tx.Where("approval_id = ?", v)
	}
	if v := strings.TrimSpace(q.OperationID); v != "" {
		tx = tx.Where("operation_id = ?", v)
	}
	if v := strings.TrimSpace(q.CorrelationID); v != "" {
		tx = tx.Where("correlation_id = ?", v)
	}
	if q.IsOffline != nil {
		tx = tx.Where("is_offline = ?", *q.IsOffline)
	}
	if q.FromMs > 0 {
		tx = tx.Where("started_at_ms >= ?", q.FromMs)
	}
	if q.ToMs > 0 {
		tx = tx.Where("started_at_ms <= ?", q.ToMs)
	}
	if v := strings.TrimSpace(q.Search); v != "" {
		like := "%" + v + "%"
		tx = tx.Where("(run_id LIKE ? OR target LIKE ? OR component LIKE ? OR action LIKE ? OR proposal_id LIKE ? OR operation_id LIKE ? OR approval_id LIKE ? OR correlation_id LIKE ? OR summary LIKE ?)",
			like, like, like, like, like, like, like, like, like)
	}

	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var runs []model.AgentAuditRun
	if err := tx.Order("started_at_ms DESC, id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&runs).Error; err != nil {
		return nil, 0, err
	}

	for i := range runs {
		runs[i].Target = redact.SensitiveString(runs[i].Target)
		runs[i].Summary = redact.SensitiveString(runs[i].Summary)
		runs[i].ErrorCode = redact.SensitiveString(runs[i].ErrorCode)
	}
	if runs == nil {
		runs = []model.AgentAuditRun{}
	}
	return runs, total, nil
}

// GetRunDetail returns a household-scoped Run header, ordered timeline events, and integrity check result.
func (e *AgentAuditEngine) GetRunDetail(runID string, householdID uint) (*RunDetail, error) {
	if e == nil || e.db == nil {
		return nil, ErrAuditCommitFailed
	}
	if householdID == 0 || strings.TrimSpace(runID) == "" {
		return nil, ErrAuditRunNotFound
	}

	var run model.AgentAuditRun
	if err := e.db.Where("run_id = ? AND household_id = ?", strings.TrimSpace(runID), householdID).First(&run).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAuditRunNotFound
		}
		return nil, err
	}

	var events []model.AgentAuditEvent
	if err := e.db.Where("run_id = ? AND household_id = ?", run.RunID, householdID).
		Order("seq ASC").
		Find(&events).Error; err != nil {
		return nil, err
	}
	for i := range events {
		redactEventOutput(&events[i])
	}
	if events == nil {
		events = []model.AgentAuditEvent{}
	}

	integrity := e.VerifyRunChain(run.RunID, householdID)
	run.Target = redact.SensitiveString(run.Target)
	run.Summary = redact.SensitiveString(run.Summary)
	run.ErrorCode = redact.SensitiveString(run.ErrorCode)

	return &RunDetail{
		Run:       run,
		Events:    events,
		Integrity: integrity,
	}, nil
}

// QueryEvents returns household-scoped paginated AgentAuditEvents matching filters.
func (e *AgentAuditEngine) QueryEvents(q EventQuery, maxLimit int) ([]model.AgentAuditEvent, int64, error) {
	if e == nil || e.db == nil {
		return nil, 0, ErrAuditCommitFailed
	}
	if q.HouseholdID == 0 {
		return nil, 0, fmt.Errorf("household_id is strictly required for audit event queries")
	}

	page := q.Page
	if page < 1 {
		page = 1
	}
	if maxLimit <= 0 {
		maxLimit = maxQueryPageSize
	}
	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > maxLimit {
		pageSize = maxLimit
	}

	tx := e.db.Model(&model.AgentAuditEvent{}).Where("household_id = ?", q.HouseholdID)
	if v := strings.TrimSpace(q.RunID); v != "" {
		tx = tx.Where("run_id = ?", v)
	}
	if v := strings.TrimSpace(q.EventType); v != "" {
		tx = tx.Where("event_type = ?", v)
	}
	if v := strings.TrimSpace(q.TriggerSource); v != "" {
		tx = tx.Where("trigger_source = ?", v)
	}
	if v := strings.TrimSpace(q.Component); v != "" {
		tx = tx.Where("component = ?", v)
	}
	if v := strings.TrimSpace(q.Action); v != "" {
		tx = tx.Where("action = ?", v)
	}
	if v := strings.TrimSpace(q.Target); v != "" {
		tx = tx.Where("target = ?", v)
	}
	if v := strings.TrimSpace(q.CorrelationID); v != "" {
		tx = tx.Where("correlation_id = ?", v)
	}
	if v := strings.TrimSpace(q.ProposalID); v != "" {
		tx = tx.Where("proposal_id = ?", v)
	}
	if v := strings.TrimSpace(q.ApprovalID); v != "" {
		tx = tx.Where("approval_id = ?", v)
	}
	if v := strings.TrimSpace(q.OperationID); v != "" {
		tx = tx.Where("operation_id = ?", v)
	}
	if v := strings.TrimSpace(q.ActorID); v != "" {
		tx = tx.Where("actor_id = ?", v)
	}
	if v := strings.TrimSpace(q.RiskLevel); v != "" {
		tx = tx.Where("risk_level = ?", strings.ToUpper(v))
	}
	if q.IsOffline != nil {
		tx = tx.Where("is_offline = ?", *q.IsOffline)
	}
	if v := strings.TrimSpace(q.Result); v != "" {
		tx = tx.Where("result = ?", v)
	}
	if q.FromMs > 0 {
		tx = tx.Where("created_at_ms >= ?", q.FromMs)
	}
	if q.ToMs > 0 {
		tx = tx.Where("created_at_ms <= ?", q.ToMs)
	}

	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	orderExpr := "seq DESC"
	if q.SortAsc {
		orderExpr = "seq ASC"
	}
	var events []model.AgentAuditEvent
	if err := tx.Order(orderExpr).
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&events).Error; err != nil {
		return nil, 0, err
	}
	for i := range events {
		redactEventOutput(&events[i])
	}
	if events == nil {
		events = []model.AgentAuditEvent{}
	}
	return events, total, nil
}

// ExportEvents produces redacted JSON or formula-injection-safe CSV bytes and a SHA-256 checksum.
func (e *AgentAuditEngine) ExportEvents(q EventQuery, format string) ([]byte, string, string, error) {
	q.Page = 1
	q.PageSize = maxExportRows
	q.SortAsc = true
	events, total, err := e.QueryEvents(q, maxExportRows)
	if err != nil {
		return nil, "", "", err
	}

	switch strings.ToLower(strings.TrimSpace(format)) {
	case "csv":
		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		header := []string{
			"seq", "audit_id", "run_id", "correlation_id", "created_at", "trigger_source",
			"actor_type", "actor_id", "event_type", "action", "component", "target",
			"decision", "risk_level", "proposal_id", "approval_id", "operation_id",
			"result", "error_code", "duration_ms", "is_offline", "synced",
			"parameters_digest", "parameters_redacted", "before_state", "after_state", "evidence",
			"previous_hash", "entry_hash", "hmac_signature",
		}
		if err := w.Write(header); err != nil {
			return nil, "", "", err
		}
		for _, ev := range events {
			row := []string{
				strconv.FormatUint(ev.Seq, 10),
				redact.EscapeCSVCell(ev.AuditID),
				redact.EscapeCSVCell(ev.RunID),
				redact.EscapeCSVCell(ev.CorrelationID),
				redact.EscapeCSVCell(FormatRFC3339Ms(ev.CreatedAt)),
				redact.EscapeCSVCell(ev.TriggerSource),
				redact.EscapeCSVCell(ev.ActorType),
				redact.EscapeCSVCell(ev.ActorID),
				redact.EscapeCSVCell(ev.EventType),
				redact.EscapeCSVCell(ev.Action),
				redact.EscapeCSVCell(ev.Component),
				redact.EscapeCSVCell(ev.Target),
				redact.EscapeCSVCell(ev.Decision),
				redact.EscapeCSVCell(ev.RiskLevel),
				redact.EscapeCSVCell(ev.ProposalID),
				redact.EscapeCSVCell(ev.ApprovalID),
				redact.EscapeCSVCell(ev.OperationID),
				redact.EscapeCSVCell(ev.Result),
				redact.EscapeCSVCell(ev.ErrorCode),
				strconv.FormatInt(ev.DurationMs, 10),
				strconv.FormatBool(ev.IsOffline),
				strconv.FormatBool(ev.Synced),
				redact.EscapeCSVCell(ev.ParametersDigest),
				redact.EscapeCSVCell(ev.ParametersRedacted),
				redact.EscapeCSVCell(ev.BeforeState),
				redact.EscapeCSVCell(ev.AfterState),
				redact.EscapeCSVCell(ev.Evidence),
				redact.EscapeCSVCell(ev.PreviousHash),
				redact.EscapeCSVCell(ev.EntryHash),
				redact.EscapeCSVCell(ev.HMACSignature),
			}
			if err := w.Write(row); err != nil {
				return nil, "", "", err
			}
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return nil, "", "", err
		}
		raw := buf.Bytes()
		sum := sha256.Sum256(raw)
		return raw, "text/csv; charset=utf-8", hex.EncodeToString(sum[:]), nil

	default:
		raw, err := json.MarshalIndent(map[string]interface{}{
			"exported_at": FormatRFC3339Ms(e.nowUTC()),
			"count":       len(events),
			"total":       total,
			"truncated":   total > int64(len(events)),
			"events":      events,
		}, "", "  ")
		if err != nil {
			return nil, "", "", err
		}
		sum := sha256.Sum256(raw)
		return raw, "application/json; charset=utf-8", hex.EncodeToString(sum[:]), nil
	}
}

func redactEventOutput(ev *model.AgentAuditEvent) {
	if ev == nil {
		return
	}
	ev.Target = redact.SensitiveString(ev.Target)
	ev.ActorID = redact.SensitiveString(ev.ActorID)
	ev.BeforeState = redact.StructuredJSON(ev.BeforeState)
	ev.AfterState = redact.StructuredJSON(ev.AfterState)
	ev.ParametersRedacted = redact.StructuredJSON(ev.ParametersRedacted)
	ev.ErrorCode = redact.SensitiveString(ev.ErrorCode)
	ev.Evidence = redact.StructuredJSON(ev.Evidence)
}
