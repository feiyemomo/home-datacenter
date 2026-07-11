// Package camera is the platformization layer for camera-type devices.
//
// It owns these concerns:
//
//  1. Registry — single source of truth for camera CRUD in the DB,
//     with side effects to Frigate's bundled go2rtc (add/remove
//     stream) and Frigate REST API (config push) at register /
//     unregister time. All credentials at rest are AES-GCM encrypted
//     via utils.SecretBox.
//  2. Go2RTCClient — minimal HTTP client around the go2rtc server
//     bundled inside Frigate (/api/streams, /api/webrtc,
//     /api/stream.m3u8, /api/recorder).
//  3. FrigateClient — HTTP client for the Frigate REST API
//     (PUT /api/config/save) to push camera definitions so Frigate's
//     AI detection and recording pipelines know about each camera.
//  4. ONVIFController — ONVIF PTZ dispatcher. Lazy-connects to the
//     device, keeps one *onvif.Device per camera id.
//  5. HealthChecker — background goroutine that TCP-dials each
//     camera's RTSP port, persists Status/LastSeenAt and publishes
//     "device.status" events on the EventBus.
//
// Nothing here imports Gin or HTTP handlers — the camera module is a
// pure service the handler layer sits on top of.
package camera
