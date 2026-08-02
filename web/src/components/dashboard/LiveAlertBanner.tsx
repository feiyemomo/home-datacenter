import { AlertTriangle, Eye } from "lucide-react";
import { alertThumbnailUrl, type CameraAlert } from "@/api/camera";
import { Badge } from "@/components/ui/badge";
import { formatLabel, formatConfidence } from "./utils";

interface LiveAlertBannerProps {
    alert: CameraAlert;
    onViewSnapshot: (alert: CameraAlert) => void;
}

export default function LiveAlertBanner({ alert, onViewSnapshot }: LiveAlertBannerProps) {
    return (
        <div className="animate-fade-in rounded-xl bg-[rgb(var(--accent-warm)/0.08)] border border-[rgb(var(--accent-warm)/0.15)] px-4 py-3">
            <div className="flex items-center gap-3">
                <div className="flex h-8 w-8 items-center justify-center rounded-full bg-[rgb(var(--accent-warm)/0.12)]">
                    <AlertTriangle size={16} className="text-[rgb(var(--accent-warm))]" />
                </div>
                <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                        <span className="text-sm font-semibold text-fg">
                            检测到 {formatLabel(alert.label)}
                        </span>
                        <Badge variant="info" className="text-[9px]">
                            {formatConfidence(alert.confidence)}
                        </Badge>
                    </div>
                    <p className="text-xs text-fg-muted">
                        {alert.camera_name ?? alert.camera_slug}
                        {alert.zones && alert.zones.length > 0
                            ? ` · ${alert.zones.join(", ")}`
                            : ""}
                    </p>
                </div>
                {alert.has_snapshot && (
                    <button
                        type="button"
                        onClick={() => onViewSnapshot(alert)}
                        className="relative h-12 w-16 shrink-0 overflow-hidden rounded-lg bg-black/30"
                        title="点击查看截图"
                    >
                        <img
                            src={alertThumbnailUrl(alert.id)}
                            alt={alert.label}
                            className="h-full w-full object-cover"
                        />
                        <span className="absolute inset-0 flex items-center justify-center bg-black/0 opacity-0 transition-all hover:bg-black/30 hover:opacity-100">
                            <Eye size={12} className="text-white" />
                        </span>
                    </button>
                )}
                <span className="text-[10px] text-fg-subtle">实时</span>
            </div>
        </div>
    );
}