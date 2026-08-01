package service

import (
	"errors"
	"strings"
	"unicode/utf8"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/utils"
)

// Domain-level errors returned by the device API. Handlers translate
// these into HTTP status codes.
var (
	// ErrInvalidDeviceName — device name was empty or longer than 64
	// runes after trimming.
	ErrInvalidDeviceName = errors.New("device name must be 1-64 chars")
)

type DeviceService struct {
	deviceRepo *repository.DeviceRepository
}

func NewDeviceService(
	deviceRepo *repository.DeviceRepository,
) *DeviceService {
	return &DeviceService{
		deviceRepo: deviceRepo,
	}
}

// CreateDevice creates a new device for the given user and returns
// the device plus the plaintext access_key (shown to the user once).
// The access_key is hashed before persistence — only the hash is
// stored, so callers must surface the plaintext key to the user
// immediately because it can never be recovered.
//
// The device name is trimmed and must be 1-64 runes; otherwise
// ErrInvalidDeviceName is returned. revoked_at is left NULL so the
// device is immediately usable for /auth/bind.
func (s *DeviceService) CreateDevice(
	userID uint,
	name string,
) (*model.Device, string, error) {
	// 1. Validate name (1-64 runes after trim).
	n := strings.TrimSpace(name)
	if c := utf8.RuneCountInString(n); c < 1 || c > 64 {
		return nil, "", ErrInvalidDeviceName
	}

	// 2. Generate the plaintext access_key (64 hex chars, same
	//    generator the user-create path uses).
	accessKey, err := utils.GenerateAccessKey()
	if err != nil {
		return nil, "", err
	}

	// 3. Persist only the SHA256 hash; revoked_at stays NULL.
	device := &model.Device{
		UserID:        userID,
		DeviceName:    n,
		AccessKeyHash: utils.HashAccessKey(accessKey),
	}
	if err := s.deviceRepo.Create(device); err != nil {
		return nil, "", err
	}

	return device, accessKey, nil
}

func (s *DeviceService) RevokeDevice(
	deviceID uint,
) error {

	return s.deviceRepo.Revoke(
		deviceID,
	)
}

// HardDeleteDevice permanently removes the device row from the
// database. The caller (handler) is responsible for ensuring the
// device has already been revoked — this method does not check
// revoked_at itself, it just deletes the row.
//
// model.Device has no gorm.DeletedAt field, so deviceRepo.Delete
// already performs a real (physical) DELETE rather than a soft
// delete; no Unscoped() is needed.
func (s *DeviceService) HardDeleteDevice(
	deviceID uint,
) error {

	return s.deviceRepo.Delete(deviceID)
}

// ListDevices returns all devices. Intended for admin views.
func (s *DeviceService) ListDevices() ([]model.Device, error) {
	return s.deviceRepo.GetAll()
}

// ListDevicesByUser returns the devices owned by a given user.
// Used when a non-admin queries the device list.
func (s *DeviceService) ListDevicesByUser(
	userID uint,
) ([]model.Device, error) {
	return s.deviceRepo.GetByUserID(userID)
}

// GetDeviceByID returns a single device by its primary key.
// Used for ownership checks before revocation.
func (s *DeviceService) GetDeviceByID(
	deviceID uint,
) (*model.Device, error) {
	return s.deviceRepo.GetByID(deviceID)
}

// RotateToken increments the device's token_version, invalidating
// all existing JWT tokens issued for this device. The client must
// re-bind with its access_key to obtain a fresh token.
func (s *DeviceService) RotateToken(deviceID uint) error {
	return s.deviceRepo.IncrementTokenVersion(deviceID)
}
