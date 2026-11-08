package security

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

func setupAlertTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite: %v", err)
	}

	if err := db.AutoMigrate(
		&model.SecurityAlertFilter{},
		&model.SecurityRecipient{},
	); err != nil {
		t.Fatalf("failed to migrate alert test tables: %v", err)
	}

	return db
}

// -------------------------------------------------------------
// Challenge 4: AlertManager Empirical Severity & Event Tests
// -------------------------------------------------------------

// Test 4.1: Severity Rank and InferAlertSeverity edge cases
func TestAlertManager_Challenge4_SeverityInference(t *testing.T) {
	cases := []struct {
		name      string
		subject   string
		alertType string
		detail    string
		action    string
		wantSev   string
	}{
		{
			name:      "Explicit critical type",
			subject:   "Alert",
			alertType: "critical",
			wantSev:   "critical",
		},
		{
			name:      "Explicit warning type",
			subject:   "Alert",
			alertType: "warning",
			wantSev:   "warning",
		},
		{
			name:      "Explicit info type",
			subject:   "Alert",
			alertType: "info",
			wantSev:   "info",
		},
		{
			name:      "Critical keyword in subject",
			subject:   "高危漏洞检测",
			alertType: "cve",
			wantSev:   "critical",
		},
		{
			name:      "Crash keyword in detail",
			subject:   "服务通知",
			alertType: "system",
			detail:    "Container crash detected in home-api",
			wantSev:   "critical",
		},
		{
			name:      "Info test keyword in subject",
			subject:   "SMTP 连通性测试邮件",
			alertType: "notice",
			wantSev:   "info",
		},
		{
			name:      "Generic warning default",
			subject:   "日常流量提醒",
			alertType: "bandwidth",
			detail:    "流量已达到80%",
			action:    "请关注",
			wantSev:   "warning",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InferAlertSeverity(tc.subject, tc.alertType, tc.detail, tc.action)
			if got != tc.wantSev {
				t.Errorf("InferAlertSeverity() = %s, want %s", got, tc.wantSev)
			}
		})
	}
}

// Test 4.2: MinSeverity Filtering ("info" vs "warning" vs "critical")
func TestAlertManager_Challenge4_MinSeverityFiltering(t *testing.T) {
	db := setupAlertTestDB(t)

	// SMTP is disabled; SendSecurityEmail returns ErrAlertNotConfigured when NOT suppressed,
	// and returns nil when suppressed by the severity filter!
	alertMgr := NewAlertManager(config.AlertConfig{
		Enabled:   true,
		Host:      "smtp.example.com",
		Port:      465,
		Username:  "admin@example.com",
		Password:  "secret",
		Recipient: "admin@example.com",
	}, db)

	// Subcase A: MinSeverity = "critical"
	t.Run("MinSeverity_Critical", func(t *testing.T) {
		filter := alertMgr.GetAlertFilters()
		filter.MinSeverity = "critical"
		alertMgr.UpdateAlertFilters(filter)

		// 1. Info event -> MUST be suppressed (returns nil)
		err := alertMgr.SendSecurityEmailSync("日常例行通知", "127.0.0.1", "info", "normal log", "none")
		if err != nil {
			t.Errorf("expected info email to be suppressed by critical threshold, got error: %v", err)
		}

		// 2. Warning event -> MUST be suppressed (returns nil)
		err = alertMgr.SendSecurityEmailSync("流量警告", "127.0.0.1", "warning", "80% quota", "none")
		if err != nil {
			t.Errorf("expected warning email to be suppressed by critical threshold, got error: %v", err)
		}

		// 3. Critical event -> MUST NOT be suppressed (attempts delivery, fails with SMTP connection error)
		err = alertMgr.SendSecurityEmailSync("高危攻击", "127.0.0.1", "critical", "RCE exploit", "ban")
		if err == nil {
			t.Errorf("expected critical email NOT to be suppressed (should attempt SMTP send and return dial error)")
		} else {
			t.Logf("Critical event passed filter and reached SMTP deliver: %v", err)
		}
	})

	// Subcase B: MinSeverity = "warning"
	t.Run("MinSeverity_Warning", func(t *testing.T) {
		filter := alertMgr.GetAlertFilters()
		filter.MinSeverity = "warning"
		alertMgr.UpdateAlertFilters(filter)

		// 1. Info event -> MUST be suppressed
		err := alertMgr.SendSecurityEmailSync("日常通知", "127.0.0.1", "info", "normal log", "none")
		if err != nil {
			t.Errorf("expected info email to be suppressed, got: %v", err)
		}

		// 2. Warning event -> MUST NOT be suppressed
		err = alertMgr.SendSecurityEmailSync("流量警告", "127.0.0.1", "warning", "80% quota", "none")
		if err == nil {
			t.Errorf("expected warning email NOT to be suppressed")
		} else {
			t.Logf("Warning event passed filter and reached SMTP: %v", err)
		}
	})

	// Subcase C: MinSeverity = "info"
	t.Run("MinSeverity_Info", func(t *testing.T) {
		filter := alertMgr.GetAlertFilters()
		filter.MinSeverity = "info"
		alertMgr.UpdateAlertFilters(filter)

		// Info event -> MUST NOT be suppressed
		err := alertMgr.SendSecurityEmailSync("日常通知", "127.0.0.1", "info", "normal log", "none")
		if err == nil {
			t.Errorf("expected info email NOT to be suppressed when MinSeverity is info")
		} else {
			t.Logf("Info event passed filter and reached SMTP: %v", err)
		}
	})

	// Subcase D: Manual connectivity test email bypasses MinSeverity = "critical"
	t.Run("ManualTestEmailBypass", func(t *testing.T) {
		filter := alertMgr.GetAlertFilters()
		filter.MinSeverity = "critical"
		alertMgr.UpdateAlertFilters(filter)

		// Even though it is a "test" (info severity), SendSecurityEmailSync must NOT suppress it
		err := alertMgr.SendSecurityEmailSync("SMTP 连通性测试邮件", "127.0.0.1", "测试", "SMTP configuration probe", "none")
		if err == nil {
			t.Errorf("expected manual test email to bypass filter and attempt send (returning dial error)")
		} else {
			t.Logf("Manual test email successfully bypassed critical threshold: %v", err)
		}
	})
}

