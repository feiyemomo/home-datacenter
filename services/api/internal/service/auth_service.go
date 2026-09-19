package service

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/utils"
)

type AuthService struct {
	userRepo   *repository.UserRepository
	deviceRepo *repository.DeviceRepository
}

func NewAuthService(
	userRepo *repository.UserRepository,
	deviceRepo *repository.DeviceRepository,
) *AuthService {
	return &AuthService{
		userRepo:   userRepo,
		deviceRepo: deviceRepo,
	}
}

// Bind exchanges an access_key for a long-lived JWT.
// Flow: user lookup -> hash key -> find device -> revoke check ->
// update last login -> sign JWT.
//
// Returns the bound device alongside the token so callers (the auth
// handler) can emit a user.login EventBus event with device_id and
// device_name without re-querying.
func (s *AuthService) Bind(
	userID uint,
	accessKey string,
) (string, *model.Device, error) {

	// Verify user exists
	if _, err := s.userRepo.GetByID(userID); err != nil {
		return "", nil, err
	}

	// Hash the access key (DB stores only the hash)
	hash := utils.HashAccessKey(accessKey)

	// Find the device by (user_id, hash)
	device, err := s.deviceRepo.GetByUserIDAndHash(userID, hash)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil, errors.New("invalid access key")
		}
		return "", nil, err
	}

	// Reject revoked devices
	if device.RevokedAt.Valid {
		return "", nil, errors.New("device revoked")
	}

	// Update last login timestamp
	now := time.Now()
	device.LastLoginAt = utils.NullTime{Time: now, Valid: true}

	if err := s.deviceRepo.Update(device); err != nil {
		return "", nil, err
	}

	// Issue a long-lived JWT (365d, see utils.TokenExpireDays).
	// Embed the device's current token_version so the middleware can
	// reject tokens issued before an admin rotation.
	token, err := utils.GenerateToken(userID, device.ID, device.TokenVersion)
	if err != nil {
		return "", nil, err
	}

	return token, device, nil
}

// GetDeviceForAuth is the lightweight lookup the /api/v1/auth/verify
// endpoint uses to re-check a JWT's device for revocation. It is
// separate from JWTAuth's in-line deviceRepo.GetByID call so the
// verify endpoint can return a clean 200/401 contract for nginx
// auth_request without dragging in the full middleware chain.
//
// Note: this is a per-request DB hit. The nginx `auth_request` is
// sub-requested on every /go2rtc/ call, so a busy dashboard can
// trigger hundreds of these per second. The query is a primary-key
// lookup (single index hit, no join), and we deliberately do not
// introduce a cache here: a revoked device MUST stop streaming on
// the next request, not after a cache TTL.
func (s *AuthService) GetDeviceForAuth(deviceID uint) (*model.Device, error) {
	return s.deviceRepo.GetByID(deviceID)
}

// Refresh re-issues a fresh long-lived JWT for an already-authenticated device,
// updating its LastLoginAt timestamp to the current time.
func (s *AuthService) Refresh(userID uint, deviceID uint) (string, *model.Device, error) {
	device, err := s.deviceRepo.GetByID(deviceID)
	if err != nil {
		return "", nil, err
	}
	if device.UserID != userID {
		return "", nil, errors.New("device does not belong to user")
	}
	if device.RevokedAt.Valid {
		return "", nil, errors.New("device revoked")
	}

	// Update last login timestamp
	now := time.Now()
	device.LastLoginAt = utils.NullTime{Time: now, Valid: true}
	if err := s.deviceRepo.Update(device); err != nil {
		return "", nil, err
	}

	token, err := utils.GenerateToken(userID, device.ID, device.TokenVersion)
	if err != nil {
		return "", nil, err
	}

	return token, device, nil
}

