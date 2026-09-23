import { useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { AlertTriangle, X, UserPlus, Check, Loader2 } from "lucide-react";
import { alertSnapshotUrl, type CameraAlert } from "@/api/camera";
import { registerPersonFromSnapshot } from "@/api/vision";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { formatLabel, formatConfidence, formatAlertTime } from "./utils";

interface AlertSnapshotModalProps {
    alert: CameraAlert;
    onClose: () => void;
}

export default function AlertSnapshotModal({ alert, onClose }: AlertSnapshotModalProps) {
    const [showEnroll, setShowEnroll] = useState(false);
    const [personName, setPersonName] = useState("");
    const [enrolling, setEnrolling] = useState(false);
    const [enrollFeedback, setEnrollFeedback] = useState<{ type: "success" | "error"; text: string } | null>(null);

    // Close on Escape key
    useEffect(() => {
        const onKey = (e: KeyboardEvent) => {
            if (e.key === "Escape") onClose();
        };
        window.addEventListener("keydown", onKey);
        return () => window.removeEventListener("keydown", onKey);
    }, [onClose]);

    const handleEnroll = async (e: React.FormEvent) => {
        e.preventDefault();
        const trimmed = personName.trim();
        if (!trimmed) {
            setEnrollFeedback({ type: "error", text: "请输入家庭成员称呼" });
            return;
        }

        setEnrolling(true);
        setEnrollFeedback(null);
        try {
            const url = alertSnapshotUrl(alert.id);
            await registerPersonFromSnapshot(trimmed, url);
            setEnrollFeedback({
                type: "success",
                text: `已成功将抓拍人物录入为家人「${trimmed}」！`,
            });
            setPersonName("");
            // Auto close enroll drawer after 2.5s
            setTimeout(() => {
                setShowEnroll(false);
                setEnrollFeedback(null);
            }, 2500);
        } catch (err: any) {
            setEnrollFeedback({
                type: "error",
                text: err?.message || "录入失败：未在截图中识别到有效人脸特征",
            });
        } finally {
            setEnrolling(false);
        }
    };

    const modalContent = (
        <div
            className="fixed inset-0 z-[9999] flex items-center justify-center bg-black/70 p-4 backdrop-blur-sm animate-fade-in"
            onClick={onClose}
        >
            <div
                className="relative max-h-[90vh] w-full max-w-3xl overflow-hidden rounded-2xl glass shadow-2xl flex flex-col"
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

                    <div className="flex items-center gap-2">
                        {/* Enroll button */}
                        <Button
                            type="button"
                            variant={showEnroll ? "primary" : "outline"}
                            size="sm"
                            className="h-8 gap-1.5 text-xs"
                            onClick={() => {
                                setShowEnroll((v) => !v);
                                setEnrollFeedback(null);
                            }}
                            title="将此画面中的人物标记录入为家庭成员"
                        >
                            <UserPlus size={14} />
                            <span>{showEnroll ? "取消录入" : "录入为家人"}</span>
                        </Button>

                        <button
                            type="button"
                            onClick={onClose}
                            className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg text-fg-muted transition-colors hover:bg-[rgb(var(--bg-subtle)/0.5)] hover:text-fg"
                        >
                            <X size={16} />
                        </button>
                    </div>
                </div>

                {/* Inline Enroll Form */}
                {showEnroll && (
                    <div className="border-b border-[rgb(var(--border)/0.5)] bg-surface-subtle p-3 px-4 animate-slide-down">
                        <form onSubmit={handleEnroll} className="flex flex-col sm:flex-row items-stretch sm:items-center gap-2.5">
                            <div className="flex-1">
                                <Input
                                    value={personName}
                                    onChange={(e) => setPersonName(e.target.value)}
                                    placeholder="输入家人备注（如：爸爸、妈妈、小明）"
                                    className="h-8 text-xs"
                                    disabled={enrolling}
                                    autoFocus
                                />
                            </div>
                            <Button
                                type="submit"
                                size="sm"
                                disabled={enrolling || !personName.trim()}
                                className="h-8 gap-1.5 text-xs bg-[rgb(var(--accent-primary))] text-white shrink-0"
                            >
                                {enrolling ? <Loader2 size={13} className="animate-spin" /> : <Check size={13} />}
                                <span>{enrolling ? "特征提取中..." : "保存家人面部"}</span>
                            </Button>
                        </form>
                        {enrollFeedback && (
                            <p
                                className={`mt-2 text-xs font-medium flex items-center gap-1.5 ${
                                    enrollFeedback.type === "success" ? "text-emerald-500" : "text-rose-500"
                                }`}
                            >
                                {enrollFeedback.text}
                            </p>
                        )}
                        <p className="mt-1 text-[11px] text-fg-muted">
                            系统将自动从当前截图中定位人脸并提取 128 维特征，后续该摄像头拍摄到此人时将自动标记身份并触发专属联动。
                        </p>
                    </div>
                )}

                {/* Image */}
                <div className="flex max-h-[75vh] items-center justify-center bg-black/40 p-2 overflow-hidden">
                    <img
                        src={alertSnapshotUrl(alert.id)}
                        alt={`${alert.label} snapshot`}
                        className="max-h-[72vh] max-w-full rounded-lg object-contain"
                    />
                </div>
            </div>
        </div>
    );

    return createPortal(modalContent, document.body);
}