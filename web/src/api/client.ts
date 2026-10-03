import axios, { AxiosError, type AxiosResponse, type InternalAxiosRequestConfig } from "axios";
import type { ApiEnvelope, BindResponse } from "@/types";

/** localStorage key for the JWT issued by /auth/bind. */
export const TOKEN_KEY = "hd_token";

/** Custom event dispatched when token is updated (via login or sliding refresh). */
export const TOKEN_UPDATED_EVENT = "hd_token_updated";

/**
 * Read the stored JWT, or null if absent.
 *
 * v1.8.41: localStorage access can throw in private-browsing / storage-
 * blocked contexts (Safari private, some WebViews, security policies).
 * A throw here used to crash the axios request interceptor and take
 * down the whole app. All token access now degrades to "no token" —
 * the user simply sees a 401 and is redirected to /login, which is
 * strictly better than a blank screen.
 */
export function getToken(): string | null {
    try {
        // v1.8.45: Cross-origin SSO token handoff (e.g. jumping from dashboard.feiyemomo.top to H3C domestic tunnel)
        if (typeof window !== "undefined") {
            let ssoToken: string | null = null;
            if (window.location.hash && window.location.hash.includes("token=")) {
                const hashContent = window.location.hash.startsWith("#")
                    ? window.location.hash.substring(1)
                    : window.location.hash;
                const params = new URLSearchParams(hashContent);
                ssoToken = params.get("token");
                if (ssoToken) {
                    params.delete("token");
                    const remaining = params.toString();
                    window.history.replaceState(
                        null,
                        "",
                        window.location.pathname + (remaining ? "#" + remaining : "") + window.location.search
                    );
                }
            } else if (window.location.search && window.location.search.includes("token=")) {
                const params = new URLSearchParams(window.location.search);
                ssoToken = params.get("token");
                if (ssoToken) {
                    params.delete("token");
                    const remaining = params.toString();
                    window.history.replaceState(
                        null,
                        "",
                        window.location.pathname + (remaining ? "?" + remaining : "") + window.location.hash
                    );
                }
            }

            if (ssoToken) {
                try {
                    localStorage.setItem(TOKEN_KEY, ssoToken);
                } catch {}
                return ssoToken;
            }
        }
        return localStorage.getItem(TOKEN_KEY);
    } catch {
        return null;
    }
}

/** Persist the JWT and notify active listeners. Best-effort; never throws. */
export function setToken(token: string): void {
    try {
        localStorage.setItem(TOKEN_KEY, token);
    } catch {
        // Storage unavailable — token lives only in memory for this
        // session. The app still works; the user just re-logs in after
        // a reload.
    }
    if (typeof window !== "undefined") {
        window.dispatchEvent(
            new CustomEvent(TOKEN_UPDATED_EVENT, { detail: { token } }),
        );
    }
}

