import { memo } from "react";
import { useNavigate } from "react-router-dom";
import { AlertTriangle, Eye, ExternalLink } from "lucide-react";
import { alertThumbnailUrl, type CameraAlert } from "@/api/camera";
import { Badge } from "@/components/ui/badge";
import { formatLabel, formatConfidence, formatAlertTime } from "./utils";

interface AlertItemProps {
    alert: CameraAlert;
    onViewSnapshot: (alert: CameraAlert) => void;
    onNavigateToCamera: (alert: CameraAlert) => void;
}

function AlertItem({ alert, onViewSnapshot, onNavigateToCamera }: AlertItemProps) {
    const navigate = useNavigate();

    const handleNavigate = () => {
        const search = new URLSearchParams();
        if (alert.camera_id) {
            search.set("camera", String(alert.camera_id));
        }
        if (alert.start_time) {
            search.set("time", String(alert.start_time));
        }
        search.set("mode", "recording");
        navigate(`/cameras?${search.toString()}`);
        onNavigateToCamera(alert);
    };

    return (
        <li className="group flex gap-3 glass-subtle rounded-xl p-2 transition-colors hover:bg-[rgb(var(--bg-subtle)/0.3)]">
            {/* Thumbnail / icon — click opens full-resolution modal */}
            {alert.has_snapshot ? (
                <button
                    type="button"
                    onClick={() => onViewSnapshot(alert)}
                    className="relative h-16 w-24 shrink-0 overflow-hidden rounded-lg bg-black/30"
                    title="点击查看大图"
                >
                    <img
                        src={alertThumbnailUrl(alert.id)}
                        alt={alert.label}
                        className="h-full w-full object-cover"
                        loading="lazy"
                    />
                    <span className="absolute inset-0 flex items-center justify-center bg-black/0 opacity-0 transition-all group-hover:bg-black/30 group-hover:opacity-100">
                        <Eye size={14} className="text-white" />
                    </span>
                </button>
            ) : (
                <div className="flex h-16 w-24 shrink-0 items-center justify-center rounded-lg bg-[rgb(var(--accent-warm)/0.1)]">
                    <AlertTriangle size={18} className="text-[rgb(var(--accent-warm)/0.5)]" />
                </div>
            )}

            {/* Info — click navigates to the camera page */}
            <button
                type="button"
                onClick={handleNavigate}
                className="min-w-0 flex-1 cursor-pointer py-0.5 text-left"
                title="查看录像 — 跳转到对应时间点的录像回放"
            >
                <div className="flex flex-wrap items-center gap-1.5">
                    <span className="text-sm font-medium text-fg">
                        {formatLabel(alert.label)}
                    </span>
                    <Badge variant="info" className="text-[8px] px-1 py-0">
                        {formatConfidence(alert.confidence)}
                    </Badge>
                    {alert.has_clip && (
                        <Badge variant="success" className="text-[8px] px-1 py-0">
                            录像
                        </Badge>
                    )}
                    {alert.has_snapshot && (
                        <Badge variant="outline" className="text-[8px] px-1 py-0">
                            截图
                        </Badge>
                    )}
                </div>
                <p className="mt-1 truncate text-[11px] text-fg-muted">
                    {alert.camera_name ?? alert.camera_slug}
                    {alert.zones && alert.zones.length > 0
                        ? ` · ${alert.zones.join(", ")}`
                        : ""}
                </p>
                <p className="mt-0.5 text-[10px] text-fg-subtle">
                    {formatAlertTime(alert.start_time)}
                </p>
            </button>

            {/* Jump icon — visible on hover */}
            <div className="flex shrink-0 items-center self-center text-fg-subtle opacity-0 transition-opacity group-hover:opacity-100">
                <ExternalLink size={14} />
            </div>
        </li>
    );
}

export default memo(AlertItem);