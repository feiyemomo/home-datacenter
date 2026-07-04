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
        "bg-sky-500 text-white hover:bg-sky-400 focus-visible:ring-sky-500 shadow-sm shadow-sky-500/20",
    secondary:
        "bg-slate-700 text-slate-100 hover:bg-slate-600 focus-visible:ring-slate-500",
    outline:
        "border border-slate-600 bg-transparent text-slate-100 hover:bg-slate-800 focus-visible:ring-slate-500",
    ghost:
        "bg-transparent text-slate-200 hover:bg-slate-800 focus-visible:ring-slate-500",
    danger:
        "bg-rose-600 text-white hover:bg-rose-500 focus-visible:ring-rose-500 shadow-sm shadow-rose-500/20",
};

const sizeClasses: Record<ButtonSize, string> = {
    sm: "h-8 px-3 text-xs",
    md: "h-9 px-4 text-sm",
    lg: "h-10 px-6 text-sm",
    icon: "h-9 w-9",
};

/**
 * Minimal Shadcn-style button: forwardRef, variant + size props,
 * focus ring, disabled styles. No Radix dependency.
 */
export const Button = forwardRef<HTMLButtonElement, ButtonProps>(
    ({ className, variant = "primary", size = "md", ...props }, ref) => {
        return (
            <button
                ref={ref}
                className={cn(
                    "inline-flex items-center justify-center gap-2 rounded-md font-medium transition-colors",
                    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:ring-offset-slate-950",
                    "disabled:pointer-events-none disabled:opacity-50",
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