/** Remove the JWT and bounce to /login. */
export async function clearTokenAndRedirect(): Promise<void> {
    // Call server-side logout to clear the HttpOnly cookie.
    // The cookie is HttpOnly so JavaScript cannot delete it directly.
    try {
        await axios.post("/api/v1/auth/logout", null, {
            headers: getToken() ? { Authorization: `Bearer ${getToken()}` } : {},
        });
    } catch {
        // Ignore errors — proceed with local cleanup anyway.
    }
    try {
        localStorage.removeItem(TOKEN_KEY);
    } catch {
        // fall through — cookie cleanup below still runs
    }
    try {
        document.cookie = "home_token=; Max-Age=0; path=/; SameSite=Lax";
    } catch {
        // ignore
    }
    if (window.location.pathname !== "/login") {
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

// ---- Retry policy (v1.8.41) ----------------------------------------
//
// Transient failures — network drops, 502/503/504 from the origin —
// are retried with exponential backoff for IDEMPOTENT methods only
// (GET, HEAD, OPTIONS, PUT, DELETE). POST is never auto-retried: we
// cannot know whether the first attempt was committed server-side
// (e.g. a PTZ command applied but the response lost on the wire), so
// retrying risks applying the side effect twice. Callers that need
// retry semantics for POSTs should implement their own idempotency key.
const MAX_RETRIES = 2;
const RETRY_BASE_MS = 1000;
const RETRY_MAX_MS = 4000;

/** Methods that are safe to retry without risking double side effects. */
function isIdempotent(method: string | undefined): boolean {
    switch ((method ?? "GET").toUpperCase()) {
        case "GET":
        case "HEAD":
        case "OPTIONS":
        case "PUT":
        case "DELETE":
            return true;
        default:
            return false;
    }
}

/** Whether the failure is likely transient and worth a retry. */
function isTransient(status: number): boolean {
    // 0 = network error / timeout (no HTTP response received).
    return status === 0 || status >= 500;
}

/** Custom per-request field so the interceptor can count retries and track refresh attempts. */
declare module "axios" {
    interface InternalAxiosRequestConfig {
        __retryCount?: number;
        __isRetryAfterRefresh?: boolean;
    }
}

function sleepMs(ms: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Shared promise for in-flight token refresh to coalesce concurrent 401s. */
let refreshPromise: Promise<string> | null = null;

// ---- Response interceptor: unwrap envelope, handle 401 with sliding refresh, retry ----
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
    async (error: AxiosError<ApiEnvelope<unknown>>) => {
        const status = error.response?.status ?? 0;
        const message =
            error.response?.data?.message ?? error.message ?? "request failed";

        // Handle 401: attempt single-flight sliding token refresh before redirecting
        if (status === 401) {
            const config = error.config;
            const url = config?.url ?? "";
            const isAuthEndpoint =
                url.includes("/auth/refresh") ||
                url.includes("/auth/bind") ||
                url.includes("/auth/logout");

            // If it's an auth endpoint itself, or no token is stored, or this request
            // was already retried once after a refresh, 401 is genuinely terminal.
            if (!config || isAuthEndpoint || !getToken() || config.__isRetryAfterRefresh) {
                clearTokenAndRedirect();
                return Promise.reject(new ApiError(status, message));
            }

            // Coalesce concurrent 401s into a single /auth/refresh call
            if (!refreshPromise) {
                refreshPromise = (async () => {
                    const currentToken = getToken();
                    if (!currentToken) {
                        throw new Error("No token available to refresh");
                    }
                    // Use a standalone axios call to bypass client interceptors
                    const res = await axios.post<ApiEnvelope<BindResponse>>(
                        "/api/v1/auth/refresh",
                        null,
                        {
                            headers: {
                                Authorization: `Bearer ${currentToken}`,
                            },
                            timeout: 10000,
                        },
                    );
                    if (res.data && res.data.code === 0 && res.data.data?.token) {
                        const newToken = res.data.data.token;
                        setToken(newToken);
                        return newToken;
                    }
                    throw new Error(res.data?.message || "Refresh failed");
                })().finally(() => {
                    refreshPromise = null;
                });
            }

            try {
                const freshToken = await refreshPromise;
                config.__isRetryAfterRefresh = true;
                config.headers.set("Authorization", `Bearer ${freshToken}`);
                return client.request(config);
            } catch {
                clearTokenAndRedirect();
                return Promise.reject(new ApiError(status, message));
            }
        }

        // v1.8.41: idempotent retry with exponential backoff. Only retry
        // idempotent methods on transient failures, and only up to
        // MAX_RETRIES times, so a permanently-dead backend isn't hammered.
        const config = error.config;
        if (config && isIdempotent(config.method) && isTransient(status)) {
            const attempt = config.__retryCount ?? 0;
            if (attempt < MAX_RETRIES) {
                config.__retryCount = attempt + 1;
                const delay = Math.min(RETRY_BASE_MS * 2 ** attempt, RETRY_MAX_MS);
                await sleepMs(delay);
                return client.request(config);
            }
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
