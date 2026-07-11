import { forwardRef, type InputHTMLAttributes, type TextareaHTMLAttributes, type SelectHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export interface InputProps extends InputHTMLAttributes<HTMLInputElement> { }

/** Single-line text/number/password input. */
export const Input = forwardRef<HTMLInputElement, InputProps>(
    ({ className, type = "text", ...props }, ref) => (
        <input
            ref={ref}
            type={type}
            className={cn(
                "flex h-9 w-full rounded-lg border border-surface-border bg-white px-3 py-1 text-sm text-fg dark:bg-surface-raised",
                "placeholder:text-fg-subtle",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-sky-500 focus-visible:ring-offset-2 focus-visible:ring-offset-surface focus-visible:border-transparent",
                "disabled:cursor-not-allowed disabled:opacity-50",
                "transition-colors",
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
                "flex min-h-[120px] w-full rounded-lg border border-surface-border bg-white px-3 py-2 text-sm text-fg dark:bg-surface-raised",
                "placeholder:text-fg-subtle font-mono",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-sky-500 focus-visible:ring-offset-2 focus-visible:ring-offset-surface focus-visible:border-transparent",
                "disabled:cursor-not-allowed disabled:opacity-50",
                "transition-colors",
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
                "flex h-9 w-full rounded-lg border border-surface-border bg-white px-3 py-1 text-sm text-fg dark:bg-surface-raised",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-sky-500 focus-visible:ring-offset-2 focus-visible:ring-offset-surface focus-visible:border-transparent",
                "disabled:cursor-not-allowed disabled:opacity-50",
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
