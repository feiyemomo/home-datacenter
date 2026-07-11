import client from "./client";
import type {
    CreateUserRequest,
    UpdateUserRequest,
    User,
    UserDeleteResponse,
    UserListResponse,
} from "@/types";

/**
 * List all users. Admin-only.
 *
 * GET /api/v1/user
 */
export async function listUsers(): Promise<UserListResponse["users"]> {
    const { data } = await client.get<UserListResponse>("/user");
    return data?.users ?? [];
}

/**
 * Create a new user. Admin-only.
 *
 * POST /api/v1/user
 */
export async function createUser(req: CreateUserRequest): Promise<User> {
    const { data } = await client.post<{
        id: number;
        name: string;
        is_admin: boolean;
    }>("/user", req);
    // The handler returns the user fields at the top level of
    // the data envelope, not as a `{user: ...}` wrapper. Unwrap
    // manually so callers receive a User shape.
    return {
        id: data.id,
        name: data.name,
        is_admin: data.is_admin,
    };
}

/**
 * Update an existing user. Both fields are optional. Admin-only.
 *
 * PUT /api/v1/user/:id
 */
export async function updateUser(
    id: number,
    req: UpdateUserRequest,
): Promise<User> {
    const { data } = await client.put<{
        id: number;
        name: string;
        is_admin: boolean;
    }>(`/user/${id}`, req);
    return {
        id: data.id,
        name: data.name,
        is_admin: data.is_admin,
    };
}

/**
 * Delete a user. Cascades to their devices. Admin-only.
 *
 * DELETE /api/v1/user/:id
 */
export async function deleteUser(id: number): Promise<number> {
    const { data } = await client.delete<UserDeleteResponse>(`/user/${id}`);
    return data?.deleted_devices ?? 0;
}
