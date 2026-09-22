package security

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// GuardManager manages the global home security arming mode.
type GuardManager struct {
	db  *gorm.DB
	bus *eventbus.Bus
	mu  sync.RWMutex

	current model.SecurityState
}

// NewGuardManager creates and seeds the initial guard state.
func NewGuardManager(db *gorm.DB, bus *eventbus.Bus) *GuardManager {
	gm := &GuardManager{
		db:  db,
		bus: bus,
	}
	gm.init()
	return gm
}

func (gm *GuardManager) init() {
	var state model.SecurityState
	if err := gm.db.First(&state).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			state = model.SecurityState{
				Mode:      model.GuardModeArmedAway,
				UpdatedBy: "system",
				UpdatedAt: time.Now(),
			}
			if err := gm.db.Create(&state).Error; err != nil {
				log.Printf("security: failed to seed default security state: %v", err)
			}
		} else {
			log.Printf("security: failed to query security state: %v", err)
		}
	}
	gm.current = state
}

// GetState returns the current state record.
func (gm *GuardManager) GetState() model.SecurityState {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	return gm.current
}

// GetMode returns the current guard mode string.
func (gm *GuardManager) GetMode() string {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	if gm.current.Mode == "" {
		return model.GuardModeArmedAway
	}
	return gm.current.Mode
}

// SetMode transitions the security mode and notifies EventBus.
func (gm *GuardManager) SetMode(mode string, updatedBy string) (model.SecurityState, error) {
	if mode != model.GuardModeDisarmed && mode != model.GuardModeArmedHome && mode != model.GuardModeArmedAway {
		return model.SecurityState{}, fmt.Errorf("invalid guard mode: %q", mode)
	}

	gm.mu.Lock()
	defer gm.mu.Unlock()

	var state model.SecurityState
	if err := gm.db.First(&state).Error; err != nil {
		state = model.SecurityState{
			Mode:      mode,
			UpdatedBy: updatedBy,
			UpdatedAt: time.Now(),
		}
		if err := gm.db.Create(&state).Error; err != nil {
			return model.SecurityState{}, err
		}
	} else {
		state.Mode = mode
		state.UpdatedBy = updatedBy
		state.UpdatedAt = time.Now()
		if err := gm.db.Save(&state).Error; err != nil {
			return model.SecurityState{}, err
		}
	}

	gm.current = state

	if gm.bus != nil {
		payload, _ := json.Marshal(map[string]any{
			"mode":       state.Mode,
			"updated_by": state.UpdatedBy,
			"updated_at": state.UpdatedAt.Unix(),
		})
		gm.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicSecurityGuardMode,
			Source:   eventbus.SourceSystem,
			Severity: eventbus.SeverityInfo,
			Payload:  payload,
		})
	}

	return state, nil
}
