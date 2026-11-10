package ops

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

type schedulerClaimCtxKey struct{}

type schedulerClaim struct {
	ProposalID     string
	ExecutionToken string
}

// WithSchedulerClaim attaches a pre-claimed execution token onto ctx when ProposalScheduler
// atomically moves a SCHEDULED proposal to EXECUTING.
func WithSchedulerClaim(ctx context.Context, proposalID, executionToken string) context.Context {
	return context.WithValue(ctx, schedulerClaimCtxKey{}, schedulerClaim{
		ProposalID:     proposalID,
		ExecutionToken: executionToken,
	})
}

// SchedulerClaimFromContext extracts a pre-claimed scheduler execution token if matching proposalID.
func SchedulerClaimFromContext(ctx context.Context, proposalID string) (string, bool) {
	if ctx == nil || proposalID == "" {
		return "", false
	}
	c, ok := ctx.Value(schedulerClaimCtxKey{}).(schedulerClaim)
	if !ok || c.ExecutionToken == "" {
		return "", false
	}
	if c.ProposalID != proposalID {
		return "", false
	}
	return c.ExecutionToken, true
}

// ProposalScheduler is the central background runner in Go Core for scheduled proposals.
// It scans for SCHEDULED proposals, validates that their schedule window has arrived,
// claims them via atomic CAS, and dispatches them to OperationExecutor.
type ProposalScheduler struct {
	db       *gorm.DB
	executor OperationExecutor
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	running  bool
}

// NewProposalScheduler creates a new ProposalScheduler with a 30-second default polling interval.
func NewProposalScheduler(db *gorm.DB, executor OperationExecutor) *ProposalScheduler {
	return &ProposalScheduler{
		db:       db,
		executor: executor,
		interval: 30 * time.Second,
		stopCh:   make(chan struct{}),
	}
}

// SetInterval updates the polling interval (useful for tests).
func (s *ProposalScheduler) SetInterval(interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interval = interval
}

// Start launches the background scheduler loop.
func (s *ProposalScheduler) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.stopCh = make(chan struct{})
	s.mu.Unlock()

	s.wg.Add(1)
	go s.runLoop()
	log.Println("[ops-scheduler] Go Core ProposalScheduler started (polling every 30s)")
}

// Stop gracefully terminates the scheduler loop and waits for active loops to finish.
func (s *ProposalScheduler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.stopCh)
	s.mu.Unlock()

	s.wg.Wait()
	log.Println("[ops-scheduler] Go Core ProposalScheduler stopped cleanly")
}

// IsRunning reports whether the scheduler loop is active.
func (s *ProposalScheduler) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *ProposalScheduler) runLoop() {
	defer s.wg.Done()
	s.mu.Lock()
	interval := s.interval
	s.mu.Unlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial sweep immediately on startup
	s.Tick()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.Tick()
		}
	}
}

// Tick performs a single scheduling cycle: scans SCHEDULED proposals, verifies window,
// atomically CAS-claims them, and dispatches execution. Exposed for testing.
func (s *ProposalScheduler) Tick() {
	if s.db == nil || s.executor == nil {
		return
	}

	var scheduled []model.OpsProposal
	err := s.db.Where("status = ?", model.ProposalStatusScheduled).Find(&scheduled).Error
	if err != nil {
		log.Printf("[ops-scheduler] query scheduled proposals failed: %v", err)
		return
	}

	for _, prop := range scheduled {
		// 1. Validate schedule window & transient safety preconditions before claiming EXECUTING (Blocker 2)
		if err := validateScheduledWindow(&prop); err != nil {
			continue
		}
		propKey := prop.ProposalID
		if propKey == "" {
			propKey = fmt.Sprintf("%d", prop.ID)
		}
		if defExec, ok := s.executor.(*DefaultOperationExecutor); ok && defExec != nil {
			canonTarget, cErr := ResolveComponent(prop.TargetComponent)
			if cErr == nil {
				preCtx, preCancel := context.WithTimeout(context.Background(), 3*time.Second)
				preErr := defExec.checkProposalSafetyPreconditions(preCtx, canonTarget)
				preCancel()
				if preErr != nil {
					log.Printf("[ops-scheduler] deferring scheduled proposal %s due to safety precondition: %v", propKey, preErr)
					continue
				}
			}
		}

		// 2. Atomic CAS claim via unified ProposalFSM: transition SCHEDULED -> EXECUTING with execution_token
		execToken := GenerateOperationID("sched")
		fsm := NewProposalFSM(s.db)
		won, casErr := fsm.TransitionStrict(ProposalTransitionParams{
			ProposalID:       propKey,
			ExpectedStatuses: []string{model.ProposalStatusScheduled},
			TargetStatus:     model.ProposalStatusExecuting,
			Updates: map[string]interface{}{
				"execution_token": execToken,
				"executed_at":     utils.NullTime{Time: time.Now().UTC(), Valid: true},
			},
			Audit: &ProposalTransitionAudit{
				OperationID: execToken,
				EventType:   "ops.proposal.scheduled_claimed",
				Actor:       "proposal-scheduler",
				Level:       model.LevelInfo,
				Message:     fmt.Sprintf("定时调度器认领低峰期提案 [%s] 进入执行", propKey),
			},
		})
		if casErr != nil || !won {
			// Won by another worker, cancelled, or DB error -> skip
			continue
		}

		log.Printf("[ops-scheduler] claimed scheduled proposal %s (ID %d, execToken %s) for execution", propKey, prop.ID, execToken)

		// 3. Dispatch execution asynchronously in goroutine with scheduler claim token
		pCopy := prop
		pCopy.Status = model.ProposalStatusExecuting
		pCopy.ExecutionToken = execToken
		go func(p model.OpsProposal, key string, tok string) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			claimCtx := WithSchedulerClaim(ctx, key, tok)
			_, _ = s.executor.ExecuteProposal(claimCtx, &p)
		}(pCopy, propKey, execToken)
	}
}
