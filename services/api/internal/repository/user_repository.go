package repository

import (
    "home-datacenter-api/internal/model"

    "gorm.io/gorm"
)

type UserRepository struct {
    db *gorm.DB
}

func NewUserRepository(db *gorm.DB) *UserRepository {
    return &UserRepository{
        db: db,
    }
}

// Create 创建用户
func (r *UserRepository) Create(user *model.User) error {
    return r.db.Create(user).Error
}

// GetByID 根据ID查询用户
func (r *UserRepository) GetByID(id uint) (*model.User, error) {
    var user model.User

    err := r.db.First(&user, id).Error
    if err != nil {
        return nil, err
    }

    return &user, nil
}

// GetByName 根据名称查询用户
func (r *UserRepository) GetByName(name string) (*model.User, error) {
    var user model.User

    err := r.db.
        Where("name = ?", name).
        First(&user).
        Error

    if err != nil {
        return nil, err
    }

    return &user, nil
}

// List 查询全部用户
func (r *UserRepository) List() ([]model.User, error) {
    var users []model.User

    err := r.db.Find(&users).Error
    if err != nil {
        return nil, err
    }

    return users, nil
}

// Delete 删除用户
func (r *UserRepository) Delete(id uint) error {
    return r.db.Delete(&model.User{}, id).Error
}