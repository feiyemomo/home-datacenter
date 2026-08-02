import React, { useEffect, useState } from "react";
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

const accentClasses: Record<StatCardProps["accent"], { gradient: string; iconBg: string; glow: string; bar: string }> = {
    sky: {
        gradient: "from-[rgb(var(--accent-info)/0.12)] via-[rgb(var(--accent-info)/0.04)] to-transparent",
        iconBg: "bg-gradient-to-br from-[rgb(var(--accent-info)/0.2)] to-[rgb(var(--accent-info)/0.08)] ring-[rgb(var(--accent-info)/0.25)] text-[rgb(var(--accent-info))] shadow-[0_4px_12px_rgb(var(--accent-info)/0.15)]",
        glow: "shadow-[0_8px_32px_rgb(var(--accent-info)/0.1)]",
        bar: "from-[rgb(var(--accent-info))] to-[rgb(var(--accent-info)/0.6)]",
    },
    emerald: {
        gradient: "from-[rgb(var(--accent-success)/0.12)] via-[rgb(var(--accent-success)/0.04)] to-transparent",
        iconBg: "bg-gradient-to-br from-[rgb(var(--accent-success)/0.2)] to-[rgb(var(--accent-success)/0.08)] ring-[rgb(var(--accent-success)/0.25)] text-[rgb(var(--accent-success))] shadow-[0_4px_12px_rgb(var(--accent-success)/0.15)]",
        glow: "shadow-[0_8px_32px_rgb(var(--accent-success)/0.1)]",
        bar: "from-[rgb(var(--accent-success))] to-[rgb(var(--accent-success)/0.6)]",
    },
    amber: {
        gradient: "from-[rgb(var(--accent-warm)/0.12)] via-[rgb(var(--accent-warm)/0.04)] to-transparent",
        iconBg: "bg-gradient-to-br from-[rgb(var(--accent-warm)/0.2)] to-[rgb(var(--accent-warm)/0.08)] ring-[rgb(var(--accent-warm)/0.25)] text-[rgb(var(--accent-warm))] shadow-[0_4px_12px_rgb(var(--accent-warm)/0.15)]",
        glow: "shadow-[0_8px_32px_rgb(var(--accent-warm)/0.1)]",
        bar: "from-[rgb(var(--accent-warm))] to-[rgb(var(--accent-warm)/0.6)]",
    },
    violet: {
        gradient: "from-[rgb(var(--accent-primary)/0.12)] via-[rgb(var(--accent-warm)/0.04)] to-transparent",
        iconBg: "bg-gradient-to-br from-[rgb(var(--accent-primary)/0.2)] to-[rgb(var(--accent-primary)/0.08)] ring-[rgb(var(--accent-primary)/0.25)] text-[rgb(var(--accent-primary))] shadow-[0_4px_12px_rgb(var(--accent-primary)/0.15)]",
        glow: "shadow-[0_8px_32px_rgb(var(--accent-primary)/0.1)]",
        bar: "from-[rgb(var(--accent-primary))] to-[rgb(var(--accent-warm)/0.7)]",
    },
};

function StatCardInner({ label, value, icon, accent, hint }: StatCardProps) {
    const [displayValue, setDisplayValue] = useState(value);
    const [animate, setAnimate] = useState(false);

    useEffect(() => {
        if (value !== displayValue) {
            setAnimate(true);
            const timer = setTimeout(() => {
                setDisplayValue(value);
                setTimeout(() => setAnimate(false), 50);
            }, 150);
            return () => clearTimeout(timer);
        }
    }, [value, displayValue]);

    return (
        <Card className={`group relative overflow-hidden glass glass-glow card-lift ${accentClasses[accent].glow} transition-all duration-400`}>
            {/* Gradient background */}
            <div
                className={`pointer-events-none absolute inset-0 bg-gradient-to-br ${accentClasses[accent].gradient}`}
            />
            {/* Subtle top highlight */}
            <div className="pointer-events-none absolute top-0 left-0 right-0 h-px bg-gradient-to-r from-transparent via-white/20 to-transparent" />
            {/* Left accent bar */}
            <div className={`pointer-events-none absolute left-0 top-4 bottom-4 w-0.5 rounded-r-full bg-gradient-to-b ${accentClasses[accent].bar} opacity-0 group-hover:opacity-100 transition-opacity duration-300`} />

            <CardHeader className="relative flex-row items-center justify-between pb-2">
                <CardTitle className="text-[11px] font-medium tracking-wider uppercase text-fg-muted">
                    {label}
                </CardTitle>
                <div
                    className={`flex h-10 w-10 items-center justify-center rounded-xl ring-1 ring-inset transition-transform duration-300 group-hover:scale-110 group-hover:rotate-3 ${accentClasses[accent].iconBg}`}
                >
                    {icon}
                </div>
            </CardHeader>
            <CardContent className="relative">
                <div className={`text-3xl font-semibold tracking-tight text-fg transition-all duration-200 ${animate ? "scale-110 opacity-0" : "scale-100 opacity-100"}`}>
                    {displayValue}
                </div>
                {hint && (
                    <div className="mt-2 text-xs text-fg-muted transition-colors group-hover:text-fg-subtle">
                        {hint}
                    </div>
                )}
            </CardContent>
        </Card>
    );
}

export const StatCard = React.memo(StatCardInner);
