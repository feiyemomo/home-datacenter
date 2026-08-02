import React from "react";
import {
    Card,
    CardContent,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";

export interface StatCardProps {
    label: string;
    value: string;
    icon: React.ReactNode;
    accent: "sky" | "emerald" | "amber" | "violet";
    hint?: React.ReactNode;
}

const accentClasses: Record<StatCardProps["accent"], { gradient: string; iconBg: string; text: string }> = {
    sky: {
        gradient: "from-[rgb(var(--accent-info)/0.15)] to-[rgb(var(--accent-info)/0)]",
        iconBg: "bg-[rgb(var(--accent-info)/0.15)] ring-[rgb(var(--accent-info)/0.3)] text-[rgb(var(--accent-info))]",
        text: "text-[rgb(var(--accent-info))]",
    },
    emerald: {
        gradient: "from-[rgb(var(--accent-success)/0.15)] to-[rgb(var(--accent-success)/0)]",
        iconBg: "bg-[rgb(var(--accent-success)/0.15)] ring-[rgb(var(--accent-success)/0.3)] text-[rgb(var(--accent-success))]",
        text: "text-[rgb(var(--accent-success))]",
    },
    amber: {
        gradient: "from-[rgb(var(--accent-warm)/0.15)] to-[rgb(var(--accent-warm)/0)]",
        iconBg: "bg-[rgb(var(--accent-warm)/0.15)] ring-[rgb(var(--accent-warm)/0.3)] text-[rgb(var(--accent-warm))]",
        text: "text-[rgb(var(--accent-warm))]",
    },
    violet: {
        gradient: "from-[rgb(var(--accent-primary)/0.15)] to-[rgb(var(--accent-warm)/0)]",
        iconBg: "bg-[rgb(var(--accent-primary)/0.15)] ring-[rgb(var(--accent-primary)/0.3)] text-[rgb(var(--accent-primary))]",
        text: "text-[rgb(var(--accent-primary))]",
    },
};

function StatCardInner({ label, value, icon, accent, hint }: StatCardProps) {
    return (
        <Card className="relative overflow-hidden">
            <div
                className={`pointer-events-none absolute inset-0 bg-gradient-to-br ${accentClasses[accent].gradient}`}
            />
            <CardHeader className="relative flex-row items-center justify-between pb-2">
                <CardTitle className="text-xs tracking-wider text-fg-muted">
                    {label}
                </CardTitle>
                <div
                    className={`flex h-9 w-9 items-center justify-center rounded-xl ring-1 ring-inset ${accentClasses[accent].iconBg}`}
                >
                    {icon}
                </div>
            </CardHeader>
            <CardContent className="relative">
                <div className="text-3xl font-semibold tracking-tight text-fg">
                    {value}
                </div>
                {hint && <div className="mt-2 text-xs text-fg-muted">{hint}</div>}
            </CardContent>
        </Card>
    );
}

export const StatCard = React.memo(StatCardInner);