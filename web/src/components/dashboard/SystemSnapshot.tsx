import { useState } from "react";
import { Code2, ChevronDown } from "lucide-react";
import {
    Card,
    CardContent,
    CardDescription,
    CardTitle,
} from "@/components/ui/card";
import type { SystemStatus } from "@/types";

interface SystemSnapshotProps {
    status: SystemStatus | null;
}

export default function SystemSnapshot({ status }: SystemSnapshotProps) {
    const [snapshotOpen, setSnapshotOpen] = useState(false);

    return (
        <Card className="animate-fade-in">
            <button
                type="button"
                onClick={() => setSnapshotOpen((v) => !v)}
                className="flex w-full items-center justify-between gap-3 p-5 text-left transition-colors hover:bg-[rgb(var(--bg-subtle)/0.2)]"
                aria-expanded={snapshotOpen}
            >
                <div className="flex items-center gap-2">
                    <Code2 size={16} className="text-fg-muted" />
                    <div>
                        <CardTitle className="text-sm">系统快照</CardTitle>
                        <CardDescription className="mt-0.5">
                            来自 <code className="font-mono">/api/v1/system/status</code> 的原始数据。
                        </CardDescription>
                    </div>
                </div>
                <ChevronDown
                    size={16}
                    className={`shrink-0 text-fg-subtle transition-transform duration-300 ${snapshotOpen ? "rotate-180" : ""}`}
                />
            </button>
            {snapshotOpen && (
                <CardContent className="animate-fade-in">
                    <pre className="glass-subtle overflow-x-auto rounded-2xl p-4 text-xs leading-relaxed text-fg">
                        {status ? JSON.stringify(status, null, 2) : "// 暂无数据"}
                    </pre>
                </CardContent>
            )}
        </Card>
    );
}