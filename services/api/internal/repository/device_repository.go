package repository

import (
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
	"time"

	"gorm.io/gorm"
)

type DeviceRepository struct {
	db *gorm.DB
}

func NewDeviceRepository(db *gorm.DB) *DeviceRepository {
	return &DeviceRepository{
		db: db,
	}
}

func (r *DeviceRepository) Create(device *model.Device) error {
	return r.db.Create(device).Error
}

func (r *DeviceRepository) GetByID(id uint) (*model.Device, error) {
	var device model.Device

	err := r.db.First(&device, id).Error
	if err != nil {
		return nil, err
	}

	return &device, nil
}

func (r *DeviceRepository) GetByUserID(userID uint) ([]model.Device, error) {
	var devices []model.Device

	err := r.db.
		Where("user_id = ?", userID).
		Find(&devices).
		Error

	return devices, err
}

// GetAll returns every device row, ordered by id ascending.
// Used by admins for the device management view.
func (r *DeviceRepository) GetAll() ([]model.Device, error) {
	var devices []model.Device

	err := r.db.
		Order("id ASC").
		Find(&devices).
		Error

	return devices, err
}

func (r *DeviceRepository) GetByAccessKeyHash(hash string) (*model.Device, error) {
	var device model.Device

	err := r.db.
		Where("access_key_hash = ?", hash).
		First(&device).
		Error

	if err != nil {
		return nil, err
	}

	return &device, nil
}

func (r *DeviceRepository) GetByUserIDAndHash(
	userID uint,
	hash string,
) (*model.Device, error) {

	var device model.Device

	err := r.db.
		Where(
			"user_id = ? AND access_key_hash = ?",
			userID,
			hash,
		).
		First(&device).
		Error

	if err != nil {
		return nil, err
	}

	return &device, nil
}

func (r *DeviceRepository) Update(
	device *model.Device,
) error {
	return r.db.Save(device).Error
}

// Revoke marks a device as revoked by setting revoked_at to now.
// The column is written through utils.NullTime so it stores a real
// SQL NULL when not revoked and a timestamp otherwise.
func (r *DeviceRepository) Revoke(
	deviceID uint,
) error {

	now := time.Now()

	return r.db.
		Model(&model.Device{}).
		Where("id = ?", deviceID).
		Update(
			"revoked_at",
			utils.NullTime{Time: now, Valid: true},
		).
		Error
}

// IsRevoked reports whether the device has been revoked.
// Uses NullTime.Valid (true when revoked_at is not NULL).
func (r *DeviceRepository) IsRevoked(
	deviceID uint,
) (bool, error) {

	device, err := r.GetByID(deviceID)
	if err != nil {
		return false, err
	}

	return device.RevokedAt.Valid, nil
}

func (r *DeviceRepository) Delete(id uint) error {
	return r.db.Delete(&model.Device{}, id).Error
}

// DeleteByUser removes every device owned by the given user.
// Used by the user-management API as a hard-delete cascade: when
// an admin deletes a user, the auth identities they owned are
// also wiped (revocation is meaningless once the user row is gone,
// so we delete rather than soft-revoke).
//
// Returns the number of rows removed so the caller can log it
// (and so the API response can surface "deleted 3 devices").
func (r *DeviceRepository) DeleteByUser(userID uint) (int64, error) {
	res := r.db.Where("user_id = ?", userID).Delete(&model.Device{})
	return res.RowsAffected, res.Error
}

// UpdateLastSeen persists last_seen_at and last_ip for a device.
// Called asynchronously by the device Manager on every heartbeat.
func (r *DeviceRepository) UpdateLastSeen(
	deviceID uint,
	ip string,
) error {

	now := time.Now()

	return r.db.
		Model(&model.Device{}).
		Where("id = ?", deviceID).
		Updates(map[string]interface{}{
			"last_seen_at": utils.NullTime{Time: now, Valid: true},
			"last_ip":      ip,
		}).
		Error
}
