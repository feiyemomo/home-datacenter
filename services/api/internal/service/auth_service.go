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

func (s *AuthService) Bind(
    userID uint,
    accessKey string,
) (string, error) {

    // 验证用户存在
    _, err := s.userRepo.GetByID(userID)
    if err != nil {
        return "", err
    }

    // Hash AccessKey
    hash := utils.HashAccessKey(accessKey)

    // 查询设备
    device, err := s.deviceRepo.GetByUserIDAndHash(
        userID,
        hash,
    )

    if err != nil {

        if errors.Is(err, gorm.ErrRecordNotFound) {
            return "", errors.New("invalid access key")
        }

        return "", err
    }

    // 检查设备是否被吊销
    if device.RevokedAt != nil {
        return "", errors.New("device revoked")
    }

    // 更新最后登录时间
    now := time.Now()

    device.LastLoginAt = &now

    if err := s.deviceRepo.Update(device); err != nil {
        return "", err
    }

    // 签发JWT
    token, err := utils.GenerateToken(
        userID,
        device.ID,
    )

    if err != nil {
        return "", err
    }

    return token, nil
}