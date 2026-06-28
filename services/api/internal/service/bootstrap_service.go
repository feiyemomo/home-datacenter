package service

import (
    "errors"

    "gorm.io/gorm"

    "home-datacenter-api/internal/model"
    "home-datacenter-api/internal/repository"
)

type BootstrapService struct {
    userRepo *repository.UserRepository
}

func NewBootstrapService(
    userRepo *repository.UserRepository,
) *BootstrapService {
    return &BootstrapService{
        userRepo: userRepo,
    }
}

func (s *BootstrapService) InitAdmin() error {

    _, err := s.userRepo.GetByName("自己")

    if err == nil {
        return nil
    }

    if !errors.Is(err, gorm.ErrRecordNotFound) {
        return err
    }

    admin := &model.User{
        Name:    "自己",
        IsAdmin: true,
    }

    return s.userRepo.Create(admin)
}