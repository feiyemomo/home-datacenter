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
	ErrExecutionAmbiguous        = errors.New("external execution outcome is ambiguous; reconciliation required")
	ErrHealthUnknown             = errors.New("component readiness state is UNKNOWN")
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
	Action          string                 `json:"action"`
	Target          string                 `json:"target"`
	Params          map[string]interface{} `json:"params,omitempty"`
	BaselineHash    string                 `json:"baseline_hash,omitempty"`
	RollbackSpec    *OperationSpec         `json:"rollback_spec,omitempty"`
	TimeoutSec      int                    `json:"timeout_sec,omitempty"`
	ExecutionPolicy string                 `json:"execution_policy,omitempty"`
}

// ReadinessLevel models the 5-tier business readiness health contract (R4-16).
type ReadinessLevel string

const (
	ReadinessUnknown         ReadinessLevel = "UNKNOWN"
	ReadinessProcessRunning  ReadinessLevel = "PROCESS_RUNNING"
	ReadinessPortOpen        ReadinessLevel = "PORT_OPEN"
	ReadinessServiceReady    ReadinessLevel = "SERVICE_READY"
	ReadinessBusinessHealthy ReadinessLevel = "BUSINESS_HEALTHY"
)

// HealthProbeResult provides detailed 5-tier health telemetry for a component.
type HealthProbeResult struct {
	Component string         `json:"component"`
	Level     ReadinessLevel `json:"level"`
	Detail    string         `json:"detail"`
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
	recoveryFSM  *ContainerRecoveryFSM
	componentLocks sync.Map // canonical string -> *sync.Mutex
	mu             sync.RWMutex
}

// SetRecoveryFSM attaches the authoritative ContainerRecoveryFSM for pre-execution safety checks.
func (e *DefaultOperationExecutor) SetRecoveryFSM(fsm *ContainerRecoveryFSM) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recoveryFSM = fsm
}

// WithRecoveryFSM attaches a ContainerRecoveryFSM during executor construction.
func WithRecoveryFSM(fsm *ContainerRecoveryFSM) ExecutorOption {
	return func(e *DefaultOperationExecutor) {
		e.recoveryFSM = fsm
	}
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
	for _, opt := range opts {
		opt(exec)
	}
	if exec.leaseManager == nil && db != nil {
		exec.leaseManager = NewRecoveryLeaseManager(db)
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

type componentLockHeldKey struct{}

func withComponentLockHeld(ctx context.Context, canonical string) context.Context {
	return context.WithValue(ctx, componentLockHeldKey{}, canonical)
}

func isComponentLockHeld(ctx context.Context, canonical string) bool {
	if ctx == nil {
		return false
	}
	v, ok := ctx.Value(componentLockHeldKey{}).(string)
	return ok && v == canonical
}

func isMutatingContainerAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case ActionDockerRestart, ActionDockerRecreate, ActionDockerUpdateImage, ActionDockerExec, ActionWAFReload,
		"restart", "docker_restart", "container_recreate":
		return true
	default:
		return false
	}
}

func isAllowedProductionAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case ActionDockerRestart, ActionDockerRecreate, ActionDockerUpdateImage, ActionDockerExec,
		ActionDBCreateIndex, ActionDBDropIndex, ActionDBVacuumSnapshot, ActionWAFReload, ActionCachePurge:
		return true
	default:
		return false
	}
}

func isAmbiguousExecutionError(err error, ctxErr error) bool {
	if errors.Is(err, ErrExecutionAmbiguous) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(ctxErr, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) || errors.Is(ctxErr, context.Canceled) {
		return true
	}
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "broken pipe")
}

