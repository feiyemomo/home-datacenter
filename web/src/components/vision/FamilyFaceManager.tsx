import { useState, useEffect } from "react";
import {
    Users,
    Trash2,
    Cpu,
    CheckCircle2,
    AlertCircle,
    RefreshCw,
    UserPlus,
    Upload,
    Loader2,
    Sparkles,
} from "lucide-react";
import {
    getVisionStatus,
    listPersons,
    deletePerson,
    registerPerson,
    type VisionStatus,
    type VisionPerson,
} from "@/api/vision";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";

export default function FamilyFaceManager() {
    const [status, setStatus] = useState<VisionStatus | null>(null);
    const [persons, setPersons] = useState<VisionPerson[]>([]);
    const [loading, setLoading] = useState(true);
    const [refreshing, setRefreshing] = useState(false);
    const [feedback, setFeedback] = useState<{ type: "success" | "error"; text: string } | null>(null);

    // Manual photo upload modal / popover
    const [manualUploadOpen, setManualUploadOpen] = useState(false);
    const [uploadName, setUploadName] = useState("");
    const [selectedFile, setSelectedFile] = useState<File | null>(null);
    const [uploading, setUploading] = useState(false);

    const loadData = async () => {
        try {
            const [statusRes, personsRes] = await Promise.all([
                getVisionStatus().catch((e) => ({
                    online: false,
                    error: e?.message || "连接失败",
                } as VisionStatus)),
                listPersons().catch(() => [] as VisionPerson[]),
            ]);
            setStatus(statusRes);
            setPersons(personsRes);
        } catch (err: any) {
            setFeedback({ type: "error", text: err?.message || "获取视觉服务数据失败" });
        } finally {
            setLoading(false);
            setRefreshing(false);
        }
    };

    useEffect(() => {
        loadData();
        const timer = setInterval(loadData, 15000);
        return () => clearInterval(timer);
    }, []);

    const handleRefresh = () => {
        setRefreshing(true);
        loadData();
    };

    const handleDelete = async (name: string) => {
        if (!confirm(`确定要移除家庭成员「${name}」的面部特征档案吗？移除后将无法再自动识别该成员。`)) {
            return;
        }
        try {
            await deletePerson(name);
            setPersons((prev) => prev.filter((p) => p.name !== name));
            setFeedback({ type: "success", text: `已移除家庭成员「${name}」` });
        } catch (err: any) {
            setFeedback({ type: "error", text: err?.message || "删除成员失败" });
        }
    };

    const handleManualRegister = async (e: React.FormEvent) => {
        e.preventDefault();
        const trimmed = uploadName.trim();
        if (!trimmed) {
            setFeedback({ type: "error", text: "请输入成员称呼" });
            return;
        }
        if (!selectedFile) {
            setFeedback({ type: "error", text: "请选择包含人物正脸的照片" });
            return;
        }

        setUploading(true);
        setFeedback(null);
        try {
            const base64 = await new Promise<string>((resolve, reject) => {
                const reader = new FileReader();
                reader.onloadend = () => {
                    const dataUrl = reader.result as string;
                    const b64 = dataUrl.includes(",") ? dataUrl.split(",")[1] : dataUrl;
                    resolve(b64);
                };
                reader.onerror = reject;
                reader.readAsDataURL(selectedFile);
            });

            await registerPerson(trimmed, base64);
            setFeedback({ type: "success", text: `成功录入家庭成员「${trimmed}」！` });
            setUploadName("");
            setSelectedFile(null);
            setManualUploadOpen(false);
            loadData();
        } catch (err: any) {
            setFeedback({ type: "error", text: err?.message || "特征录入失败：未检测到有效人脸" });
        } finally {
            setUploading(false);
        }
    };

    const getGateBadge = (gate?: string) => {
        switch (gate) {
            case "circuit_break":
                return <Badge variant="danger">超载熔断保护 (已暂停推理)</Badge>;
            case "degraded":
                return <Badge variant="warning">负荷降级 (仅运行人脸识别)</Badge>;
            case "normal":
            default:
                return <Badge variant="success">全量检测运行中 (人脸 + 姿态)</Badge>;
        }
    };

    return (
        <div className="space-y-5 animate-fade-in">
            {/* Top overview card */}
            <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
                {/* Vision AI Service Status */}
                <Card className="glass-subtle border-[rgb(var(--border)/0.5)]">
                    <CardHeader className="pb-2">
                        <div className="flex items-center justify-between">
                            <CardTitle className="text-xs font-semibold text-fg-muted uppercase tracking-wider flex items-center gap-1.5">
                                <Sparkles size={14} className="text-[rgb(var(--accent-primary))]" />
                                AI 视觉感知引擎
                            </CardTitle>
                            <Badge variant={status?.online ? "success" : "danger"}>
                                {status?.online ? "在线" : "离线"}
                            </Badge>
                        </div>
                    </CardHeader>
                    <CardContent className="space-y-2 pt-1 text-xs">
                        <div className="flex justify-between items-center text-fg">
                            <span className="text-fg-muted">YuNet 人脸特征引擎:</span>
                            <span className="font-medium flex items-center gap-1">
                                {status?.face_engine_ready ? (
                                    <>
                                        <CheckCircle2 size={12} className="text-emerald-500" />
                                        <span>就绪</span>
                                    </>
                                ) : (
                                    <>
                                        <AlertCircle size={12} className="text-amber-500" />
                                        <span>未就绪</span>
                                    </>
                                )}
                            </span>
                        </div>
                        <div className="flex justify-between items-center text-fg">
                            <span className="text-fg-muted">MoveNet 姿态/摔倒引擎:</span>
                            <span className="font-medium flex items-center gap-1">
                                {status?.pose_engine_ready ? (
                                    <>
                                        <CheckCircle2 size={12} className="text-emerald-500" />
                                        <span>就绪</span>
                                    </>
                                ) : (
                                    <>
                                        <AlertCircle size={12} className="text-amber-500" />
                                        <span>未就绪</span>
                                    </>
                                )}
                            </span>
                        </div>
                    </CardContent>
                </Card>

                {/* CPU Gating Card */}
                <Card className="glass-subtle border-[rgb(var(--border)/0.5)]">
                    <CardHeader className="pb-2">
                        <div className="flex items-center justify-between">
                            <CardTitle className="text-xs font-semibold text-fg-muted uppercase tracking-wider flex items-center gap-1.5">
                                <Cpu size={14} className="text-[rgb(var(--accent-info))]" />
                                CPU 门控状态
                            </CardTitle>
                            <span className="text-xs font-mono font-bold text-fg">
                                {status?.cpu_usage_percent !== undefined
                                    ? `${status.cpu_usage_percent.toFixed(1)}%`
                                    : "--"}
                            </span>
                        </div>
                    </CardHeader>
                    <CardContent className="space-y-2 pt-1 text-xs">
                        <div className="flex items-center justify-between">
                            <span className="text-fg-muted">调度模式:</span>
                            {getGateBadge(status?.cpu_gate)}
                        </div>
                        <p className="text-[11px] text-fg-muted leading-relaxed">
                            当主机负荷 &gt;60% 自动暂停姿态检测以防卡顿，&gt;80% 触发自愈熔断保护。
                        </p>
                    </CardContent>
                </Card>

                {/* Family Members Count */}
                <Card className="glass-subtle border-[rgb(var(--border)/0.5)]">
                    <CardHeader className="pb-2">
                        <div className="flex items-center justify-between">
                            <CardTitle className="text-xs font-semibold text-fg-muted uppercase tracking-wider flex items-center gap-1.5">
                                <Users size={14} className="text-[rgb(var(--accent-warm))]" />
                                家人档案库
                            </CardTitle>
                            <Button
                                variant="ghost"
                                size="sm"
                                onClick={handleRefresh}
                                disabled={refreshing}
                                className="h-6 w-6 p-0 text-fg-muted hover:text-fg"
                                title="刷新"
                            >
                                <RefreshCw size={12} className={refreshing ? "animate-spin" : ""} />
                            </Button>
                        </div>
                    </CardHeader>
                    <CardContent className="pt-1 text-xs space-y-1">
                        <div className="flex items-baseline gap-2">
                            <span className="text-2xl font-bold font-mono text-fg">{persons.length}</span>
                            <span className="text-fg-muted">位已登记家庭成员</span>
                        </div>
                        <p className="text-[11px] text-fg-muted">
                            支持在录像抓拍中一键标记录入，自动进行人脸特征向量对齐。
                        </p>
                    </CardContent>
                </Card>
            </div>

            {/* Feedback alert */}
            {feedback && (
                <div
                    className={`p-3 rounded-xl text-xs flex items-center justify-between animate-fade-in ${
                        feedback.type === "success"
                            ? "bg-emerald-500/10 text-emerald-500 border border-emerald-500/20"
                            : "bg-rose-500/10 text-rose-500 border border-rose-500/20"
                    }`}
                >
                    <span>{feedback.text}</span>
                    <button
                        onClick={() => setFeedback(null)}
                        className="text-xs opacity-70 hover:opacity-100 ml-2"
                    >
                        ✕
                    </button>
                </div>
            )}

            {/* Family Members List Section */}
            <div className="rounded-2xl glass border border-[rgb(var(--border)/0.5)] p-5 space-y-4">
                <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3 pb-3 border-b border-[rgb(var(--border)/0.4)]">
                    <div>
                        <h3 className="text-sm font-semibold text-fg flex items-center gap-2">
                            <Users size={16} className="text-[rgb(var(--accent-primary))]" />
                            家庭成员人脸库 (Face Registry)
                        </h3>
                        <p className="text-xs text-fg-muted mt-0.5">
                            AI 摄像头识别到此列表中的家庭成员时，将自动区分家人与陌生人，并在自动化中触发专属联动。
                        </p>
                    </div>

                    <div className="flex items-center gap-2">
                        <Button
                            size="sm"
                            variant="outline"
                            className="h-8 gap-1.5 text-xs"
                            onClick={() => setManualUploadOpen((v) => !v)}
                        >
                            <Upload size={13} />
                            <span>本地照片录入</span>
                        </Button>
                    </div>
                </div>

                {/* Manual Photo Upload Section */}
                {manualUploadOpen && (
                    <div className="p-4 rounded-xl bg-surface-subtle border border-[rgb(var(--border)/0.6)] animate-slide-down space-y-3">
                        <div className="flex items-center justify-between">
                            <span className="text-xs font-semibold text-fg">本地照片录入家庭成员</span>
                            <button
                                type="button"
                                onClick={() => setManualUploadOpen(false)}
                                className="text-fg-muted hover:text-fg text-xs"
                            >
                                ✕
                            </button>
                        </div>
                        <form onSubmit={handleManualRegister} className="flex flex-col sm:flex-row items-stretch sm:items-center gap-3">
                            <Input
                                value={uploadName}
                                onChange={(e) => setUploadName(e.target.value)}
                                placeholder="输入称呼（如：爸爸、妈妈、小明）"
                                className="h-8 text-xs flex-1"
                                disabled={uploading}
                            />
                            <input
                                type="file"
                                accept="image/jpeg,image/png,image/webp"
                                onChange={(e) => setSelectedFile(e.target.files?.[0] || null)}
                                className="text-xs text-fg-muted file:mr-2 file:py-1 file:px-2.5 file:rounded-lg file:border-0 file:text-xs file:font-semibold file:bg-[rgb(var(--accent-primary)/0.15)] file:text-[rgb(var(--accent-primary))] hover:file:bg-[rgb(var(--accent-primary)/0.25)]"
                                disabled={uploading}
                            />
                            <Button
                                type="submit"
                                size="sm"
                                disabled={uploading || !uploadName.trim() || !selectedFile}
                                className="h-8 text-xs gap-1.5 bg-[rgb(var(--accent-primary))] text-white shrink-0"
                            >
                                {uploading ? <Loader2 size={13} className="animate-spin" /> : <UserPlus size={13} />}
                                <span>{uploading ? "提取特征中..." : "保存家人"}</span>
                            </Button>
                        </form>
                    </div>
                )}

                {/* Persons Cards Grid */}
                {loading ? (
                    <div className="py-12 flex flex-col items-center justify-center gap-2 text-fg-muted text-xs">
                        <Loader2 size={20} className="animate-spin text-[rgb(var(--accent-primary))]" />
                        <span>加载家人档案中...</span>
                    </div>
                ) : persons.length > 0 ? (
                    <div className="grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-6 gap-3">
                        {persons.map((p) => (
                            <div
                                key={p.name}
                                className="group relative flex flex-col items-center justify-center p-4 rounded-xl border border-[rgb(var(--border)/0.5)] bg-surface hover:border-[rgb(var(--accent-primary)/0.4)] transition-all shadow-sm"
                            >
                                <div className="h-12 w-12 rounded-full bg-gradient-to-tr from-[rgb(var(--accent-primary)/0.2)] to-[rgb(var(--accent-warm)/0.2)] flex items-center justify-center text-[rgb(var(--accent-primary))] font-bold text-base mb-2 border border-[rgb(var(--border)/0.3)]">
                                    {p.name.slice(0, 1)}
                                </div>
                                <span className="text-xs font-semibold text-fg text-center truncate w-full" title={p.name}>
                                    {p.name}
                                </span>
                                <Badge variant="outline" className="text-[10px] mt-1 py-0 px-1 text-emerald-500 border-emerald-500/30">
                                    已建模
                                </Badge>

                                <button
                                    type="button"
                                    onClick={() => handleDelete(p.name)}
                                    className="absolute top-2 right-2 p-1 rounded-lg text-fg-subtle opacity-0 group-hover:opacity-100 hover:text-red-500 hover:bg-red-500/10 transition-all"
                                    title={`删除 ${p.name}`}
                                >
                                    <Trash2 size={13} />
                                </button>
                            </div>
                        ))}
                    </div>
                ) : (
                    <div className="rounded-xl border border-dashed border-[rgb(var(--border)/0.6)] p-8 text-center space-y-2">
                        <div className="mx-auto h-12 w-12 rounded-full bg-[rgb(var(--accent-primary)/0.1)] flex items-center justify-center text-[rgb(var(--accent-primary))]">
                            <Users size={22} />
                        </div>
                        <h4 className="text-xs font-semibold text-fg">暂无录入的家庭成员</h4>
                        <p className="text-xs text-fg-muted max-w-md mx-auto">
                            无需手动拍照上传！在摄像头告警抓拍或录像回放中，点击图片右上角的
                            <span className="font-semibold text-fg">「录入为家人」</span>
                            ，AI 引擎将自动从画面截取中提取人脸特征并建立家人档案。
                        </p>
                    </div>
                )}
            </div>
        </div>
    );
}