// Test 4.3: Container Crash Event Triggering
func TestAlertManager_Challenge4_ContainerCrashEventTriggering(t *testing.T) {
	db := setupAlertTestDB(t)

	alertMgr := NewAlertManager(config.AlertConfig{
		Enabled:   true,
		Host:      "smtp.example.com",
		Port:      465,
		Username:  "admin@example.com",
		Password:  "secret",
		Recipient: "admin@example.com",
	}, db)

	// Subcase A: container_crash = true -> triggers alert
	t.Run("CrashAlertEnabled", func(t *testing.T) {
		filter := alertMgr.GetAlertFilters()
		filter.ContainerCrash = true
		alertMgr.UpdateAlertFilters(filter)

		err := alertMgr.HandleContainerCrash("home-frigate", "exit code 137 OOMKilled")
		// SendSecurityEmail runs asynchronously in background, returns nil if alert was accepted
		if err != nil {
			t.Errorf("HandleContainerCrash returned unexpected error: %v", err)
		}
	})

	// Subcase B: container_crash = false -> suppressed by filter
	t.Run("CrashAlertDisabled", func(t *testing.T) {
		filter := alertMgr.GetAlertFilters()
		filter.ContainerCrash = false
		alertMgr.UpdateAlertFilters(filter)

		err := alertMgr.HandleContainerCrash("home-frigate", "exit code 137 OOMKilled")
		if err != nil {
			t.Errorf("HandleContainerCrash should cleanly return nil when disabled, got: %v", err)
		}
	})

	// Subcase C: EventBus subscriber integration (matching main.go logic)
	t.Run("EventBusSubscriberIntegration", func(t *testing.T) {
		bus := eventbus.New()

		crashedContainers := make([]string, 0)
		bus.Subscribe(eventbus.TopicSystemLog, func(event eventbus.Event) {
			if event.Severity == eventbus.SeverityCritical {
				var payload struct {
					Action        string `json:"action"`
					ContainerName string `json:"container_name"`
					Success       bool   `json:"success"`
					Detail        string `json:"detail"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err == nil {
					if payload.Action == "container.restart" && !payload.Success {
						crashedContainers = append(crashedContainers, payload.ContainerName)
						_ = alertMgr.HandleContainerCrash(payload.ContainerName, payload.Detail)
					}
				}
			}
		})

		// Publish failed restart event
		failPayload, _ := json.Marshal(map[string]any{
			"action":         "container.restart",
			"container_name": "home-vision",
			"success":        false,
			"detail":         "Docker API error: container died immediately",
		})
		bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicSystemLog,
			Severity: eventbus.SeverityCritical,
			Payload:  failPayload,
		})

		// Publish successful restart event (should NOT trigger crash)
		successPayload, _ := json.Marshal(map[string]any{
			"action":         "container.restart",
			"container_name": "home-web",
			"success":        true,
			"detail":         "restarted cleanly",
		})
		bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicSystemLog,
			Severity: eventbus.SeverityCritical,
			Payload:  successPayload,
		})

		time.Sleep(50 * time.Millisecond)

		if len(crashedContainers) != 1 || crashedContainers[0] != "home-vision" {
			t.Fatalf("expected exactly home-vision to trigger crash event, got: %+v", crashedContainers)
		}
		t.Logf("EventBus integration verified: crashed containers intercepted: %+v", crashedContainers)
	})
}