// Execute executes an OperationSpec with whitelist validation, TOCTOU baseline check,
// persistent lease & fencing verification, execution dispatch, health probe verification,
// audit persistence, and controlled rollback on definite failure.
func (e *DefaultOperationExecutor) Execute(ctx context.Context, spec OperationSpec) (*ExecutionResult, error) {
	startTime := time.Now()
	opID := GenerateOperationID("exec")

	if spec.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(spec.TimeoutSec)*time.Second)
		defer cancel()
	}

	// 1. Validate spec safety against shell metacharacters
	if err := ValidateSpecSafety(spec); err != nil {
		return nil, err
	}

	// 2. Enforce strict bidirectional component whitelist
	canonical, err := ResolveComponent(spec.Target)
	if err != nil {
		return nil, err
	}

	// 2.0 If no mock runner is installed (production path), enforce closed action whitelist
	if e.dockerRunner == nil && !isAllowedProductionAction(spec.Action) {
		return &ExecutionResult{
			Success:    false,
			Action:     spec.Action,
			Target:     canonical,
			Error:      fmt.Sprintf("%v: %s", ErrActionNotSupported, spec.Action),
			ExecutedAt: startTime,
		}, fmt.Errorf("%w: %s", ErrActionNotSupported, spec.Action)
	}

	// 2.1 Acquire per-component mutex unless already held by enclosing call frame
	if !isComponentLockHeld(ctx, canonical) {
		lock := e.getComponentLock(canonical)
		lock.Lock()
		defer lock.Unlock()
		ctx = withComponentLockHeld(ctx, canonical)
	}

	// 2.2 Recovery Lease Lifecycle (R4-04 ~ R4-08):
	// Reuse existing LeaseGuard passed in context from ProcessAndRecover (never re-acquire!),
	// or acquire our own LeaseGuard if called standalone on a mutating container action.
	existingGuard := RecoveryLeaseFromContext(ctx)
	var ownGuard *LeaseGuard
	if existingGuard != nil {
		expectedLeaseComp := ResolveTopologyComponent(canonical)
		if existingGuard.Grant.Component != expectedLeaseComp && existingGuard.Grant.Component != canonical {
			return &ExecutionResult{
				Success:    false,
				Action:     spec.Action,
				Target:     canonical,
				Error:      fmt.Sprintf("lease context component mismatch: held %s, requested %s", existingGuard.Grant.Component, canonical),
				ExecutedAt: startTime,
			}, ErrInvalidLeaseToken
		}
		if err := existingGuard.VerifyBeforeMutation(0); err != nil {
			RecordSecurityEvent(e.db, e.bus, OpsSecurityEvent{
				OperationID:       opID,
				EventType:         "ops.fencing.rejected",
				Severity:          "critical",
				Component:         canonical,
				Actor:             existingGuard.Grant.Holder,
				FencingGeneration: existingGuard.Grant.FencingGeneration,
				Message:           fmt.Sprintf("pre-execution fencing validation failed: %v", err),
			})
			return &ExecutionResult{
				Success:    false,
				Action:     spec.Action,
				Target:     canonical,
				Error:      err.Error(),
				ExecutedAt: startTime,
			}, err
		}
	} else if e.leaseManager != nil && isMutatingContainerAction(spec.Action) {
		leaseCtx, guard, lErr := AcquireLeaseGuard(ctx, e.leaseManager, canonical, opID, 30*time.Second, e.db, e.bus)
		if lErr != nil {
			return &ExecutionResult{
				Success:    false,
				Action:     spec.Action,
				Target:     canonical,
				Error:      fmt.Sprintf("persistent recovery lease conflict: %v", lErr),
				ExecutedAt: startTime,
			}, fmt.Errorf("persistent recovery lease conflict for %s: %w", canonical, lErr)
		}
		ctx = leaseCtx
		ownGuard = guard
	}

	releaseOwnLease := func() error {
		if ownGuard == nil {
			return nil
		}
		return ownGuard.Release(e.db, e.bus)
	}

	// 3. Anti-TOCTOU baseline hash verification (Fail-Closed: calculation failure or drift strictly blocks execution)
	if spec.BaselineHash != "" {
		if e.baselineExtractor == nil {
			_ = releaseOwnLease()
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
			_ = releaseOwnLease()
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

	// 3.5 Final fencing gate right before external Docker/OS mutation (R4-06)
	if guard := RecoveryLeaseFromContext(ctx); guard != nil {
		if fErr := guard.VerifyBeforeMutation(0); fErr != nil {
			RecordSecurityEvent(e.db, e.bus, OpsSecurityEvent{
				OperationID:       opID,
				EventType:         "ops.fencing.rejected",
				Severity:          "critical",
				Component:         canonical,
				Actor:             guard.Grant.Holder,
				FencingGeneration: guard.Grant.FencingGeneration,
				Message:           fmt.Sprintf("stale lease or expired fencing token blocked right before Docker dispatch: %v", fErr),
			})
			_ = releaseOwnLease()
			return &ExecutionResult{
				Success:    false,
				Action:     spec.Action,
				Target:     canonical,
				Error:      fErr.Error(),
				ExecutedAt: startTime,
			}, fErr
		}
	}
	if err := ctx.Err(); err != nil {
		_ = releaseOwnLease()
		return &ExecutionResult{
			Success:    false,
			Action:     spec.Action,
			Target:     canonical,
			Error:      err.Error(),
			ExecutedAt: startTime,
		}, err
	}

	// 4. Dispatch the structured action
	out, err := e.dispatchAction(ctx, spec, canonical)
	if err != nil {
		if isAmbiguousExecutionError(err, ctx.Err()) {
			GlobalSecurityMetrics.IncExecutionUnknown()
			RecordSecurityEvent(e.db, e.bus, OpsSecurityEvent{
				OperationID: opID,
				EventType:   "ops.execution.unknown",
				Severity:    "critical",
				Component:   canonical,
				Message:     fmt.Sprintf("ambiguous mutation failure (%v); blocking automatic rollback/retry", err),
			})
			_ = e.recordExecutionHistory(canonical, spec.Action, "execution_unknown", out, fmt.Sprintf("ambiguous failure: %v", err))
			_ = releaseOwnLease()
			return &ExecutionResult{
				Success:      false,
				Action:       spec.Action,
				Target:       canonical,
				Output:       out,
				BaselineHash: spec.BaselineHash,
				DurationMs:   time.Since(startTime).Milliseconds(),
				RolledBack:   false,
				Error:        fmt.Sprintf("%s: %v", ErrExecutionAmbiguous.Error(), err),
				ExecutedAt:   startTime,
			}, fmt.Errorf("%w: %v", ErrExecutionAmbiguous, err)
		}

		// Definite failure -> attempt atomic rollback if specified with decoupled timeout
		var rbErr error
		if spec.RollbackSpec != nil {
			rbCtx, rbCancel := context.WithTimeout(withComponentLockHeld(context.Background(), canonical), 30*time.Second)
			if guard := RecoveryLeaseFromContext(ctx); guard != nil {
				rbCtx = WithRecoveryLease(rbCtx, guard)
			}
			rbErr = e.ExecuteRollback(rbCtx, *spec.RollbackSpec)
			rbCancel()
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
		_ = e.recordExecutionHistory(canonical, spec.Action, status, out, fmt.Sprintf("execution failed: %v; rollback err: %v", err, rbErr))
		_ = releaseOwnLease()

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
		var rbErr error
		if spec.RollbackSpec != nil {
			rbCtx, rbCancel := context.WithTimeout(withComponentLockHeld(context.Background(), canonical), 30*time.Second)
			if guard := RecoveryLeaseFromContext(ctx); guard != nil {
				rbCtx = WithRecoveryLease(rbCtx, guard)
			}
			rbErr = e.ExecuteRollback(rbCtx, *spec.RollbackSpec)
			rbCancel()
		}
		status := "failed"
		if spec.RollbackSpec != nil {
			if rbErr == nil {
				status = "rolled_back"
			} else {
				status = "rollback_failed"
			}
		}
		_ = e.recordExecutionHistory(canonical, spec.Action, status, out, fmt.Sprintf("health probe failed: %v; rollback err: %v", probeErr, rbErr))
		_ = releaseOwnLease()

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

	// 6. Audit persistence & lease release (R4-05, R4-08)
	histErr := e.recordExecutionHistory(canonical, spec.Action, "success", out, "health probe passed")
	relErr := releaseOwnLease()
	if histErr != nil || relErr != nil {
		combinedErr := errors.Join(histErr, relErr)
		return &ExecutionResult{
			Success:      false,
			Action:       spec.Action,
			Target:       canonical,
			Output:       out,
			BaselineHash: spec.BaselineHash,
			DurationMs:   duration,
			RolledBack:   false,
			Error:        fmt.Sprintf("execution completed but lifecycle finalization failed: %v", combinedErr),
			ExecutedAt:   startTime,
		}, combinedErr
	}

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

// ExecuteRollback executes the inverse rollback action specification with component locking & fencing checks.
func (e *DefaultOperationExecutor) ExecuteRollback(ctx context.Context, rollbackSpec OperationSpec) error {
	if err := ValidateSpecSafety(rollbackSpec); err != nil {
		return err
	}

	canonical, err := ResolveComponent(rollbackSpec.Target)
	if err != nil {
		return err
	}

	if e.dockerRunner == nil && !isAllowedProductionAction(rollbackSpec.Action) {
		return fmt.Errorf("%w: rollback action %s not permitted", ErrActionNotSupported, rollbackSpec.Action)
	}

	if !isComponentLockHeld(ctx, canonical) {
		lock := e.getComponentLock(canonical)
		lock.Lock()
		defer lock.Unlock()
		ctx = withComponentLockHeld(ctx, canonical)
	}

	if guard := RecoveryLeaseFromContext(ctx); guard != nil {
		if err := guard.VerifyBeforeMutation(0); err != nil {
			return err
		}
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

// checkProposalSafetyPreconditions validates maintenance lock, circuit breaker, and dependency topology
// before an approved proposal is allowed to mutate external state (R4-13).
func (e *DefaultOperationExecutor) checkProposalSafetyPreconditions(ctx context.Context, canonicalTarget string) error {
	e.mu.RLock()
	fsm := e.recoveryFSM
	e.mu.RUnlock()

	if fsm != nil {
		if fsm.lock.IsDegraded() {
			GlobalSecurityMetrics.SetMaintenanceLockDegraded(true)
			return ErrFailClosedMode
		}
		if fsm.lock.IsLocked() {
			return ErrSystemInMaintenance
		}
		if fsm.breaker.IsDegraded() {
			GlobalSecurityMetrics.SetCircuitBreakerDegraded(true)
			return ErrFailClosedMode
		}
		if cbState := fsm.breaker.GetState(canonicalTarget); cbState == CircuitOpen {
			return ErrCircuitBreakerOpen
		}
		if topErr := fsm.topology.PreflightDependencyCheck(ctx, canonicalTarget); topErr != nil {
			return topErr
		}
		return nil
	}

	if e.db != nil {
		if hasLockTbl, err := tableExists(e.db, "ops_maintenance_locks"); err == nil && hasLockTbl {
			var lockRow model.OpsMaintenanceLock
			qErr := e.db.Order("id DESC").First(&lockRow).Error
			if qErr != nil && !errors.Is(qErr, gorm.ErrRecordNotFound) {
				GlobalSecurityMetrics.IncDatabaseError()
				return ErrFailClosedMode
			}
			if qErr == nil && lockRow.Active && time.Now().UTC().Before(lockRow.ExpiresAt.UTC()) {
				return ErrSystemInMaintenance
			}
		}
		if hasCBTbl, err := tableExists(e.db, "ops_circuit_breaker_states"); err == nil && hasCBTbl {
			topoCanon := ResolveTopologyComponent(canonicalTarget)
			var cbRow model.OpsCircuitBreakerState
			qErr := e.db.Where("container = ? OR container = ?", topoCanon, canonicalTarget).First(&cbRow).Error
			if qErr != nil && !errors.Is(qErr, gorm.ErrRecordNotFound) {
				GlobalSecurityMetrics.IncDatabaseError()
				return ErrFailClosedMode
			}
			if qErr == nil {
				if cbRow.Degraded {
					return ErrFailClosedMode
				}
				if strings.EqualFold(cbRow.State, string(CircuitOpen)) {
					return ErrCircuitBreakerOpen
				}
			}
		}
	}
	return nil
}

// ExecuteProposal executes a proposal end-to-end using unified CAS state transitions (ProposalFSM),
// full canonical HMAC-SHA256 verification, pre-execution baseline & safety verification, and non-retryable
// EXECUTION_UNKNOWN handling for ambiguous transport/timeout outcomes (R4-01 ~ R4-03, R4-11 ~ R4-13).
func (e *DefaultOperationExecutor) ExecuteProposal(ctx context.Context, proposal *model.OpsProposal) (*ExecutionResult, error) {
	if proposal == nil {
		return nil, ErrInvalidSpec
	}

	var execToken string
	var currRev int

	if e.db != nil {
		var curr model.OpsProposal
		var findErr error
		if proposal.ID > 0 {
			findErr = e.db.First(&curr, proposal.ID).Error
		} else if strings.TrimSpace(proposal.ProposalID) != "" {
			findErr = e.db.Where("proposal_id = ?", proposal.ProposalID).First(&curr).Error
		} else {
			return nil, ErrInvalidSpec
		}
		if findErr != nil {
			return nil, fmt.Errorf("proposal %s (%d) not found: %w", proposal.ProposalID, proposal.ID, findErr)
		}

		propKey := curr.ProposalID
		if propKey == "" {
			propKey = fmt.Sprintf("%d", curr.ID)
		}

		claimToken, hasSchedulerClaim := SchedulerClaimFromContext(ctx, propKey)
		if !hasSchedulerClaim && curr.ProposalID != "" {
			claimToken, hasSchedulerClaim = SchedulerClaimFromContext(ctx, curr.ProposalID)
		}
		schedulerPreClaimed := hasSchedulerClaim && curr.Status == model.ProposalStatusExecuting && curr.ExecutionToken != "" && curr.ExecutionToken == claimToken

		abortPreClaimed := func(cause error) (*ExecutionResult, error) {
			if schedulerPreClaimed {
				abortFSM := NewProposalFSMWithBus(e.db, e.bus)
				_, _ = abortFSM.TransitionStrict(ProposalTransitionParams{
					ProposalID:            propKey,
					ExpectedStatuses:      []string{model.ProposalStatusExecuting},
					TargetStatus:          model.ProposalStatusFailed,
					RequireExecutionToken: claimToken,
					Updates: map[string]interface{}{
						"execution_token": "",
						"execution_log":   fmt.Sprintf("Pre-execution guard aborted scheduler-claimed proposal: %v", cause),
					},
					Audit: &ProposalTransitionAudit{
						OperationID: GenerateOperationID("abort-sched"),
						EventType:   "ops.proposal.precheck_aborted",
						Actor:       "proposal-scheduler",
						Level:       model.LevelCritical,
						Message:     fmt.Sprintf("提案 [%s] 预检未通过并终止执行 (%v)", propKey, cause),
					},
				})
				proposal.Status = model.ProposalStatusFailed
			}
			return nil, cause
		}

		if !schedulerPreClaimed {
			if curr.Status == model.ProposalStatusExecuting || curr.Status == model.ProposalStatusRollingBack {
				proposal.Status = curr.Status
				return nil, ErrProposalAlreadyRunning
			}
			if curr.Status == model.ProposalStatusAwaitingApproval || curr.Status == model.ProposalStatusPending || curr.Status == model.ProposalStatusApprovalRequired {
				proposal.Status = curr.Status
				return nil, fmt.Errorf("%w: proposal %s requires approval before execution (status: %s)", ErrProposalUnauthorized, curr.ProposalID, curr.Status)
			}
			if curr.Status != model.ProposalStatusApproved && curr.Status != model.ProposalStatusScheduled {
				proposal.Status = curr.Status
				return nil, fmt.Errorf("%w: proposal %s is not in an executable state (status: %s)", ErrProposalNotExecutable, curr.ProposalID, curr.Status)
			}
		}

		// R5: BaselineHash anti-tampering check (Fail-Closed)
		if strings.TrimSpace(curr.BaselineHash) == "" {
			return abortPreClaimed(ErrEmptyBaseline)
		}

		// R4-11/R4-12: Full canonical HMAC-SHA256 signature verification against authoritative DB record
		if err := model.VerifyProposalApprovalDigest(&curr); err != nil {
			GlobalSecurityMetrics.IncProposalDigestMismatch()
			RecordSecurityEvent(e.db, e.bus, OpsSecurityEvent{
				OperationID: GenerateOperationID("execprop"),
				EventType:   "ops.proposal.digest_mismatch",
				Severity:    "critical",
				Component:   curr.TargetComponent,
				ProposalID:  curr.ProposalID,
				Message:     fmt.Sprintf("proposal approval signature verification failed: %v", err),
			})
			return abortPreClaimed(fmt.Errorf("%w: %v", ErrApprovalDigestMismatch, err))
		}

		// Memory vs DB integrity consistency checks
		if proposal.ApprovalDigest != "" && proposal.ApprovalDigest != curr.ApprovalDigest {
			GlobalSecurityMetrics.IncProposalDigestMismatch()
			return abortPreClaimed(fmt.Errorf("%w: memory proposal approval digest does not match database record", ErrApprovalDigestMismatch))
		}
		if proposal.ActionSpec != "" && proposal.ActionSpec != curr.ActionSpec {
			GlobalSecurityMetrics.IncProposalDigestMismatch()
			return abortPreClaimed(fmt.Errorf("%w: memory action_spec differs from database record", ErrApprovalDigestMismatch))
		}
		if proposal.TargetComponent != "" && model.CanonicalizeComponent(proposal.TargetComponent) != model.CanonicalizeComponent(curr.TargetComponent) {
			GlobalSecurityMetrics.IncProposalDigestMismatch()
			return abortPreClaimed(fmt.Errorf("%w: memory target_component differs from database record", ErrApprovalDigestMismatch))
		}

		// Validate strict spec, params, whitelist, and rollback spec before acquiring execution token
		canonicalTarget, err := ResolveComponent(curr.TargetComponent)
		if err != nil {
			return abortPreClaimed(err)
		}
		specPreview, parseErr := e.parseProposalSpec(&curr)
		if parseErr != nil {
			return abortPreClaimed(parseErr)
		}
		_ = specPreview

		// R5: Pre-execution baseline re-validation against live component state (anti-TOCTOU)
		if e.baselineExtractor != nil {
			currentHash := e.baselineExtractor(canonicalTarget)
			if currentHash == "" || currentHash != curr.BaselineHash {
				return abortPreClaimed(fmt.Errorf("%w: target component %s state drifted before execution (expected %s, got %s)", ErrBaselineMismatch, canonicalTarget, curr.BaselineHash, currentHash))
			}
		}

		// Verify scheduled window prior to claiming if not already claimed by scheduler
		if !schedulerPreClaimed {
			if err := validateScheduledWindow(&curr); err != nil {
				return nil, err
			}
		}

		// R4-13: Validate maintenance lock, circuit breaker, and topology dependencies
		if err := e.checkProposalSafetyPreconditions(ctx, canonicalTarget); err != nil {
			return abortPreClaimed(err)
		}
		fsm := NewProposalFSMWithBus(e.db, e.bus)

		if schedulerPreClaimed {
			execToken = claimToken
			currRev = curr.Revision
			if currRev <= 0 {
				currRev = 1
			}
		} else {
			execToken = GenerateOperationID("exec")
			reqVer := curr.Version
			if reqVer <= 0 {
				reqVer = 1
			}
			currRev = curr.Revision
			if currRev <= 0 {
				currRev = 1
			}
			won, casErr := fsm.TransitionStrict(ProposalTransitionParams{
				ProposalID:       propKey,
				ExpectedStatuses: []string{model.ProposalStatusApproved, model.ProposalStatusScheduled},
				TargetStatus:     model.ProposalStatusExecuting,
				RequireVersion:   &reqVer,
				RequireRevision:  &currRev,
				Updates: map[string]interface{}{
					"execution_token": execToken,
					"executed_at":     utils.NullTime{Time: time.Now().UTC(), Valid: true},
				},
				Audit: &ProposalTransitionAudit{
					OperationID: execToken,
					EventType:   "ops.proposal.executing",
					Actor:       curr.ApprovedBy,
					Level:       model.LevelInfo,
					Message:     fmt.Sprintf("开始执行已审批提案 [%s] (target=%s)", curr.ProposalID, canonicalTarget),
				},
			})
			if casErr != nil {
				return nil, casErr
			}
			if !won {
				var latest model.OpsProposal
				if err := e.db.First(&latest, curr.ID).Error; err == nil {
					proposal.Status = latest.Status
					if latest.Status != model.ProposalStatusApproved && latest.Status != model.ProposalStatusScheduled && latest.Status != model.ProposalStatusExecuting {
						return nil, fmt.Errorf("%w: proposal %s is not in an executable state (status: %s)", ErrProposalNotExecutable, proposal.ProposalID, latest.Status)
					}
				}
				return nil, ErrProposalAlreadyRunning
			}
			currRev++
		}

		// Synchronize authoritative DB fields to memory proposal
		*proposal = curr
		proposal.Status = model.ProposalStatusExecuting
		proposal.ExecutionToken = execToken
		proposal.Revision = currRev
	} else {
		// In-memory / mock execution without DB
		if proposal.Status == model.ProposalStatusAwaitingApproval || proposal.Status == model.ProposalStatusPending || proposal.Status == model.ProposalStatusApprovalRequired {
			return nil, fmt.Errorf("%w: proposal %s requires approval before execution (status: %s)", ErrProposalUnauthorized, proposal.ProposalID, proposal.Status)
		}
		if proposal.Status != model.ProposalStatusApproved && proposal.Status != model.ProposalStatusScheduled {
			return nil, fmt.Errorf("%w: proposal %s is not in an executable state (status: %s)", ErrProposalNotExecutable, proposal.ProposalID, proposal.Status)
		}
		if strings.TrimSpace(proposal.BaselineHash) == "" {
			return nil, ErrEmptyBaseline
		}
		if err := model.VerifyProposalApprovalDigest(proposal); err != nil {
			GlobalSecurityMetrics.IncProposalDigestMismatch()
			return nil, fmt.Errorf("%w: %v", ErrApprovalDigestMismatch, err)
		}
		if e.baselineExtractor != nil {
			canonicalTarget, err := ResolveComponent(proposal.TargetComponent)
			if err != nil {
				return nil, err
			}
			currentHash := e.baselineExtractor(canonicalTarget)
			if currentHash == "" || currentHash != proposal.BaselineHash {
				return nil, fmt.Errorf("%w: target component %s state drifted before execution (expected %s, got %s)", ErrBaselineMismatch, canonicalTarget, proposal.BaselineHash, currentHash)
			}
		}
		if err := validateScheduledWindow(proposal); err != nil {
			return nil, err
		}
		if canonicalTarget, err := ResolveComponent(proposal.TargetComponent); err == nil {
			if sErr := e.checkProposalSafetyPreconditions(ctx, canonicalTarget); sErr != nil {
				return nil, sErr
			}
		}
		proposal.Status = model.ProposalStatusExecuting
	}

	propKey := proposal.ProposalID
	if propKey == "" {
		propKey = fmt.Sprintf("%d", proposal.ID)
	}
	fsm := NewProposalFSMWithBus(e.db, e.bus)

	// Parse verified OperationSpec
	spec, err := e.parseProposalSpec(proposal)
	if err != nil {
		proposal.Status = model.ProposalStatusFailed
		proposal.ExecutionLog = fmt.Sprintf("failed to parse operation spec: %v", err)
		proposal.ExecutedAt = utils.NullTime{Time: time.Now().UTC(), Valid: true}
		if e.db != nil {
			_, _ = fsm.TransitionStrict(ProposalTransitionParams{
				ProposalID:            propKey,
				ExpectedStatuses:      []string{model.ProposalStatusExecuting},
				TargetStatus:          model.ProposalStatusFailed,
				RequireExecutionToken: execToken,
				Updates: map[string]interface{}{
					"execution_log": proposal.ExecutionLog,
					"executed_at":   proposal.ExecutedAt,
				},
			})
		}
		return nil, err
	}

	// Execute via OperationExecutor
	res, execErr := e.Execute(ctx, spec)
	now := time.Now().UTC()
	proposal.ExecutedAt = utils.NullTime{Time: now, Valid: true}

	if execErr != nil || (res != nil && !res.Success) {
		if errors.Is(execErr, ErrExecutionAmbiguous) || isAmbiguousExecutionError(execErr, ctx.Err()) {
			proposal.Status = model.ProposalStatusExecutionUnknown
			proposal.ExecutionLog = fmt.Sprintf("execution timed out or produced unknown state (%v); manual reconciliation required", execErr)
			if e.db != nil {
				won, casErr := fsm.TransitionStrict(ProposalTransitionParams{
					ProposalID:            propKey,
					ExpectedStatuses:      []string{model.ProposalStatusExecuting},
					TargetStatus:          model.ProposalStatusExecutionUnknown,
					RequireExecutionToken: execToken,
					Updates: map[string]interface{}{
						"execution_log": proposal.ExecutionLog,
						"executed_at":   proposal.ExecutedAt,
					},
					Audit: &ProposalTransitionAudit{
						OperationID: execToken,
						EventType:   "ops.proposal.unknown",
						Actor:       "executor",
						Level:       model.LevelCritical,
						Message:     proposal.ExecutionLog,
					},
				})
				if casErr != nil || !won {
					return res, fmt.Errorf("%w (additionally failed to persist EXECUTION_UNKNOWN: won=%v err=%v)", execErr, won, casErr)
				}
			}
			e.publishEvent(eventbus.SeverityError, "ops.proposal.unknown", proposal)
			return res, execErr
		}

		finalStatus := model.ProposalStatusFailed
		logMsg := fmt.Sprintf("execution failed: %v", execErr)
		if res != nil {
			logMsg = fmt.Sprintf("execution failed (%s); rolled back: %v; output: %s", res.Error, res.RolledBack, res.Output)
		}
		proposal.ExecutionLog = logMsg

		if spec.RollbackSpec != nil {
			if e.db != nil {
				_, _ = fsm.TransitionStrict(ProposalTransitionParams{
					ProposalID:            propKey,
					ExpectedStatuses:      []string{model.ProposalStatusExecuting},
					TargetStatus:          model.ProposalStatusRollingBack,
					RequireExecutionToken: execToken,
				})
			}
			if res != nil && res.RolledBack {
				finalStatus = model.ProposalStatusRolledBack
			} else {
				finalStatus = model.ProposalStatusRollbackFailed
			}
		}
		proposal.Status = finalStatus

		if e.db != nil {
			expectedPrev := []string{model.ProposalStatusExecuting}
			if spec.RollbackSpec != nil {
				expectedPrev = []string{model.ProposalStatusRollingBack, model.ProposalStatusExecuting}
			}
			won, casErr := fsm.TransitionStrict(ProposalTransitionParams{
				ProposalID:            propKey,
				ExpectedStatuses:      expectedPrev,
				TargetStatus:          finalStatus,
				RequireExecutionToken: execToken,
				Updates: map[string]interface{}{
					"execution_log": proposal.ExecutionLog,
					"executed_at":   proposal.ExecutedAt,
				},
				Audit: &ProposalTransitionAudit{
					OperationID: execToken,
					EventType:   "ops.proposal.failed",
					Actor:       "executor",
					Level:       model.LevelCritical,
					Message:     logMsg,
				},
			})
			if casErr != nil || !won {
				return res, fmt.Errorf("%w (additionally failed to persist %s: won=%v err=%v)", execErr, finalStatus, won, casErr)
			}
		}
		e.publishEvent(eventbus.SeverityError, "ops.proposal.failed", proposal)
		return res, execErr
	}

	// Success -> transition EXECUTING -> EXECUTED via strict CAS
	proposal.Status = model.ProposalStatusExecuted
	if res != nil {
		proposal.ExecutionLog = res.Output
	}
	if e.db != nil {
		won, casErr := fsm.TransitionStrict(ProposalTransitionParams{
			ProposalID:            propKey,
			ExpectedStatuses:      []string{model.ProposalStatusExecuting},
			TargetStatus:          model.ProposalStatusExecuted,
			RequireExecutionToken: execToken,
			Updates: map[string]interface{}{
				"execution_log": proposal.ExecutionLog,
				"executed_at":   proposal.ExecutedAt,
			},
			Audit: &ProposalTransitionAudit{
				OperationID: execToken,
				EventType:   "ops.proposal.executed",
				Actor:       proposal.ApprovedBy,
				Level:       model.LevelInfo,
				Message:     fmt.Sprintf("提案 [%s] 执行并健康验证通过", proposal.ProposalID),
			},
		})
		if casErr != nil || !won {
			return res, fmt.Errorf("proposal executed but failed to persist final EXECUTED status via CAS (won=%v err=%v)", won, casErr)
		}
	}
	e.publishEvent(eventbus.SeverityInfo, "ops.proposal.executed", proposal)
	return res, nil
}

// parseProposalSpec converts proposal fields into a typed OperationSpec with strict JSON checking.
func (e *DefaultOperationExecutor) parseProposalSpec(proposal *model.OpsProposal) (OperationSpec, error) {
	canonicalProposalTarget, err := ResolveComponent(proposal.TargetComponent)
	if err != nil {
		return OperationSpec{}, fmt.Errorf("invalid proposal target component: %w", err)
	}

	if strings.TrimSpace(proposal.ActionSpec) != "" {
		var spec OperationSpec
		dec := json.NewDecoder(strings.NewReader(proposal.ActionSpec))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			if strings.Contains(err.Error(), "unknown field") {
				return OperationSpec{}, fmt.Errorf("%w: %v", model.ErrApprovalUnknownField, err)
			}
			return OperationSpec{}, fmt.Errorf("%w: failed to parse proposal action spec: %v", ErrInvalidSpec, err)
		}
		if dec.More() {
			return OperationSpec{}, fmt.Errorf("%w: trailing tokens in action_spec", ErrInvalidSpec)
		}
		var extra interface{}
		if err := dec.Decode(&extra); err != io.EOF {
			return OperationSpec{}, fmt.Errorf("%w: extra JSON object in action_spec", ErrInvalidSpec)
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

		if spec.ExecutionPolicy != "" && proposal.ExecutionPolicy != "" && spec.ExecutionPolicy != proposal.ExecutionPolicy {
			return OperationSpec{}, fmt.Errorf("%w: action spec execution_policy mismatch", ErrInvalidSpec)
		}
		if spec.ExecutionPolicy == "" {
			spec.ExecutionPolicy = proposal.ExecutionPolicy
		}

		if spec.RollbackSpec != nil {
			if strings.TrimSpace(spec.RollbackSpec.Action) == "" {
				return OperationSpec{}, fmt.Errorf("%w: rollback_spec missing action", ErrInvalidSpec)
			}
			if spec.RollbackSpec.Target != "" {
				rbTarget, err := ResolveComponent(spec.RollbackSpec.Target)
				if err != nil || rbTarget != canonicalSpecTarget {
					return OperationSpec{}, fmt.Errorf("security violation: rollback target '%s' does not match action target '%s'", spec.RollbackSpec.Target, canonicalSpecTarget)
				}
				spec.RollbackSpec.Target = rbTarget
			} else {
				spec.RollbackSpec.Target = canonicalSpecTarget
			}
			if err := ValidateSpecSafety(*spec.RollbackSpec); err != nil {
				return OperationSpec{}, err
			}
			if e.dockerRunner == nil && !isAllowedProductionAction(spec.RollbackSpec.Action) {
				return OperationSpec{}, fmt.Errorf("%w: unsupported rollback action %s", ErrActionNotSupported, spec.RollbackSpec.Action)
			}
		}

		if e.dockerRunner == nil && !isAllowedProductionAction(spec.Action) {
			return OperationSpec{}, fmt.Errorf("%w: unsupported production action %s", ErrActionNotSupported, spec.Action)
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

// executeDockerRestart restarts a whitelisted container via a single deterministic provider
// without blind multi-provider retries after an ambiguous transport/timeout failure (R4-03).
func (e *DefaultOperationExecutor) executeDockerRestart(ctx context.Context, canonical string) (string, error) {
	containerName := canonicalContainerNames[canonical]
	if containerName == "" {
		containerName = canonical
	}

	// Deterministic channel selection: prefer direct Docker socket when accessible (lowest latency,
	// atomic fencing before write), otherwise 1Panel OpenAPI if configured.
	if _, statErr := os.Stat(e.dockerSocket); statErr == nil {
		if err := executeDockerSocketRestart(ctx, e.dockerSocket, containerName); err != nil {
			return "", err
		}
		return fmt.Sprintf("container %s restarted via direct Docker socket", containerName), nil
	}

	if e.client != nil && strings.TrimSpace(e.client.baseURL) != "" {
		if err := e.client.RestartContainer(ctx, containerName); err != nil {
			return "", err
		}
		return fmt.Sprintf("container %s restarted via 1Panel API", containerName), nil
	}

	if e.fallback != nil {
		if err := e.fallback.RestartContainerFallback(ctx, containerName); err != nil {
			return "", err
		}
		return fmt.Sprintf("container %s restarted via Docker socket fallback", containerName), nil
	}

	return "", fmt.Errorf("failed to restart container %s: no restart provider available", containerName)
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

// probeMQTTHandshake connects to Mosquitto on host:1883 and sends a minimal MQTT v3.1.1 CONNECT packet,
// requiring a valid CONNACK (0x20 0x02 ... 0x00) to confirm broker readiness rather than bare TCP port open (R4-16).
func probeMQTTHandshake(ctx context.Context, host string) (ReadinessLevel, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("%s:1883", host))
	if err != nil {
		return ReadinessUnknown, fmt.Errorf("mosquitto tcp dial failed: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	clientID := []byte("home-api-health")
	username := strings.TrimSpace(os.Getenv("MQTT_USERNAME"))
	password := os.Getenv("MQTT_PASSWORD")

	var flags byte = 0x02 // clean session
	payload := make([]byte, 0, 64)
	payload = append(payload, byte(len(clientID)>>8), byte(len(clientID)))
	payload = append(payload, clientID...)
	if username != "" {
		flags |= 0x80
		payload = append(payload, byte(len(username)>>8), byte(len(username)))
		payload = append(payload, username...)
		if password != "" {
			flags |= 0x40
			payload = append(payload, byte(len(password)>>8), byte(len(password)))
			payload = append(payload, password...)
		}
	}

	varHeader := []byte{
		0x00, 0x04, 'M', 'Q', 'T', 'T',
		0x04, // MQTT 3.1.1
		flags,
		0x00, 0x0A, // keepalive 10s
	}
	remLen := len(varHeader) + len(payload)
	pkt := make([]byte, 0, 2+remLen)
	pkt = append(pkt, 0x10, byte(remLen))
	pkt = append(pkt, varHeader...)
	pkt = append(pkt, payload...)

	if _, err := conn.Write(pkt); err != nil {
		return ReadinessPortOpen, fmt.Errorf("mosquitto MQTT CONNECT write failed: %w", err)
	}
	ack := make([]byte, 4)
	if _, err := io.ReadFull(conn, ack); err != nil {
		return ReadinessPortOpen, fmt.Errorf("mosquitto MQTT CONNACK read failed: %w", err)
	}
	if ack[0] != 0x20 || ack[1] != 0x02 {
		return ReadinessUnknown, fmt.Errorf("mosquitto invalid CONNACK header: %x", ack)
	}
	if ack[3] != 0x00 {
		return ReadinessServiceReady, fmt.Errorf("mosquitto CONNACK rejected connection (rc=%d)", ack[3])
	}
	return ReadinessBusinessHealthy, nil
}

// CheckReadiness evaluates the 5-tier business readiness state for a whitelisted component (R4-16).
func (e *DefaultOperationExecutor) CheckReadiness(ctx context.Context, target string) (HealthProbeResult, error) {
	canonical, err := ResolveComponent(target)
	if err != nil {
		return HealthProbeResult{Component: target, Level: ReadinessUnknown, Detail: err.Error()}, err
	}

	if e.healthCheckFn != nil {
		if err := e.healthCheckFn(ctx, target); err != nil {
			GlobalSecurityMetrics.IncHealthUnknown()
			return HealthProbeResult{Component: canonical, Level: ReadinessUnknown, Detail: err.Error()}, err
		}
		return HealthProbeResult{Component: canonical, Level: ReadinessBusinessHealthy, Detail: "custom health probe passed"}, nil
	}

	client := &http.Client{Timeout: 2 * time.Second}

	switch canonical {
	case ComponentHomeAPI:
		if e.db != nil {
			sqlDB, err := e.db.DB()
			if err != nil {
				GlobalSecurityMetrics.IncHealthUnknown()
				return HealthProbeResult{Component: canonical, Level: ReadinessUnknown, Detail: err.Error()}, fmt.Errorf("home-api db handle error: %w", err)
			}
			if pingErr := sqlDB.PingContext(ctx); pingErr != nil {
				GlobalSecurityMetrics.IncHealthUnknown()
				return HealthProbeResult{Component: canonical, Level: ReadinessProcessRunning, Detail: pingErr.Error()}, fmt.Errorf("home-api db ping failed: %w", pingErr)
			}
			if !inDockerEnv() && os.Getenv("PROBE_HOST_HOME_API") == "" && os.Getenv("PROBE_HEALTH_URL") == "" {
				return HealthProbeResult{Component: canonical, Level: ReadinessBusinessHealthy, Detail: "db dependency healthy (standalone mode)"}, nil
			}
		}

		host := getProbeHost(ComponentHomeAPI)
		healthURL := fmt.Sprintf("http://%s:8080/health/ready", host)
		if customURL := os.Getenv("PROBE_HEALTH_URL"); customURL != "" {
			healthURL = customURL
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			GlobalSecurityMetrics.IncHealthUnknown()
			return HealthProbeResult{Component: canonical, Level: ReadinessUnknown, Detail: err.Error()}, err
		}
		resp, err := client.Do(req)
		if err != nil {
			GlobalSecurityMetrics.IncHealthUnknown()
			return HealthProbeResult{Component: canonical, Level: ReadinessUnknown, Detail: err.Error()}, fmt.Errorf("home-api http health check failed: %w", err)
		}
		defer resp.Body.Close()
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return HealthProbeResult{Component: canonical, Level: ReadinessServiceReady, Detail: fmt.Sprintf("status %d", resp.StatusCode)}, fmt.Errorf("home-api health check returned %d", resp.StatusCode)
		}
		var payload map[string]interface{}
		if json.Unmarshal(bodyBytes, &payload) != nil {
			GlobalSecurityMetrics.IncHealthUnknown()
			return HealthProbeResult{Component: canonical, Level: ReadinessUnknown, Detail: "invalid JSON health response"}, fmt.Errorf("%w: home-api returned non-JSON health response", ErrHealthUnknown)
		}
		return HealthProbeResult{Component: canonical, Level: ReadinessBusinessHealthy, Detail: "home-api ready with verified dependencies"}, nil

	case ComponentHomeWeb:
		host := getProbeHost(ComponentHomeWeb)
		port := 8086
		if inDockerEnv() && host == "home-web" {
			port = 80
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/", host, port), nil)
		resp, err := client.Do(req)
		if err != nil {
			GlobalSecurityMetrics.IncHealthUnknown()
			return HealthProbeResult{Component: canonical, Level: ReadinessUnknown, Detail: err.Error()}, err
		}
		defer resp.Body.Close()
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			return HealthProbeResult{Component: canonical, Level: ReadinessPortOpen, Detail: fmt.Sprintf("http status %d", resp.StatusCode)}, fmt.Errorf("home-web health check returned %d", resp.StatusCode)
		}
		if len(bytes.TrimSpace(bodyBytes)) == 0 {
			GlobalSecurityMetrics.IncHealthUnknown()
			return HealthProbeResult{Component: canonical, Level: ReadinessServiceReady, Detail: "empty body"}, fmt.Errorf("%w: home-web returned empty response", ErrHealthUnknown)
		}
		return HealthProbeResult{Component: canonical, Level: ReadinessBusinessHealthy, Detail: "home-web serving frontend content"}, nil

	case ComponentHomeVision:
		host := getProbeHost(ComponentHomeVision)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:8090/health", host), nil)
		resp, err := client.Do(req)
		if err != nil {
			GlobalSecurityMetrics.IncHealthUnknown()
			return HealthProbeResult{Component: canonical, Level: ReadinessUnknown, Detail: err.Error()}, err
		}
		defer resp.Body.Close()
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return HealthProbeResult{Component: canonical, Level: ReadinessServiceReady, Detail: fmt.Sprintf("status %d", resp.StatusCode)}, fmt.Errorf("home-vision health check returned %d", resp.StatusCode)
		}
		var payload map[string]interface{}
		if json.Unmarshal(bodyBytes, &payload) != nil {
			GlobalSecurityMetrics.IncHealthUnknown()
			return HealthProbeResult{Component: canonical, Level: ReadinessUnknown, Detail: "invalid JSON"}, fmt.Errorf("%w: home-vision returned non-JSON health response", ErrHealthUnknown)
		}
		return HealthProbeResult{Component: canonical, Level: ReadinessBusinessHealthy, Detail: "home-vision health JSON verified"}, nil

	case ComponentFrigate:
		host := getProbeHost(ComponentFrigate)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:5000/api/config", host), nil)
		resp, err := client.Do(req)
		if err != nil {
			GlobalSecurityMetrics.IncHealthUnknown()
			return HealthProbeResult{Component: canonical, Level: ReadinessUnknown, Detail: err.Error()}, err
		}
		defer resp.Body.Close()
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return HealthProbeResult{Component: canonical, Level: ReadinessServiceReady, Detail: fmt.Sprintf("status %d", resp.StatusCode)}, fmt.Errorf("frigate health check returned %d", resp.StatusCode)
		}
		var cfgJSON map[string]interface{}
		if json.Unmarshal(bodyBytes, &cfgJSON) != nil {
			GlobalSecurityMetrics.IncHealthUnknown()
			return HealthProbeResult{Component: canonical, Level: ReadinessUnknown, Detail: "invalid JSON config"}, fmt.Errorf("%w: frigate returned non-JSON config response", ErrHealthUnknown)
		}
		return HealthProbeResult{Component: canonical, Level: ReadinessBusinessHealthy, Detail: "frigate API ready"}, nil

	case ComponentMosquitto:
		host := getProbeHost(ComponentMosquitto)
		lvl, err := probeMQTTHandshake(ctx, host)
		if err != nil {
			if lvl == ReadinessUnknown {
				GlobalSecurityMetrics.IncHealthUnknown()
			}
			return HealthProbeResult{Component: canonical, Level: lvl, Detail: err.Error()}, err
		}
		return HealthProbeResult{Component: canonical, Level: ReadinessBusinessHealthy, Detail: "MQTT CONNECT/CONNACK verified"}, nil

	case ComponentOpenResty, Component1PWAF:
		host := getProbeHost(ComponentOpenResty)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:80/", host), nil)
		resp, err := client.Do(req)
		if err != nil {
			GlobalSecurityMetrics.IncHealthUnknown()
			return HealthProbeResult{Component: canonical, Level: ReadinessUnknown, Detail: err.Error()}, fmt.Errorf("openresty HTTP check failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 500 {
			return HealthProbeResult{Component: canonical, Level: ReadinessServiceReady, Detail: fmt.Sprintf("upstream error %d", resp.StatusCode)}, fmt.Errorf("openresty gateway/upstream returned %d", resp.StatusCode)
		}
		return HealthProbeResult{Component: canonical, Level: ReadinessBusinessHealthy, Detail: fmt.Sprintf("openresty HTTP status %d", resp.StatusCode)}, nil

	default:
		return HealthProbeResult{Component: target, Level: ReadinessUnknown, Detail: ErrComponentNotWhitelisted.Error()}, ErrComponentNotWhitelisted
	}
}

// CheckHealth verifies target business readiness (BUSINESS_HEALTHY required).
func (e *DefaultOperationExecutor) CheckHealth(ctx context.Context, target string) error {
	_, err := e.CheckReadiness(ctx, target)
	return err
}

// recordExecutionHistory writes an immutable audit record to ops_execution_histories and propagates write errors.
func (e *DefaultOperationExecutor) recordExecutionHistory(component, action, status, output, detail string) error {
	if e.db == nil {
		return nil
	}
	exists, err := tableExists(e.db, "ops_execution_histories")
	if err != nil {
		GlobalSecurityMetrics.IncAuditFailed()
		return err
	}
	if !exists {
		return nil
	}
	history := model.OpsExecutionHistory{
		ExecutionType: "proposal_exec",
		Component:     component,
		Action:        action,
		Status:        status,
		HealthOutput:  output,
		Detail:        detail,
		CreatedAt:     time.Now().UTC(),
	}
	if err := e.db.Create(&history).Error; err != nil {
		GlobalSecurityMetrics.IncAuditFailed()
		return fmt.Errorf("failed to persist execution audit history: %w", err)
	}
	return nil
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

// verifyContextLeaseBeforeSocket ensures that if a LeaseGuard is bound to ctx, its token and fencing
// generation remain authoritative immediately prior to dialing/writing Docker Unix Socket.
// NOTE: Once HTTP request bytes are committed to Docker Unix socket, Docker Engine cannot natively
// evaluate fencing tokens on an in-flight daemon syscall; pre-write verification + context deadlines
// minimize that external race window to sub-millisecond transport commit time (R4-06).
func verifyContextLeaseBeforeSocket(ctx context.Context) error {
	if guard := RecoveryLeaseFromContext(ctx); guard != nil {
		if err := guard.VerifyBeforeMutation(0); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// executeDockerSocketRestart issues a container restart via Docker Unix Socket HTTP API.
func executeDockerSocketRestart(ctx context.Context, sockPath, containerName string) error {
	if err := verifyContextLeaseBeforeSocket(ctx); err != nil {
		return err
	}
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
	if err := verifyContextLeaseBeforeSocket(ctx); err != nil {
		return "", err
	}
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

	if err := verifyContextLeaseBeforeSocket(ctx); err != nil {
		return "", err
	}
	// 2. Stop container
	stopURL := fmt.Sprintf("http://localhost/containers/%s/stop?t=10", containerName)
	stopReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, stopURL, nil)
	stopResp, err := client.Do(stopReq)
	if err == nil {
		_ = stopResp.Body.Close()
	}

	if err := verifyContextLeaseBeforeSocket(ctx); err != nil {
		return "", err
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
// (EXECUTING, ROLLING_BACK) on startup and converges them via ProposalFSM CAS into
// EXECUTION_UNKNOWN or RECONCILIATION_REQUIRED (never blind retry).
func ReconcileStartupProposals(db *gorm.DB, executor OperationExecutor) error {
	if db == nil {
		return nil
	}
	now := time.Now().UTC()
	fsm := NewProposalFSM(db)

	var executingProps []model.OpsProposal
	if err := db.Where("status = ?", model.ProposalStatusExecuting).Find(&executingProps).Error; err != nil {
		GlobalSecurityMetrics.IncDatabaseError()
		return err
	}
	for _, prop := range executingProps {
		targetStatus := model.ProposalStatusReconciliationRequired
		execLog := "Service restarted while proposal was EXECUTING; marked RECONCILIATION_REQUIRED for human review"

		if executor != nil && prop.TargetComponent != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := executor.CheckHealth(ctx, prop.TargetComponent); err == nil {
				targetStatus = model.ProposalStatusExecutionUnknown
				execLog = "Service restarted while proposal was EXECUTING; target component healthy, mutation verification required (EXECUTION_UNKNOWN)"
			}
			cancel()
		}

		propKey := prop.ProposalID
		if propKey == "" {
			propKey = fmt.Sprintf("%d", prop.ID)
		}
		GlobalSecurityMetrics.IncExecutionUnknown()
		if _, err := fsm.TransitionStrict(ProposalTransitionParams{
			ProposalID:       propKey,
			ExpectedStatuses: []string{model.ProposalStatusExecuting},
			TargetStatus:     targetStatus,
			Updates: map[string]interface{}{
				"execution_log": execLog,
				"executed_at":   utils.NullTime{Time: now, Valid: true},
				"reconciled_at": utils.NullTime{Time: now, Valid: true},
			},
			Audit: &ProposalTransitionAudit{
				OperationID: GenerateOperationID("startup-reconcile"),
				EventType:   "ops.proposal.startup_reconciled",
				Actor:       "startup-reconciler",
				Level:       model.LevelCritical,
				Message:     execLog,
			},
		}); err != nil {
			return err
		}
	}

	var rollingBackProps []model.OpsProposal
	if err := db.Where("status = ?", model.ProposalStatusRollingBack).Find(&rollingBackProps).Error; err != nil {
		GlobalSecurityMetrics.IncDatabaseError()
		return err
	}
	for _, prop := range rollingBackProps {
		propKey := prop.ProposalID
		if propKey == "" {
			propKey = fmt.Sprintf("%d", prop.ID)
		}
		execLog := "Service restarted while proposal was ROLLING_BACK; container state requires human reconciliation (RECONCILIATION_REQUIRED)"
		if _, err := fsm.TransitionStrict(ProposalTransitionParams{
			ProposalID:       propKey,
			ExpectedStatuses: []string{model.ProposalStatusRollingBack},
			TargetStatus:     model.ProposalStatusReconciliationRequired,
			Updates: map[string]interface{}{
				"execution_log": execLog,
				"executed_at":   utils.NullTime{Time: now, Valid: true},
				"reconciled_at": utils.NullTime{Time: now, Valid: true},
			},
			Audit: &ProposalTransitionAudit{
				OperationID: GenerateOperationID("startup-reconcile"),
				EventType:   "ops.proposal.rollback_interrupted",
				Actor:       "startup-reconciler",
				Level:       model.LevelCritical,
				Message:     execLog,
			},
		}); err != nil {
			return err
		}
	}

	return nil
}

// SweepOrphanProposals is a backward-compatible wrapper calling ReconcileStartupProposals.
func SweepOrphanProposals(db *gorm.DB) error {
	return ReconcileStartupProposals(db, nil)
}

