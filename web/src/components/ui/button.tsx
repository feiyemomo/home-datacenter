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
        "bg-gradient-to-r from-[rgb(var(--accent-primary)/0.85)] to-[rgb(var(--accent-primary)/0.6)] text-white relative hover:shadow-[0_0_18px_rgb(var(--accent-primary)/0.3),0_0_30px_rgb(var(--accent-warm)/0.15)]",
    secondary:
        "glass text-fg hover:bg-[rgb(var(--bg-subtle)/0.3)]",
    outline:
        "glass-subtle text-fg hover:bg-[rgb(var(--bg-subtle)/0.3)] hover:border-[rgb(var(--accent-warm)/0.2)]",
    ghost:
        "bg-transparent text-fg-muted hover:bg-[rgb(var(--bg-subtle)/0.4)] hover:text-fg",
    danger:
        "bg-gradient-to-r from-[rgb(var(--accent-danger)/0.85)] to-[rgb(var(--accent-danger)/0.65)] text-white hover:shadow-[0_0_18px_rgb(var(--accent-danger)/0.3)]",
};

const sizeClasses: Record<ButtonSize, string> = {
    sm: "h-8 px-3 text-xs rounded-xl",
    md: "h-9 px-4 text-sm rounded-xl",
    lg: "h-10 px-6 text-sm rounded-xl",
    icon: "h-9 w-9 rounded-xl",
};

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(
    ({ className, variant = "primary", size = "md", ...props }, ref) => {
        return (
            <button
                ref={ref}
                className={cn(
                    "inline-flex items-center justify-center gap-2 font-medium",
                    "transition-all duration-[0.35s] ease-[cubic-bezier(0.32,0.72,0,1)]",
                    "active:scale-[0.98]",
                    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[rgb(var(--accent-primary)/0.4)] focus-visible:ring-offset-0",
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
