package repository

import (
    "home-datacenter-api/internal/model"
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

func (r *DeviceRepository) Revoke(
    deviceID uint,
) error {

    now := time.Now()

    return r.db.
        Model(&model.Device{}).
        Where("id = ?", deviceID).
        Update(
            "revoked_at",
            &now,
        ).
        Error
}

func (r *DeviceRepository) IsRevoked(
    deviceID uint,
) (bool, error) {

    device, err := r.GetByID(deviceID)
    if err != nil {
        return false, err
    }

    return device.RevokedAt != nil, nil
}


func (r *DeviceRepository) Delete(id uint) error {
    return r.db.Delete(&model.Device{}, id).Error
}