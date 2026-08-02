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

const accentClasses: Record<StatCardProps["accent"], { iconBg: string }> = {
    sky: {
        iconBg: "bg-gradient-to-br from-[rgb(var(--accent-info)/0.15)] to-[rgb(var(--accent-info)/0.05)] ring-[rgb(var(--accent-info)/0.15)] text-[rgb(var(--accent-info))] shadow-[0_2px_8px_rgb(var(--accent-info)/0.1)]",
    },
    emerald: {
        iconBg: "bg-gradient-to-br from-[rgb(var(--accent-success)/0.15)] to-[rgb(var(--accent-success)/0.05)] ring-[rgb(var(--accent-success)/0.15)] text-[rgb(var(--accent-success))] shadow-[0_2px_8px_rgb(var(--accent-success)/0.1)]",
    },
    amber: {
        iconBg: "bg-gradient-to-br from-[rgb(var(--accent-warm)/0.15)] to-[rgb(var(--accent-warm)/0.05)] ring-[rgb(var(--accent-warm)/0.15)] text-[rgb(var(--accent-warm))] shadow-[0_2px_8px_rgb(var(--accent-warm)/0.1)]",
    },
    violet: {
        iconBg: "bg-gradient-to-br from-[rgb(var(--accent-primary)/0.15)] to-[rgb(var(--accent-primary)/0.05)] ring-[rgb(var(--accent-primary)/0.15)] text-[rgb(var(--accent-primary))] shadow-[0_2px_8px_rgb(var(--accent-primary)/0.1)]",
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
        <Card className={`group relative overflow-hidden glass card-lift transition-all duration-[350ms] ease-[cubic-bezier(0.32,0.72,0,1)]`}>
            <div className="pointer-events-none absolute inset-0 bg-gradient-to-br from-[rgb(var(--bg-subtle)/0.5)] to-transparent" />

            <CardHeader className="relative flex-row items-center justify-between pb-1">
                <CardTitle className="text-[11px] font-medium tracking-wider uppercase text-fg-muted">
                    {label}
                </CardTitle>
                <div
                    className={`flex h-9 w-9 items-center justify-center rounded-full ring-1 ring-inset [&_svg]:h-4 [&_svg]:w-4 ${accentClasses[accent].iconBg}`}
                >
                    {icon}
                </div>
            </CardHeader>
            <CardContent className="relative">
                <div className={`text-4xl font-semibold tracking-tight text-fg transition-all duration-200 ${animate ? "scale-110 opacity-0" : "scale-100 opacity-100"}`}>
                    {displayValue}
                </div>
                {hint && (
                    <div className="mt-2 text-xs text-fg-muted">
                        {hint}
                    </div>
                )}
            </CardContent>
        </Card>
    );
}

export const StatCard = React.memo(StatCardInner);
