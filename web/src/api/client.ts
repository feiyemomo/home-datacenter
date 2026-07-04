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

export default client;
