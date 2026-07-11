import { useCallback, useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Camera as CameraIcon, Plus, Trash2, RefreshCcw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { listCameras, deleteCamera } from "@/api/camera";
import type { Camera, WsMessage } from "@/types";
import { useAuth } from "@/hooks/useAuth";
import { useWebSocket } from "@/hooks/useWebSocket";
import { LiveVideo } from "@/components/LiveVideo";

/**
 * Cameras — list + live view + delete.
 *
 * Registration has moved to a dedicated page (/cameras/new,
 * DeviceCreate.tsx). The list page is now strictly for *browsing*
 * — the operator can refresh, watch live, and remove a camera, but
 * not stand up a new one inline. This keeps the cards above the
 * fold and gives the create flow its own URL to bookmark / share.
 */
export default function Cameras() {
    const { isAdmin } = useAuth();
    const nav = useNavigate();
    const [cams, setCams] = useState<Camera[]>([]);
    const [loading, setLoading] = useState(false);
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
                    <CameraIcon className="h-5 w-5 text-sky-600 dark:text-sky-300" />
                    <h2 className="text-lg font-semibold text-fg">Cameras</h2>
                    <Badge variant="outline">{cams.length}</Badge>
                </div>
                <div className="flex gap-2">
                    <Button size="sm" variant="outline" onClick={refresh} disabled={loading}>
                        <RefreshCcw size={14} className="mr-1" />
                        Refresh
                    </Button>
                    {isAdmin && (
                        <Button size="sm" onClick={() => nav("/cameras/new")}>
                            <Plus size={14} className="mr-1" />
                            Register
                        </Button>
                    )}
                </div>
            </div>

            {error && (
                <div className="rounded-md border border-rose-500/40 bg-rose-500/10 px-3 py-2 text-sm text-rose-700 dark:text-rose-200">
                    {error}
                </div>
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
                            <div className="col-span-full rounded-md border border-dashed border-surface-border p-8 text-center text-sm text-fg-muted">
                                No cameras registered. {isAdmin ? "Click Register to add one." : "Ask an admin to register one."}
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
    const statusVariant =
        cam.status === "online"
            ? "success"
            : cam.status === "offline"
                ? "danger"
                : "warning";

    return (
        <div className="flex flex-col overflow-hidden rounded-xl border border-surface-border bg-surface-raised shadow-sm shadow-black/5 transition-shadow hover:shadow-md hover:shadow-black/10 dark:bg-surface-raised/90 dark:shadow-black/20 dark:hover:shadow-black/30">
            {/* Header */}
            <div className="flex items-center justify-between gap-3 border-b border-surface-border bg-surface-subtle/40 px-4 py-3">
                <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                        <h3 className="truncate text-sm font-semibold text-fg">
                            {cam.name}
                        </h3>
                        {cam.transcode && (
                            <Badge variant="info" className="shrink-0 text-[9px]">x264</Badge>
                        )}
                    </div>
                    <p className="truncate text-[11px] text-fg-muted">
                        {cam.vendor} · {cam.host}
                    </p>
                </div>
                <div className="flex shrink-0 items-center gap-2">
                    <Badge variant={statusVariant} className="text-[10px] capitalize">
                        {cam.status}
                    </Badge>
                    {isAdmin && (
                        <button
                            className="rounded-md p-1.5 text-fg-subtle transition-colors hover:bg-rose-500/10 hover:text-rose-600 dark:hover:text-rose-300"
                            onClick={onDelete}
                            aria-label="Delete camera"
                            title="Delete camera"
                        >
                            <Trash2 size={14} />
                        </button>
                    )}
                </div>
            </div>

            {/* Video */}
            <div className="relative aspect-video bg-black">
                <LiveVideo camera={cam} isAdmin={isAdmin} onWsMessage={onWsMessage} />
            </div>
        </div>
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
