// Package mqtt centralizes MQTT topic naming and provides helpers
// to build and parse topic strings.
//
// Topic schema (all topics are under the "home-datacenter/" prefix):
//
//	home-datacenter/devices/{device_id}/status      device → server
//	home-datacenter/devices/{device_id}/telemetry   device → server
//	home-datacenter/devices/{device_id}/command     server → device
//	home-datacenter/devices/{device_id}/events      bidirectional
//
//	home-datacenter/users/{user_id}/notifications   server → app
//
//	home-datacenter/system/broadcast                server → all
//	home-datacenter/system/health                   server → all
//
// The server subscribes to:
//
//	home-datacenter/devices/+/status
//	home-datacenter/devices/+/telemetry
//	home-datacenter/devices/+/events
//
// The server publishes to:
//
//	home-datacenter/devices/{device_id}/command
//	home-datacenter/users/{user_id}/notifications
//	home-datacenter/system/broadcast
package mqtt

import (
	"fmt"
	"strconv"
	"strings"
)

// Prefix is the root namespace for all topics.
const Prefix = "home-datacenter"

// Topic builders — produce fully-qualified topic strings.

// DeviceStatus — device publishes its online/offline/heartbeat here.
func DeviceStatus(deviceID uint) string {
	return fmt.Sprintf("%s/devices/%d/status", Prefix, deviceID)
}

// DeviceTelemetry — device publishes sensor / state data here.
func DeviceTelemetry(deviceID uint) string {
	return fmt.Sprintf("%s/devices/%d/telemetry", Prefix, deviceID)
}

// DeviceCommand — server publishes commands to a device here.
func DeviceCommand(deviceID uint) string {
	return fmt.Sprintf("%s/devices/%d/command", Prefix, deviceID)
}

// DeviceEvents — bidirectional custom events for a device.
func DeviceEvents(deviceID uint) string {
	return fmt.Sprintf("%s/devices/%d/events", Prefix, deviceID)
}

// UserNotifications — server pushes notifications to a user's apps.
func UserNotifications(userID uint) string {
	return fmt.Sprintf("%s/users/%d/notifications", Prefix, userID)
}

// SystemBroadcast — system-wide announcement.
func SystemBroadcast() string {
	return Prefix + "/system/broadcast"
}

// SystemHealth — periodic system health publication.
func SystemHealth() string {
	return Prefix + "/system/health"
}

// Subscription filters (with MQTT + wildcard) the server uses.

// SubscribeDeviceStatus matches status messages from any device.
func SubscribeDeviceStatus() string {
	return Prefix + "/devices/+/status"
}

// SubscribeDeviceTelemetry matches telemetry from any device.
func SubscribeDeviceTelemetry() string {
	return Prefix + "/devices/+/telemetry"
}

// SubscribeDeviceEvents matches events from any device.
func SubscribeDeviceEvents() string {
	return Prefix + "/devices/+/events"
}

// ParsedTopic is the result of parsing an MQTT topic string.
type ParsedTopic struct {
	Domain  string // "devices" | "users" | "system"
	ID      uint   // device_id or user_id (0 for system)
	Subtype string // "status" | "telemetry" | "command" | "events" | ...
}

// ParseTopic splits a topic like
// "home-datacenter/devices/3/telemetry" into its components.
// Returns ok=false if the topic does not match the expected schema.
func ParseTopic(topic string) (ParsedTopic, bool) {
	if !strings.HasPrefix(topic, Prefix+"/") {
		return ParsedTopic{}, false
	}
	rest := strings.TrimPrefix(topic, Prefix+"/")
	parts := strings.Split(rest, "/")

	if len(parts) < 2 {
		return ParsedTopic{}, false
	}

	pt := ParsedTopic{Domain: parts[0]}

	switch parts[0] {
	case "devices", "users":
		if len(parts) < 3 {
			return ParsedTopic{}, false
		}
		id, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			return ParsedTopic{}, false
		}
		pt.ID = uint(id)
		pt.Subtype = parts[2]
	case "system":
		pt.Subtype = parts[1]
	default:
		return ParsedTopic{}, false
	}

	return pt, true
}
