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
                        <Button size="sm" onClick={() => nav("/cameras/new")}>
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
