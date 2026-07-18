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
    default: "glass-subtle text-fg-muted",
    success: "bg-[rgb(var(--accent-success)/0.15)] text-[rgb(var(--accent-success))] glass-subtle",
    warning: "bg-[rgb(220 180 100/0.15)] text-[rgb(220 180 100)] glass-subtle",
    danger: "bg-[rgb(var(--accent-danger)/0.15)] text-[rgb(var(--accent-danger))] glass-subtle",
    info: "bg-[rgb(var(--accent-info)/0.15)] text-[rgb(var(--accent-info))] glass-subtle",
    outline: "glass-subtle text-fg-muted",
};

export function Badge({
    className,
    variant = "default",
    ...props
}: BadgeProps) {
    return (
        <span
            className={cn(
                "inline-flex items-center gap-1 rounded-full px-2.5 py-0.5 text-xs font-medium",
                "transition-all duration-200 ease-out",
                variantClasses[variant],
                className,
            )}
            {...props}
        />
    );
}
