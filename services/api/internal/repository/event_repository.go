package repository

import (
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

// EventRepository persists and queries StoredEvent rows.
type EventRepository struct {
	db *gorm.DB
}

// NewEventRepository creates an EventRepository backed by the given DB.
func NewEventRepository(db *gorm.DB) *EventRepository {
	return &EventRepository{db: db}
}

// EventListParams holds optional query filters for List().
type EventListParams struct {
	Topic  string             // exact match; empty = no filter
	Source string             // exact match; empty = no filter
	Status *model.EventStatus // nil = no filter
	Since  *time.Time         // events after this timestamp
	Before *time.Time         // events before this timestamp
	Limit  int                // max rows; clamped to [1, 100]
	Offset int                // pagination offset
}

// List queries events with optional filters, ordered by timestamp DESC
// (newest first). Returns the matching rows and the total count (for
// pagination).
func (r *EventRepository) List(params EventListParams) ([]model.StoredEvent, int64, error) {
	if params.Limit < 1 {
		params.Limit = 20
	}
	if params.Limit > 100 {
		params.Limit = 100
	}

	q := r.db.Model(&model.StoredEvent{})

	if params.Topic != "" {
		q = q.Where("topic = ?", params.Topic)
	}
	if params.Source != "" {
		q = q.Where("source = ?", params.Source)
	}
	if params.Status != nil {
		q = q.Where("status = ?", *params.Status)
	}
	if params.Since != nil {
		q = q.Where("timestamp >= ?", *params.Since)
	}
	if params.Before != nil {
		q = q.Where("timestamp < ?", *params.Before)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var rows []model.StoredEvent
	err := q.Order("timestamp DESC").
		Limit(params.Limit).
		Offset(params.Offset).
		Find(&rows).Error
	if err != nil {
		return nil, 0, err
	}

	return rows, total, nil
}

// GetByID returns a single event by primary key. Returns gorm.ErrRecordNotFound
// if the event does not exist.
func (r *EventRepository) GetByID(id uint) (*model.StoredEvent, error) {
	var ev model.StoredEvent
	if err := r.db.First(&ev, id).Error; err != nil {
		return nil, err
	}
	return &ev, nil
}

// Insert persists a new event row. The caller is responsible for
// setting ID=0 so the DB auto-increments.
func (r *EventRepository) Insert(ev *model.StoredEvent) error {
	return r.db.Create(ev).Error
}

// Delete removes an event row by ID. No ownership check — callers
// should verify authorization before calling this.
func (r *EventRepository) Delete(id uint) error {
	return r.db.Delete(&model.StoredEvent{}, id).Error
}

// UpdateStatus sets the status field on the given event row.
func (r *EventRepository) UpdateStatus(id uint, status model.EventStatus) error {
	return r.db.Model(&model.StoredEvent{}).Where("id = ?", id).
		Update("status", status).Error
}

// PurgeOlderThan deletes all events whose timestamp is before the
// given cutoff. Typically called periodically in the background to
// prevent unbounded database growth.
func (r *EventRepository) PurgeOlderThan(cutoff time.Time) (int64, error) {
	result := r.db.Where("timestamp < ?", cutoff).Delete(&model.StoredEvent{})
	return result.RowsAffected, result.Error
}
