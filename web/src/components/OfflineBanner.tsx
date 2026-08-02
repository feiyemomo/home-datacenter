import { RefreshCw, X } from "lucide-react";
import { useOnlineStatus } from "@/hooks/useOnlineStatus";
import { cn } from "@/lib/utils";

interface OfflineBannerProps {
    /** Called when the user clicks the "重试" button. */
    onRetry?: () => void;
}

/**
 * OfflineBanner — a fixed glass banner at the top of the viewport
 * that appears when the browser detects a network disconnection.
 *
 * - Shows "网络已断开，显示缓存数据" while offline.
 * - Dismiss button (X) hides the banner until the next offline event.
 * - "重试" button triggers onRetry for manual data refresh.
 * - Slides in/out from the top with a smooth transition.
 */
export default function OfflineBanner({ onRetry }: OfflineBannerProps) {
    const { isOnline, wasOffline, dismissOffline } = useOnlineStatus();

    const show = !isOnline && wasOffline;

    return (
        <div
            className={cn(
                "fixed left-0 right-0 top-0 z-50 transition-all duration-500 ease-out",
                show
                    ? "translate-y-0 opacity-100"
                    : "-translate-y-full opacity-0 pointer-events-none",
            )}
        >
            <div className="mx-auto flex max-w-4xl items-center gap-3 rounded-bl-2xl rounded-br-2xl border border-t-0 border-[rgb(var(--accent-danger)/0.15)] bg-[rgb(var(--accent-danger)/0.08)] px-4 py-3 shadow-lg backdrop-blur-2xl">
                {/* Icon */}
                <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-[rgb(var(--accent-danger)/0.1)]">
                    <RefreshCw
                        size={14}
                        className="animate-spin text-[rgb(var(--accent-danger)/0.8)]"
                        style={{ animationDuration: "3s" }}
                    />
                </div>

                {/* Message */}
                <p className="flex-1 text-sm font-medium text-[rgb(var(--accent-danger)/0.9)]">
                    网络已断开，显示缓存数据
                </p>

                {/* Retry button */}
                {onRetry && (
                    <button
                        type="button"
                        onClick={onRetry}
                        className="flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-xs font-medium text-[rgb(var(--accent-danger)/0.9)] transition-colors hover:bg-[rgb(var(--accent-danger)/0.1)] active:bg-[rgb(var(--accent-danger)/0.15)]"
                    >
                        <RefreshCw size={12} />
                        重试
                    </button>
                )}

                {/* Dismiss button */}
                <button
                    type="button"
                    onClick={dismissOffline}
                    className="flex h-6 w-6 items-center justify-center rounded-full text-[rgb(var(--accent-danger)/0.5)] transition-colors hover:bg-[rgb(var(--accent-danger)/0.1)] hover:text-[rgb(var(--accent-danger)/0.9)]"
                    aria-label="关闭提示"
                >
                    <X size={14} />
                </button>
            </div>
        </div>
    );
}