import { useEffect, useState } from "react";
import { Activity, RefreshCw } from "lucide-react";
import { listSystemLogs } from "@/api/system";
import { useAuth } from "@/hooks/useAuth";
import type { SystemLog } from "@/types";
import {
    Card,
    CardContent,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";

/**
 * Logs page — full system log browser.
 *
 * Shows the complete system log with pagination and auto-refresh.
 * Only accessible to admin users.
 */
export default function Logs() {
    const { isAdmin } = useAuth();
    const [logs, setLogs] = useState<SystemLog[]>([]);
    const [total, setTotal] = useState(0);
    const [loading, setLoading] = useState(false);
    const [page, setPage] = useState(0);
    const pageSize = 50;

    const loadLogs = (offset: number) => {
        setLoading(true);
        listSystemLogs(pageSize, offset)
            .then((res) => {
                setLogs(res.logs ?? []);
                setTotal(res.total);
            })
            .catch(() => {})
            .finally(() => setLoading(false));
    };

    useEffect(() => {
        loadLogs(page * pageSize);
    }, [page]);

    if (!isAdmin) {
        return (
            <div className="space-y-6">
                <div className="animate-fade-in glass-subtle relative overflow-hidden rounded-2xl px-5 py-4">
                    <h2 className="text-lg font-semibold tracking-tight text-fg">系统日志</h2>
                    <p className="mt-0.5 text-xs text-fg-muted">无权限查看系统日志。</p>
                </div>
            </div>
        );
    }

    const totalPages = Math.ceil(total / pageSize);

    return (
        <div className="space-y-6">
            {/* Page header */}
            <div className="animate-fade-in glass-subtle relative overflow-hidden rounded-2xl px-5 py-4">
                <div className="pointer-events-none absolute inset-y-0 left-0 w-1 bg-gradient-to-b from-[rgb(var(--accent-warm)/0.8)] via-[rgb(var(--accent-primary)/0.5)] to-transparent" />
                <div className="relative flex items-center justify-between">
                    <div>
                        <h2 className="text-lg font-semibold tracking-tight text-fg">系统日志</h2>
                        <p className="mt-0.5 text-xs text-fg-muted">
                            共 {total} 条日志记录。
                        </p>
                    </div>
                    <button
                        type="button"
                        onClick={() => loadLogs(page * pageSize)}
                        disabled={loading}
                        className="inline-flex items-center gap-1 text-xs text-fg-muted transition-colors hover:text-fg disabled:opacity-50"
                    >
                        <RefreshCw size={12} className={loading ? "animate-spin" : ""} />
                        刷新
                    </button>
                </div>
            </div>

            {/* Log list */}
            <Card className="animate-fade-in">
                <CardHeader className="flex-row items-center justify-between pb-2">
                    <CardTitle className="flex items-center gap-2 text-xs tracking-wider text-fg-muted">
                        <Activity size={16} /> 日志记录
                    </CardTitle>
                    <Badge variant="outline" className="text-[10px]">
                        {total} 条
                    </Badge>
                </CardHeader>
                <CardContent>
                    {loading && logs.length === 0 ? (
                        <div className="flex items-center justify-center py-6 text-xs text-fg-muted">
                            <RefreshCw size={12} className="mr-1.5 animate-spin" />
                            加载中…
                        </div>
                    ) : logs.length === 0 ? (
                        <div className="py-6 text-center text-xs text-fg-subtle">暂无日志</div>
                    ) : (
                        <ul className="space-y-1.5">
                            {logs.map((log) => (
                                <li
                                    key={log.id}
                                    className="glass-subtle flex items-start gap-2 rounded-xl px-3 py-2 text-xs"
                                >
                                    <span className="shrink-0 text-fg-subtle" title={new Date(log.ts * 1000).toLocaleString()}>
                                        {new Date(log.ts * 1000).toLocaleTimeString()}
                                    </span>
                                    <Badge
                                        variant={
                                            log.level === "critical" ? "danger"
                                                : log.level === "normal" ? "info"
                                                    : "outline"
                                        }
                                        className="text-[9px] shrink-0"
                                    >
                                        {log.level === "critical" ? "严重"
                                            : log.level === "normal" ? "普通"
                                                : "信息"}
                                    </Badge>
                                    <span className="min-w-0 flex-1 truncate text-fg-muted">
                                        {log.message || log.event_type}
                                    </span>
                                </li>
                            ))}
                        </ul>
                    )}

                    {/* Pagination */}
                    {totalPages > 1 && (
                        <div className="mt-4 flex items-center justify-center gap-2">
                            <button
                                type="button"
                                onClick={() => setPage(Math.max(0, page - 1))}
                                disabled={page === 0}
                                className="rounded-lg px-3 py-1 text-xs text-fg-muted transition-colors hover:bg-[rgb(var(--bg-subtle)/0.3)] hover:text-fg disabled:opacity-30"
                            >
                                上一页
                            </button>
                            <span className="text-xs text-fg-muted">
                                {page + 1} / {totalPages}
                            </span>
                            <button
                                type="button"
                                onClick={() => setPage(Math.min(totalPages - 1, page + 1))}
                                disabled={page >= totalPages - 1}
                                className="rounded-lg px-3 py-1 text-xs text-fg-muted transition-colors hover:bg-[rgb(var(--bg-subtle)/0.3)] hover:text-fg disabled:opacity-30"
                            >
                                下一页
                            </button>
                        </div>
                    )}
                </CardContent>
            </Card>
        </div>
    );
}