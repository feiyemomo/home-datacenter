// Package camera is the platformization layer for camera-type devices.
//
// It owns three concerns:
//
//  1. Registry — single source of truth for camera CRUD in the DB,
//     with side effects to go2rtc (add/remove stream) at register /
//     unregister time. All credentials at rest are AES-GCM encrypted
//     via utils.SecretBox.
//  2. Go2RTCClient — minimal HTTP client around the go2rtc server
//     (/api/streams, /api/webrtc, /api/stream.m3u8).
//  3. ONVIFController — ONVIF PTZ dispatcher. Lazy-connects to the
//     device, keeps one *onvif.Device per camera id.
//  4. HealthChecker — background goroutine that TCP-dials each
//     camera's RTSP port, persists Status/LastSeenAt and publishes
//     "device.status" events on the EventBus.
//
// Nothing here imports Gin or HTTP handlers — the camera module is a
// pure service the handler layer sits on top of.
package camera
