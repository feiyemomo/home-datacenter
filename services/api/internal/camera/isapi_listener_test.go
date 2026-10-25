package camera

import (
	"encoding/xml"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

func TestISAPIEventAlert_IsHuman(t *testing.T) {
	xmlHuman := `
<EventNotificationAlert version="2.0" xmlns="http://www.hikvision.com/ver20/XMLSchema">
<ipAddress>192.168.31.100</ipAddress>
<portNo>80</portNo>
<channelID>1</channelID>
<dateTime>2026-10-24T20:02:38+08:00</dateTime>
<activePostCount>1</activePostCount>
<eventType>VMD</eventType>
<eventState>active</eventState>
<eventDescription>motion alarm</eventDescription>
<channelName>Camera 01</channelName>
<targetType>human</targetType>
</EventNotificationAlert>`

	var alert ISAPIEventAlert
	if err := xml.Unmarshal([]byte(xmlHuman), &alert); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !alert.IsHuman() {
		t.Errorf("expected IsHuman() = true, got false")
	}

	xmlSmart := `
<EventNotificationAlert version="2.0" xmlns="http://www.hikvision.com/ver20/XMLSchema">
<eventType>fielddetection</eventType>
<eventState>active</eventState>
<DetectionRegionList>
  <DetectionRegionEntry>
    <regionID>1</regionID>
    <targetType>human</targetType>
  </DetectionRegionEntry>
</DetectionRegionList>
</EventNotificationAlert>`

	var alert2 ISAPIEventAlert
	if err := xml.Unmarshal([]byte(xmlSmart), &alert2); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !alert2.IsHuman() {
		t.Errorf("expected IsHuman() = true for fielddetection, got false")
	}

	xmlVideoLoss := `
<EventNotificationAlert version="2.0" xmlns="http://www.hikvision.com/ver20/XMLSchema">
<eventType>videoloss</eventType>
<eventState>inactive</eventState>
</EventNotificationAlert>`

	var alert3 ISAPIEventAlert
	if err := xml.Unmarshal([]byte(xmlVideoLoss), &alert3); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if alert3.IsHuman() {
		t.Errorf("expected IsHuman() = false for videoloss, got true")
	}
}

func TestISAPIAlertListener_SessionCoalescing(t *testing.T) {
	// Ensure AI alert source mode is "camera" so cross-source dedup does not interfere
	aiMgr := GetGlobalAIAlertSourceManager("")
	_ = aiMgr.SetSource(AISourceCamera)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	_ = db.AutoMigrate(&model.Camera{})

	cam := model.Camera{
		ID:   1,
		Name: "Front Door",
		Host: "192.168.1.100",
	}
	_ = db.Create(&cam)

	bus := eventbus.New()
	reg := &Registry{DB: db}
	listener := NewISAPIAlertListener(reg, bus, nil, nil)

	var published []eventbus.Event
	bus.Subscribe(eventbus.TopicCameraPersonRecognized, func(e eventbus.Event) {
		published = append(published, e)
	})

	xmlAlert := []byte(`
<EventNotificationAlert version="2.0">
<eventType>fielddetection</eventType>
<eventState>active</eventState>
<targetType>human</targetType>
</EventNotificationAlert>`)

	// 1. First alert -> session created, first-report published
	listener.handleAlertXML(cam, xmlAlert)
	if len(published) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(published))
	}

	// 2. Sub-dedup alert within 5s -> dropped by dedup
	listener.handleAlertXML(cam, xmlAlert)
	if len(published) != 1 {
		t.Fatalf("expected alert within dedup window to be dropped, got %d", len(published))
	}

	// 3. Fast-forward past dedup (6s), still within session & sub-heartbeat -> count++, no publish
	listener.mu.Lock()
	listener.lastAlert[cam.ID] = listener.lastAlert[cam.ID].Add(-6 * time.Second)
	listener.mu.Unlock()

	listener.handleAlertXML(cam, xmlAlert)
	if len(published) != 1 {
		t.Fatalf("expected sub-heartbeat alert to be coalesced silently, got %d", len(published))
	}
	listener.mu.Lock()
	sess := listener.sessions[cam.ID]
	if sess == nil || sess.Count != 2 {
		t.Fatalf("expected session count=2, got %+v", sess)
	}
	listener.mu.Unlock()

	// 4. Fast-forward past heartbeat (35s) -> should publish update with count=3
	listener.mu.Lock()
	listener.lastAlert[cam.ID] = listener.lastAlert[cam.ID].Add(-10 * time.Second)
	sess.LastPublish = sess.LastPublish.Add(-35 * time.Second)
	listener.mu.Unlock()

	listener.handleAlertXML(cam, xmlAlert)
	if len(published) != 2 {
		t.Fatalf("expected 2 published events after heartbeat, got %d", len(published))
	}
}

func TestISAPIAlertListener_MotionFallbackToggle(t *testing.T) {
	aiMgr := GetGlobalAIAlertSourceManager("")
	_ = aiMgr.SetSource(AISourceCamera)
	aiMgr.SetMotionFallback(false) // 默认为严格模式

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	_ = db.AutoMigrate(&model.Camera{})

	cam := model.Camera{
		ID:   2,
		Name: "Yard",
		Host: "192.168.1.101",
	}
	_ = db.Create(&cam)

	bus := eventbus.New()
	reg := &Registry{DB: db}
	listener := NewISAPIAlertListener(reg, bus, nil, nil)

	var published []eventbus.Event
	bus.Subscribe(eventbus.TopicCameraPersonRecognized, func(e eventbus.Event) {
		published = append(published, e)
	})

	// 普通移动侦测（没有携带 human 目标属性）
	xmlVMD := []byte(`
<EventNotificationAlert version="2.0">
<eventType>VMD</eventType>
<eventState>active</eventState>
<targetType>other</targetType>
</EventNotificationAlert>`)

	// 1. 严格模式下：普通 VMD 直接被丢弃，不发布报警
	listener.handleAlertXML(cam, xmlVMD)
	if len(published) != 0 {
		t.Fatalf("expected VMD to be dropped in strict mode, but got %d events", len(published))
	}

	// 2. 一键切换为兼容增强模式 (Plan B)
	aiMgr.SetMotionFallback(true)

	// 重置每相机 dedup 时间以允许新判定
	listener.mu.Lock()
	delete(listener.lastAlert, cam.ID)
	listener.mu.Unlock()

	listener.handleAlertXML(cam, xmlVMD)
	if len(published) != 1 {
		t.Fatalf("expected VMD to be processed in fallback mode, got %d events", len(published))
	}

	// 3. 一键切换回严格模式
	aiMgr.SetMotionFallback(false)

	listener.mu.Lock()
	delete(listener.lastAlert, cam.ID)
	listener.mu.Unlock()

	listener.handleAlertXML(cam, xmlVMD)
	if len(published) != 1 {
		t.Fatalf("expected VMD to be dropped after toggling back to strict mode, got %d events", len(published))
	}
}
