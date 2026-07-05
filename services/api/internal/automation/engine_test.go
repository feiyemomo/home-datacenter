package automation

import (
	"net"
	"testing"
	"time"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

func TestTriggerMatches(t *testing.T) {
	cases := []struct {
		trigger, topic string
		want           bool
	}{
		{"*", "anything", true},
		{"*", "", true},
		{"", "anything", true},
		{"device", "device.status", true},
		{"device", "device.telemetry", true},
		{"device", "device", true},
		{"device", "device.1.status", true},
		{"device", "camera.online", false},
		{"device.1", "device.1.status", true},
		{"device.1", "device.10.status", false}, // segment boundary
		{"device.1", "device.1", true},
		{"camera.motion", "camera.motion", true},
		{"camera.motion", "camera.motion.detected", true}, // segment boundary
		{"camera", "camera.online", true},
	}
	for _, c := range cases {
		got := triggerMatches(c.trigger, c.topic)
		if got != c.want {
			t.Errorf("triggerMatches(%q, %q) = %v, want %v",
				c.trigger, c.topic, got, c.want)
		}
	}
}

func TestTimeInRange(t *testing.T) {
	parse := func(s string) time.Time {
		t, _ := time.Parse("15:04", s)
		return t
	}
	cases := []struct {
		name     string
		t        time.Time
		gte, lte string
		want     bool
	}{
		{"no bounds", parse("14:00"), "", "", true},
		{"gte only within", parse("14:00"), "10:00", "", true},
		{"gte only outside", parse("09:00"), "10:00", "", false},
		{"lte only within", parse("14:00"), "", "18:00", true},
		{"lte only outside", parse("20:00"), "", "18:00", false},
		{"range no wrap within", parse("14:00"), "10:00", "18:00", true},
		{"range no wrap outside", parse("20:00"), "10:00", "18:00", false},
		{"range wrap midnight within evening", parse("23:00"), "22:00", "06:00", true},
		{"range wrap midnight within morning", parse("02:00"), "22:00", "06:00", true},
		{"range wrap midnight outside", parse("10:00"), "22:00", "06:00", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := timeInRange(c.t, c.gte, c.lte)
			if got != c.want {
				t.Errorf("timeInRange(%v, %q, %q) = %v, want %v",
					c.t.Format("15:04"), c.gte, c.lte, got, c.want)
			}
		})
	}
}

func TestConditionMatches(t *testing.T) {
	// Payload equality.
	ev := eventbus.Event{
		Topic:     "camera.offline",
		Payload:   []byte(`{"camera_id":3,"status":"offline","host":"10.0.0.5"}`),
		Timestamp: time.Now(),
	}
	if !conditionMatches(model.Condition{}, ev) {
		t.Error("empty condition should match any event")
	}
	if !conditionMatches(model.Condition{
		PayloadEQ: map[string]any{"status": "offline"},
	}, ev) {
		t.Error("payload_eq status=offline should match")
	}
	if conditionMatches(model.Condition{
		PayloadEQ: map[string]any{"status": "online"},
	}, ev) {
		t.Error("payload_eq status=online should NOT match")
	}
	if conditionMatches(model.Condition{
		PayloadEQ: map[string]any{"missing_key": "x"},
	}, ev) {
		t.Error("payload_eq with missing key should NOT match")
	}
	// Multiple keys, all match.
	if !conditionMatches(model.Condition{
		PayloadEQ: map[string]any{
			"status":    "offline",
			"camera_id": float64(3), // JSON numbers decode as float64
		},
	}, ev) {
		t.Error("payload_eq with all matching keys should match")
	}
}

func TestConditionMatchesMalformedPayload(t *testing.T) {
	ev := eventbus.Event{
		Topic:   "device.status",
		Payload: []byte(`not json`),
	}
	if conditionMatches(model.Condition{
		PayloadEQ: map[string]any{"status": "online"},
	}, ev) {
		t.Error("malformed payload should not match payload_eq")
	}
	// But a condition with no payload_eq should still match.
	if !conditionMatches(model.Condition{}, ev) {
		t.Error("empty condition should match even with malformed payload")
	}
}

func TestIsAllowedMQTTTopic(t *testing.T) {
	cases := []struct {
		topic string
		want  bool
	}{
		{"home-datacenter/devices/1/command", true},
		{"home-datacenter/cameras/2/event", true},
		{"home-datacenter/", true},
		{"devices/1/command", false},
		{"$SYS/broker/version", false},
		{"", false},
	}
	for _, c := range cases {
		got := isAllowedMQTTTopic(c.topic)
		if got != c.want {
			t.Errorf("isAllowedMQTTTopic(%q) = %v, want %v",
				c.topic, got, c.want)
		}
	}
}

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"127.0.0.1", false},       // loopback
		{"10.0.0.1", false},        // private
		{"192.168.1.1", false},     // private
		{"172.16.0.1", false},      // private
		{"169.254.169.254", false}, // link-local (AWS metadata)
		{"0.0.0.0", false},         // unspecified
		{"::1", false},             // loopback v6
		{"fc00::1", false},         // private v6
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("failed to parse IP %q", c.ip)
		}
		if got := isPublicIP(ip); got != c.want {
			t.Errorf("isPublicIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}
