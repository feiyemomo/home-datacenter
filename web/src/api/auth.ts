import client from "./client";
import type { BindResponse } from "@/types";

/**
 * Exchange (user_id, access_key) for a long-lived JWT.
 * The token is NOT stored here; the caller (AuthContext) owns
 * persistence so it can also decode the claims.
 *
 * POST /api/v1/auth/bind
 */
export async function bind(
    userId: number,
    accessKey: string,
): Promise<string> {
    const { data } = await client.post<BindResponse>("/auth/bind", {
        user_id: userId,
        access_key: accessKey,
    });
    // Response interceptor already unwrapped `data` from the envelope.
    return data.token;
}
