import type { HTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export type BadgeVariant =
    | "default"
    | "success"
    | "warning"
    | "danger"
    | "info"
    | "outline";

export interface BadgeProps extends HTMLAttributes<HTMLSpanElement> {
    variant?: BadgeVariant;
}

const variantClasses: Record<BadgeVariant, string> = {
    default: "glass-subtle text-fg-muted border border-current/10",
    success: "bg-[rgb(var(--accent-success)/0.1)] text-[rgb(var(--accent-success))] glass-subtle border border-current/10",
    warning: "bg-[rgb(var(--accent-warm)/0.1)] text-[rgb(var(--accent-warm))] glass-subtle border border-current/10",
    danger: "bg-[rgb(var(--accent-danger)/0.1)] text-[rgb(var(--accent-danger))] glass-subtle border border-current/10",
    info: "bg-[rgb(var(--accent-info)/0.1)] text-[rgb(var(--accent-info))] glass-subtle border border-current/10",
    outline: "glass-subtle text-fg-muted border border-current/10",
};

export function Badge({
    className,
    variant = "default",
    ...props
}: BadgeProps) {
    return (
        <span
            className={cn(
                "inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium",
                "transition-all duration-300 ease-[cubic-bezier(0.32,0.72,0,1)]",
                variantClasses[variant],
                className,
            )}
            {...props}
        />
    );
}
