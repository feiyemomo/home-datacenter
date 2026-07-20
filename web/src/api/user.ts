import client from "./client";
import type {
    CreateUserRequest,
    CreateUserResponse,
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
 *
 * When `initial_device_name` is provided in the request, the
 * response includes a `device` object and `access_key` with
 * the plaintext AccessKey (only available at creation time).
 */
export async function createUser(req: CreateUserRequest): Promise<CreateUserResponse> {
    const { data } = await client.post<CreateUserResponse>("/user", req);
    return data;
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
