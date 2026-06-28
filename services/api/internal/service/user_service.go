package service

import (
    "home-datacenter-api/internal/model"
    "home-datacenter-api/internal/repository"
)

type UserService struct {
    userRepo *repository.UserRepository
}

func NewUserService(
    userRepo *repository.UserRepository,
) *UserService {
    return &UserService{
        userRepo: userRepo,
    }
}

func (s *UserService) GetByID(
    id uint,
) (*model.User, error) {
    return s.userRepo.GetByID(id)
}