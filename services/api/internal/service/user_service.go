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

// GetIsAdmin reports whether the given user is an admin.
// Used by the WebSocket handler to decide event routing scope.
func (s *UserService) GetIsAdmin(userID uint) (bool, error) {
	user, err := s.userRepo.GetByID(userID)
	if err != nil {
		return false, err
	}
	return user.IsAdmin, nil
}
