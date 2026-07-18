import { forwardRef, type ButtonHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export type ButtonVariant =
    | "primary"
    | "secondary"
    | "outline"
    | "ghost"
    | "danger";
export type ButtonSize = "sm" | "md" | "lg" | "icon";

export interface ButtonProps
    extends ButtonHTMLAttributes<HTMLButtonElement> {
    variant?: ButtonVariant;
    size?: ButtonSize;
}

const variantClasses: Record<ButtonVariant, string> = {
    primary:
        "bg-gradient-to-r from-[rgb(var(--accent-primary)/0.9)] to-[rgb(var(--accent-primary)/0.7)] text-white hover:from-[rgb(var(--accent-primary))] hover:to-[rgb(var(--accent-primary)/0.8)] glass-glow",
    secondary:
        "glass text-fg hover:bg-[rgb(var(--bg-subtle)/0.5)] glass-transition",
    outline:
        "glass-subtle text-fg hover:bg-[rgb(var(--bg-subtle)/0.5)] glass-transition",
    ghost:
        "bg-transparent text-fg-muted hover:bg-[rgb(var(--bg-subtle)/0.3)] hover:text-fg glass-transition",
    danger:
        "bg-[rgb(var(--accent-danger)/0.8)] text-white hover:bg-[rgb(var(--accent-danger))] glass-glow",
};

const sizeClasses: Record<ButtonSize, string> = {
    sm: "h-8 px-3 text-xs rounded-lg",
    md: "h-9 px-4 text-sm rounded-xl",
    lg: "h-10 px-6 text-sm rounded-xl",
    icon: "h-9 w-9 rounded-lg",
};

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(
    ({ className, variant = "primary", size = "md", ...props }, ref) => {
        return (
            <button
                ref={ref}
                className={cn(
                    "inline-flex items-center justify-center gap-2 font-medium",
                    "transition-all duration-300 ease-out",
                    "active:scale-[0.97]",
                    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[rgb(var(--accent-primary)/0.5)] focus-visible:ring-offset-2 focus-visible:ring-offset-surface",
                    "disabled:pointer-events-none disabled:opacity-40",
                    variantClasses[variant],
                    sizeClasses[size],
                    className,
                )}
                {...props}
            />
        );
    },
);
Button.displayName = "Button";
