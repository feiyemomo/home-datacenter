/**
 * Shared TypeScript types for the Home Datacenter dashboard.
 *
 * The backend wraps every response in:
 *   { code: 0, message: "success", data: <T> }
 * On error: { code: <http_status>, message: "<error>", data: null }
 */

/** Standard API envelope. code === 0 means success. */
export interface ApiEnvelope<T> {
    code: number;
    message: string;
    data: T | null;
}

/**
 * Nullable datetime.
 *
 * The Go backend uses utils.NullTime whose MarshalJSON renders
 * either `null` (Valid=false) or an RFC3339 string (Valid=true).
 * We keep the {Time, Valid} shape here too so we tolerate either
 * serialization if the server ever changes.
 */
export type NullTime = string | { Time: string; Valid: boolean } | null;

/** A device row from GET /api/v1/device/list. */
export interface Device {
    id: number;
    user_id: number;
    device_name: string;
    last_login_at: NullTime;
    revoked_at: NullTime;
    last_ip: string;
    created_at: string;
    updated_at: string;
}

/** Response of GET /api/v1/device/list. */
export interface DeviceListResponse {
    devices: Device[];
}

/** Response of GET /api/v1/user/me. */
export interface User {
    id: number;
    name: string;
    is_admin: boolean;
}

/** A user row from GET /api/v1/user (admin list). */
export interface UserListEntry {
    id: number;
    name: string;
    is_admin: boolean;
    created_at: string;
    updated_at: string;
    device_count: number;
}

/** Response of GET /api/v1/user. */
export interface UserListResponse {
    users: UserListEntry[];
}

/** Response of DELETE /api/v1/user/:id. */
export interface UserDeleteResponse {
    deleted_devices: number;
}

/** Request body for POST /api/v1/user (create). */
export interface CreateUserRequest {
    name: string;
    is_admin: boolean;
}

/** Request body for PUT /api/v1/user/:id (partial update). */
export interface UpdateUserRequest {
    name?: string;
    is_admin?: boolean;
}

/** Response of POST /api/v1/auth/bind. */
export interface BindResponse {
    token: string;
}

/** Response of GET /api/v1/system/status. */
export interface SystemStatus {
    mqtt_connected: boolean;
    ws_clients: number;
    online_device_count: number;
    online_device_ids: number[];
    uptime_seconds: number;
    server_time: string;
}

/** Request body for POST /api/v1/mqtt/publish. */
export interface PublishMqttRequest {
    topic: string;
    payload: string;
    qos: 0 | 1 | 2;
}

/** Response of POST /api/v1/mqtt/publish. */
export interface PublishMqttResponse {
    topic: string;
    payload: string;
    qos: number;
}

/** Decoded JWT payload (HS256, signed server-side). */
export interface JwtClaims {
    user_id: number;
    device_id: number;
    exp?: number;
    iat?: number;
}

/** Canonical WebSocket message envelope. */
export interface WsMessage<T = unknown> {
    type: "event" | "heartbeat" | "online_list" | "error" | "broadcast" | string;
    topic?: string;
    payload?: T;
    ts: number;
}

// -------------------- Camera platformization (Phase 4) --------------------

/** A camera row from GET /api/v1/cameras/:id. */
export interface Camera {
    id: number;
    type: "camera";
    name: string;
    vendor: string;
    host: string;
    onvif_port: number;
    rtsp_port: number;
    channel_id: number;
    status: "online" | "offline" | "unknown";
    last_seen_at: string | null;
    capabilities: CameraCapabilities;
    meta: CameraMeta;
    presets: Record<string, string>;
    stream: CameraStream;
    /**
     * Server-side ffmpeg H.264 transcoding is on. Reflects the
     * backend `cameras.transcode` column; the dashboard uses it
     * to surface a small "x264" badge next to the camera name so
     * the operator can spot which cameras are paying the
     * transcode cost at a glance.
     */
    transcode?: boolean;
    /**
     * Output codec: "passthrough" | "h264" | "h265" | "".
     *
     * Only "h264" is settable via the dashboard / PUT /cameras/:id/codec
     * — WebRTC's RTP codec registry does not include H.265, so
     * passthrough/h265 always 502 on Chrome/Edge/Firefox WebRTC.
     * Legacy cameras may still carry "passthrough" or "h265" in the
     * DB (set before this restriction); the dashboard shows them as
     * "(legacy)" in the codec dropdown and offers "H.264" for migration.
     * When non-empty, this field is the source of truth and overrides
     * the legacy transcode bool. Empty inherits legacy behavior.
     */
    codec?: string;
    created_at: string;
    updated_at: string;
}

