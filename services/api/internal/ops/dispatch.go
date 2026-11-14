package ops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// ExecutionOutcomeCategory represents the structured confidence level of an operation outcome.
type ExecutionOutcomeCategory string

const (
	// OutcomeNotDispatched indicates the request failed before reaching the target transport/service.
	// Zero side effects on the target daemon; safe for policy-driven fallback if explicitly configured.
	OutcomeNotDispatched ExecutionOutcomeCategory = "NOT_DISPATCHED"

	// OutcomeDispatchedUnknown indicates the request was at least partially transmitted to the target
	// transport or the target daemon returned a transient/ambiguous error (timeout, RST, 500/502/503/504).
	// Side effects may have occurred; blind retry or cross-channel retry is STRICTLY FORBIDDEN.
	OutcomeDispatchedUnknown ExecutionOutcomeCategory = "DISPATCHED_UNKNOWN"

	// OutcomeExecuted indicates explicit positive confirmation from the target daemon (e.g. 200/204).
	OutcomeExecuted ExecutionOutcomeCategory = "EXECUTED"

	// OutcomeFailedConfirmed indicates explicit negative confirmation from the target daemon (e.g. 404 Not Found, 400 Bad Request).
	// The target daemon confirmed that the mutation was not performed and container was not found/invalid.
	OutcomeFailedConfirmed ExecutionOutcomeCategory = "FAILED_CONFIRMED"

	// OutcomeReconciliationRequired indicates inconsistent state or unverified mutation requiring reconciliation.
	OutcomeReconciliationRequired ExecutionOutcomeCategory = "RECONCILIATION_REQUIRED"
)

// DispatchStage tracks the microsecond-level lifecycle of an outgoing mutation dispatch.
type DispatchStage string

const (
	StageInit             DispatchStage = "INIT"
	StagePreCheck         DispatchStage = "PRE_CHECK"
	StageDialing          DispatchStage = "DIALING"
	StageConnected        DispatchStage = "CONNECTED"
	StageWroteHeaders     DispatchStage = "WROTE_HEADERS"
	StageWroteRequest     DispatchStage = "WROTE_REQUEST" // Irreversible point of no return: bytes sent to remote/socket buffer
	StageWaitingResponse  DispatchStage = "WAITING_RESPONSE"
	StageResponseReceived DispatchStage = "RESPONSE_RECEIVED"
	StageCompleted        DispatchStage = "COMPLETED"
)

// ExecutionAttemptRecord records metadata for a single dispatch attempt across any execution channel.
type ExecutionAttemptRecord struct {
	AttemptID         string                   `json:"attempt_id"`
	OperationID       string                   `json:"operation_id,omitempty"`
	ParentStep        string                   `json:"parent_step,omitempty"`
	Channel           string                   `json:"channel"` // "docker_socket", "1panel_api", "host_fallback"
	Action            string                   `json:"action"`
	Target            string                   `json:"target"`
	ContainerID       string                   `json:"container_id,omitempty"`
	FencingGeneration int64                    `json:"fencing_generation,omitempty"`
	Stage             DispatchStage            `json:"stage"`
	Dispatched        bool                     `json:"dispatched"`
	Category          ExecutionOutcomeCategory `json:"category"`
	StatusCode        int                      `json:"status_code,omitempty"`
	Error             string                   `json:"error,omitempty"`
	DurationMs        int64                    `json:"duration_ms"`
	Timestamp         time.Time                `json:"timestamp"`
}

// StageTracker tracks the in-flight dispatch stage and determines whether the point-of-no-return has been crossed.
type StageTracker struct {
	mu             sync.Mutex
	attemptID      string
	operationID    string
	parentStep     string
	containerID    string
	fencingGen     int64
	channel        string
	target         string
	action         string
	stage          DispatchStage
	dispatched     bool
	category       ExecutionOutcomeCategory
	statusCode     int
	err            error
	startTime      time.Time
	dispatchedTime time.Time
	completedTime  time.Time
}

