package service

import (
	"errors"
	"time"

	"gorm.io/gorm"

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
func (s *AuthService) Bind(
	userID uint,
	accessKey string,
) (string, error) {

	// Verify user exists
	if _, err := s.userRepo.GetByID(userID); err != nil {
		return "", err
	}

	// Hash the access key (DB stores only the hash)
	hash := utils.HashAccessKey(accessKey)

	// Find the device by (user_id, hash)
	device, err := s.deviceRepo.GetByUserIDAndHash(userID, hash)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", errors.New("invalid access key")
		}
		return "", err
	}

	// Reject revoked devices
	if device.RevokedAt.Valid {
		return "", errors.New("device revoked")
	}

	// Update last login timestamp
	now := time.Now()
	device.LastLoginAt = utils.NullTime{Time: now, Valid: true}

	if err := s.deviceRepo.Update(device); err != nil {
		return "", err
	}

	// Issue a long-lived JWT (365d, see utils.TokenExpireDays)
	token, err := utils.GenerateToken(userID, device.ID)
	if err != nil {
		return "", err
	}

	return token, nil
}
