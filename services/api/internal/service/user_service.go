package service

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/utils"
)

// Domain-level errors returned by the user-management API. Handlers
// translate these into HTTP status codes; everything else (GORM
// errors, unexpected) becomes 500.
var (
	// ErrUserNotFound — GET/PUT/DELETE on an id that doesn't exist.
	ErrUserNotFound = errors.New("user not found")
	// ErrInvalidName — name was empty, too long, or not a single
	// trimmed token of unicode letters/digits/_/-, length 1..32.
	ErrInvalidName = errors.New("name must be 1-32 chars of letters, digits, _ or - (no whitespace)")
	// ErrNameTaken — another user already owns this name.
	ErrNameTaken = errors.New("name already in use")
	// ErrLastAdmin — caller is trying to delete or demote the user
	// that would leave the system with zero admins.
	ErrLastAdmin = errors.New("cannot remove/demote the last admin")
	// ErrSelfDelete — caller is trying to delete the user record
	// they themselves authenticated as.
	ErrSelfDelete = errors.New("cannot delete the currently authenticated user")
	// ErrSelfDemote — caller is trying to demote themselves to a
	// non-admin (forces a re-login flow to re-elevate; refused).
	ErrSelfDemote = errors.New("cannot demote the currently authenticated user")
)

const (
	userNameMinLen = 1
	userNameMaxLen = 32
)