// NewStageTracker creates a new tracker for an execution attempt.
func NewStageTracker(channel, action, target string) *StageTracker {
	return &StageTracker{
		attemptID: GenerateAttemptID(channel),
		channel:   channel,
		action:    action,
		target:    target,
		stage:     StageInit,
		category:  OutcomeNotDispatched,
		startTime: time.Now().UTC(),
	}
}

// GenerateAttemptID produces a unique attempt ID with channel prefix.
func GenerateAttemptID(channel string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("att_%s_%d_%s", channel, time.Now().UnixNano(), hex.EncodeToString(b))
}

// Action returns the target action name.
func (t *StageTracker) Action() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.action
}

// Target returns the target container/component name.
func (t *StageTracker) Target() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.target
}

// SetOperationID sets the global operation ID.
func (t *StageTracker) SetOperationID(opID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.operationID = opID
}

// OperationID returns the global operation ID.
func (t *StageTracker) OperationID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.operationID
}

// SetParentStep sets the multi-step parent stage name (e.g. "recreate.stop", "exec.start").
func (t *StageTracker) SetParentStep(step string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.parentStep = step
}

// ParentStep returns the multi-step parent stage name.
func (t *StageTracker) ParentStep() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.parentStep
}

// SetContainerID sets the resolved or inspected container ID.
func (t *StageTracker) SetContainerID(cid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.containerID = cid
}

// ContainerID returns the resolved or inspected container ID.
func (t *StageTracker) ContainerID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.containerID
}

// SetFencingGen sets the fencing generation.
func (t *StageTracker) SetFencingGen(gen int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fencingGen = gen
}

// FencingGen returns the fencing generation.
func (t *StageTracker) FencingGen() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fencingGen
}

// AttemptID returns the unique attempt ID.
func (t *StageTracker) AttemptID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.attemptID
}

// Channel returns the execution channel name.
func (t *StageTracker) Channel() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.channel
}

// Stage returns current dispatch stage.
func (t *StageTracker) Stage() DispatchStage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stage
}

// IsDispatched returns true if the mutation request has been transmitted (or partially transmitted) over the wire/socket.
func (t *StageTracker) IsDispatched() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dispatched
}

// Category returns the outcome category.
func (t *StageTracker) Category() ExecutionOutcomeCategory {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.category
}

// MarkStage advances the dispatch stage and updates the dispatched point-of-no-return flag.
func (t *StageTracker) MarkStage(stage DispatchStage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stage = stage
	if stage == StageWroteHeaders || stage == StageWroteRequest || stage == StageWaitingResponse || stage == StageResponseReceived || stage == StageCompleted {
		t.dispatched = true
		if t.dispatchedTime.IsZero() {
			t.dispatchedTime = time.Now().UTC()
		}
	}
}

// RecordResponse categorizes the execution outcome based on HTTP status code and response payload.
func (t *StageTracker) RecordResponse(statusCode int, respErr error) ExecutionOutcomeCategory {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.statusCode = statusCode
	t.err = respErr
	t.completedTime = time.Now().UTC()
	t.stage = StageCompleted

	if respErr != nil {
		if t.dispatched {
			t.category = OutcomeDispatchedUnknown
		} else {
			t.category = OutcomeNotDispatched
		}
		return t.category
	}

	switch {
	case statusCode >= 200 && statusCode < 300:
		t.category = OutcomeExecuted
	case statusCode == http.StatusNotFound:
		// Explicit 404: confirmed container does not exist; no mutation was applied
		t.category = OutcomeFailedConfirmed
	case statusCode == http.StatusBadRequest || statusCode == http.StatusConflict:
		// Explicit client-side conflict or invalid parameters rejected by daemon
		t.category = OutcomeFailedConfirmed
	case statusCode >= 500:
		// 5xx: daemon encountered internal error during or after mutation (e.g. timeout stopping container)
		// We cannot confirm whether container stopped, started, or hung.
		t.category = OutcomeDispatchedUnknown
	default:
		if t.dispatched {
			t.category = OutcomeDispatchedUnknown
		} else {
			t.category = OutcomeNotDispatched
		}
	}
	return t.category
}

