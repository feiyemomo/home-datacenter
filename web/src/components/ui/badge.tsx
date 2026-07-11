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
    default: "bg-surface-subtle text-fg-muted ring-1 ring-inset ring-surface-border",
    success: "bg-emerald-500/15 text-emerald-700 dark:text-emerald-300 ring-1 ring-inset ring-emerald-500/30",
    warning: "bg-amber-500/15 text-amber-700 dark:text-amber-300 ring-1 ring-inset ring-amber-500/30",
    danger: "bg-rose-500/15 text-rose-700 dark:text-rose-300 ring-1 ring-inset ring-rose-500/30",
    info: "bg-sky-500/15 text-sky-700 dark:text-sky-300 ring-1 ring-inset ring-sky-500/30",
    outline: "border border-surface-border text-fg-muted",
};

/** Compact pill used for status / counts. */
export function Badge({
    className,
    variant = "default",
    ...props
}: BadgeProps) {
    return (
        <span
            className={cn(
                "inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium",
                variantClasses[variant],
                className,
            )}
            {...props}
        />
    );
}
