package ops

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

func setupR4DigestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "r4_digest.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	if err := EnsureOpsSchema(db); err != nil {
		t.Fatalf("EnsureOpsSchema failed: %v", err)
	}
	_ = db.AutoMigrate(&model.SystemLog{})
	if sqlDB, sErr := db.DB(); sErr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	return db
}

// TestR4_22_ApprovalDigest_AllFieldsTamperAndCryptoIntegrity exercises all 13 bound fields,
// missing/weak HMAC keys, downgrade attacks, keyring rotation, and strict JSON unknown fields (R4-11, R4-12).
func TestR4_22_ApprovalDigest_AllFieldsTamperAndCryptoIntegrity(t *testing.T) {
	model.SetApprovalHMACKeyringForTest("k1", map[string]string{
		"k1": "super-secret-hmac-key-material-alpha-0123456789",
		"k2": "super-secret-hmac-key-material-bravo-9876543210",
	})
	defer model.SetApprovalHMACKeyringForTest("", nil)

	schedStr := "03:30"
	makeSignedProposal := func(t *testing.T, db *gorm.DB, propID string) model.OpsProposal {
		t.Helper()
		p := model.OpsProposal{
			ProposalID:       propID,
			HouseholdID:      1,
			Title:            "Signed Proposal " + propID,
			TargetComponent:  "home-web",
			RiskLevel:        model.RiskLevelHigh,
			EstimatedSeconds: 45,
			ExecutionPlan:    "docker restart home-web",
			RollbackPlan:     "docker restart home-web",
			ActionSpec:       `{"action":"docker.restart","target":"home-web","params":{"grace_sec":5},"timeout_sec":30,"execution_policy":"strict","rollback_spec":{"action":"docker.restart","target":"home-web"}}`,
			ExecutionPolicy:  "strict",
			BaselineHash:     model.ComputeDefaultBaseline("home-web"),
			Status:           model.ProposalStatusApproved,
			ApprovedBy:       "admin-alice",
			ApprovedAt:       utils.NullTime{Time: time.Now().UTC().Truncate(time.Second), Valid: true},
			Version:          1,
			Revision:         1,
			CreatedAt:        time.Now().UTC(),
		}
		digest, err := model.ComputeProposalApprovalDigest(&p)
		if err != nil {
			t.Fatalf("ComputeProposalApprovalDigest failed: %v", err)
		}
		p.ApprovalDigest = digest
		if err := db.Create(&p).Error; err != nil {
			t.Fatalf("db.Create failed: %v", err)
		}
		return p
	}

	tamperCases := []struct {
		name   string
		mutate func(db *gorm.DB, p *model.OpsProposal)
	}{
		{"01_TargetComponent", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("target_component", "home-api")
		}},
		{"02_ActionSpec_Action", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("action_spec", `{"action":"docker.recreate","target":"home-web","params":{"grace_sec":5},"timeout_sec":30,"execution_policy":"strict","rollback_spec":{"action":"docker.restart","target":"home-web"}}`)
		}},
		{"03_ActionSpec_Target", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("action_spec", `{"action":"docker.restart","target":"frigate","params":{"grace_sec":5},"timeout_sec":30,"execution_policy":"strict","rollback_spec":{"action":"docker.restart","target":"home-web"}}`)
		}},
		{"04_ActionSpec_Params", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("action_spec", `{"action":"docker.restart","target":"home-web","params":{"grace_sec":999},"timeout_sec":30,"execution_policy":"strict","rollback_spec":{"action":"docker.restart","target":"home-web"}}`)
		}},
		{"05_ActionSpec_TimeoutSec", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("action_spec", `{"action":"docker.restart","target":"home-web","params":{"grace_sec":5},"timeout_sec":999,"execution_policy":"strict","rollback_spec":{"action":"docker.restart","target":"home-web"}}`)
		}},
		{"06_ExecutionPlan", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("execution_plan", "rm -rf /data")
		}},
		{"07_RollbackPlan", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("rollback_plan", "tampered rollback command")
		}},
		{"08_RollbackSpec", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("action_spec", `{"action":"docker.restart","target":"home-web","params":{"grace_sec":5},"timeout_sec":30,"execution_policy":"strict","rollback_spec":{"action":"docker.recreate","target":"home-web"}}`)
		}},
		{"09_ExecutionPolicy", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("execution_policy", "permissive")
		}},
		{"10_ScheduledAt", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("scheduled_at", schedStr)
		}},
		{"11_BaselineHash", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("baseline_hash", "sha256:tamperedbaseline0000000000000000000000000000000000000000000000")
		}},
		{"12_ApprovedBy", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("approved_by", "eve-attacker")
		}},
		{"13_ApprovedAt", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("approved_at", time.Now().UTC().Add(-2*time.Hour))
		}},
		{"14_Version", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("version", 99)
		}},
		{"15_HouseholdID", func(db *gorm.DB, p *model.OpsProposal) {
			db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("household_id", 2)
		}},
	}

	db := setupR4DigestDB(t)
	runnerCalled := 0
	executor := NewOperationExecutor(
		db, eventbus.New(), nil, nil,
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			runnerCalled++
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	for _, tc := range tamperCases {
		t.Run("Tamper_"+tc.name, func(t *testing.T) {
			p := makeSignedProposal(t, db, "prop-tamper-"+tc.name)
			tc.mutate(db, &p)

			beforeMismatches := GlobalSecurityMetrics.Snapshot().ProposalDigestMismatch
			_, err := executor.ExecuteProposal(context.Background(), &p)
			if !errors.Is(err, ErrApprovalDigestMismatch) {
				t.Fatalf("tamper '%s': expected ErrApprovalDigestMismatch, got %v", tc.name, err)
			}
			afterMismatches := GlobalSecurityMetrics.Snapshot().ProposalDigestMismatch
			if afterMismatches <= beforeMismatches {
				t.Fatalf("expected proposal_digest_mismatch metric increment")
			}
		})
	}
	if runnerCalled != 0 {
		t.Fatalf("CRITICAL: tampered proposals invoked Docker runner %d times!", runnerCalled)
	}

	t.Run("16_LegacySHA256DowngradeRejected", func(t *testing.T) {
		p := makeSignedProposal(t, db, "prop-downgrade")
		db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("approval_digest", "sha256:"+strings.Repeat("a", 64))
		_, err := executor.ExecuteProposal(context.Background(), &p)
		if !errors.Is(err, ErrApprovalDigestMismatch) {
			t.Fatalf("expected downgrade sha256 digest rejected, got %v", err)
		}
	})

	t.Run("17_UnknownJSONFieldRejected", func(t *testing.T) {
		h := NewOpsHandler(nil, nil, db, eventbus.New(), nil)
		r := makeAdminRouter(h)
		payload := `{"title":"Inject Extra","target_component":"home-web","risk_level":"HIGH","action_spec":"{\"action\":\"docker.restart\",\"target\":\"home-web\",\"injected_field\":\"evil\"}"}`
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 rejecting unknown JSON field in action_spec, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("18_HMACKeyRotationAndWrongKeyRejected", func(t *testing.T) {
		// Sign while active key is k1
		p := makeSignedProposal(t, db, "prop-rotate")

		// Rotate active key to k2 while retaining k1 in verification ring -> STILL verifies
		model.SetApprovalHMACKeyringForTest("k2", map[string]string{
			"k1": "super-secret-hmac-key-material-alpha-0123456789",
			"k2": "super-secret-hmac-key-material-bravo-9876543210",
		})
		var reloaded model.OpsProposal
		db.Where("id = ?", p.ID).First(&reloaded)
		if err := model.VerifyProposalApprovalDigest(&reloaded); err != nil {
			t.Fatalf("expected rotated keyring retaining k1 to verify k1-signed proposal: %v", err)
		}

		// Wrong k1 key material -> MUST FAIL
		model.SetApprovalHMACKeyringForTest("k1", map[string]string{
			"k1": "completely-different-key-material-zzzzzzzzzzzzzzzz",
		})
		if err := model.VerifyProposalApprovalDigest(&reloaded); err == nil {
			t.Fatalf("expected wrong HMAC key material to reject signature")
		}
	})

	t.Run("19_MissingHMACKeyBootGuardFailsClosed", func(t *testing.T) {
		model.SetApprovalHMACKeyringForTest("", nil) // Clear test override temporarily
		t.Setenv("OPS_APPROVAL_HMAC_KEY", "")
		t.Setenv("OPS_APPROVAL_HMAC_KEYS", "")
		if err := model.ValidateApprovalHMACKeyBootGuard(true); !errors.Is(err, model.ErrApprovalKeyMissing) {
			t.Fatalf("expected ErrApprovalKeyMissing from boot guard in production, got %v", err)
		}
		t.Setenv("OPS_APPROVAL_HMAC_KEY", "short")
		if err := model.ValidateApprovalHMACKeyBootGuard(true); err == nil {
			t.Fatalf("expected boot guard to reject short <32-byte HMAC key")
		}
		t.Setenv("OPS_APPROVAL_HMAC_KEY", "change-me-please-32-bytes-placeholder")
		if err := model.ValidateApprovalHMACKeyBootGuard(true); err == nil {
			t.Fatalf("expected boot guard to reject weak placeholder HMAC key")
		}
	})
}