// RecordFailure records a client-level error and categorizes the outcome based on whether dispatch started.
func (t *StageTracker) RecordFailure(err error) ExecutionOutcomeCategory {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.err = err
	t.completedTime = time.Now().UTC()

	if t.dispatched {
		t.category = OutcomeDispatchedUnknown
	} else {
		t.category = OutcomeNotDispatched
	}
	return t.category
}

// ToRecord exports an immutable copy of the attempt record.
func (t *StageTracker) ToRecord() ExecutionAttemptRecord {
	t.mu.Lock()
	defer t.mu.Unlock()

	endTime := t.completedTime
	if endTime.IsZero() {
		endTime = time.Now().UTC()
	}
	durationMs := endTime.Sub(t.startTime).Milliseconds()

	errStr := ""
	if t.err != nil {
		errStr = t.err.Error()
	}

	return ExecutionAttemptRecord{
		AttemptID:         t.attemptID,
		OperationID:       t.operationID,
		ParentStep:        t.parentStep,
		Channel:           t.channel,
		Action:            t.action,
		Target:            t.target,
		ContainerID:       t.containerID,
		FencingGeneration: t.fencingGen,
		Stage:             t.stage,
		Dispatched:        t.dispatched,
		Category:          t.category,
		StatusCode:        t.statusCode,
		Error:             errStr,
		DurationMs:        durationMs,
		Timestamp:         t.startTime,
	}
}

// BindHTTPTrace attaches an httptrace.ClientTrace to the given context to monitor low-level socket events.
func (t *StageTracker) BindHTTPTrace(ctx context.Context) context.Context {
	trace := &httptrace.ClientTrace{
		GetConn: func(hostPort string) {
			t.MarkStage(StageDialing)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			t.MarkStage(StageConnected)
		},
		WroteHeaders: func() {
			t.MarkStage(StageWroteHeaders)
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				t.MarkStage(StageWroteRequest)
			}
		},
		GotFirstResponseByte: func() {
			t.MarkStage(StageResponseReceived)
		},
	}
	return httptrace.WithClientTrace(ctx, trace)
}

type trackerCtxKey struct{}
type trackerSinkKey struct{}
type operationIDCtxKey struct{}

// WithOperationID attaches an operation ID to the context.
func WithOperationID(ctx context.Context, opID string) context.Context {
	if opID == "" {
		return ctx
	}
	return context.WithValue(ctx, operationIDCtxKey{}, opID)
}

// OperationIDFromContext extracts the operation ID from the context if present.
func OperationIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v := ctx.Value(operationIDCtxKey{}); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// WithStageTracker attaches the StageTracker to the context and binds HTTP trace.
func WithStageTracker(ctx context.Context, tracker *StageTracker) context.Context {
	if tracker == nil {
		return ctx
	}
	if tracker.OperationID() == "" {
		if opID := OperationIDFromContext(ctx); opID != "" {
			tracker.SetOperationID(opID)
		}
	}
	if guard := RecoveryLeaseFromContext(ctx); guard != nil && tracker.FencingGen() == 0 {
		tracker.SetFencingGen(guard.Grant.FencingGeneration)
	}
	ctx = context.WithValue(ctx, trackerCtxKey{}, tracker)
	ReportTrackerToSink(ctx, tracker)
	return tracker.BindHTTPTrace(ctx)
}

// StageTrackerFromContext extracts the active StageTracker from context if present.
func StageTrackerFromContext(ctx context.Context) *StageTracker {
	if ctx == nil {
		return nil
	}
	if v := ctx.Value(trackerCtxKey{}); v != nil {
		if t, ok := v.(*StageTracker); ok {
			return t
		}
	}
	return nil
}

// WithTrackerSink configures a sink function in context to receive any StageTracker created during dispatch.
func WithTrackerSink(ctx context.Context, sink func(*StageTracker)) context.Context {
	return context.WithValue(ctx, trackerSinkKey{}, sink)
}

// ReportTrackerToSink forwards the tracker to any registered sink in context.
func ReportTrackerToSink(ctx context.Context, tracker *StageTracker) {
	if ctx == nil || tracker == nil {
		return
	}
	if v := ctx.Value(trackerSinkKey{}); v != nil {
		if sink, ok := v.(func(*StageTracker)); ok && sink != nil {
			sink(tracker)
		}
	}
}

