package log

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

func TestSubscriberBuildEntry_SecurityGuardMode(t *testing.T) {
	sub := &Subscriber{}

	tests := []struct {
		mode      string
		updatedBy string
		expected  string
	}{
		{"away", "admin", "用户 admin 将安防布防模式更改为 【离家布防】"},
		{model.GuardModeArmedAway, "admin", "用户 admin 将安防布防模式更改为 【离家布防】"},
		{"home", "alice", "用户 alice 将安防布防模式更改为 【在家守护】"},
		{model.GuardModeArmedHome, "alice", "用户 alice 将安防布防模式更改为 【在家守护】"},
		{"disarmed", "bob", "用户 bob 将安防布防模式更改为 【撤防免打扰】"},
		{model.GuardModeDisarmed, "bob", "用户 bob 将安防布防模式更改为 【撤防免打扰】"},
		{"custom_mode", "", "用户 系统 将安防布防模式更改为 【custom_mode】"},
	}

	for _, tc := range tests {
		payload, err := json.Marshal(map[string]interface{}{
			"mode":       tc.mode,
			"updated_by": tc.updatedBy,
			"updated_at": time.Now().Unix(),
		})
		if err != nil {
			t.Fatalf("marshal failed: %v", err)
		}

		entry := sub.buildEntry(eventbus.TopicSecurityGuardMode, eventbus.Event{
			Topic:   eventbus.TopicSecurityGuardMode,
			Payload: payload,
		})

		if entry == nil {
			t.Fatalf("expected non-nil entry for mode %s", tc.mode)
		}
		if entry.Message != tc.expected {
			t.Errorf("expected message %q, got %q", tc.expected, entry.Message)
		}
		if entry.Level != model.LevelNormal {
			t.Errorf("expected level %s, got %s", model.LevelNormal, entry.Level)
		}
	}
}

func TestSubscriberBuildEntry_VisionPerson(t *testing.T) {
	sub := &Subscriber{}

	// Test register
	regPayload, err := json.Marshal(eventbus.VisionPersonManagePayload{
		AdminID:   1,
		AdminName: "admin",
		Name:      "张三",
		Action:    "register",
	})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	entryReg := sub.buildEntry(eventbus.TopicVisionPersonRegister, eventbus.Event{
		Topic:   eventbus.TopicVisionPersonRegister,
		Payload: regPayload,
	})
	if entryReg == nil {
		t.Fatal("expected non-nil entry for person register")
	}
	expectedReg := "管理员 admin 录入家庭成员【张三】的人脸特征档案"
	if entryReg.Message != expectedReg {
		t.Errorf("expected %q, got %q", expectedReg, entryReg.Message)
	}
	if entryReg.Level != model.LevelNormal {
		t.Errorf("expected level %s, got %s", model.LevelNormal, entryReg.Level)
	}

	// Test delete
	delPayload, err := json.Marshal(eventbus.VisionPersonManagePayload{
		AdminID:   1,
		AdminName: "admin",
		Name:      "李四",
		Action:    "delete",
	})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	entryDel := sub.buildEntry(eventbus.TopicVisionPersonDelete, eventbus.Event{
		Topic:   eventbus.TopicVisionPersonDelete,
		Payload: delPayload,
	})
	if entryDel == nil {
		t.Fatal("expected non-nil entry for person delete")
	}
	expectedDel := "管理员 admin 删除家庭成员【李四】的人脸档案"
	if entryDel.Message != expectedDel {
		t.Errorf("expected %q, got %q", expectedDel, entryDel.Message)
	}
	if entryDel.Level != model.LevelWarning {
		t.Errorf("expected level %s, got %s", model.LevelWarning, entryDel.Level)
	}
}

func TestSubscriberBuildEntry_DetailedCameraUpdate(t *testing.T) {
	sub := &Subscriber{}

	// Preset detail test
	payload1, err := json.Marshal(eventbus.CameraManagePayload{
		AdminID:    1,
		CameraID:   1,
		CameraName: "客厅摄像头",
		Action:     "update",
		Detail:     "保存预置位【沙发角度】",
	})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	entry1 := sub.buildEntry(eventbus.TopicCameraUpdate, eventbus.Event{
		Topic:   eventbus.TopicCameraUpdate,
		Payload: payload1,
	})
	if entry1 == nil {
		t.Fatal("expected non-nil entry for camera update")
	}
	expected1 := "管理员 #1 保存预置位【沙发角度】（摄像头【客厅摄像头】）"
	if entry1.Message != expected1 {
		t.Errorf("expected %q, got %q", expected1, entry1.Message)
	}

	// Codec detail test
	payload2, err := json.Marshal(eventbus.CameraManagePayload{
		AdminID:    1,
		CameraID:   1,
		CameraName: "门前摄像头",
		Action:     "update",
		Detail:     "编码格式更改为【H.265 (HEVC)】",
	})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	entry2 := sub.buildEntry(eventbus.TopicCameraUpdate, eventbus.Event{
		Topic:   eventbus.TopicCameraUpdate,
		Payload: payload2,
	})
	if entry2 == nil {
		t.Fatal("expected non-nil entry for camera update")
	}
	expected2 := "管理员 #1 将摄像头【门前摄像头】的编码格式更改为【H.265 (HEVC)】"
	if entry2.Message != expected2 {
		t.Errorf("expected %q, got %q", expected2, entry2.Message)
	}
}

func TestSubscriberBuildEntry_DetailedUserUpdate(t *testing.T) {
	sub := &Subscriber{}

	payload, err := json.Marshal(eventbus.UserManagePayload{
		AdminID:    1,
		AdminName:  "root",
		TargetID:   3,
		TargetName: "worker1",
		Action:     "update",
		Detail:     "角色设置为【管理员】",
	})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	entry := sub.buildEntry(eventbus.TopicUserUpdate, eventbus.Event{
		Topic:   eventbus.TopicUserUpdate,
		Payload: payload,
	})
	if entry == nil {
		t.Fatal("expected non-nil entry for user update")
	}
	if !strings.Contains(entry.Message, "角色设置为【管理员】") {
		t.Errorf("expected message to contain detail, got %q", entry.Message)
	}
}

func TestSubscriberBuildEntry_DetailedAutomationUpdate(t *testing.T) {
	sub := &Subscriber{}

	payload, err := json.Marshal(eventbus.AutomationManagePayload{
		AdminID:  1,
		RuleID:   10,
		RuleName: "夜间人体移动开灯",
		Action:   "update",
		Detail:   "启用规则",
	})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	entry := sub.buildEntry(eventbus.TopicAutomationUpdate, eventbus.Event{
		Topic:   eventbus.TopicAutomationUpdate,
		Payload: payload,
	})
	if entry == nil {
		t.Fatal("expected non-nil entry for automation update")
	}
	expected := "管理员 #1 更新自动化规则【夜间人体移动开灯】（启用规则）"
	if entry.Message != expected {
		t.Errorf("expected %q, got %q", expected, entry.Message)
	}
}
