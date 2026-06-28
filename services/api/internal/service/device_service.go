package service

import (
    "home-datacenter-api/internal/model"
    "home-datacenter-api/internal/repository"
    "home-datacenter-api/internal/utils"
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

func (s *DeviceService) CreateDevice(
    userID uint,
    deviceName string,
) (string, error) {

    accessKey, err := utils.GenerateAccessKey()
    if err != nil {
        return "", err
    }

    device := &model.Device{
        UserID:        userID,
        DeviceName:    deviceName,
        AccessKeyHash: utils.HashAccessKey(accessKey),
    }

    if err := s.deviceRepo.Create(device); err != nil {
        return "", err
    }

    return accessKey, nil
}


func (s *DeviceService) RevokeDevice(
    deviceID uint,
) error {

    return s.deviceRepo.Revoke(
        deviceID,
    )
}