export interface CameraCapabilities {
    ptz?: boolean;
    audio?: boolean;
    motion?: boolean;
    [k: string]: unknown;
}

export interface CameraMeta {
    onvif_profile?: string;
    recording?: CameraRecordingPlan;
    [k: string]: unknown;
}

/** Canonical device.event payload (motion / AI). */
export interface CameraEventMessage {
    device_id: number;
    type: "camera";
    event: "motion" | "ai" | string;
    confidence?: number;
    ts: number;
}

export interface CameraStream {
    stream_name: string;
    webrtc_url: string;
    hls_url: string;
}

/** Per-segment recording metadata. */
export interface CameraRecording {
    id: number;
    camera_id: number;
    start_at: string;
    end_at: string;
    duration_seconds: number;
    size_bytes: number;
    size_human: string;
    file_path: string;
}

export interface CameraRecordingPlan {
    enabled: boolean;
    segment_seconds?: number;
    retention_days?: number;
    output_dir?: string;
    cron?: string;
}

/** Canonical status event payload (matches the Go eventbus). */
export interface CameraStatusEvent {
    device_id: number;
    type: "camera";
    status: "online" | "offline" | "heartbeat" | string;
    ts: number;
}

/** PTZ command string. */
export type PTZCommand =
    | "left"
    | "right"
    | "up"
    | "down"
    | "stop"
    | "zoom_in"
    | "zoom_out";

/** ONVIF preset entry. */
export interface PresetEntry {
    token: string;
    name: string;
}

/** ICE config returned by GET /api/v1/cameras/ice. */
export interface IceServerConfig {
    urls: string | string[];
    username?: string;
    credential?: string;
}

export interface IceConfig {
    ice_servers: IceServerConfig[];
    webrtc_base: string;
}

export interface SetPresetRequest {
    token: string;
}

// -------------------- Network capability (Phase 10) --------------------

/** IPv6 availability and public reachability. */
export interface IPv6Status {
    enabled: boolean;
    reachable: boolean;
    address?: string;
    checked_at: string;
}

/** NAT type detected via STUN. */
export interface NATStatus {
    type: "cone" | "symmetric" | "unknown";
    public_ip?: string;
    public_port?: number;
    checked_at: string;
}

/** P2P feasibility assessment. */
export interface P2PStatus {
    supported: boolean;
    reason: string;
}

/** Relay fallback status. */
export interface RelayStatus {
    available: boolean;
    type: string;
}

/** Recommended connection strategy for the mobile app. */
export type ConnectionStrategy = "ipv6_direct" | "p2p" | "relay";

/** Full network capability report from GET /api/v1/network/status. */
export interface NetworkStatus {
    ipv6: IPv6Status;
    nat: NATStatus;
    p2p: P2PStatus;
    relay: RelayStatus;
    /** Recommended INITIAL connection — always "relay". Connect immediately, then probe `strategy` for upgrade. */
    initial: ConnectionStrategy;
    /** BEST achievable path after probing. Client upgrades from `initial` to this if probe succeeds. */
    strategy: ConnectionStrategy;
    quality: number; // 1-5
    checked_at: string;
}

/** Server's P2P endpoint for UDP hole punching. */
export interface ServerEndpoint {
    public_ip: string;
    public_port: number;
    ipv6?: string;
    nat_type: string;
    strategy: string;
}

/** A registered P2P peer. */
export interface PeerEndpoint {
    peer_id: string;
    public_ip: string;
    public_port: number;
    ipv6?: string;
    registered_at: string;
    expires_at: string;
}

// -------------------- Events (Phase 6) --------------------

/** A stored event from GET /api/v1/events. */
export interface StoredEvent {
    id: number;
    type: string;
    source: string;
    severity: "info" | "warn" | "error" | "critical" | string;
    status?: string;
    payload: Record<string, unknown> | null;
    timestamp: string;
}
