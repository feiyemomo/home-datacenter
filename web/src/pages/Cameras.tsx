import { useCallback, useEffect, useState } from "react";
import { Camera as CameraIcon, Plus, Trash2, RefreshCcw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { listCameras, registerCamera, deleteCamera } from "@/api/camera";
import type { Camera, WsMessage } from "@/types";
import { useAuth } from "@/hooks/useAuth";
import { useWebSocket } from "@/hooks/useWebSocket";
import { LiveVideo } from "@/components/LiveVideo";

interface DraftCam {
    name: string;
    host: string;
    vendor: string;
    onvif_port: number;
    rtsp_port: number;
    channel_id: number;
    username: string;
    password: string;
    ptz: boolean;
    audio: boolean;
    motion: boolean;
}

const EMPTY_DRAFT: DraftCam = {
    name: "",
    host: "",
    vendor: "hikvision",
    onvif_port: 80,
    rtsp_port: 554,
    channel_id: 101,
    username: "admin",
    password: "",
    ptz: true,
    audio: true,
    motion: true,
};

export default function Cameras() {
    const { isAdmin } = useAuth();
    const [cams, setCams] = useState<Camera[]>([]);
    const [loading, setLoading] = useState(false);
    const [showForm, setShowForm] = useState(false);
    const [draft, setDraft] = useState<DraftCam>(EMPTY_DRAFT);
    const [error, setError] = useState<string | null>(null);

    const refresh = useCallback(async () => {
        setLoading(true);
        setError(null);
        try {
            const list = await listCameras();
            setCams(list);
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        } finally {
            setLoading(false);
        }
    }, []);

    useEffect(() => {
        void refresh();
    }, [refresh]);

    async function submit() {
        setError(null);
        try {
            await registerCamera(draft);
            setDraft(EMPTY_DRAFT);
            setShowForm(false);
            await refresh();
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        }
    }

    async function remove(id: number) {
        if (!isAdmin) return;
        if (!confirm(`Delete camera ${id}?`)) return;
        try {
            await deleteCamera(id);
            await refresh();
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        }
    }

    return (
        <div className="space-y-4">
            <div className="flex items-center justify-between">
                <div className="flex items-center gap-2">
                    <CameraIcon className="h-5 w-5 text-sky-300" />
                    <h2 className="text-lg font-semibold text-slate-100">Cameras</h2>
                    <Badge variant="outline">{cams.length}</Badge>
                </div>
                <div className="flex gap-2">
                    <Button size="sm" variant="outline" onClick={refresh} disabled={loading}>
                        <RefreshCcw size={14} className="mr-1" />
                        Refresh
                    </Button>
                    {isAdmin && (
                        <Button size="sm" onClick={() => setShowForm((s) => !s)}>
                            <Plus size={14} className="mr-1" />
                            Register
                        </Button>
                    )}
                </div>
            </div>

            {error && (
                <div className="rounded-md border border-rose-500/40 bg-rose-500/10 px-3 py-2 text-sm text-rose-200">
                    {error}
                </div>
            )}

            {showForm && isAdmin && (
                <Card>
                    <CardHeader>
                        <CardTitle className="text-sm">Register camera</CardTitle>
                    </CardHeader>
                    <CardContent>
                        <form
                            className="grid grid-cols-2 gap-3 text-sm"
                            onSubmit={(e) => { e.preventDefault(); void submit(); }}
                        >
                            <Field label="Name">
                                <Input value={draft.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} required />
                            </Field>
                            <Field label="Vendor">
                                <Input value={draft.vendor} onChange={(e) => setDraft({ ...draft, vendor: e.target.value })} />
                            </Field>
                            <Field label="Host">
                                <Input placeholder="192.168.31.100" value={draft.host} onChange={(e) => setDraft({ ...draft, host: e.target.value })} required />
                            </Field>
                            <Field label="ONVIF port">
                                <Input type="number" value={draft.onvif_port} onChange={(e) => setDraft({ ...draft, onvif_port: +e.target.value })} />
                            </Field>
                            <Field label="RTSP port">
                                <Input type="number" value={draft.rtsp_port} onChange={(e) => setDraft({ ...draft, rtsp_port: +e.target.value })} />
                            </Field>
                            <Field label="Channel (Hik: 101/201)">
                                <Input type="number" value={draft.channel_id} onChange={(e) => setDraft({ ...draft, channel_id: +e.target.value })} />
                            </Field>
                            <Field label="Username">
                                <Input value={draft.username} onChange={(e) => setDraft({ ...draft, username: e.target.value })} />
                            </Field>
                            <Field label="Password">
                                <Input type="password" value={draft.password} onChange={(e) => setDraft({ ...draft, password: e.target.value })} required />
                            </Field>
                            <div className="col-span-2 flex flex-wrap gap-3 text-xs text-slate-300">
                                <Toggle label="PTZ" v={draft.ptz} on={(v) => setDraft({ ...draft, ptz: v })} />
                                <Toggle label="Audio" v={draft.audio} on={(v) => setDraft({ ...draft, audio: v })} />
                                <Toggle label="Motion" v={draft.motion} on={(v) => setDraft({ ...draft, motion: v })} />
                            </div>
                            <div className="col-span-2 flex gap-2">
                                <Button type="submit">Register</Button>
                                <Button type="button" variant="ghost" onClick={() => setShowForm(false)}>Cancel</Button>
                            </div>
                        </form>
                    </CardContent>
                </Card>
            )}

            <WsBridge>
                {(onMsg) => (
                    <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
                        {cams.map((cam) => (
                            <CamCard
                                key={cam.id}
                                cam={cam}
                                isAdmin={isAdmin}
                                onDelete={() => remove(cam.id)}
                                onWsMessage={onMsg}
                            />
                        ))}
                        {cams.length === 0 && !loading && (
                            <div className="col-span-full rounded-md border border-dashed border-slate-700 p-8 text-center text-sm text-slate-500">
                                No cameras registered. {isAdmin ? "Click Register." : "Ask an admin to register one."}
                            </div>
                        )}
                    </div>
                )}
            </WsBridge>
        </div>
    );
}

function CamCard({
    cam,
    isAdmin,
    onDelete,
    onWsMessage,
}: {
    cam: Camera;
    isAdmin: boolean;
    onDelete: () => void;
    onWsMessage: (handler: (m: WsMessage) => void) => () => void;
}) {
    return (
        <div className="relative">
            <LiveVideo camera={cam} isAdmin={isAdmin} onWsMessage={onWsMessage} />
            {isAdmin && (
                <button
                    className="absolute right-2 top-2 rounded-md bg-rose-500/20 p-1.5 text-rose-300 ring-1 ring-rose-500/30 hover:bg-rose-500/30"
                    onClick={onDelete}
                    aria-label="Delete camera"
                >
                    <Trash2 size={14} />
                </button>
            )}
        </div>
    );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
    return (
        <label className="flex flex-col gap-1">
            <span className="text-xs text-slate-400">{label}</span>
            {children}
        </label>
    );
}

function Toggle({ label, v, on }: { label: string; v: boolean; on: (v: boolean) => void }) {
    return (
        <label className="inline-flex items-center gap-2">
            <input type="checkbox" checked={v} onChange={(e) => on(e.target.checked)} />
            {label}
        </label>
    );
}

/**
 * WsBridge — child-as-render hook wrapper. The Cameras page owns
 * one WebSocket connection, and each CamCard subscribes to its own
 * "device.<id>" topic via the passed-down callback.
 */
function WsBridge({ children }: { children: (onMsg: (h: (m: WsMessage) => void) => () => void) => React.ReactNode }) {
    const ws = useWebSocket(true);
    const [last, setLast] = useState<WsMessage | null>(null);

    useEffect(() => {
        if (ws.lastMessage) setLast(ws.lastMessage);
    }, [ws.lastMessage]);

    // Provide each consumer a stable "give-me-the-latest-message"
    // hook. The actual message routing happens inside LiveVideo
    // (filters by device_id).
    const onMsg = useCallback(
        (h: (m: WsMessage) => void) => {
            // Force a re-render trigger by reading `last` so the
            // consumer re-subscribes only on identity change.
            void last;
            h(ws.lastMessage ?? { type: "noop", ts: 0 });
            // The page is the single WS owner; the consumer just
            // polls the latest message via the same channel. No
            // explicit unsubscribe is required.
            return () => undefined;
        },
        [last, ws.lastMessage],
    );

    return <>{children(onMsg)}</>;
}
