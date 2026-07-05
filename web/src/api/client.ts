import axios, { AxiosError, type AxiosResponse, type InternalAxiosRequestConfig } from "axios";
import type { ApiEnvelope } from "@/types";

/** localStorage key for the JWT issued by /auth/bind. */
export const TOKEN_KEY = "hd_token";

/** Read the stored JWT, or null if absent. */
export function getToken(): string | null {
    return localStorage.getItem(TOKEN_KEY);
}

/** Persist the JWT. */
export function setToken(token: string): void {
    localStorage.setItem(TOKEN_KEY, token);
}

/** Remove the JWT and bounce to /login. */
export function clearTokenAndRedirect(): void {
    localStorage.removeItem(TOKEN_KEY);
    // Avoid clobbering history when already on a public route.
    if (!window.location.pathname.startsWith("/login")) {
        window.location.assign("/login");
    }
}

/**
 * Pre-configured axios instance.
 *
 * - Base URL: /api/v1 (Vite proxy in dev, nginx in prod)
 * - Request interceptor: attach Authorization: Bearer <token>
 * - Response interceptor: unwrap `response.data.data`
 *   and surface API errors; on 401, clear token + redirect.
 */
const client = axios.create({
    baseURL: "/api/v1",
    timeout: 15000,
    headers: { "Content-Type": "application/json" },
});

// ---- Request interceptor: attach JWT ----
client.interceptors.request.use(
    (config: InternalAxiosRequestConfig) => {
        const token = getToken();
        if (token) {
            config.headers.set("Authorization", `Bearer ${token}`);
        }
        return config;
    },
    (error) => Promise.reject(error),
);

// ---- Response interceptor: unwrap envelope, handle 401 ----
client.interceptors.response.use(
    (response: AxiosResponse<ApiEnvelope<unknown>>) => {
        const envelope = response.data;
        // Some endpoints (none in our API today) might return non-envelope
        // payloads; guard against that.
        if (envelope && typeof envelope === "object" && "code" in envelope) {
            if (envelope.code !== 0) {
                // Business error surfaced with HTTP 200 (shouldn't happen with
                // the current backend, but be defensive).
                return Promise.reject(new ApiError(envelope.code, envelope.message));
            }
            // Return the unwrapped `data` field as the resolved value.
            return { ...response, data: envelope.data } as AxiosResponse;
        }
        return response;
    },
    (error: AxiosError<ApiEnvelope<unknown>>) => {
        const status = error.response?.status ?? 0;
        const message =
            error.response?.data?.message ?? error.message ?? "request failed";

        if (status === 401) {
            clearTokenAndRedirect();
        }

        return Promise.reject(new ApiError(status, message));
    },
);

/** Error thrown by the client for any API-level failure. */
export class ApiError extends Error {
    code: number;
    constructor(code: number, message: string) {
        super(message);
        this.name = "ApiError";
        this.code = code;
    }
}

/**
 * authedFetch — a thin wrapper over `fetch` that attaches the
 * dashboard's JWT to the Authorization header. Use this for any
 * request that goes through nginx's `/go2rtc/` location, which is
 * gated by `auth_request` against /api/v1/auth/verify (see
 * web/nginx.conf). Without the header, the request returns 401
 * from nginx and never reaches go2rtc.
 *
 * Plain `axios` calls do NOT need this — they already attach
 * Authorization via the request interceptor above. This helper
 * exists for the two paths that bypass axios:
 *
 *   - useWebRTCStream.ts: fetch(...) for the SDP POST (binary-ish
 *     SDP body, no JSON envelope, so axios is overkill).
 *   - useHLSStream.ts:    hls.js can be configured to use
 *     `xhrSetup` to add a header on its internal XHRs, but the
 *     simpler/more reliable path is to override the loader via
 *     `Hls.DefaultConfig.loader`. We use xhrSetup (per-stream
 *     instance) because hooking the loader globally also affects
 *     segments in ways that complicate cleanup.
 */
export function authedFetch(input: RequestInfo | URL, init: RequestInit = {}): Promise<Response> {
    const token = getToken();
    const headers = new Headers(init.headers);
    if (token) {
        headers.set("Authorization", `Bearer ${token}`);
    }
    return fetch(input, { ...init, headers });
}

/**
 * authHeaderFor — return the literal "Authorization: Bearer …"
 * header value for the current session, or null if no token is
 * stored. Useful when the caller needs to attach the header to a
 * non-fetch transport (e.g. hls.js's `xhrSetup`).
 */
export function authHeaderFor(): { name: string; value: string } | null {
    const token = getToken();
    if (!token) return null;
    return { name: "Authorization", value: `Bearer ${token}` };
}

export default client;
