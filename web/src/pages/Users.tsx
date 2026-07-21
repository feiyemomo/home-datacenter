import { useCallback, useEffect, useState } from "react";
import { Plus, RefreshCw, Trash2, Loader2, UserCog, ShieldAlert, Copy, Check, Key } from "lucide-react";
import { createUser, deleteUser, listUsers, updateUser } from "@/api/user";
import { ApiError } from "@/api/client";
import { useAuth } from "@/hooks/useAuth";
import { cn, formatDateTime } from "@/lib/utils";
import type { UserListEntry, CreateUserResponse } from "@/types";
import {
    Card,
    CardContent,
    CardDescription,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";

/**
 * Users page (admin-only).
 *
 * Renders a CRUD grid for /api/v1/user:
 *   - GET    /user              -> list rows + device_count
 *   - POST   /user              -> create (name, is_admin)
 *   - PUT    /user/:id          -> rename / promote / demote
 *   - DELETE /user/:id          -> cascade-delete user + their devices
 *
 * The /me endpoint is the source of truth for the currently
 * authenticated user; we use it to know who "me" is in the row
 * actions (self row gets a "view-only" treatment) and to enforce
 * the last-admin / self-delete guards with the same UX the
 * backend's HTTP layer already returns (we just disable the
 * buttons that would be rejected server-side).
 */
export default function Users() {
    const auth = useAuth();
    const me = auth.user;

    const [users, setUsers] = useState<UserListEntry[]>([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState<string | null>(null);

    // Create form state
    const [showCreate, setShowCreate] = useState(false);
    const [newName, setNewName] = useState("");
    const [newIsAdmin, setNewIsAdmin] = useState(false);
    const [newDeviceName, setNewDeviceName] = useState("");
    const [creating, setCreating] = useState(false);
    const [createResult, setCreateResult] = useState<CreateUserResponse | null>(null);
    const [copied, setCopied] = useState(false);

    // Per-row edit state. The map key is the user id; the value
    // is the staged name input. is_admin flips immediately on
    // toggle (no input) and is sent on save.
    const [editing, setEditing] = useState<Record<number, string>>({});
    const [savingId, setSavingId] = useState<number | null>(null);
    const [deletingId, setDeletingId] = useState<number | null>(null);
    const [confirmDeleteId, setConfirmDeleteId] = useState<number | null>(null);

    const adminCount = users.filter((u) => u.is_admin).length;

    const refresh = useCallback(async () => {
        setError(null);
        try {
            const rows = await listUsers();
            setUsers(rows);
        } catch (err) {
            setError(
                err instanceof ApiError
                    ? err.message
                    : err instanceof Error
                        ? err.message
                        : "加载用户失败",
            );
        } finally {
            setLoading(false);
        }
    }, []);

    useEffect(() => {
        refresh();
    }, [refresh]);

    async function handleCreate() {
        const name = newName.trim();
        if (!name) {
            setError("请输入名称");
            return;
        }
        const deviceName = newDeviceName.trim();
        setCreating(true);
        setError(null);
        try {
            const result = await createUser({
                name,
                is_admin: newIsAdmin,
                initial_device_name: deviceName || undefined,
            });
            setNewName("");
            setNewIsAdmin(false);
            setNewDeviceName("");
            setShowCreate(false);
            setCreateResult(result);
            await refresh();
        } catch (err) {
            setError(
                err instanceof ApiError
                    ? err.message
                    : err instanceof Error
                        ? err.message
                        : "创建失败",
            );
        } finally {
            setCreating(false);
        }
    }

    function handleCopyAccessKey() {
        if (!createResult?.access_key) return;
        navigator.clipboard.writeText(createResult.access_key).then(() => {
            setCopied(true);
            window.setTimeout(() => setCopied(false), 2000);
        });
    }

    async function handleSave(u: UserListEntry) {
        const draft = editing[u.id];
        const trimmed = (draft ?? "").trim();
        const nameChanged = trimmed && trimmed !== u.name;
        if (!nameChanged) {
            setEditing((prev) => {
                const { [u.id]: _drop, ...rest } = prev;
                return rest;
            });
            return;
        }
        setSavingId(u.id);
        setError(null);
        try {
            await updateUser(u.id, { name: trimmed });
            setEditing((prev) => {
                const { [u.id]: _drop, ...rest } = prev;
                return rest;
            });
            await refresh();
        } catch (err) {
            setError(
                err instanceof ApiError
                    ? err.message
                    : err instanceof Error
                        ? err.message
                        : "update failed",
            );
        } finally {
            setSavingId(null);
        }
    }

    async function handleToggleAdmin(u: UserListEntry) {
        // Optimistic UI: flip the local flag immediately so the
        // checkbox feels instant. We refresh on success; on
        // failure we re-fetch to revert.
        const nextIsAdmin = !u.is_admin;
        // Backend will reject the demotion if it would leave
        // zero admins. We mirror the guard client-side so the
        // operator sees the error inline instead of having to
        // round-trip.
        if (u.is_admin && !nextIsAdmin && adminCount <= 1) {
            setError("无法降级最后一位管理员");
            return;
        }
        if (u.id === me?.id && u.is_admin && !nextIsAdmin) {
            setError("无法降级当前登录用户");
            return;
        }
        setSavingId(u.id);
        setError(null);
        try {
            await updateUser(u.id, { is_admin: nextIsAdmin });
            await refresh();
        } catch (err) {
            setError(
                err instanceof ApiError
                    ? err.message
                    : err instanceof Error
                        ? err.message
                        : "更新失败",
            );
        } finally {
            setSavingId(null);
        }
    }

    async function handleDelete(u: UserListEntry) {
        setDeletingId(u.id);
        setError(null);
        try {
            await deleteUser(u.id);
            setConfirmDeleteId(null);
            await refresh();
        } catch (err) {
            setError(
                err instanceof ApiError
                    ? err.message
                    : err instanceof Error
                        ? err.message
                        : "删除失败",
            );
        } finally {
            setDeletingId(null);
        }
    }

    return (
        <div className="animate-fade-in space-y-6">
            <div className="flex flex-wrap items-center justify-between gap-3">
                <div>
                    <h2 className="text-lg font-semibold text-fg">用户</h2>
                    <p className="text-xs text-fg-muted">
                        仅管理员可见。共 {users.length} 位用户 · {adminCount} 位管理员。
                    </p>
                </div>
                <div className="flex items-center gap-2">
                    <Button
                        variant="outline"
                        size="sm"
                        onClick={refresh}
                        disabled={loading}
                    >
                        {loading ? (
                            <Loader2 size={14} className="animate-spin" />
                        ) : (
                            <RefreshCw size={14} />
                        )}
                        刷新
                    </Button>
                    <Button size="sm" onClick={() => setShowCreate((s) => !s)}>
                        <Plus size={14} />
                        {showCreate ? "取消" : "新建用户"}
                    </Button>
                </div>
            </div>

            {error && (
                <div className="flex items-start gap-2 rounded-xl glass bg-[rgb(var(--accent-danger)/0.1)] px-4 py-3 text-xs text-[rgb(var(--accent-danger))]">
                    <ShieldAlert size={16} className="mt-0.5 shrink-0" />
                    <span>{error}</span>
                </div>
            )}

            {showCreate && (
                <Card>
                    <CardHeader>
                        <CardTitle className="text-base">创建用户</CardTitle>
                        <CardDescription>
                            名称为 1-32 个字符，可包含字母、数字、_ 或 -。可选择同时创建首个设备 —— 其访问密钥将在创建后仅显示一次。
                        </CardDescription>
                    </CardHeader>
                    <CardContent>
                        <form
                            className="grid grid-cols-1 gap-3 md:grid-cols-[1fr_1fr_auto_auto]"
                            onSubmit={(e) => {
                                e.preventDefault();
                                handleCreate();
                            }}
                        >
                            <Input
                                placeholder="名称（例如 alice）"
                                value={newName}
                                onChange={(e) => setNewName(e.target.value)}
                                maxLength={32}
                                autoFocus
                            />
                            <Input
                                placeholder="设备名称（可选，例如 alice-laptop）"
                                value={newDeviceName}
                                onChange={(e) => setNewDeviceName(e.target.value)}
                                maxLength={64}
                            />
                            <label className="flex items-center gap-2 text-sm text-fg-muted">
                                <input
                                    type="checkbox"
                                    checked={newIsAdmin}
                                    onChange={(e) => setNewIsAdmin(e.target.checked)}
                                    className="h-4 w-4 rounded border-[rgb(var(--border))] bg-[rgb(var(--glass-base))] text-[rgb(var(--accent-info))] focus:ring-[rgb(var(--accent-info))]"
                                />
                                管理员
                            </label>
                            <Button type="submit" disabled={creating || !newName.trim()}>
                                {creating ? (
                                    <Loader2 size={14} className="animate-spin" />
                                ) : (
                                    <Plus size={14} />
                                )}
                                创建
                            </Button>
                        </form>
                    </CardContent>
                </Card>
            )}

            {/* Access Key reveal modal — shown once after successful
                user creation when initial_device_name was provided. */}
            {createResult && createResult.access_key && (
                <Card className="border-[rgb(var(--accent-warm)/0.4)] bg-[rgb(var(--accent-warm)/0.08)]">
                    <CardHeader>
                        <CardTitle className="flex items-center gap-2 text-base text-[rgb(var(--accent-warm))]">
                            <Key size={16} />
                            用户已创建 —— 请保存访问密钥
                        </CardTitle>
                        <CardDescription>
                            访问密钥仅此一次显示。请复制并发送给 <strong>{createResult.name}</strong>
                            {createResult.device ? `（设备：${createResult.device.device_name}）` : ""}。之后无法再次恢复。
                        </CardDescription>
                    </CardHeader>
                    <CardContent className="space-y-3">
                        <div className="flex items-center gap-2">
                            <code className="flex-1 truncate rounded-lg glass-subtle px-3 py-2 font-mono text-sm text-fg">
                                {createResult.access_key}
                            </code>
                            <Button
                                size="sm"
                                variant="outline"
                                onClick={handleCopyAccessKey}
                                className="shrink-0"
                            >
                                {copied ? (
                                    <>
                                        <Check size={14} className="mr-1 text-[rgb(var(--accent-success))]" />
                                        已复制
                                    </>
                                ) : (
                                    <>
                                        <Copy size={14} className="mr-1" />
                                        复制
                                    </>
                                )}
                            </Button>
                        </div>
                        <div className="flex justify-end">
                            <Button
                                size="sm"
                                variant="ghost"
                                onClick={() => setCreateResult(null)}
                            >
                                关闭
                            </Button>
                        </div>
                    </CardContent>
                </Card>
            )}

            <Card>
                <CardHeader>
                    <CardTitle className="flex items-center gap-2">
                        <UserCog size={16} /> 用户列表
                    </CardTitle>
                    <CardDescription>
                        删除用户将级联删除其所有设备 —— 那些设备的 JWT 令牌将立即失效。
                    </CardDescription>
                </CardHeader>
                <CardContent className="p-0">
                    <div className="overflow-x-auto">
                        <table className="w-full text-left text-sm">
                            <thead className="glass-subtle border-b border-[rgb(var(--border)/0.3)] text-xs uppercase tracking-wider text-fg-muted">
                                <tr>
                                    <th className="px-4 py-3 font-medium">名称</th>
                                    <th className="px-4 py-3 font-medium">角色</th>
                                    <th className="px-4 py-3 font-medium">设备</th>
                                    <th className="px-4 py-3 font-medium">创建于</th>
                                    <th className="px-4 py-3 text-right font-medium">操作</th>
                                </tr>
                            </thead>
                            <tbody className="divide-[rgb(var(--border)/0.3)]">
                                {users.length === 0 && !loading && (
                                    <tr>
                                        <td
                                            colSpan={5}
                                            className="px-4 py-10 text-center text-fg-muted"
                                        >
                                            未找到用户。
                                        </td>
                                    </tr>
                                )}
                                {users.map((u) => {
                                    const isMe = me?.id === u.id;
                                    const isEditing = editing[u.id] !== undefined;
                                    const isOnlyAdmin = u.is_admin && adminCount <= 1;
                                    const isBusy = savingId === u.id || deletingId === u.id;
                                    return (
                                        <tr
                                            key={u.id}
                                            className={cn(
                                                "transition-colors hover:bg-[rgb(var(--bg-subtle)/0.3)]",
                                                isMe && "bg-[rgb(var(--accent-primary)/0.05)]",
                                            )}
                                        >
                                            <td className="px-4 py-3">
                                                <div className="flex items-center gap-3">
                                                    <div
                                                        className={cn(
                                                            "flex h-8 w-8 items-center justify-center rounded-xl text-sm font-semibold",
                                                            u.is_admin
                                                                ? "bg-gradient-to-br from-[rgb(var(--accent-warm)/0.3)] to-[rgb(var(--accent-warm)/0.1)] text-[rgb(var(--accent-warm))]"
                                                                : "bg-gradient-to-br from-[rgb(var(--accent-primary)/0.3)] to-[rgb(var(--accent-warm)/0.2)] text-[rgb(var(--accent-info))]",
                                                        )}
                                                    >
                                                        {u.name.slice(0, 1).toUpperCase()}
                                                    </div>
                                                    <div className="flex-1">
                                                        {isEditing ? (
                                                            <Input
                                                                value={editing[u.id]}
                                                                onChange={(e) =>
                                                                    setEditing((prev) => ({
                                                                        ...prev,
                                                                        [u.id]: e.target.value,
                                                                    }))
                                                                }
                                                                maxLength={32}
                                                                className="h-7 max-w-[180px] text-sm"
                                                                autoFocus
                                                                onKeyDown={(e) => {
                                                                    if (e.key === "Enter") {
                                                                        e.preventDefault();
                                                                        handleSave(u);
                                                                    } else if (e.key === "Escape") {
                                                                        setEditing((prev) => {
                                                                            const { [u.id]: _d, ...rest } = prev;
                                                                            return rest;
                                                                        });
                                                                    }
                                                                }}
                                                            />
                                                        ) : (
                                                            <div className="font-medium text-fg">
                                                                {u.name}
                                                            </div>
                                                        )}
                                                        <div className="text-[11px] text-fg-muted">
                                                            id #{u.id}
                                                            {isMe && (
                                                                <Badge
                                                                    variant="info"
                                                                    className="ml-2 text-[9px]"
                                                                >
                                                                    你
                                                                </Badge>
                                                            )}
                                                        </div>
                                                    </div>
                                                </div>
                                            </td>
                                            <td className="px-4 py-3">
                                                <label
                                                    className={cn(
                                                        "inline-flex cursor-pointer items-center gap-2 rounded-lg px-2 py-1 text-xs transition-colors",
                                                        isOnlyAdmin || isMe
                                                            ? "cursor-not-allowed opacity-60"
                                                            : "hover:bg-[rgb(var(--bg-subtle)/0.3)]",
                                                    )}
                                                    title={
                                                        isOnlyAdmin
                                                            ? "无法降级唯一管理员"
                                                            : isMe
                                                                ? "无法降级自己"
                                                                : u.is_admin
                                                                    ? "降级为普通用户"
                                                                    : "提升为管理员"
                                                    }
                                                >
                                                    <input
                                                        type="checkbox"
                                                        checked={u.is_admin}
                                                        disabled={
                                                            isBusy ||
                                                            isOnlyAdmin ||
                                                            isMe
                                                        }
                                                        onChange={() => handleToggleAdmin(u)}
                                                        className="h-3.5 w-3.5 rounded border-[rgb(var(--border))] bg-[rgb(var(--glass-base))] text-[rgb(var(--accent-info))] focus:ring-[rgb(var(--accent-info))] disabled:cursor-not-allowed"
                                                    />
                                                    <span className="text-fg-muted">
                                                        {u.is_admin ? "管理员" : "用户"}
                                                    </span>
                                                </label>
                                            </td>
                                            <td className="px-4 py-3 text-fg-muted">
                                                {u.device_count}
                                            </td>
                                            <td className="px-4 py-3 text-fg-muted">
                                                {formatDateTime(u.created_at)}
                                            </td>
                                            <td className="px-4 py-3 text-right">
                                                {isEditing ? (
                                                    <div className="flex items-center justify-end gap-2">
                                                        <Button
                                                            variant="ghost"
                                                            size="sm"
                                                            onClick={() =>
                                                                setEditing((prev) => {
                                                                    const { [u.id]: _d, ...rest } = prev;
                                                                    return rest;
                                                                })
                                                            }
                                                            disabled={isBusy}
                                                        >
                                                            取消
                                                        </Button>
                                                        <Button
                                                            variant="primary"
                                                            size="sm"
                                                            onClick={() => handleSave(u)}
                                                            disabled={isBusy}
                                                        >
                                                            {savingId === u.id ? (
                                                                <Loader2
                                                                    size={14}
                                                                    className="animate-spin"
                                                                />
                                                            ) : null}
                                                            保存
                                                        </Button>
                                                    </div>
                                                ) : confirmDeleteId === u.id ? (
                                                    <div className="flex items-center justify-end gap-2">
                                                        <Button
                                                            variant="ghost"
                                                            size="sm"
                                                            onClick={() => setConfirmDeleteId(null)}
                                                            disabled={isBusy}
                                                        >
                                                            取消
                                                        </Button>
                                                        <Button
                                                            variant="danger"
                                                            size="sm"
                                                            onClick={() => handleDelete(u)}
                                                            disabled={isBusy || isMe}
                                                            title={
                                                                isMe
                                                                    ? "无法删除自己"
                                                                    : isOnlyAdmin
                                                                        ? "无法删除唯一管理员"
                                                                        : "删除用户并级联删除设备"
                                                            }
                                                        >
                                                            {deletingId === u.id ? (
                                                                <Loader2
                                                                    size={14}
                                                                    className="animate-spin"
                                                                />
                                                            ) : (
                                                                <Trash2 size={14} />
                                                            )}
                                                            确认
                                                        </Button>
                                                    </div>
                                                ) : (
                                                    <div className="flex items-center justify-end gap-1">
                                                        <Button
                                                            variant="ghost"
                                                            size="sm"
                                                            onClick={() =>
                                                                setEditing((prev) => ({
                                                                    ...prev,
                                                                    [u.id]: u.name,
                                                                }))
                                                            }
                                                            disabled={isBusy}
                                                        >
                                                            重命名
                                                        </Button>
                                                        <Button
                                                            variant="ghost"
                                                            size="sm"
                                                            className="text-fg-muted hover:text-[rgb(var(--accent-danger))]"
                                                            onClick={() => setConfirmDeleteId(u.id)}
                                                            disabled={isMe || isOnlyAdmin}
                                                            title={
                                                                isMe
                                                                    ? "无法删除自己"
                                                                    : isOnlyAdmin
                                                                        ? "无法删除唯一管理员"
                                                                        : "删除用户并级联删除设备"
                                                            }
                                                        >
                                                            <Trash2 size={14} />
                                                            删除
                                                        </Button>
                                                    </div>
                                                )}
                                            </td>
                                        </tr>
                                    );
                                })}
                            </tbody>
                        </table>
                    </div>
                </CardContent>
            </Card>
        </div>
    );
}