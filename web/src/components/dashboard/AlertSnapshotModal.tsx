import { useEffect } from "react";
import { AlertTriangle, X } from "lucide-react";
import { alertSnapshotUrl, type CameraAlert } from "@/api/camera";
import { Badge } from "@/components/ui/badge";
import { formatLabel, formatConfidence, formatAlertTime } from "./utils";

interface AlertSnapshotModalProps {
    alert: CameraAlert;
    onClose: () => void;
}

export default function AlertSnapshotModal({ alert, onClose }: AlertSnapshotModalProps) {
    // Close on Escape key
    useEffect(() => {
        const onKey = (e: KeyboardEvent) => {
            if (e.key === "Escape") onClose();
        };
        window.addEventListener("keydown", onKey);
        return () => window.removeEventListener("keydown", onKey);
    }, [onClose]);

    return (
        <div
            className="fixed inset-0 z-50 flex items-center justify-center bg-black/70 p-4 backdrop-blur-sm animate-fade-in"
            onClick={onClose}
        >
            <div
                className="relative max-h-[90vh] max-w-3xl overflow-hidden rounded-2xl glass shadow-2xl"
                onClick={(e) => e.stopPropagation()}
            >
                {/* Header */}
                <div className="flex items-center justify-between gap-3 border-b border-[rgb(var(--border)/0.5)] px-4 py-3">
                    <div className="min-w-0">
                        <div className="flex items-center gap-2">
                            <AlertTriangle size={14} className="text-[rgb(var(--accent-warm))]" />
                            <span className="text-sm font-semibold text-fg">
                                {formatLabel(alert.label)}
                            </span>
                            <Badge variant="info" className="text-[9px]">
                                {formatConfidence(alert.confidence)}
                            </Badge>
                        </div>
                        <p className="mt-0.5 truncate text-[11px] text-fg-muted">
                            {alert.camera_name ?? alert.camera_slug}
                            {alert.zones && alert.zones.length > 0
                                ? ` · ${alert.zones.join(", ")}`
                                : ""}
                            {" · "}
                            {formatAlertTime(alert.start_time)}
                        </p>
                    </div>
                    <button
                        type="button"
                        onClick={onClose}
                        className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg text-fg-muted transition-colors hover:bg-[rgb(var(--bg-subtle)/0.5)] hover:text-fg"
                    >
                        <X size={16} />
                    </button>
                </div>
                {/* Image */}
                <div className="flex max-h-[75vh] items-center justify-center bg-black/40 p-2">
                    <img
                        src={alertSnapshotUrl(alert.id)}
                        alt={`${alert.label} snapshot`}
                        className="max-h-[72vh] max-w-full rounded-lg object-contain"
                    />
                </div>
            </div>
        </div>
    );
}