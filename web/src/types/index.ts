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
