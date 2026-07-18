import { forwardRef, type InputHTMLAttributes, type TextareaHTMLAttributes, type SelectHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export interface InputProps extends InputHTMLAttributes<HTMLInputElement> { }

export const Input = forwardRef<HTMLInputElement, InputProps>(
    ({ className, type = "text", ...props }, ref) => (
        <input
            ref={ref}
            type={type}
            className={cn(
                "flex h-9 w-full rounded-xl px-3 py-1 text-sm text-fg",
                "glass-subtle",
                "placeholder:text-fg-subtle",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[rgb(var(--accent-primary)/0.5)] focus-visible:bg-[rgb(var(--glass-bg)/0.5)]",
                "disabled:cursor-not-allowed disabled:opacity-40",
                "transition-all duration-300 ease-out",
                className,
            )}
            {...props}
        />
    ),
);
Input.displayName = "Input";

export interface TextareaProps
    extends TextareaHTMLAttributes<HTMLTextAreaElement> { }

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaProps>(
    ({ className, ...props }, ref) => (
        <textarea
            ref={ref}
            className={cn(
                "flex min-h-[120px] w-full rounded-xl px-3 py-2 text-sm text-fg font-mono",
                "glass-subtle",
                "placeholder:text-fg-subtle",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[rgb(var(--accent-primary)/0.5)] focus-visible:bg-[rgb(var(--glass-bg)/0.5)]",
                "disabled:cursor-not-allowed disabled:opacity-40",
                "transition-all duration-300 ease-out",
                className,
            )}
            {...props}
        />
    ),
);
Textarea.displayName = "Textarea";

export interface SelectProps
    extends SelectHTMLAttributes<HTMLSelectElement> { }

export const Select = forwardRef<HTMLSelectElement, SelectProps>(
    ({ className, children, ...props }, ref) => (
        <select
            ref={ref}
            className={cn(
                "flex h-9 w-full rounded-xl px-3 py-1 text-sm text-fg",
                "glass-subtle",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[rgb(var(--accent-primary)/0.5)]",
                "disabled:cursor-not-allowed disabled:opacity-40",
                "transition-all duration-300 ease-out",
                className,
            )}
            {...props}
        >
            {children}
        </select>
    ),
);
Select.displayName = "Select";

export interface LabelProps
    extends React.LabelHTMLAttributes<HTMLLabelElement> { }

export function Label({ className, ...props }: LabelProps) {
    return (
        <label
            className={cn(
                "text-xs font-medium uppercase tracking-wider text-fg-muted",
                className,
            )}
            {...props}
        />
    );
}
