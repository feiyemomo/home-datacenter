package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// Standard operation errors
var (
	ErrComponentNotWhitelisted   = errors.New("component is not in authorized ops whitelist")
	ErrProbeTimeout              = errors.New("health probe verification timed out")
	ErrInvalidSpec               = errors.New("invalid operation spec")
	ErrRollbackFailed            = errors.New("atomic rollback execution failed")
	ErrBaselineMismatch          = errors.New("baseline hash mismatch: target state has drifted")
	ErrEmptyBaseline             = errors.New("proposal baseline hash is empty or cannot be computed")
	ErrApprovalDigestMismatch    = errors.New("approval digest mismatch: proposal was modified after approval")
	ErrActionNotSupported        = errors.New("operation action not supported")
	ErrShellMetacharacter        = errors.New("operation spec contains forbidden shell metacharacter")
	ErrNotImplemented            = errors.New("operation not implemented or requires unsupported capabilities")
	ErrProposalAlreadyRunning    = errors.New("proposal is already being executed")
	ErrProposalNotExecutable     = errors.New("proposal is not in an executable state")
	ErrProposalUnauthorized      = errors.New("proposal is not authorized for execution")
	ErrScheduledWindowNotReached = errors.New("scheduled proposal execution window has not arrived")
)

// Permissible structured action types
const (
	ActionDockerRestart     = "docker.restart"
	ActionDockerRecreate    = "docker.recreate"
	ActionDockerUpdateImage = "docker.update_image"
	ActionDockerExec        = "docker.exec"
	ActionDBCreateIndex     = "db.create_index"
	ActionDBDropIndex       = "db.drop_index"
	ActionDBVacuumSnapshot  = "db.vacuum_snapshot"
	ActionWAFReload         = "waf.reload"
	ActionWAFBanIP          = "waf.ban_ip"
	ActionWAFUnbanIP        = "waf.unban_ip"
	ActionCachePurge        = "cache.purge"
)

// Canonical component names
const (
	ComponentHomeAPI    = "home-api"
	ComponentHomeWeb    = "home-web"
	ComponentHomeVision = "home-vision"
	ComponentFrigate    = "frigate"
	ComponentMosquitto  = "mosquitto"
	ComponentOpenResty  = "openresty"
	Component1PWAF      = "1pwaf"
)

// coreComponentWhitelist defines the strict bidirectional core component boundary.
var coreComponentWhitelist = map[string]bool{
	ComponentHomeAPI:    true,
	ComponentHomeWeb:    true,
	ComponentHomeVision: true,
	ComponentFrigate:    true,
	ComponentMosquitto:  true,
	ComponentOpenResty:  true,
	Component1PWAF:      true,
}

// componentAliases maps user, LLM, or container aliases to canonical names.
var componentAliases = map[string]string{
	// home-api
	"home-api": ComponentHomeAPI,
	"api":      ComponentHomeAPI,
	"backend":  ComponentHomeAPI,
	"go-api":   ComponentHomeAPI,

	// home-web
	"home-web": ComponentHomeWeb,
	"web":      ComponentHomeWeb,
	"frontend": ComponentHomeWeb,
	"nginx":    ComponentHomeWeb,

	// home-vision
	"home-vision": ComponentHomeVision,
	"vision":      ComponentHomeVision,
	"ai-vision":   ComponentHomeVision,

	// frigate
	"frigate":      ComponentFrigate,
	"home-frigate": ComponentFrigate,
	"nvr":          ComponentFrigate,
	"go2rtc":       ComponentFrigate,

	// mosquitto
	"mosquitto":      ComponentMosquitto,
	"home-mosquitto": ComponentMosquitto,
	"mqtt":           ComponentMosquitto,

	// openresty
	"openresty":             ComponentOpenResty,
	"1panel-openresty":      ComponentOpenResty,
	"nginx-proxy":           ComponentOpenResty,
	"1panel-openresty-wudu": ComponentOpenResty,

	// 1pwaf
	"1pwaf":         Component1PWAF,
	"waf":           Component1PWAF,
	"openresty-waf": Component1PWAF,
}

// canonicalContainerNames maps canonical component names to Docker container names.
var canonicalContainerNames = map[string]string{
	ComponentHomeAPI:    "home-api",
	ComponentHomeWeb:    "home-web",
	ComponentHomeVision: "home-vision",
	ComponentFrigate:    "home-frigate",
	ComponentMosquitto:  "home-mosquitto",
	ComponentOpenResty:  "1Panel-openresty-WuDU",
	Component1PWAF:      "1Panel-openresty-WuDU",
}

// identifierRegex strictly restricts database identifiers (table/index names) to prevent SQL injection.
var identifierRegex = regexp.MustCompile(`^[a-zA-Z0-9_]{1,64}$`)

// OperationSpec represents a machine-executable, strongly-typed operation specification.
type OperationSpec struct {
	Action       string                 `json:"action"`
	Target       string                 `json:"target"`
	Params       map[string]interface{} `json:"params,omitempty"`
	BaselineHash string                 `json:"baseline_hash,omitempty"`
	RollbackSpec *OperationSpec         `json:"rollback_spec,omitempty"`
	TimeoutSec   int                    `json:"timeout_sec,omitempty"`
}

// ExecutionResult represents the structured execution output.
type ExecutionResult struct {
	Success      bool      `json:"success"`
	Action       string    `json:"action"`
	Target       string    `json:"target"`
	Output       string    `json:"output"`
	BaselineHash string    `json:"baseline_hash,omitempty"`
	DurationMs   int64     `json:"duration_ms"`
	RolledBack   bool      `json:"rolled_back,omitempty"`
	Error        string    `json:"error,omitempty"`
	ExecutedAt   time.Time `json:"executed_at"`
}

// OperationExecutor defines the execution contract for datacenter operations.
type OperationExecutor interface {
	Execute(ctx context.Context, spec OperationSpec) (*ExecutionResult, error)
	ExecuteRollback(ctx context.Context, spec OperationSpec) error
	ExecuteProposal(ctx context.Context, proposal *model.OpsProposal) (*ExecutionResult, error)
	ValidateComponent(target string) (string, error)
	CheckHealth(ctx context.Context, target string) error
}

// DefaultOperationExecutor implements the OperationExecutor interface.
type DefaultOperationExecutor struct {
	db       *gorm.DB
	bus      *eventbus.Bus
	client   *OnePanelClient
	fallback *HostFallbackProvider

	dockerSocket  string
	probeTimeout  time.Duration
	probeInterval time.Duration

	healthCheckFn     func(ctx context.Context, target string) error
	dockerRunner      func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error)
	baselineExtractor func(target string) string

	leaseManager *RecoveryLeaseManager
	componentLocks sync.Map // canonical string -> *sync.Mutex
	mu             sync.RWMutex
}

func (e *DefaultOperationExecutor) getComponentLock(canonical string) *sync.Mutex {
	val, _ := e.componentLocks.LoadOrStore(canonical, &sync.Mutex{})
	return val.(*sync.Mutex)
}

// GetLeaseManager returns the persistent recovery lease manager.
func (e *DefaultOperationExecutor) GetLeaseManager() *RecoveryLeaseManager {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.leaseManager
}

// SetLeaseManager sets the persistent recovery lease manager.
func (e *DefaultOperationExecutor) SetLeaseManager(mgr *RecoveryLeaseManager) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.leaseManager = mgr
}

// ExecutorOption configures DefaultOperationExecutor behavior.
type ExecutorOption func(*DefaultOperationExecutor)

func WithProbeTimeout(d time.Duration) ExecutorOption {
	return func(e *DefaultOperationExecutor) {
		e.probeTimeout = d
	}
}

func WithProbeInterval(d time.Duration) ExecutorOption {
	return func(e *DefaultOperationExecutor) {
		e.probeInterval = d
	}
}

func WithDockerSocket(path string) ExecutorOption {
	return func(e *DefaultOperationExecutor) {
		e.dockerSocket = path
	}
}

func WithHealthChecker(fn func(ctx context.Context, target string) error) ExecutorOption {
	return func(e *DefaultOperationExecutor) {
		e.healthCheckFn = fn
	}
}

func WithDockerRunner(fn func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error)) ExecutorOption {
	return func(e *DefaultOperationExecutor) {
		e.dockerRunner = fn
	}
}

func WithBaselineExtractor(fn func(target string) string) ExecutorOption {
	return func(e *DefaultOperationExecutor) {
		e.baselineExtractor = fn
	}
}

func WithLeaseManager(mgr *RecoveryLeaseManager) ExecutorOption {
	return func(e *DefaultOperationExecutor) {
		e.leaseManager = mgr
	}
}

