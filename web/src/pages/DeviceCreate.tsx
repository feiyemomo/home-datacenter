import { useMemo, useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import {
    Camera as CameraIcon,
    CheckCircle2,
    ChevronLeft,
    Eye,
    EyeOff,
    Info,
    Loader2,
    Plug,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input, Select } from "@/components/ui/input";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { useAuth } from "@/hooks/useAuth";
import { registerCamera } from "@/api/camera";
import { cn } from "@/lib/utils";

/**
 * Vendor presets — a small opinionated list of common defaults.
 * Picking a vendor only fills the form; the operator can still
 * override every field. The ChannelID conventions below are the
 * 90% case for that vendor; some firmware (e.g. newer Dahua) uses
 * 1/2 instead of 101/201 — the operator can change it.
 *
 * NOTE: these are URL/path patterns only. Credentials still go in
 * the RTSP URL via the userinfo component built by the registry
 * (`rtsp://<user>:<pass>@<host>:<port>/Streaming/Channels/<ch>`).
 */
const VENDOR_PRESETS: Record<
    string,
    { label: string; rtspPort: number; onvifPort: number; channelId: number; ptz: boolean; notes: string }
> = {
    hikvision: {
        label: "Hikvision",
        rtspPort: 554,
        onvifPort: 80,
        channelId: 101,
        ptz: true,
        notes: "通道 101/201 = 摄像头 1 的主/子码流。RTSP 路径固定。",
    },
    dahua: {
        label: "Dahua / Uniview",
        rtspPort: 554,
        onvifPort: 80,
        channelId: 1,
        ptz: true,
        notes: "通道 1/2 = 主/子码流。部分固件仍使用 101/201 — 请用 VLC 验证。",
    },
    ezviz: {
        label: "Ezviz（云中继局域网模式）",
        rtspPort: 554,
        onvifPort: 80,
        channelId: 1,
        ptz: false,
        notes: "纯云设备不暴露局域网 RTSP。若测试无画面，请禁用。",
    },
    reolink: {
        label: "Reolink",
        rtspPort: 554,
        onvifPort: 8000,
        channelId: 0,
        ptz: true,
        notes: "通道 0 为主码流，1 为子码流。ONVIF 端口通常为 8000，非一般默认的 80。",
    },
    onvif_generic: {
        label: "通用 ONVIF",
        rtspPort: 554,
        onvifPort: 80,
        channelId: 1,
        ptz: false,
        notes: "未知厂商。ONVIF 自动发现可能会在注册时填充 profile token。",
    },
};

interface DraftCam {
    name: string;
    vendor: string;
    host: string;
    onvif_port: number;
    rtsp_port: number;
    channel_id: number;
    username: string;
    password: string;
    ptz: boolean;
    audio: boolean;
    motion: boolean;
    transcode: boolean;
}

const EMPTY_DRAFT: DraftCam = {
    name: "",
    vendor: "hikvision",
    host: "",
    onvif_port: 80,
    rtsp_port: 554,
    channel_id: 101,
    username: "admin",
    password: "",
    ptz: true,
    audio: true,
    motion: true,
    transcode: false,
};

type SubmitState =
    | { kind: "idle" }
    | { kind: "submitting" }
    | { kind: "ok"; id: number; name: string }
    | { kind: "err"; message: string };

/**
 * DeviceCreate — dedicated, full-page form for registering a new
 * camera (the only kind of "device" an admin can create today).
 *
 * Why a separate page:
 *   - The Cameras list page is for *browsing* cameras and watching
 *     their live view; cramming a long form there pushes the cards
 *     down and the form's "Cancel" loses its meaning.
 *   - A standalone page is the right place to surface vendor
 *     presets, the synthesized RTSP URL, and a clear success state
 *     that hands the operator back to the live view.
 *   - The form remains the only admin write path; the API is the
 *     same POST /api/v1/cameras used by the inline form.
 */
export default function DeviceCreate() {
    const { isAdmin } = useAuth();
    const nav = useNavigate();

    const [draft, setDraft] = useState<DraftCam>(EMPTY_DRAFT);
    const [showPassword, setShowPassword] = useState(false);
    const [submit, setSubmit] = useState<SubmitState>({ kind: "idle" });

    // Apply a vendor preset. Keeps the operator's existing host /
    // name / credentials — only fills the ports & capabilities
    // that the vendor is opinionated about.
    function applyVendor(vendor: string) {
        const p = VENDOR_PRESETS[vendor];
        if (!p) return;
        setDraft((d) => ({
            ...d,
            vendor,
            rtsp_port: p.rtspPort,
            onvif_port: p.onvifPort,
            channel_id: p.channelId,
            ptz: p.ptz,
        }));
    }

    // The RTSP URL the registry will build. Mirrors
    // camera/registry.go rtspURL() exactly so the operator can
    // sanity-check before submitting. Audio is always stripped at
    // the source — see registry.go rtspURL comment. When
    // `transcode` is on, the URL gains a `&video=h264` fragment
    // that tells go2rtc to run the source through ffmpeg, output
    // H.264 (escape for HEVC cameras on Chromium WebRTC).
    const rtspPreview = useMemo(() => {
        const u = draft.username || "<user>";
        const p = draft.password ? "•••" : "<pass>";
        const ch = draft.channel_id || 1;
        const frag = draft.transcode
            ? "#audio=0&video=h264"
            : "#audio=0";
        return `rtsp://${u}:${p}@${draft.host || "<host>"}:${draft.rtsp_port || 554}/Streaming/Channels/${ch}${frag}`;
    }, [draft]);

    // We do not block the page on isAdmin() failing — the API
    // will return 403 if a non-admin posts, and we surface that
    // as a normal error. The "admin only" gate is for cleanliness
    // of the UI ("you don't have permission to use this form").
    if (!isAdmin) {
        return (
            <div className="animate-fade-in space-y-4">
                <PageHeader onBack={() => nav("/cameras")} />
                <Card>
                    <CardContent className="p-8 text-center text-sm text-fg-muted">
                        <Info className="mx-auto mb-2 h-6 w-6 text-fg-subtle" />
                        只有管理员可以注册新设备。请联系管理员添加此摄像头，或以管理员身份登录。
                    </CardContent>
                </Card>
            </div>
        );
    }

    async function onSubmit(e: FormEvent) {
        e.preventDefault();
        setSubmit({ kind: "submitting" });
        try {
            const cam = await registerCamera({
                name: draft.name.trim(),
                vendor: draft.vendor,
                host: draft.host.trim(),
                onvif_port: draft.onvif_port,
                rtsp_port: draft.rtsp_port,
                channel_id: draft.channel_id,
                username: draft.username,
                password: draft.password,
                ptz: draft.ptz,
                audio: draft.audio,
                motion: draft.motion,
                transcode: draft.transcode,
            });
            setSubmit({ kind: "ok", id: cam.id, name: cam.name });
        } catch (err) {
            setSubmit({
                kind: "err",
                message: err instanceof Error ? err.message : String(err),
            });
        }
    }

    function reset() {
        setDraft(EMPTY_DRAFT);
        setSubmit({ kind: "idle" });
    }

    return (
        <div className="animate-fade-in mx-auto max-w-3xl space-y-6">
            <PageHeader onBack={() => nav("/cameras")} />

            {/* Success state — replaces the form after a successful submit.
                We keep the page up so the operator can copy the new
                camera's id and review the RTSP URL they ended up with. */}
            {submit.kind === "ok" ? (
                <Card>
                    <CardContent className="space-y-4 p-8 text-center">
                        <CheckCircle2 className="mx-auto h-10 w-10 text-[rgb(var(--accent-success))]" />
                        <div>
                            <p className="text-base font-semibold text-fg">
                                “{submit.name}” 已注册
                            </p>
                            <p className="mt-1 text-xs text-fg-muted">
                                摄像头 #{submit.id} 已添加到 go2rtc 和摄像头列表。
                            </p>
                        </div>
                        <div className="flex justify-center gap-2">
                            <Button onClick={() => nav("/cameras")}>
                                打开摄像头列表
                            </Button>
                            <Button variant="outline" onClick={reset}>
                                继续注册
                            </Button>
                        </div>
                    </CardContent>
                </Card>
            ) : (
                <form className="space-y-6" onSubmit={onSubmit}>
                    {submit.kind === "err" && (
                        <div className="rounded-xl glass bg-[rgb(var(--accent-danger)/0.1)] px-4 py-3 text-xs text-[rgb(var(--accent-danger))]">
                            {submit.message}
                        </div>
                    )}

                    {/* 1. Identity */}
                    <Card>
                        <CardHeader>
                            <CardTitle className="flex items-center gap-2 text-base">
                                <CameraIcon size={16} className="text-[rgb(var(--accent-info))]" />
                                标识
                            </CardTitle>
                            <CardDescription>
                                面板名称同时作为 go2rtc 的流密钥，因此两个摄像头不能重名。请选择一个一眼能识别的名称。
                            </CardDescription>
                        </CardHeader>
                        <CardContent className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                            <Field label="显示名称" required>
                                <Input
                                    placeholder="例如：前门 / garage-north"
                                    value={draft.name}
                                    onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                                    required
                                    autoFocus
                                />
                            </Field>
                            <Field label="厂商">
                                <Select
                                    value={draft.vendor}
                                    onChange={(e) => applyVendor(e.target.value)}
                                >
                                    {Object.entries(VENDOR_PRESETS).map(([k, v]) => (
                                        <option key={k} value={k}>
                                            {v.label}
                                        </option>
                                    ))}
                                </Select>
                            </Field>
                            <div className="sm:col-span-2">
                                <p className="text-xs text-fg-subtle">
                                    {VENDOR_PRESETS[draft.vendor]?.notes ??
                                        "选择厂商以自动填充端口和通道 ID。"}
                                </p>
                            </div>
                        </CardContent>
                    </Card>

                    {/* 2. Network */}
                    <Card>
                        <CardHeader>
                            <CardTitle className="flex items-center gap-2 text-base">
                                <Plug size={16} className="text-[rgb(var(--accent-info))]" />
                                网络
                            </CardTitle>
                            <CardDescription>
                                摄像头监听地址。主机可以是 IP，也可以是 home-api 所在局域网内可解析的主机名（注册中心会直接与之通信）。
                            </CardDescription>
                        </CardHeader>
                        <CardContent className="grid grid-cols-1 gap-4 sm:grid-cols-3">
                            <Field label="主机" required className="sm:col-span-3">
                                <Input
                                    placeholder="192.168.31.100"
                                    value={draft.host}
                                    onChange={(e) => setDraft({ ...draft, host: e.target.value })}
                                    required
                                />
                            </Field>
                            <Field label="ONVIF 端口">
                                <Input
                                    type="number"
                                    min={1}
                                    max={65535}
                                    value={draft.onvif_port}
                                    onChange={(e) => setDraft({ ...draft, onvif_port: +e.target.value || 0 })}
                                />
                            </Field>
                            <Field label="RTSP 端口">
                                <Input
                                    type="number"
                                    min={1}
                                    max={65535}
                                    value={draft.rtsp_port}
                                    onChange={(e) => setDraft({ ...draft, rtsp_port: +e.target.value || 0 })}
                                />
                            </Field>
                            <Field label="通道 ID">
                                <Input
                                    type="number"
                                    min={0}
                                    value={draft.channel_id}
                                    onChange={(e) => setDraft({ ...draft, channel_id: +e.target.value || 0 })}
                                />
                            </Field>
                        </CardContent>
                    </Card>

                    {/* 3. Credentials */}
                    <Card>
                        <CardHeader>
                            <CardTitle className="text-base">凭证</CardTitle>
                            <CardDescription>
                                静态加密存储（通过 SecretBox 使用 AES-256-GCM），任何 GET 接口都不会返回。注册中心只会将其发送到摄像头的 RTSP / ONVIF 端口。
                            </CardDescription>
                        </CardHeader>
                        <CardContent className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                            <Field label="用户名">
                                <Input
                                    value={draft.username}
                                    onChange={(e) => setDraft({ ...draft, username: e.target.value })}
                                    autoComplete="off"
                                />
                            </Field>
                            <Field label="密码" required>
                                <div className="relative">
                                    <Input
                                        type={showPassword ? "text" : "password"}
                                        value={draft.password}
                                        onChange={(e) => setDraft({ ...draft, password: e.target.value })}
                                        required
                                        autoComplete="new-password"
                                        className="pr-10"
                                    />
                                    <button
                                        type="button"
                                        onClick={() => setShowPassword((s) => !s)}
                                        className="absolute right-2 top-1/2 -translate-y-1/2 rounded p-1 text-fg-subtle hover:text-fg"
                                        aria-label={showPassword ? "隐藏密码" : "显示密码"}
                                    >
                                        {showPassword ? <EyeOff size={14} /> : <Eye size={14} />}
                                    </button>
                                </div>
                            </Field>
                        </CardContent>
                    </Card>

                    {/* 4. Capabilities + RTSP preview */}
                    <Card>
                        <CardHeader>
                            <CardTitle className="text-base">能力与预览</CardTitle>
                            <CardDescription>
                                PTZ / 音频 / 动检 标志位会在面板中用于图标门控。“转码为 H.264”会让摄像头经过 go2rtc 的 ffmpeg 管道处理 — 适用于希望通过 WebRTC 在 Chrome、Edge 或 Android 上观看的 HEVC 摄像头（这些浏览器无法在 WebRTC RTP 通道中解码 H.265）。
                            </CardDescription>
                        </CardHeader>
                        <CardContent className="space-y-4">
                            <div className="flex flex-wrap gap-3 text-sm text-fg-muted">
                                <Toggle
                                    label="PTZ"
                                    v={draft.ptz}
                                    on={(ptz) => setDraft({ ...draft, ptz })}
                                />
                                <Toggle
                                    label="音频"
                                    v={draft.audio}
                                    on={(audio) => setDraft({ ...draft, audio })}
                                />
                                <Toggle
                                    label="动检"
                                    v={draft.motion}
                                    on={(motion) => setDraft({ ...draft, motion })}
                                />
                                <Toggle
                                    label="转码为 H.264 (ffmpeg)"
                                    v={draft.transcode}
                                    on={(transcode) => setDraft({ ...draft, transcode })}
                                />
                            </div>
                            <div className="glass-subtle rounded-xl p-4">
                                <div className="mb-1 flex items-center gap-2 text-[10px] uppercase tracking-widest text-fg-subtle">
                                    <Badge variant="info" className="text-[9px]">RTSP</Badge>
                                    <span>将以下 URL 发送给 go2rtc</span>
                                </div>
                                <code className="block break-all font-mono text-xs text-fg">
                                    {rtspPreview}
                                </code>
                            </div>
                        </CardContent>
                    </Card>

                    <div className="flex flex-wrap items-center justify-end gap-2">
                        <Button
                            type="button"
                            variant="ghost"
                            onClick={() => nav("/cameras")}
                            disabled={submit.kind === "submitting"}
                        >
                            取消
                        </Button>
                        <Button type="submit" disabled={submit.kind === "submitting"}>
                            {submit.kind === "submitting" ? (
                                <Loader2 size={14} className="animate-spin" />
                            ) : null}
                            {submit.kind === "submitting" ? "注册中…" : "注册摄像头"}
                        </Button>
                    </div>
                </form>
            )}
        </div>
    );
}

function PageHeader({ onBack }: { onBack: () => void }) {
    return (
        <div className="flex items-center gap-3">
            <Button variant="ghost" size="icon" onClick={onBack} aria-label="返回摄像头列表">
                <ChevronLeft size={18} />
            </Button>
            <div>
                <h2 className="text-lg font-semibold text-fg">注册新设备</h2>
                <p className="text-xs text-fg-subtle">
                    向平台添加一个摄像头。若 ONVIF profile token 与流地址为空，将自动发现。
                </p>
            </div>
        </div>
    );
}

function Field({
    label,
    children,
    required,
    className,
}: {
    label: string;
    children: React.ReactNode;
    required?: boolean;
    className?: string;
}) {
    return (
        <label className={cn("flex flex-col gap-1", className)}>
            <span className="text-xs text-fg-muted">
                {label}
                {required && <span className="ml-0.5 text-[rgb(var(--accent-danger))]">*</span>}
            </span>
            {children}
        </label>
    );
}

function Toggle({ label, v, on }: { label: string; v: boolean; on: (v: boolean) => void }) {
    return (
        <label className="glass-subtle rounded-xl inline-flex cursor-pointer items-center gap-2 px-3 py-1.5 text-xs">
            <input type="checkbox" checked={v} onChange={(e) => on(e.target.checked)} />
            {label}
        </label>
    );
}