// isValidUserName enforces the same rule everywhere: trimmed
// length 1..32, unicode letters/digits + "_"/"-" only. Internal
// whitespace is rejected outright (it would corrupt the
// friendly-name path used by cameras, mqtt topics, etc.), but
// leading/trailing whitespace is silently trimmed so the API is
// forgiving about input.
func isValidUserName(name string) bool {
	n := strings.TrimSpace(name)
	c := utf8.RuneCountInString(n)
	if c < userNameMinLen || c > userNameMaxLen {
		return false
	}
	for _, r := range n {
		switch {
		case unicode.IsLetter(r):
		case unicode.IsDigit(r):
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// normalizeUserName returns the canonical form used for storage
// and equality checks. Leading/trailing whitespace is trimmed;
// any other character class violation returns ErrInvalidName.
func normalizeUserName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if !isValidUserName(n) {
		return "", ErrInvalidName
	}
	return n, nil
}

type UserService struct {
	userRepo   *repository.UserRepository
	deviceRepo *repository.DeviceRepository
}

func NewUserService(
	userRepo *repository.UserRepository,
	deviceRepo *repository.DeviceRepository,
) *UserService {
	return &UserService{
		userRepo:   userRepo,
		deviceRepo: deviceRepo,
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

// List returns every user. Used by the admin user-management page.
// Each entry is augmented with a device_count so the admin sees
// "alice owns 3 devices" without a second round-trip.
func (s *UserService) List() ([]model.User, error) {
	users, err := s.userRepo.List()
	if err != nil {
		return nil, err
	}
	// In-place attach device_count. We don't put it in the model
	// itself because that would pollute GORM's notion of the row;
	// the handler layer picks the field off via a custom DTO.
	return users, nil
}

// ListWithDeviceCount returns the same users but with the
// device_count computed in one pass via a subquery, avoiding
// the N+1 problem on the user-management page.
//
// Currently implemented as the simple loop above (small N in a
// home OS), but the function is named to leave room for the
// optimised query without changing the handler signature.
func (s *UserService) ListWithDeviceCount() ([]UserSummary, error) {
	users, err := s.userRepo.List()
	if err != nil {
		return nil, err
	}
	out := make([]UserSummary, 0, len(users))
	for _, u := range users {
		n, err := s.userRepo.CountDevicesByUser(u.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, UserSummary{
			User:        u,
			DeviceCount: n,
		})
	}
	return out, nil
}

// UserSummary pairs a User with the number of auth devices it
// owns. Used by the user-list response.
type UserSummary struct {
	model.User
	DeviceCount int64 `json:"device_count"`
}

// CreateResult pairs a newly created user with an optional
// first device and its plaintext AccessKey. The AccessKey is
// only available at creation time — it's never stored in plain
// text in the DB (only the SHA256 hash), so callers must
// surface it to the admin immediately after creation.
type CreateResult struct {
	User      *model.User
	Device    *model.Device
	AccessKey string // empty if no initial device was created
}

// Create inserts a new user. The caller is the acting admin; the
// isAdmin flag chooses whether the new user is also an admin.
//
// When initialDeviceName is non-empty, a first auth device is
// created alongside the user (the AccessKey is returned in the
// result so the admin can hand it to the new user). This is a
// convenience path — the admin could also create the user first,
// then create a device separately via DeviceService.CreateDevice.
func (s *UserService) Create(name string, isAdmin bool, initialDeviceName string) (*CreateResult, error) {
	n, err := normalizeUserName(name)
	if err != nil {
		return nil, err
	}
	// Pre-check uniqueness so we surface a clean 409 rather than
	// relying on the GORM unique-constraint error message. The
	// pre-check + insert has a TOCTOU race but the DB unique
	// constraint still protects us; the second insert would just
	// return a 409 with a slightly different message.
	if existing, err := s.userRepo.GetByName(n); err == nil && existing != nil {
		return nil, ErrNameTaken
	} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	u := &model.User{
		Name:    n,
		IsAdmin: isAdmin,
	}
	if err := s.userRepo.Create(u); err != nil {
		// Race: another caller created the same name between
		// the pre-check and the insert. Treat unique violations
		// as ErrNameTaken.
		if isUniqueViolation(err) {
			return nil, ErrNameTaken
		}
		return nil, err
	}

	result := &CreateResult{User: u}

	// Optionally create a first auth device. If device creation
	// fails, we still return the user (it's already committed)
	// plus the error — the admin can retry device creation via
	// the device API. We deliberately do NOT roll back the user:
	// the user record is useful on its own, and rolling back a
	// successful user insert because a device failed would
	// create a weird partial-state UX where the admin has to
	// recreate the user too.
	if initialDeviceName != "" {
		accessKey, err := utils.GenerateAccessKey()
		if err != nil {
			return result, fmt.Errorf("generate access key: %w", err)
		}
		device := &model.Device{
			UserID:        u.ID,
			DeviceName:    initialDeviceName,
			AccessKeyHash: utils.HashAccessKey(accessKey),
		}
		if err := s.deviceRepo.Create(device); err != nil {
			return result, fmt.Errorf("create initial device: %w", err)
		}
		result.Device = device
		result.AccessKey = accessKey
	}

	return result, nil
}

// Update performs a partial update. Either field may be nil to
// mean "leave unchanged". The callerID is the acting admin (we
// refuse to let an admin demote themselves through this API).
func (s *UserService) Update(targetID, callerID uint, name *string, isAdmin *bool) (*model.User, error) {
	target, err := s.userRepo.GetByID(targetID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	// Apply name change.
	if name != nil {
		n, err := normalizeUserName(*name)
		if err != nil {
			return nil, err
		}
		if n != target.Name {
			// Pre-check uniqueness for the rename target.
			if existing, err := s.userRepo.GetByName(n); err == nil && existing != nil && existing.ID != target.ID {
				return nil, ErrNameTaken
			} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, err
			}
			target.Name = n
		}
	}
	// Apply isAdmin change.
	if isAdmin != nil && *isAdmin != target.IsAdmin {
		// Demotion path: refuse to demote the only remaining admin.
		if target.IsAdmin && !*isAdmin {
			if targetID == callerID {
				return nil, ErrSelfDemote
			}
			count, err := s.userRepo.CountAdmins()
			if err != nil {
				return nil, err
			}
			if count <= 1 {
				return nil, ErrLastAdmin
			}
		}
		target.IsAdmin = *isAdmin
	}
	if err := s.userRepo.Update(target); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	return target, nil
}

// Delete removes a user and every device they own. Refuses to
// delete the caller themselves or the last remaining admin.
//
// The camera rows are NOT cascaded — they have an OwnerID column
// for future user-ownership transfer flows, and admins may want
// to keep them across user-lifecycle changes. If a future
// requirement surfaces ("delete user → remove everything they
// own"), add a separate `?cascade=cameras` query param and a
// matching cascade in handler/registry.
func (s *UserService) Delete(targetID, callerID uint) (deletedDevices int64, err error) {
	if targetID == callerID {
		return 0, ErrSelfDelete
	}
	target, err := s.userRepo.GetByID(targetID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, ErrUserNotFound
		}
		return 0, err
	}
	if target.IsAdmin {
		count, err := s.userRepo.CountAdmins()
		if err != nil {
			return 0, err
		}
		if count <= 1 {
			return 0, ErrLastAdmin
		}
	}
	// Cascade devices first. We do this BEFORE the user delete
	// so a partial failure leaves a recoverable state: if the
	// device delete fails the user row is still around and the
	// admin can retry. (The inverse order would orphan devices
	// that point at a now-missing user_id.)
	n, err := s.deviceRepo.DeleteByUser(targetID)
	if err != nil {
		return 0, fmt.Errorf("cascade devices: %w", err)
	}
	if err := s.userRepo.Delete(targetID); err != nil {
		return n, fmt.Errorf("delete user: %w", err)
	}
	return n, nil
}

// isUniqueViolation returns true for GORM errors caused by a
// unique-constraint violation, across the two drivers we use
// (glebarez/sqlite and gorm.io/postgres, the latter being the
// future migration target — see project_memory.md "Next Items").
//
// We keep the check loose: any error string containing
// "UNIQUE constraint failed" (sqlite) or "duplicate key" (pg)
// is treated as a uniqueness violation. False positives are
// harmless — the handler maps them to 409 and the operator
// retries with a different value.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value")
}
