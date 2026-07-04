import { useCallback, useEffect, useRef, useState } from "react";
import { getToken } from "@/api/client";
import { decodeJwtPayload } from "@/lib/utils";
import type { JwtClaims, WsMessage } from "@/types";

interface UseWebSocketResult {
    /** The most recent parsed message (or null). */
    lastMessage: WsMessage | null;
    /** Send a raw object over the socket. Returns false if not connected. */
    sendMessage: (msg: Record<string, unknown>) => boolean;
    /** Subscribe to a topic prefix (server-side filter). */
    subscribe: (topic: string) => void;
    /** Stop receiving a topic prefix. */
    unsubscribe: (topic: string) => void;
    isConnected: boolean;
    /** Number of reconnect attempts since the last successful connection. */
    reconnectCount: number;
}

const MAX_RETRIES = 5;
const RECONNECT_DELAY_MS = 3000;
const HEARTBEAT_INTERVAL_MS = 25000;

/**
 * WebSocket hook with auto-reconnect and heartbeat.
 *
 * - Connects on mount if a token exists; uses /api/v1/ws?token=<jwt>
 * - Auto-reconnects on close (3s delay, max 5 retries)
 * - Sends a heartbeat every 25s to keep the server's pong timer happy
 * - Parses incoming JSON into the WsMessage envelope
 *
 * The browser handles protocol-level ping/pong automatically; this
 * hook's heartbeat is the application-level keepalive.
 */
export function useWebSocket(autoConnect = true): UseWebSocketResult {
    const [lastMessage, setLastMessage] = useState<WsMessage | null>(null);
    const [isConnected, setIsConnected] = useState(false);
    const [reconnectCount, setReconnectCount] = useState(0);

    const socketRef = useRef<WebSocket | null>(null);
    const retriesRef = useRef(0);
    const reconnectTimerRef = useRef<number | null>(null);
    const heartbeatTimerRef = useRef<number | null>(null);
    const manualCloseRef = useRef(false);

    const clearTimers = useCallback(() => {
        if (reconnectTimerRef.current !== null) {
            window.clearTimeout(reconnectTimerRef.current);
            reconnectTimerRef.current = null;
        }
        if (heartbeatTimerRef.current !== null) {
            window.clearInterval(heartbeatTimerRef.current);
            heartbeatTimerRef.current = null;
        }
    }, []);

    const startHeartbeat = useCallback(() => {
        if (heartbeatTimerRef.current !== null) return;
        heartbeatTimerRef.current = window.setInterval(() => {
            const sock = socketRef.current;
            if (sock && sock.readyState === WebSocket.OPEN) {
                sock.send(JSON.stringify({ type: "heartbeat" }));
            }
        }, HEARTBEAT_INTERVAL_MS);
    }, []);

    const connect = useCallback(() => {
        const token = getToken();
        if (!token) return;

        // Don't stack connections.
        if (socketRef.current) {
            socketRef.current.close();
            socketRef.current = null;
        }

        const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
        // The Vite dev proxy forwards /api/v1/ws to the backend with ws:true.
        const url = `${proto}//${window.location.host}/api/v1/ws?token=${encodeURIComponent(
            token,
        )}`;

        let socket: WebSocket;
        try {
            socket = new WebSocket(url);
        } catch {
            scheduleReconnect();
            return;
        }
        socketRef.current = socket;

        socket.onopen = () => {
            retriesRef.current = 0;
            setReconnectCount(0);
            setIsConnected(true);
            startHeartbeat();
        };

        socket.onmessage = (event) => {
            if (typeof event.data !== "string") return;
            try {
                const parsed = JSON.parse(event.data) as WsMessage;
                setLastMessage(parsed);
            } catch {
                // Ignore malformed frames; the server only sends JSON.
            }
        };

        socket.onerror = () => {
            // The close handler will run next and trigger a reconnect.
        };

        socket.onclose = () => {
            setIsConnected(false);
            clearTimers();
            socketRef.current = null;
            if (!manualCloseRef.current) {
                scheduleReconnect();
            }
        };
    }, [clearTimers, startHeartbeat]);

    const scheduleReconnect = useCallback(() => {
        if (retriesRef.current >= MAX_RETRIES) return;
        if (reconnectTimerRef.current !== null) return;
        retriesRef.current += 1;
        setReconnectCount(retriesRef.current);
        reconnectTimerRef.current = window.setTimeout(() => {
            reconnectTimerRef.current = null;
            connect();
        }, RECONNECT_DELAY_MS);
    }, [connect]);

    const sendMessage = useCallback(
        (msg: Record<string, unknown>): boolean => {
            const sock = socketRef.current;
            if (!sock || sock.readyState !== WebSocket.OPEN) return false;
            sock.send(JSON.stringify(msg));
            return true;
        },
        [],
    );

    const subscribe = useCallback(
        (topic: string) => sendMessage({ type: "subscribe", topic }),
        [sendMessage],
    );

    const unsubscribe = useCallback(
        (topic: string) => sendMessage({ type: "unsubscribe", topic }),
        [sendMessage],
    );

    // Mount: connect if we have a token. Reconnect when the token changes.
    useEffect(() => {
        if (!autoConnect) return;
        const token = getToken();
        if (!token) return;

        // Verify the token isn't obviously expired before connecting.
        const claims = decodeJwtPayload<JwtClaims>(token);
        if (claims?.exp && claims.exp * 1000 < Date.now()) return;

        manualCloseRef.current = false;
        connect();

        return () => {
            manualCloseRef.current = true;
            clearTimers();
            if (socketRef.current) {
                socketRef.current.close();
                socketRef.current = null;
            }
        };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [autoConnect]);

    return {
        lastMessage,
        sendMessage,
        subscribe,
        unsubscribe,
        isConnected,
        reconnectCount,
    };
}
