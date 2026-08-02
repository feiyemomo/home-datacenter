import { AlertCircle, RefreshCw } from "lucide-react";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";

export interface ErrorRetryProps {
    message: string;
    onRetry: () => void;
    fullPage?: boolean;
    className?: string;
}

/**
 * ErrorRetry — unified error state component.
 *
 * When `fullPage` is true, renders a centered full-page layout
 * suitable for route-level errors. When false (default), renders
 * a compact inline row suitable for card- or section-level errors.
 *
 * Both variants share the same glass aesthetic, danger-colored
 * AlertCircle icon, and an outlined retry button with hover lift.
 */
export function ErrorRetry({
    message,
    onRetry,
    fullPage = false,
    className,
}: ErrorRetryProps) {
    if (fullPage) {
        return (
            <div
                className={cn(
                    "flex min-h-[50vh] w-full items-center justify-center p-6 animate-fade-in",
                    className,
                )}
            >
                <div className="flex w-full max-w-sm flex-col items-center gap-5 rounded-2xl glass p-8 text-center">
                    <div className="flex h-14 w-14 items-center justify-center rounded-full bg-[rgb(var(--accent-danger)/0.15)]">
                        <AlertCircle
                            size={28}
                            className="text-[rgb(var(--accent-danger))]"
                        />
                    </div>
                    <p className="text-sm leading-relaxed text-fg">{message}</p>
                    <Button variant="outline" onClick={onRetry}>
                        <RefreshCw size={14} />
                        重试
                    </Button>
                </div>
            </div>
        );
    }

    return (
        <div
            className={cn(
                "flex items-center gap-3 rounded-xl glass-subtle px-4 py-3 animate-fade-in",
                className,
            )}
        >
            <AlertCircle
                size={16}
                className="shrink-0 text-[rgb(var(--accent-danger))]"
            />
            <p className="flex-1 text-xs leading-relaxed text-fg-muted">
                {message}
            </p>
            <Button variant="outline" size="sm" onClick={onRetry}>
                <RefreshCw size={12} />
                重试
            </Button>
        </div>
    );
}