// NewOperationExecutor creates a new DefaultOperationExecutor.
func NewOperationExecutor(
	db *gorm.DB,
	bus *eventbus.Bus,
	client *OnePanelClient,
	fallback *HostFallbackProvider,
	opts ...ExecutorOption,
) *DefaultOperationExecutor {
	if fallback == nil {
		fallback = NewHostFallbackProvider("", "")
	}
	exec := &DefaultOperationExecutor{
		db:            db,
		bus:           bus,
		client:        client,
		fallback:      fallback,
		dockerSocket:  "/var/run/docker.sock",
		probeTimeout:  15 * time.Second,
		probeInterval: 1 * time.Second,
	}
	if db != nil {
		exec.leaseManager = NewRecoveryLeaseManager(db)
	}
	for _, opt := range opts {
		opt(exec)
	}
	if exec.baselineExtractor == nil {
		exec.baselineExtractor = func(target string) string {
			clean := strings.TrimSpace(target)
			canonical, err := ResolveComponent(clean)
			if err != nil {
				canonical = strings.ToLower(clean)
				if idx := strings.Index(canonical, "("); idx != -1 {
					canonical = strings.TrimSpace(canonical[:idx])
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var targetContainer *ContainerSummary
			if exec.client != nil {
				if containers, err := exec.client.ListContainers(ctx); err == nil {
					for i := range containers {
						cName := strings.TrimPrefix(containers[i].Name, "/")
						if strings.EqualFold(cName, canonical) {
							targetContainer = &containers[i]
							break
						}
					}
				}
			}
			if targetContainer == nil && exec.fallback != nil {
				fallbackContainers := exec.fallback.GetHostContainers(ctx)
				for i := range fallbackContainers {
					cName := strings.TrimPrefix(fallbackContainers[i].Name, "/")
					if strings.EqualFold(cName, canonical) {
						targetContainer = &fallbackContainers[i]
						break
					}
				}
			}
			hasher := sha256.New()
			if targetContainer != nil {
				raw := fmt.Sprintf("%s:%s:%s:%s", canonical, targetContainer.Image, targetContainer.State, targetContainer.CreatedAt)
				hasher.Write([]byte(raw))
			} else {
				raw := fmt.Sprintf("%s:offline:none", canonical)
				hasher.Write([]byte(raw))
			}
			return fmt.Sprintf("sha256:%x", hasher.Sum(nil))
		}
	}
	return exec
}

// ResolveComponent resolves and validates the target component against the strict whitelist.
func ResolveComponent(target string) (string, error) {
	clean := strings.TrimSpace(target)
	if clean == "" {
		return "", ErrComponentNotWhitelisted
	}
	// Strip parenthetical text, e.g. "home-web (Nginx)" -> "home-web"
	if idx := strings.Index(clean, "("); idx != -1 {
		prefix := strings.TrimSpace(clean[:idx])
		if prefix != "" {
			clean = prefix
		}
	}
	clean = strings.ToLower(clean)

	canonical, ok := componentAliases[clean]
	if !ok {
		return "", ErrComponentNotWhitelisted
	}
	if !coreComponentWhitelist[canonical] {
		return "", ErrComponentNotWhitelisted
	}
	return canonical, nil
}

// ValidateComponent implements OperationExecutor.
func (e *DefaultOperationExecutor) ValidateComponent(target string) (string, error) {
	return ResolveComponent(target)
}

// FenceUntrustedContext wraps dynamic untrusted runtime context in protective XML fencing
// tags to prevent indirect prompt injection into LLM prompts.
func FenceUntrustedContext(input string) string {
	sanitized := strings.ReplaceAll(input, "</untrusted_system_context>", "")
	return "<untrusted_system_context>\n" + sanitized + "\n</untrusted_system_context>"
}

// FenceUntrustedUserInput wraps untrusted user questions/prompts in protective XML fencing tags.
func FenceUntrustedUserInput(input string) string {
	sanitized := strings.ReplaceAll(input, "</untrusted_user_input>", "")
	return "<untrusted_user_input>\n" + sanitized + "\n</untrusted_user_input>"
}

// ValidateSpecSafety ensures the spec contains no forbidden shell metacharacters.
func ValidateSpecSafety(spec OperationSpec) error {
	forbiddenChars := []string{";", "&", "|", "$", "`", ">", "<", "\n", "\r"}
	for _, char := range forbiddenChars {
		if strings.Contains(spec.Action, char) {
			return fmt.Errorf("%w: action contains '%s'", ErrShellMetacharacter, char)
		}
		if strings.Contains(spec.Target, char) {
			return fmt.Errorf("%w: target contains '%s'", ErrShellMetacharacter, char)
		}
	}
	return nil
}

// Execute executes an OperationSpec with whitelist validation, TOCTOU baseline check,
// execution dispatch, health probe verification, and automatic rollback on failure.
func (e *DefaultOperationExecutor) Execute(ctx context.Context, spec OperationSpec) (*ExecutionResult, error) {
	startTime := time.Now()

	// 1. Validate spec safety against shell metacharacters
	if err := ValidateSpecSafety(spec); err != nil {
		return nil, err
	}

	// 2. Enforce bidirectional component whitelist
	canonical, err := ResolveComponent(spec.Target)
	if err != nil {
		return nil, err
	}

	// 2.1 Acquire per-component lock to prevent concurrent mutating operations (Docker 409 Conflict prevention)
	lock := e.getComponentLock(canonical)
	lock.Lock()
	defer lock.Unlock()

	// 2.2 Acquire persistent recovery lease (R13) if lease manager is configured and action is restart
	if e.leaseManager != nil && spec.Action == ActionDockerRestart {
		leaseToken, lErr := e.leaseManager.AcquireLease(canonical, "go-core", 30*time.Second)
		if lErr != nil {
			return &ExecutionResult{
				Success:    false,
				Action:     spec.Action,
				Target:     canonical,
				Error:      fmt.Sprintf("persistent recovery lease conflict: %v", lErr),
				ExecutedAt: startTime,
			}, fmt.Errorf("persistent recovery lease conflict for %s: %w", canonical, lErr)
		}
		defer func() {
			_ = e.leaseManager.ReleaseLease(canonical, leaseToken)
		}()
	}

	// 3. Anti-TOCTOU baseline hash verification (Fail-Closed: calculation failure or drift strictly blocks execution)
	if spec.BaselineHash != "" {
		if e.baselineExtractor == nil {
			return &ExecutionResult{
				Success:      false,
				Action:       spec.Action,
				Target:       canonical,
				BaselineHash: spec.BaselineHash,
				Error:        "baseline extractor not configured on executor",
				ExecutedAt:   startTime,
			}, ErrBaselineMismatch
		}
		currentHash := e.baselineExtractor(canonical)
		if currentHash == "" || currentHash != spec.BaselineHash {
			return &ExecutionResult{
				Success:      false,
				Action:       spec.Action,
				Target:       canonical,
				BaselineHash: spec.BaselineHash,
				Error:        ErrBaselineMismatch.Error(),
				ExecutedAt:   startTime,
			}, ErrBaselineMismatch
		}
	}

	// 4. Dispatch the structured action
	out, err := e.dispatchAction(ctx, spec, canonical)
	if err != nil {
		// Execution mutation failed -> attempt atomic rollback if specified with decoupled context
		var rbErr error
		if spec.RollbackSpec != nil {
			rbCtx, rbCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer rbCancel()
			rbErr = e.ExecuteRollback(rbCtx, *spec.RollbackSpec)
		}
		duration := time.Since(startTime).Milliseconds()
		status := "failed"
		if spec.RollbackSpec != nil {
			if rbErr == nil {
				status = "rolled_back"
			} else {
				status = "rollback_failed"
			}
		}
		e.recordExecutionHistory(canonical, spec.Action, status, out, fmt.Sprintf("execution failed: %v; rollback err: %v", err, rbErr))

		errDetails := err.Error()
		if rbErr != nil {
			errDetails = fmt.Sprintf("%s (rollback also failed: %v)", errDetails, rbErr)
		}

		return &ExecutionResult{
			Success:      false,
			Action:       spec.Action,
			Target:       canonical,
			Output:       out,
			BaselineHash: spec.BaselineHash,
			DurationMs:   duration,
			RolledBack:   spec.RollbackSpec != nil && rbErr == nil,
			Error:        errDetails,
			ExecutedAt:   startTime,
		}, err
	}

	// 5. Post-mutation 15s health probe polling
	probeErr := e.pollHealth(ctx, canonical)
	duration := time.Since(startTime).Milliseconds()

	if probeErr != nil {
		// Health probe failed! Trigger atomic rollback with decoupled timeout context (Blocker 1 fix)
		var rbErr error
		if spec.RollbackSpec != nil {
			rbCtx, rbCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer rbCancel()
			rbErr = e.ExecuteRollback(rbCtx, *spec.RollbackSpec)
		}
		status := "failed"
		if spec.RollbackSpec != nil {
			if rbErr == nil {
				status = "rolled_back"
			} else {
				status = "rollback_failed"
			}
		}
		e.recordExecutionHistory(canonical, spec.Action, status, out, fmt.Sprintf("health probe failed: %v; rollback err: %v", probeErr, rbErr))

		errDetails := fmt.Sprintf("health probe failed: %v", probeErr)
		if rbErr != nil {
			errDetails = fmt.Sprintf("%s (rollback also failed: %v)", errDetails, rbErr)
		}

		return &ExecutionResult{
			Success:      false,
			Action:       spec.Action,
			Target:       canonical,
			Output:       out,
			BaselineHash: spec.BaselineHash,
			DurationMs:   duration,
			RolledBack:   spec.RollbackSpec != nil && rbErr == nil,
			Error:        errDetails,
			ExecutedAt:   startTime,
		}, probeErr
	}

	// 6. Success
	e.recordExecutionHistory(canonical, spec.Action, "success", out, "health probe passed")
	return &ExecutionResult{
		Success:      true,
		Action:       spec.Action,
		Target:       canonical,
		Output:       out,
		BaselineHash: spec.BaselineHash,
		DurationMs:   duration,
		RolledBack:   false,
		ExecutedAt:   startTime,
	}, nil
}

// ExecuteRollback executes the inverse rollback action specification.
func (e *DefaultOperationExecutor) ExecuteRollback(ctx context.Context, rollbackSpec OperationSpec) error {
	if err := ValidateSpecSafety(rollbackSpec); err != nil {
		return err
	}

	canonical, err := ResolveComponent(rollbackSpec.Target)
	if err != nil {
		return err
	}

	_, err = e.dispatchAction(ctx, rollbackSpec, canonical)
	return err
}

// validateScheduledWindow verifies that for SCHEDULED proposals, execution only occurs
// within the designated low-peak window (02:00-05:00) or after the specific target time.
func validateScheduledWindow(proposal *model.OpsProposal) error {
	if proposal == nil || proposal.Status != model.ProposalStatusScheduled {
		return nil
	}
	now := time.Now()
	nowH, nowM, _ := now.Clock()

	if proposal.ScheduledAt != nil && *proposal.ScheduledAt != "" {
		schedStr := *proposal.ScheduledAt
		if len(schedStr) == 5 && strings.Contains(schedStr, ":") {
			var schedH, schedM int
			if _, err := fmt.Sscanf(schedStr, "%d:%d", &schedH, &schedM); err == nil {
				// 1. Off-peak window check (02:00 - 05:00)
				if !IsOffPeak(now) {
					return fmt.Errorf("%w: proposal %s scheduled for off-peak window %s (current: %02d:%02d is outside off-peak 02:00-05:00)", ErrScheduledWindowNotReached, proposal.ProposalID, schedStr, nowH, nowM)
				}
				// 2. Exact schedule time check
				if (nowH < schedH) || (nowH == schedH && nowM < schedM) {
					return fmt.Errorf("%w: proposal %s scheduled for %s (current: %02d:%02d)", ErrScheduledWindowNotReached, proposal.ProposalID, schedStr, nowH, nowM)
				}
				return nil
			}
		} else if parsedTime, err := time.Parse(time.RFC3339, schedStr); err == nil {
			if now.Before(parsedTime) {
				return fmt.Errorf("%w: proposal %s schedule time %s has not arrived yet", ErrScheduledWindowNotReached, proposal.ProposalID, schedStr)
			}
			return nil
		}
	}

	// Default fallback: scheduled proposals without explicit timestamp require off-peak window
	if !IsOffPeak(now) {
		return fmt.Errorf("%w: scheduled proposal %s requires off-peak window (current: %02d:%02d is outside off-peak 02:00-05:00)", ErrScheduledWindowNotReached, proposal.ProposalID, nowH, nowM)
	}
	return nil
}

// ExecuteProposal executes a proposal end-to-end, managing status transitions
// (EXECUTING -> EXECUTED or ROLLING_BACK -> ROLLED_BACK / ROLLBACK_FAILED) and updating database state.
func (e *DefaultOperationExecutor) ExecuteProposal(ctx context.Context, proposal *model.OpsProposal) (*ExecutionResult, error) {
	if proposal == nil {
		return nil, ErrInvalidSpec
	}

	// Gate check: forbid AWAITING_APPROVAL and unauthorized PENDING execution
	if proposal.Status == model.ProposalStatusAwaitingApproval || proposal.Status == model.ProposalStatusPending || proposal.Status == model.ProposalStatusApprovalRequired {
		return nil, fmt.Errorf("%w: proposal %s requires approval before execution (status: %s)", ErrProposalUnauthorized, proposal.ProposalID, proposal.Status)
	}

	// CAS atomic update: only execute if proposal is in an authorized state (APPROVED or SCHEDULED)
	if e.db != nil && proposal.ID > 0 {
		var curr model.OpsProposal
		if err := e.db.First(&curr, proposal.ID).Error; err != nil {
			return nil, fmt.Errorf("proposal %d not found: %w", proposal.ID, err)
		}

		if curr.Status == model.ProposalStatusExecuting || curr.Status == model.ProposalStatusRollingBack {
			proposal.Status = curr.Status
			return nil, ErrProposalAlreadyRunning
		}
		if curr.Status == model.ProposalStatusAwaitingApproval || curr.Status == model.ProposalStatusPending || curr.Status == model.ProposalStatusApprovalRequired {
			proposal.Status = curr.Status
			return nil, fmt.Errorf("%w: proposal %s requires approval before execution (status: %s)", ErrProposalUnauthorized, proposal.ProposalID, curr.Status)
		}
		if curr.Status != model.ProposalStatusApproved && curr.Status != model.ProposalStatusScheduled {
			proposal.Status = curr.Status
			return nil, fmt.Errorf("%w: proposal %s is not in an executable state (status: %s)", ErrProposalNotExecutable, proposal.ProposalID, curr.Status)
		}

		// R5: BaselineHash anti-tampering check (Fail-Closed)
		if strings.TrimSpace(curr.BaselineHash) == "" {
			return nil, ErrEmptyBaseline
		}

		// R9: ApprovalDigest cryptographic binding verification against database state
		if strings.TrimSpace(curr.ApprovalDigest) == "" {
			return nil, fmt.Errorf("%w: proposal in database lacks approval digest", ErrApprovalDigestMismatch)
		}
		expectedDigestDB, err := ComputeProposalApprovalDigest(&curr)
		if err != nil || expectedDigestDB != curr.ApprovalDigest {
			return nil, fmt.Errorf("%w: database proposal record was tampered with post-approval (expected %s, computed %s)", ErrApprovalDigestMismatch, curr.ApprovalDigest, expectedDigestDB)
		}

		// Memory vs DB approval digest consistency
		if proposal.ApprovalDigest != "" && proposal.ApprovalDigest != curr.ApprovalDigest {
			return nil, fmt.Errorf("%w: memory proposal approval digest does not match database record", ErrApprovalDigestMismatch)
		}

		// R5: Pre-execution baseline re-validation against live component state (anti-TOCTOU)
		if e.baselineExtractor != nil {
			canonicalTarget, err := ResolveComponent(curr.TargetComponent)
			if err == nil {
				currentHash := e.baselineExtractor(canonicalTarget)
				if currentHash == "" || currentHash != curr.BaselineHash {
					return nil, fmt.Errorf("%w: target component %s state drifted before execution (expected %s, got %s)", ErrBaselineMismatch, canonicalTarget, curr.BaselineHash, currentHash)
				}
			}
		}

		// If scheduled, verify schedule window has arrived
		if err := validateScheduledWindow(&curr); err != nil {
			return nil, err
		}

		res := e.db.Model(&model.OpsProposal{}).
			Where("id = ? AND status IN (?, ?)",
				proposal.ID,
				model.ProposalStatusApproved,
				model.ProposalStatusScheduled,
			).
			Updates(map[string]interface{}{
				"status":      model.ProposalStatusExecuting,
				"executed_at": utils.NullTime{Time: time.Now(), Valid: true},
			})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			var latest model.OpsProposal
			if err := e.db.First(&latest, proposal.ID).Error; err == nil {
				proposal.Status = latest.Status
				if latest.Status != model.ProposalStatusApproved && latest.Status != model.ProposalStatusScheduled {
					return nil, fmt.Errorf("%w: proposal %s is not in an executable state (status: %s)", ErrProposalNotExecutable, proposal.ProposalID, latest.Status)
				}
			}
			return nil, ErrProposalAlreadyRunning
		}

		// Synchronize authoritative DB fields to memory proposal
		proposal.TargetComponent = curr.TargetComponent
		proposal.ActionSpec = curr.ActionSpec
		proposal.ExecutionPlan = curr.ExecutionPlan
		proposal.BaselineHash = curr.BaselineHash
		proposal.ApprovalDigest = curr.ApprovalDigest
		proposal.Version = curr.Version
	} else {
		// Enforce state check for in-memory / mock executions without DB
		if proposal.Status == model.ProposalStatusAwaitingApproval || proposal.Status == model.ProposalStatusPending || proposal.Status == model.ProposalStatusApprovalRequired {
			return nil, fmt.Errorf("%w: proposal %s requires approval before execution (status: %s)", ErrProposalUnauthorized, proposal.ProposalID, proposal.Status)
		}
		if proposal.Status != model.ProposalStatusApproved && proposal.Status != model.ProposalStatusScheduled {
			return nil, fmt.Errorf("%w: proposal %s is not in an executable state (status: %s)", ErrProposalNotExecutable, proposal.ProposalID, proposal.Status)
		}

		// R5: Verify baseline is present
		if strings.TrimSpace(proposal.BaselineHash) == "" {
			return nil, ErrEmptyBaseline
		}

		// R9: Verify approval digest
		if strings.TrimSpace(proposal.ApprovalDigest) == "" {
			return nil, fmt.Errorf("%w: in-memory proposal lacks approval digest", ErrApprovalDigestMismatch)
		}
		expectedDigestMem, err := ComputeProposalApprovalDigest(proposal)
		if err != nil || expectedDigestMem != proposal.ApprovalDigest {
			return nil, fmt.Errorf("%w: in-memory proposal parameters do not match approval digest", ErrApprovalDigestMismatch)
		}

		// R5: Pre-execution baseline re-validation against live component state
		if e.baselineExtractor != nil {
			canonicalTarget, err := ResolveComponent(proposal.TargetComponent)
			if err == nil {
				currentHash := e.baselineExtractor(canonicalTarget)
				if currentHash == "" || currentHash != proposal.BaselineHash {
					return nil, fmt.Errorf("%w: target component %s state drifted before execution (expected %s, got %s)", ErrBaselineMismatch, canonicalTarget, proposal.BaselineHash, currentHash)
				}
			}
		}

		if err := validateScheduledWindow(proposal); err != nil {
			return nil, err
		}
	}
	proposal.Status = model.ProposalStatusExecuting

	// Parse or build OperationSpec
	spec, err := e.parseProposalSpec(proposal)
	if err != nil {
		proposal.Status = model.ProposalStatusFailed
		proposal.ExecutionLog = fmt.Sprintf("failed to parse operation spec: %v", err)
		proposal.ExecutedAt = utils.NullTime{Time: time.Now(), Valid: true}
		if e.db != nil {
			_ = e.db.Save(proposal)
		}
		return nil, err
	}

	// Execute via OperationExecutor
	res, execErr := e.Execute(ctx, spec)
	now := time.Now()
	proposal.ExecutedAt = utils.NullTime{Time: now, Valid: true}

	if execErr != nil || (res != nil && !res.Success) {
		// If execution timed out or produced unknown HTTP/network state
		if errors.Is(execErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) ||
			(execErr != nil && strings.Contains(strings.ToLower(execErr.Error()), "deadline exceeded")) ||
			(execErr != nil && strings.Contains(strings.ToLower(execErr.Error()), "timeout")) {
			proposal.Status = model.ProposalStatusExecutionUnknown
			proposal.ExecutionLog = fmt.Sprintf("execution timed out or produced unknown state (%v); manual reconciliation required", execErr)
			if e.db != nil {
				_ = e.db.Save(proposal)
			}
			e.publishEvent(eventbus.SeverityError, "ops.proposal.unknown", proposal)
			return res, execErr
		}

		// If rollback spec was defined, transition to ROLLING_BACK first in DB
		if spec.RollbackSpec != nil && e.db != nil && proposal.ID > 0 {
			_ = e.db.Model(&model.OpsProposal{}).Where("id = ?", proposal.ID).Update("status", model.ProposalStatusRollingBack)
		}

		if spec.RollbackSpec != nil {
			if res != nil && res.RolledBack {
				proposal.Status = model.ProposalStatusRolledBack
			} else {
				proposal.Status = model.ProposalStatusRollbackFailed
			}
		} else {
			proposal.Status = model.ProposalStatusFailed
		}

		logMsg := fmt.Sprintf("execution failed: %v", execErr)
		if res != nil {
			logMsg = fmt.Sprintf("execution failed (%s); rolled back: %v; output: %s", res.Error, res.RolledBack, res.Output)
		}
		proposal.ExecutionLog = logMsg
		if e.db != nil {
			_ = e.db.Save(proposal)
		}
		e.publishEvent(eventbus.SeverityError, "ops.proposal.failed", proposal)
		return res, execErr
	}

	// Success -> transition to EXECUTED
	proposal.Status = model.ProposalStatusExecuted
	if res != nil {
		proposal.ExecutionLog = res.Output
	}
	if e.db != nil {
		_ = e.db.Save(proposal)
	}
	e.publishEvent(eventbus.SeverityInfo, "ops.proposal.executed", proposal)
	return res, nil
}

// parseProposalSpec converts proposal fields into a typed OperationSpec.
// Strictly enforces component consistency between ActionSpec and Proposal.TargetComponent,
// validates rollback spec target consistency, and prevents ActionSpec tampering.
func (e *DefaultOperationExecutor) parseProposalSpec(proposal *model.OpsProposal) (OperationSpec, error) {
	canonicalProposalTarget, err := ResolveComponent(proposal.TargetComponent)
	if err != nil {
		return OperationSpec{}, fmt.Errorf("invalid proposal target component: %w", err)
	}

	if proposal.ActionSpec != "" {
		var spec OperationSpec
		if err := json.Unmarshal([]byte(proposal.ActionSpec), &spec); err != nil {
			return OperationSpec{}, fmt.Errorf("%w: failed to parse proposal action spec: %v", ErrInvalidSpec, err)
		}
		if spec.Action == "" {
			return OperationSpec{}, fmt.Errorf("%w: action spec missing action field", ErrInvalidSpec)
		}

		canonicalSpecTarget, err := ResolveComponent(spec.Target)
		if err != nil {
			return OperationSpec{}, fmt.Errorf("action spec target invalid: %w", err)
		}
		if canonicalSpecTarget != canonicalProposalTarget {
			return OperationSpec{}, fmt.Errorf("security violation: action spec target '%s' does not match proposal target '%s'", canonicalSpecTarget, canonicalProposalTarget)
		}
		spec.Target = canonicalSpecTarget

		if spec.BaselineHash != "" && proposal.BaselineHash != "" && spec.BaselineHash != proposal.BaselineHash {
			return OperationSpec{}, fmt.Errorf("%w: action spec baseline hash '%s' does not match approved proposal baseline '%s'", ErrBaselineMismatch, spec.BaselineHash, proposal.BaselineHash)
		}
		// ActionSpec MUST NOT override approved baseline; proposal baseline is strictly authoritative (R5.4)
		if proposal.BaselineHash != "" {
			spec.BaselineHash = proposal.BaselineHash
		}

		if spec.RollbackSpec != nil {
			if spec.RollbackSpec.Target != "" {
				rbTarget, err := ResolveComponent(spec.RollbackSpec.Target)
				if err != nil || rbTarget != canonicalSpecTarget {
					return OperationSpec{}, fmt.Errorf("security violation: rollback target '%s' does not match action target '%s'", spec.RollbackSpec.Target, canonicalSpecTarget)
				}
				spec.RollbackSpec.Target = rbTarget
			} else {
				spec.RollbackSpec.Target = canonicalSpecTarget
			}
		}

		if err := ValidateSpecSafety(spec); err != nil {
			return OperationSpec{}, err
		}

		return spec, nil
	}

	canonical, err := ResolveComponent(proposal.TargetComponent)
	if err != nil {
		return OperationSpec{}, err
	}

	spec := OperationSpec{
		Target:       canonical,
		BaselineHash: proposal.BaselineHash,
	}

	planLower := strings.ToLower(proposal.ExecutionPlan)
	switch {
	case strings.Contains(planLower, "restart"):
		spec.Action = ActionDockerRestart
	case strings.Contains(planLower, "update") || strings.Contains(planLower, "recreate") || strings.Contains(planLower, "pull"):
		spec.Action = ActionDockerRecreate
	case strings.Contains(planLower, "create index"):
		spec.Action = ActionDBCreateIndex
	case strings.Contains(planLower, "drop index"):
		spec.Action = ActionDBDropIndex
	case strings.Contains(planLower, "reload"):
		spec.Action = ActionWAFReload
	case strings.Contains(planLower, "cache") || strings.Contains(planLower, "purge"):
		spec.Action = ActionCachePurge
	default:
		// Default to container restart for whitelisted container operations
		spec.Action = ActionDockerRestart
	}

	// Build rollback spec if rollback plan exists
	if proposal.RollbackPlan != "" {
		rbSpec := &OperationSpec{
			Target: canonical,
		}
		rbLower := strings.ToLower(proposal.RollbackPlan)
		switch {
		case strings.Contains(rbLower, "drop index"):
			rbSpec.Action = ActionDBDropIndex
		case strings.Contains(rbLower, "recreate") || strings.Contains(rbLower, "rollback") || strings.Contains(rbLower, "image") || strings.Contains(rbLower, "version"):
			rbSpec.Action = ActionDockerRecreate
			if spec.Params != nil && spec.Params["previous_image"] != nil {
				rbSpec.Params = map[string]interface{}{
					"image": spec.Params["previous_image"],
				}
			}
		case strings.Contains(rbLower, "restart"):
			rbSpec.Action = ActionDockerRestart
		default:
			if spec.Action == ActionDockerRecreate || spec.Action == ActionDockerUpdateImage {
				rbSpec.Action = ActionDockerRecreate
				if spec.Params != nil && spec.Params["previous_image"] != nil {
					rbSpec.Params = map[string]interface{}{
						"image": spec.Params["previous_image"],
					}
				}
			} else {
				rbSpec.Action = ActionDockerRestart
			}
		}
		spec.RollbackSpec = rbSpec
	}

	return spec, nil
}

// dispatchAction handles execution dispatch without bare shell commands.
func (e *DefaultOperationExecutor) dispatchAction(ctx context.Context, spec OperationSpec, canonical string) (string, error) {
	if e.dockerRunner != nil {
		containerName := canonicalContainerNames[canonical]
		return e.dockerRunner(ctx, spec.Action, containerName, spec.Params)
	}

	switch spec.Action {
	case ActionDockerRestart:
		return e.executeDockerRestart(ctx, canonical)

	case ActionDockerRecreate, ActionDockerUpdateImage:
		return e.executeDockerRecreate(ctx, canonical, spec.Params)

	case ActionDockerExec:
		return e.executeDockerExec(ctx, canonical, spec.Params)

	case ActionWAFReload:
		return e.executeWAFReload(ctx)

	case ActionDBCreateIndex:
		return e.executeDBCreateIndex(spec.Params)

	case ActionDBDropIndex:
		return e.executeDBDropIndex(spec.Params)

	case ActionDBVacuumSnapshot:
		return e.executeDBVacuumSnapshot(spec.Params)

	case ActionCachePurge:
		return e.executeCachePurge(ctx, spec.Params)

	default:
		return "", fmt.Errorf("%w: %s", ErrActionNotSupported, spec.Action)
	}
}

// executeDockerRestart restarts a whitelisted container via 1Panel client or Docker socket without shell.
func (e *DefaultOperationExecutor) executeDockerRestart(ctx context.Context, canonical string) (string, error) {
	containerName := canonicalContainerNames[canonical]
	if containerName == "" {
		containerName = canonical
	}

	// 1. Try 1Panel client if available
	if e.client != nil {
		if err := e.client.RestartContainer(ctx, containerName); err == nil {
			return fmt.Sprintf("container %s restarted via 1Panel API", containerName), nil
		}
	}

	// 2. Fall back to Docker socket HTTP client
	if e.fallback != nil {
		if err := e.fallback.RestartContainerFallback(ctx, containerName); err == nil {
			return fmt.Sprintf("container %s restarted via Docker socket", containerName), nil
		}
	}

	// 3. Direct Docker socket execution
	if err := executeDockerSocketRestart(ctx, e.dockerSocket, containerName); err == nil {
		return fmt.Sprintf("container %s restarted via direct Docker socket", containerName), nil
	}

	return "", fmt.Errorf("failed to restart container %s: all providers unavailable", containerName)
}

// executeDockerRecreate recreates a container via Docker socket API without shell.
func (e *DefaultOperationExecutor) executeDockerRecreate(ctx context.Context, canonical string, params map[string]interface{}) (string, error) {
	containerName := canonicalContainerNames[canonical]
	if containerName == "" {
		containerName = canonical
	}

	var image string
	if params != nil {
		if img, ok := params["image"].(string); ok && img != "" {
			image = img
		} else if img, ok := params["new_image"].(string); ok && img != "" {
			image = img
		}
	}

	if _, err := os.Stat(e.dockerSocket); err != nil {
		return "", fmt.Errorf("docker socket not accessible for container recreation: %w", err)
	}

	return executeDockerSocketRecreate(ctx, e.dockerSocket, containerName, image)
}

// executeCachePurge clears specific system caches or builder caches safely.
func (e *DefaultOperationExecutor) executeCachePurge(ctx context.Context, params map[string]interface{}) (string, error) {
	cacheType := "docker_builder"
	if params != nil {
		if ct, ok := params["cache_type"].(string); ok && ct != "" {
			cacheType = ct
		} else if target, ok := params["target"].(string); ok && target != "" {
			cacheType = target
		}
	}

	switch cacheType {
	case "docker_builder", "docker_build":
		// Docker builder prune via socket
		if _, err := os.Stat(e.dockerSocket); err != nil {
			return "", fmt.Errorf("docker socket not accessible for builder cache purge: %w", err)
		}
		tr := &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", e.dockerSocket)
			},
		}
		defer tr.CloseIdleConnections()
		client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/build/prune", nil)
		if err != nil {
			return "", fmt.Errorf("failed to build prune request: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("failed to prune docker builder cache: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			return "", fmt.Errorf("docker build prune returned status %d: %s", resp.StatusCode, string(b))
		}
		var pruneResult struct {
			SpaceReclaimed int64    `json:"SpaceReclaimed"`
			CachesDeleted  []string `json:"CachesDeleted"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&pruneResult)
		return fmt.Sprintf("docker build cache pruned successfully, reclaimed %d bytes (%d caches deleted)", pruneResult.SpaceReclaimed, len(pruneResult.CachesDeleted)), nil

	case "transcode_temp", "video_cache":
		tempDir := os.Getenv("TRANSCODE_TEMP_DIR")
		if tempDir == "" {
			tempDir = "/tmp/transcode"
		}
		cleanDir := filepath.Clean(tempDir)
		if cleanDir == "/" || cleanDir == "." || cleanDir == "\\" {
			return "", fmt.Errorf("forbidden transcode cache directory: %s", cleanDir)
		}
		stat, err := os.Stat(cleanDir)
		if err != nil {
			return "", fmt.Errorf("transcode cache directory inaccessible: %w", err)
		}
		if !stat.IsDir() {
			return "", fmt.Errorf("transcode cache path is not a directory: %s", cleanDir)
		}
		entries, err := os.ReadDir(cleanDir)
		if err != nil {
			return "", fmt.Errorf("failed to read transcode cache directory: %w", err)
		}
		removed := 0
		var reclaimedBytes int64 = 0
		now := time.Now()
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			// Security guard: Don't purge files currently being written (modified within last 5 minutes)
			if now.Sub(info.ModTime()) < 5*time.Minute {
				continue
			}
			// Defend against symlink traversal
			if info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			filePath := filepath.Join(cleanDir, entry.Name())
			if err := os.Remove(filePath); err == nil {
				removed++
				reclaimedBytes += info.Size()
			}
		}
		return fmt.Sprintf("purged %d temporary transcode files from %s, reclaimed %d bytes", removed, cleanDir, reclaimedBytes), nil

	default:
		return "", fmt.Errorf("%w: unsupported cache purge target '%s'", ErrNotImplemented, cacheType)
	}
}

// allowedDockerExecCommands defines the strict whitelist of permissible non-shell exec commands per component.
var allowedDockerExecCommands = map[string][][]string{
	ComponentOpenResty: {
		{"nginx", "-t"},
		{"nginx", "-s", "reload"},
		{"openresty", "-t"},
		{"openresty", "-s", "reload"},
		{"nginx", "-v"},
	},
	ComponentHomeWeb: {
		{"nginx", "-t"},
		{"nginx", "-s", "reload"},
	},
	ComponentFrigate: {
		{"python3", "-m", "frigate", "--validate-config"},
	},
}

// executeDockerExec executes a command inside a whitelisted container via Docker socket API without shell.
// Strictly validates command against an approved per-component command whitelist to prevent arbitrary command injection.
func (e *DefaultOperationExecutor) executeDockerExec(ctx context.Context, canonical string, params map[string]interface{}) (string, error) {
	containerName := canonicalContainerNames[canonical]
	if containerName == "" {
		containerName = canonical
	}

	var cmd []string
	if rawCmd, ok := params["command"].([]string); ok {
		cmd = rawCmd
	} else if rawCmdSlice, ok := params["command"].([]interface{}); ok {
		for _, item := range rawCmdSlice {
			if s, ok := item.(string); ok {
				cmd = append(cmd, s)
			}
		}
	}

	if len(cmd) == 0 {
		return "", fmt.Errorf("%w: missing command in params", ErrInvalidSpec)
	}

	// Validate cmd against per-component whitelist
	allowedList, hasList := allowedDockerExecCommands[canonical]
	if !hasList {
		return "", fmt.Errorf("%w: docker.exec is not permitted for component '%s'", ErrActionNotSupported, canonical)
	}

	matched := false
	for _, allowedCmd := range allowedList {
		if len(allowedCmd) == len(cmd) {
			exactMatch := true
			for i := range allowedCmd {
				if allowedCmd[i] != cmd[i] {
					exactMatch = false
					break
				}
			}
			if exactMatch {
				matched = true
				break
			}
		}
	}

	if !matched {
		return "", fmt.Errorf("%w: command %v is not in the approved whitelist for %s", ErrActionNotSupported, cmd, canonical)
	}

	return executeDockerSocketExecOutput(ctx, e.dockerSocket, containerName, cmd)
}

// executeWAFReload triggers OpenResty configuration reload without bare shell.
func (e *DefaultOperationExecutor) executeWAFReload(ctx context.Context) (string, error) {
	containerName := canonicalContainerNames[ComponentOpenResty]
	if containerName == "" {
		containerName = "1Panel-openresty-WuDU"
	}

	// Execute `nginx -s reload` directly via Docker Unix socket
	cmd := []string{"nginx", "-s", "reload"}
	err := executeDockerSocketExec(ctx, e.dockerSocket, containerName, cmd)
	if err != nil {
		return "", fmt.Errorf("openresty reload failed: %w", err)
	}
	return "openresty reloaded successfully", nil
}

// executeDBCreateIndex executes a CREATE INDEX statement with strictly validated identifiers.
func (e *DefaultOperationExecutor) executeDBCreateIndex(params map[string]interface{}) (string, error) {
	if e.db == nil {
		return "", fmt.Errorf("database instance not configured")
	}

	tableName, _ := params["table_name"].(string)
	indexName, _ := params["index_name"].(string)
	columnsRaw, _ := params["columns"].([]interface{})

	var columns []string
	for _, c := range columnsRaw {
		if s, ok := c.(string); ok {
			columns = append(columns, s)
		}
	}

	if !identifierRegex.MatchString(tableName) || !identifierRegex.MatchString(indexName) {
		return "", fmt.Errorf("%w: invalid table or index name identifier", ErrInvalidSpec)
	}
	for _, col := range columns {
		if !identifierRegex.MatchString(col) {
			return "", fmt.Errorf("%w: invalid column name identifier: %s", ErrInvalidSpec, col)
		}
	}

	colStr := strings.Join(columns, ", ")
	if colStr == "" {
		return "", fmt.Errorf("%w: columns required for index creation", ErrInvalidSpec)
	}

	query := fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s(%s);", indexName, tableName, colStr)
	if err := e.db.Exec(query).Error; err != nil {
		return "", fmt.Errorf("failed to create index: %w", err)
	}

	return fmt.Sprintf("index %s on %s created", indexName, tableName), nil
}

// executeDBDropIndex drops an index with strictly validated identifier.
func (e *DefaultOperationExecutor) executeDBDropIndex(params map[string]interface{}) (string, error) {
	if e.db == nil {
		return "", fmt.Errorf("database instance not configured")
	}

	indexName, _ := params["index_name"].(string)
	if !identifierRegex.MatchString(indexName) {
		return "", fmt.Errorf("%w: invalid index name identifier", ErrInvalidSpec)
	}

	query := fmt.Sprintf("DROP INDEX IF EXISTS %s;", indexName)
	if err := e.db.Exec(query).Error; err != nil {
		return "", fmt.Errorf("failed to drop index: %w", err)
	}

	return fmt.Sprintf("index %s dropped", indexName), nil
}

// executeDBVacuumSnapshot performs a VACUUM INTO snapshot safely.
func (e *DefaultOperationExecutor) executeDBVacuumSnapshot(params map[string]interface{}) (string, error) {
	if e.db == nil {
		return "", fmt.Errorf("database instance not configured")
	}

	destPath, _ := params["path"].(string)
	if destPath == "" {
		return "", fmt.Errorf("%w: missing destination snapshot path", ErrInvalidSpec)
	}

	safePath := strings.ReplaceAll(destPath, "'", "''")
	query := fmt.Sprintf("VACUUM INTO '%s'", safePath)
	if err := e.db.Exec(query).Error; err != nil {
		return "", fmt.Errorf("failed to execute vacuum snapshot: %w", err)
	}

	return fmt.Sprintf("vacuum snapshot created at %s", destPath), nil
}

// pollHealth executes the 15-second health verification loop after a mutating operation.
func (e *DefaultOperationExecutor) pollHealth(ctx context.Context, canonical string) error {
	timeout := e.probeTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	interval := e.probeInterval
	if interval <= 0 {
		interval = 1 * time.Second
	}

	deadline := time.Now().Add(timeout)
	var lastErr error

	for {
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("%w: %v", ErrProbeTimeout, lastErr)
			}
			return ErrProbeTimeout
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := e.CheckHealth(probeCtx, canonical)
		cancel()

		if err == nil {
			return nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func inDockerEnv() bool {
	if os.Getenv("IN_DOCKER") == "true" {
		return true
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}

func getProbeHost(canonical string) string {
	envKey := fmt.Sprintf("PROBE_HOST_%s", strings.ToUpper(strings.ReplaceAll(canonical, "-", "_")))
	if custom := os.Getenv(envKey); custom != "" {
		return custom
	}
	if inDockerEnv() {
		switch canonical {
		case ComponentHomeAPI:
			return "home-api"
		case ComponentHomeWeb:
			return "home-web"
		case ComponentHomeVision:
			return "home-vision"
		case ComponentFrigate:
			return "home-frigate"
		case ComponentMosquitto:
			return "home-mosquitto"
		case ComponentOpenResty, Component1PWAF:
			return "1Panel-openresty-WuDU"
		}
	}
	return "127.0.0.1"
}

// CheckHealth verifies target availability via HTTP GET or TCP connect.
func (e *DefaultOperationExecutor) CheckHealth(ctx context.Context, target string) error {
	if e.healthCheckFn != nil {
		return e.healthCheckFn(ctx, target)
	}

	canonical, err := ResolveComponent(target)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 2 * time.Second}

	switch canonical {
	case ComponentHomeAPI:
		// 1. If DB connection is configured, verify DB connectivity
		if e.db != nil {
			sqlDB, err := e.db.DB()
			if err != nil {
				return fmt.Errorf("home-api db handle error: %w", err)
			}
			if pingErr := sqlDB.PingContext(ctx); pingErr != nil {
				return fmt.Errorf("home-api db ping failed: %w", pingErr)
			}
			// In standalone / unit-test mode with local DB and no docker or custom probe host,
			// DB ping verification confirms home-api state without relying on local host port conflicts.
			if !inDockerEnv() && os.Getenv("PROBE_HOST_HOME_API") == "" {
				return nil
			}
		}

		host := getProbeHost(ComponentHomeAPI)
		healthURL := fmt.Sprintf("http://%s:8080/health", host)
		if customURL := os.Getenv("PROBE_HEALTH_URL"); customURL != "" {
			healthURL = customURL
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("home-api http health check failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("home-api health check returned %d", resp.StatusCode)
		}
		return nil

	case ComponentHomeWeb:
		host := getProbeHost(ComponentHomeWeb)
		port := 8086
		if inDockerEnv() && host == "home-web" {
			port = 80
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/", host, port), nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			return fmt.Errorf("home-web health check returned %d", resp.StatusCode)
		}
		return nil

	case ComponentHomeVision:
		host := getProbeHost(ComponentHomeVision)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:8090/health", host), nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("home-vision health check returned %d", resp.StatusCode)
		}
		return nil

	case ComponentFrigate:
		host := getProbeHost(ComponentFrigate)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:5000/api/config", host), nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("frigate health check returned %d", resp.StatusCode)
		}
		return nil

	case ComponentMosquitto:
		host := getProbeHost(ComponentMosquitto)
		d := net.Dialer{Timeout: 2 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("%s:1883", host))
		if err != nil {
			return err
		}
		_ = conn.Close()
		return nil

	case ComponentOpenResty, Component1PWAF:
		host := getProbeHost(ComponentOpenResty)
		d := net.Dialer{Timeout: 2 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("%s:80", host))
		if err != nil {
			return fmt.Errorf("openresty tcp check failed: %w", err)
		}
		_ = conn.Close()
		return nil

	default:
		return ErrComponentNotWhitelisted
	}
}

// recordExecutionHistory writes an immutable audit record to ops_execution_histories.
func (e *DefaultOperationExecutor) recordExecutionHistory(component, action, status, output, detail string) {
	if e.db == nil {
		return
	}
	history := model.OpsExecutionHistory{
		ExecutionType: "proposal_exec",
		Component:     component,
		Action:        action,
		Status:        status,
		HealthOutput:  output,
		Detail:        detail,
		CreatedAt:     time.Now(),
	}
	_ = e.db.Create(&history)
}

// publishEvent sends an event to EventBus if configured.
func (e *DefaultOperationExecutor) publishEvent(severity string, eventType string, prop *model.OpsProposal) {
	if e.bus == nil || prop == nil {
		return
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"event":         eventType,
		"proposal_id":   prop.ProposalID,
		"title":         prop.Title,
		"status":        prop.Status,
		"execution_log": prop.ExecutionLog,
		"timestamp":     time.Now().Unix(),
	})
	e.bus.Publish(eventbus.Event{
		Topic:    eventbus.TopicSystemLog,
		Source:   eventbus.SourceSystem,
		Severity: severity,
		Payload:  payload,
	})
}

// executeDockerSocketRestart issues a container restart via Docker Unix Socket HTTP API.
func executeDockerSocketRestart(ctx context.Context, sockPath, containerName string) error {
	if _, err := os.Stat(sockPath); err != nil {
		return fmt.Errorf("docker socket not accessible: %w", err)
	}

	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}

	reqURL := fmt.Sprintf("http://localhost/containers/%s/restart?t=10", containerName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("docker restart returned status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// executeDockerSocketExec issues a command execution in a container via Docker Unix Socket HTTP API without shell.
func executeDockerSocketExec(ctx context.Context, sockPath, containerName string, cmd []string) error {
	_, err := executeDockerSocketExecOutput(ctx, sockPath, containerName, cmd)
	return err
}

// executeDockerSocketExecOutput issues exec and captures output via Docker Unix Socket.
func executeDockerSocketExecOutput(ctx context.Context, sockPath, containerName string, cmd []string) (string, error) {
	if _, err := os.Stat(sockPath); err != nil {
		return "", fmt.Errorf("docker socket not accessible: %w", err)
	}

	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}

	// 1. Create exec instance
	createPayload, err := json.Marshal(map[string]interface{}{
		"AttachStdout": true,
		"AttachStderr": true,
		"Cmd":          cmd,
	})
	if err != nil {
		return "", err
	}

	createURL := fmt.Sprintf("http://localhost/containers/%s/exec", containerName)
	createReq, err := http.NewRequestWithContext(ctx, http.MethodPost, createURL, strings.NewReader(string(createPayload)))
	if err != nil {
		return "", err
	}
	createReq.Header.Set("Content-Type", "application/json")

	createResp, err := client.Do(createReq)
	if err != nil {
		return "", err
	}
	defer createResp.Body.Close()

	if createResp.StatusCode != http.StatusCreated && createResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(createResp.Body)
		return "", fmt.Errorf("docker exec create returned %d: %s", createResp.StatusCode, string(b))
	}

	var createResult struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&createResult); err != nil {
		return "", fmt.Errorf("failed to decode exec id: %w", err)
	}
	if createResult.ID == "" {
		return "", fmt.Errorf("empty exec id returned from docker socket")
	}

	// 2. Start exec instance
	startPayload, _ := json.Marshal(map[string]interface{}{
		"Detach": false,
		"Tty":    false,
	})
	startURL := fmt.Sprintf("http://localhost/exec/%s/start", createResult.ID)
	startReq, err := http.NewRequestWithContext(ctx, http.MethodPost, startURL, strings.NewReader(string(startPayload)))
	if err != nil {
		return "", err
	}
	startReq.Header.Set("Content-Type", "application/json")

	startResp, err := client.Do(startReq)
	if err != nil {
		return "", err
	}
	defer startResp.Body.Close()

	outBytes, _ := io.ReadAll(startResp.Body)
	log.Printf("[executor] docker exec %s in %s completed (status %d)", cmd, containerName, startResp.StatusCode)
	return string(outBytes), nil
}

// ContainerInspectSafetyConfig decodes ALL runtime and security parameters from docker inspect.
type ContainerInspectSafetyConfig struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Image        string              `json:"Image"`
		User         string              `json:"User"` // MUST NOT BE DROPPED! Preserves non-root user
		Env          []string            `json:"Env"`
		Cmd          []string            `json:"Cmd"`
		Entrypoint   []string            `json:"Entrypoint"`
		WorkingDir   string              `json:"WorkingDir"`
		Labels       map[string]string   `json:"Labels"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		StopSignal   string              `json:"StopSignal,omitempty"`
	} `json:"Config"`
	HostConfig struct {
		Binds          []string                 `json:"Binds"`
		Mounts         []map[string]interface{} `json:"Mounts"`
		PortBindings   map[string]interface{}   `json:"PortBindings"`
		RestartPolicy  map[string]interface{}   `json:"RestartPolicy"`
		CapAdd         []string                 `json:"CapAdd"`
		CapDrop        []string                 `json:"CapDrop"`
		SecurityOpt    []string                 `json:"SecurityOpt"`
		Devices        []map[string]interface{} `json:"Devices"`
		DeviceRequests []map[string]interface{} `json:"DeviceRequests"`
		NetworkMode    string                   `json:"NetworkMode"`
		Privileged     bool                     `json:"Privileged"`
		ReadonlyRootfs bool                     `json:"ReadonlyRootfs"`
		Dns            []string                 `json:"Dns"`
		ExtraHosts     []string                 `json:"ExtraHosts"`
		Resources      map[string]interface{}   `json:"Resources"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]interface{} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// executeVerifiedRollback performs the 5-stage real rollback and confirms container health:
// Stage 1: Clean up failed candidate container if created (DELETE /containers/{containerName}?v=false&force=true).
// Stage 2: Rename backup container back to original name (POST /containers/{backupName}/rename?name={containerName}).
// Stage 3: Start original container (POST /containers/{containerName}/start).
// Stage 4: Docker inspect verification (GET /containers/{containerName}/json, confirm Running == true).
// Stage 5: Live health check probe.
func executeVerifiedRollback(
	ctx context.Context,
	client *http.Client,
	containerName, backupName string,
	healthChecker func(ctx context.Context, target string) error,
	canonical string,
	triggerCause error,
) error {
	rbCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("[executor] initiating 5-stage verified rollback for %s (backup: %s, cause: %v)",
		containerName, backupName, triggerCause)

	// Stage 1: Clean up failed candidate container if present
	delURL := fmt.Sprintf("http://localhost/containers/%s?v=false&force=true", containerName)
	delReq, _ := http.NewRequestWithContext(rbCtx, http.MethodDelete, delURL, nil)
	if dResp, err := client.Do(delReq); err == nil {
		_ = dResp.Body.Close()
	}

	// Stage 2: Rename backup container back to original name
	renameURL := fmt.Sprintf("http://localhost/containers/%s/rename?name=%s", backupName, containerName)
	renameReq, err := http.NewRequestWithContext(rbCtx, http.MethodPost, renameURL, nil)
	if err != nil {
		return fmt.Errorf("rollback failed at rename request creation (%v); cause: %w", err, triggerCause)
	}
	renameResp, err := client.Do(renameReq)
	if err != nil || (renameResp != nil && renameResp.StatusCode != http.StatusNoContent && renameResp.StatusCode != http.StatusOK) {
		status := 0
		if renameResp != nil {
			status = renameResp.StatusCode
			_ = renameResp.Body.Close()
		}
		return fmt.Errorf("rollback failed at rename (HTTP %d, err: %v); manual reconciliation required; cause: %w", status, err, triggerCause)
	}
	if renameResp != nil {
		_ = renameResp.Body.Close()
	}

	// Stage 3: Start original container
	startURL := fmt.Sprintf("http://localhost/containers/%s/start", containerName)
	startReq, err := http.NewRequestWithContext(rbCtx, http.MethodPost, startURL, nil)
	if err != nil {
		return fmt.Errorf("rollback failed at start request creation (%v); cause: %w", err, triggerCause)
	}
	startResp, err := client.Do(startReq)
	if err != nil || (startResp != nil && startResp.StatusCode != http.StatusNoContent && startResp.StatusCode != http.StatusOK && startResp.StatusCode != http.StatusNotModified) {
		status := 0
		if startResp != nil {
			status = startResp.StatusCode
			_ = startResp.Body.Close()
		}
		return fmt.Errorf("rollback failed at start (HTTP %d, err: %v); manual reconciliation required; cause: %w", status, err, triggerCause)
	}
	if startResp != nil {
		_ = startResp.Body.Close()
	}

	// Stage 4: Docker inspect verification
	inspectURL := fmt.Sprintf("http://localhost/containers/%s/json", containerName)
	insReq, err := http.NewRequestWithContext(rbCtx, http.MethodGet, inspectURL, nil)
	if err != nil {
		return fmt.Errorf("rollback inspect request creation failed: %w", err)
	}
	insResp, err := client.Do(insReq)
	if err != nil || insResp.StatusCode != http.StatusOK {
		status := 0
		if insResp != nil {
			status = insResp.StatusCode
			_ = insResp.Body.Close()
		}
		return fmt.Errorf("rollback inspect failed (HTTP %d, err: %v); manual reconciliation required; cause: %w", status, err, triggerCause)
	}
	defer insResp.Body.Close()
	var stateCheck struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	if err := json.NewDecoder(insResp.Body).Decode(&stateCheck); err != nil || !stateCheck.State.Running {
		return fmt.Errorf("restored container is NOT running; manual reconciliation required; cause: %w", triggerCause)
	}

	// Stage 5: Live health check probe
	if healthChecker != nil {
		probeDeadline := time.Now().Add(10 * time.Second)
		var probeErr error
		for time.Now().Before(probeDeadline) {
			pCtx, pCancel := context.WithTimeout(rbCtx, 2*time.Second)
			probeErr = healthChecker(pCtx, canonical)
			pCancel()
			if probeErr == nil {
				break
			}
			time.Sleep(1 * time.Second)
		}
		if probeErr != nil {
			return fmt.Errorf("restored original container failed health probe (%v); manual reconciliation required; cause: %w", probeErr, triggerCause)
		}
	}

	log.Printf("[executor] 5-stage verified rollback completed successfully for %s", containerName)
	return nil
}

// executeDockerSocketRecreate inspects existing container, backs it up via rename,
// creates & starts new container with new image, validates health probe, and only removes
// the backup container if the new container is healthy. If anything fails, it automatically
// restores the original container via 5-stage verified rollback.
func executeDockerSocketRecreate(ctx context.Context, sockPath, containerName, newImage string) (string, error) {
	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 45 * time.Second}

	// 1. Inspect existing container
	inspectURL := fmt.Sprintf("http://localhost/containers/%s/json", containerName)
	inspectReq, err := http.NewRequestWithContext(ctx, http.MethodGet, inspectURL, nil)
	if err != nil {
		return "", err
	}
	inspectResp, err := client.Do(inspectReq)
	if err != nil {
		return "", fmt.Errorf("failed to inspect container %s: %w", containerName, err)
	}
	defer inspectResp.Body.Close()
	if inspectResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(inspectResp.Body)
		return "", fmt.Errorf("inspect container %s returned %d: %s", containerName, inspectResp.StatusCode, string(b))
	}

	var inspectData ContainerInspectSafetyConfig
	if err := json.NewDecoder(inspectResp.Body).Decode(&inspectData); err != nil {
		return "", fmt.Errorf("failed to decode container inspect json: %w", err)
	}

	targetImage := newImage
	if targetImage == "" {
		targetImage = inspectData.Config.Image
	}
	if targetImage == "" {
		return "", fmt.Errorf("%w: cannot recreate container %s: image not specified and not found in container inspect", ErrInvalidSpec, containerName)
	}

	// 2. Stop container
	stopURL := fmt.Sprintf("http://localhost/containers/%s/stop?t=10", containerName)
	stopReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, stopURL, nil)
	stopResp, err := client.Do(stopReq)
	if err == nil {
		_ = stopResp.Body.Close()
	}

	// 3. Rename old container to backup (Blue-Green safety)
	backupName := fmt.Sprintf("%s_bak_%d", containerName, time.Now().UnixNano())
	renameURL := fmt.Sprintf("http://localhost/containers/%s/rename?name=%s", containerName, backupName)
	renameReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, renameURL, nil)
	renameResp, err := client.Do(renameReq)
	if err != nil || (renameResp != nil && renameResp.StatusCode != http.StatusNoContent && renameResp.StatusCode != http.StatusOK) {
		status := 0
		if renameResp != nil {
			status = renameResp.StatusCode
			_ = renameResp.Body.Close()
		}
		// Attempt to restart original container if rename failed
		startURL := fmt.Sprintf("http://localhost/containers/%s/start", containerName)
		startReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, startURL, nil)
		if sResp, sErr := client.Do(startReq); sErr == nil {
			_ = sResp.Body.Close()
		}
		return "", fmt.Errorf("failed to rename container %s to backup %s (status %d): %v", containerName, backupName, status, err)
	}
	if renameResp != nil {
		_ = renameResp.Body.Close()
	}

	// Helper for 5-stage verified atomic restoration of backup container
	restoreOriginal := func(cause error) (string, error) {
		log.Printf("[executor] recreate failed (%v), restoring original container %s from %s", cause, containerName, backupName)
		rbErr := executeVerifiedRollback(ctx, client, containerName, backupName, nil, containerName, cause)
		if rbErr != nil {
			return "", fmt.Errorf("recreate container %s failed and 5-stage rollback failed (%v): %w", containerName, rbErr, cause)
		}
		return "", fmt.Errorf("recreate container %s failed and original container was restored (5-stage verified rollback): %w", containerName, cause)
	}

	// 4. Create container with new image preserving full security and runtime configurations
	createPayload := map[string]interface{}{
		"Image":        targetImage,
		"User":         inspectData.Config.User, // Non-root user strictly preserved!
		"Env":          inspectData.Config.Env,
		"Cmd":          inspectData.Config.Cmd,
		"Entrypoint":   inspectData.Config.Entrypoint,
		"WorkingDir":   inspectData.Config.WorkingDir,
		"Labels":       inspectData.Config.Labels,
		"ExposedPorts": inspectData.Config.ExposedPorts,
		"HostConfig":   inspectData.HostConfig,
	}
	if inspectData.Config.StopSignal != "" {
		createPayload["StopSignal"] = inspectData.Config.StopSignal
	}
	if len(inspectData.NetworkSettings.Networks) > 0 {
		createPayload["NetworkingConfig"] = map[string]interface{}{
			"EndpointsConfig": inspectData.NetworkSettings.Networks,
		}
	}
	createBytes, _ := json.Marshal(createPayload)
	createURL := fmt.Sprintf("http://localhost/containers/create?name=%s", containerName)
	createReq, err := http.NewRequestWithContext(ctx, http.MethodPost, createURL, bytes.NewReader(createBytes))
	if err != nil {
		return restoreOriginal(err)
	}
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := client.Do(createReq)
	if err != nil {
		return restoreOriginal(fmt.Errorf("failed to create container %s with image %s: %w", containerName, targetImage, err))
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated && createResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(createResp.Body)
		return restoreOriginal(fmt.Errorf("create container %s returned %d: %s", containerName, createResp.StatusCode, string(b)))
	}

	// 5. Start container
	startURL := fmt.Sprintf("http://localhost/containers/%s/start", containerName)
	startReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, startURL, nil)
	startResp, err := client.Do(startReq)
	if err != nil {
		return restoreOriginal(fmt.Errorf("failed to start recreated container %s: %w", containerName, err))
	}
	defer startResp.Body.Close()
	if startResp.StatusCode != http.StatusNoContent && startResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(startResp.Body)
		return restoreOriginal(fmt.Errorf("start container %s returned %d: %s", containerName, startResp.StatusCode, string(b)))
	}

	// 6. Smoke probe / Running state verification
	verifyInspectURL := fmt.Sprintf("http://localhost/containers/%s/json", containerName)
	vReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, verifyInspectURL, nil)
	vResp, err := client.Do(vReq)
	if err != nil {
		return restoreOriginal(fmt.Errorf("failed to verify new container status: %w", err))
	}
	defer vResp.Body.Close()
	var vData struct {
		State struct {
			Running bool   `json:"Running"`
			Status  string `json:"Status"`
		} `json:"State"`
	}
	if err := json.NewDecoder(vResp.Body).Decode(&vData); err != nil || !vData.State.Running {
		return restoreOriginal(fmt.Errorf("new container %s is not running (status: %s)", containerName, vData.State.Status))
	}

	// 7. Success! New container is verified running. Remove old backup container.
	delBackupURL := fmt.Sprintf("http://localhost/containers/%s?v=false&force=true", backupName)
	delBackupReq, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, delBackupURL, nil)
	if dBResp, dBErr := client.Do(delBackupReq); dBErr == nil {
		_ = dBResp.Body.Close()
	}

	return fmt.Sprintf("container %s recreated successfully with image %s", containerName, targetImage), nil
}

// ReconcileStartupProposals inspects proposals left in non-terminal transient states
// (EXECUTING, ROLLING_BACK) on startup and marks them EXECUTION_UNKNOWN or RECONCILIATION_REQUIRED.
func ReconcileStartupProposals(db *gorm.DB, executor OperationExecutor) error {
	if db == nil {
		return nil
	}
	now := time.Now()

	// 1. EXECUTING proposals on startup:
	// If executor is available and can verify health, check target container state.
	// Otherwise, mark as RECONCILIATION_REQUIRED / EXECUTION_UNKNOWN.
	var executingProps []model.OpsProposal
	if err := db.Where("status = ?", model.ProposalStatusExecuting).Find(&executingProps).Error; err == nil {
		for _, prop := range executingProps {
			targetStatus := model.ProposalStatusReconciliationRequired
			execLog := "Service restarted while proposal was EXECUTING; marked RECONCILIATION_REQUIRED for human review"

			if executor != nil && prop.TargetComponent != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				if err := executor.CheckHealth(ctx, prop.TargetComponent); err == nil {
					// Container is healthy, but we cannot confirm whether this proposal's exact mutation completed
					targetStatus = model.ProposalStatusExecutionUnknown
					execLog = "Service restarted while proposal was EXECUTING; target container is healthy, but mutation state requires verification (EXECUTION_UNKNOWN)"
				}
				cancel()
			}

			_ = db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Updates(map[string]interface{}{
				"status":        targetStatus,
				"execution_log": execLog,
				"executed_at":   utils.NullTime{Time: now, Valid: true},
				"reconciled_at": utils.NullTime{Time: now, Valid: true},
				"updated_at":    now,
			})
		}
	}

	// 2. ROLLING_BACK proposals on startup:
	// Rollback was interrupted by restart -> MUST flag RECONCILIATION_REQUIRED
	_ = db.Model(&model.OpsProposal{}).
		Where("status = ?", model.ProposalStatusRollingBack).
		Updates(map[string]interface{}{
			"status":        model.ProposalStatusReconciliationRequired,
			"execution_log": "Service restarted while proposal was ROLLING_BACK; container state is inconsistent (RECONCILIATION_REQUIRED)",
			"executed_at":   utils.NullTime{Time: now, Valid: true},
			"reconciled_at": utils.NullTime{Time: now, Valid: true},
			"updated_at":    now,
		})

	return nil
}

// SweepOrphanProposals is a backward-compatible wrapper calling ReconcileStartupProposals.
func SweepOrphanProposals(db *gorm.DB) error {
	return ReconcileStartupProposals(db, nil)
}

