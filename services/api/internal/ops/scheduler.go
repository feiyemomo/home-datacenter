package ops

import (
	"context"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

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
		// 1. Validate schedule window
		if err := validateScheduledWindow(&prop); err != nil {
			// Outside off-peak or target time not reached -> skip
			continue
		}

		// 2. Atomic CAS claim: transition SCHEDULED -> EXECUTING
		res := s.db.Model(&model.OpsProposal{}).
			Where("id = ? AND status = ?", prop.ID, model.ProposalStatusScheduled).
			Updates(map[string]interface{}{
				"status":      model.ProposalStatusExecuting,
				"executed_at": utils.NullTime{Time: time.Now(), Valid: true},
			})
		if res.Error != nil || res.RowsAffected == 0 {
			// Won by another worker, cancelled, or DB error -> skip
			continue
		}

		log.Printf("[ops-scheduler] claimed scheduled proposal %s (ID %d) for execution", prop.ProposalID, prop.ID)

		// 3. Dispatch execution asynchronously in goroutine
		pCopy := prop
		go func(p model.OpsProposal) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			_, _ = s.executor.ExecuteProposal(ctx, &p)
		}(pCopy)
	}
}
