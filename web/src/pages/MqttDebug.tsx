import { useEffect, useMemo, useState, type FormEvent } from "react";
import { Radio, Send, Trash2, Loader2, CheckCircle2, AlertTriangle } from "lucide-react";
import { publishMqtt } from "@/api/system";
import { ApiError } from "@/api/client";
import { useWebSocket } from "@/hooks/useWebSocket";
import { cn } from "@/lib/utils";
import type { PublishMqttResponse } from "@/types";
import {
    Card,
    CardContent,
    CardDescription,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input, Label, Select, Textarea } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";

const QUICK_TOPICS = [
    "home-datacenter/devices/1/telemetry",
    "home-datacenter/devices/1/command",
    "home-datacenter/system/broadcast",
];

const DEFAULT_PAYLOAD = JSON.stringify({ cmd: "reboot" }, null, 2);

interface LogEntry {
    ts: number;
    type: string;
    topic?: string;
    payload?: unknown;
    ok: boolean;
    text?: string;
}

/**
 * MQTT Debug console (admin only).
 *
 * - Publish form with quick topic presets and JSON validation
 * - Live event log fed by the global WebSocket (subscribes to all
 *   topics by sending an empty-prefix subscribe)
 */
export default function MqttDebug() {
    const [topic, setTopic] = useState(QUICK_TOPICS[0]);
    const [payload, setPayload] = useState(DEFAULT_PAYLOAD);
    const [qos, setQos] = useState<0 | 1 | 2>(1);
    const [submitting, setSubmitting] = useState(false);
    const [result, setResult] = useState<PublishMqttResponse | null>(null);
    const [error, setError] = useState<string | null>(null);

    const { lastMessage, sendMessage, isConnected } = useWebSocket();
    const [log, setLog] = useState<LogEntry[]>([]);

    // Validate the payload as JSON in real time.
    const { valid, parseError } = useMemo(() => {
        try {
            JSON.parse(payload);
            return { valid: true, parseError: null as string | null };
        } catch (e) {
            return {
                valid: false,
                parseError: e instanceof Error ? e.message : "invalid JSON",
            };
        }
    }, [payload]);

    // Subscribe broadly so the log catches every event the server emits.
    useEffect(() => {
        sendMessage({ type: "subscribe", topic: "" });
    }, [sendMessage]);

    // Append incoming WS messages to the log.
    useEffect(() => {
        if (!lastMessage) return;
        setLog((prev) =>
            [
                {
                    ts: lastMessage.ts * 1000,
                    type: lastMessage.type,
                    topic: lastMessage.topic,
                    payload: lastMessage.payload,
                    ok: lastMessage.type !== "error",
                    text: lastMessage.type === "error" ? String(lastMessage.payload) : undefined,
                },
                ...prev,
            ].slice(0, 100),
        );
    }, [lastMessage]);

    async function handlePublish(e: FormEvent) {
        e.preventDefault();
        setError(null);
        setResult(null);

        if (!topic.trim()) {
            setError("topic is required");
            return;
        }
        if (!valid) {
            setError(`payload is not valid JSON: ${parseError}`);
            return;
        }

        setSubmitting(true);
        try {
            const res = await publishMqtt({
                topic: topic.trim(),
                payload,
                qos,
            });
            setResult(res);
            setLog((prev) =>
                [
                    {
                        ts: Date.now(),
                        type: "publish",
                        topic: res.topic,
                        payload: res.payload,
                        ok: true,
                        text: `published (qos=${res.qos})`,
                    },
                    ...prev,
                ].slice(0, 100),
            );
        } catch (err) {
            const msg =
                err instanceof ApiError
                    ? err.message
                    : err instanceof Error
                        ? err.message
                        : "publish failed";
            setError(msg);
        } finally {
            setSubmitting(false);
        }
    }

    return (
        <div className="animate-fade-in space-y-6">
            <div className="flex flex-wrap items-center justify-between gap-3">
                <div>
                    <h2 className="flex items-center gap-2 text-lg font-semibold text-slate-100">
                        <Radio size={18} /> MQTT Debug
                    </h2>
                    <p className="text-xs text-slate-500">
                        Publish raw messages and watch live events. Admin only.
                    </p>
                </div>
                <Badge variant={isConnected ? "success" : "outline"}>
                    <span
                        className={cn(
                            "mr-1 inline-block h-1.5 w-1.5 rounded-full",
                            isConnected ? "bg-emerald-400 animate-pulse" : "bg-slate-500",
                        )}
                    />
                    {isConnected ? "ws connected" : "ws offline"}
                </Badge>
            </div>

            <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
                {/* Publish form */}
                <Card>
                    <CardHeader>
                        <CardTitle>Publish</CardTitle>
                        <CardDescription>
                            Send a message to an MQTT topic via the broker.
                        </CardDescription>
                    </CardHeader>
                    <CardContent>
                        <form onSubmit={handlePublish} className="space-y-4">
                            <div className="space-y-2">
                                <Label htmlFor="topic">Topic</Label>
                                <Input
                                    id="topic"
                                    value={topic}
                                    onChange={(e) => setTopic(e.target.value)}
                                    className="font-mono"
                                    placeholder="home-datacenter/devices/1/command"
                                    disabled={submitting}
                                />
                                <div className="flex flex-wrap gap-1.5 pt-1">
                                    {QUICK_TOPICS.map((t) => (
                                        <button
                                            key={t}
                                            type="button"
                                            onClick={() => setTopic(t)}
                                            className={cn(
                                                "rounded-full border px-2.5 py-1 text-[11px] font-mono transition-colors",
                                                topic === t
                                                    ? "border-[rgb(var(--accent-primary)/0.5)] bg-[rgb(var(--accent-primary)/0.1)] text-sky-300"
                                                    : "border-[rgb(var(--border)/0.3)] text-slate-400 hover:border-slate-500 hover:text-slate-200",
                                            )}
                                        >
                                            {t}
                                        </button>
                                    ))}
                                </div>
                            </div>

                            <div className="space-y-2">
                                <Label htmlFor="payload">
                                    Payload{" "}
                                    <span
                                        className={cn(
                                            "ml-2 inline-flex items-center gap-1 normal-case tracking-normal",
                                            valid ? "text-emerald-400" : "text-rose-400",
                                        )}
                                    >
                                        {valid ? (
                                            <>
                                                <CheckCircle2 size={11} /> valid JSON
                                            </>
                                        ) : (
                                            <>
                                                <AlertTriangle size={11} /> invalid
                                            </>
                                        )}
                                    </span>
                                </Label>
                                <Textarea
                                    id="payload"
                                    value={payload}
                                    onChange={(e) => setPayload(e.target.value)}
                                    className={cn(
                                        "min-h-[140px]",
                                        !valid && "border-rose-500/60 focus-visible:ring-rose-500",
                                    )}
                                    spellCheck={false}
                                    disabled={submitting}
                                />
                                {parseError && (
                                    <p className="font-mono text-[11px] text-rose-400">
                                        {parseError}
                                    </p>
                                )}
                            </div>

                            <div className="flex items-end gap-3">
                                <div className="w-28 space-y-2">
                                    <Label htmlFor="qos">QoS</Label>
                                    <Select
                                        id="qos"
                                        value={qos}
                                        onChange={(e) =>
                                            setQos(Number(e.target.value) as 0 | 1 | 2)
                                        }
                                        disabled={submitting}
                                    >
                                        <option value={0}>0</option>
                                        <option value={1}>1</option>
                                        <option value={2}>2</option>
                                    </Select>
                                </div>
                                <Button
                                    type="submit"
                                    disabled={submitting || !valid}
                                    className="flex-1"
                                >
                                    {submitting ? (
                                        <>
                                            <Loader2 size={16} className="animate-spin" />
                                            Publishing…
                                        </>
                                    ) : (
                                        <>
                                            <Send size={16} />
                                            Publish
                                        </>
                                    )}
                                </Button>
                            </div>

                            {error && (
                                <div className="rounded-xl glass bg-[rgb(var(--accent-danger)/0.1)] px-4 py-3 text-xs text-[rgb(var(--accent-danger))]">
                                    {error}
                                </div>
                            )}
                            {result && (
                                <div className="rounded-xl glass bg-[rgb(var(--accent-success)/0.1)] px-4 py-3 text-xs text-[rgb(var(--accent-success))]">
                                    Published to <code className="font-mono">{result.topic}</code>{" "}
                                    (qos {result.qos}).
                                </div>
                            )}
                        </form>
                    </CardContent>
                </Card>

                {/* Event log */}
                <Card className="flex flex-col">
                    <CardHeader className="flex-row items-center justify-between">
                        <div>
                            <CardTitle>Event log</CardTitle>
                            <CardDescription>
                                Live WebSocket messages (newest first).
                            </CardDescription>
                        </div>
                        <Button
                            variant="ghost"
                            size="sm"
                            onClick={() => setLog([])}
                            disabled={log.length === 0}
                        >
                            <Trash2 size={14} />
                            Clear
                        </Button>
                    </CardHeader>
                    <CardContent className="flex-1">
                        <div className="glass-subtle rounded-xl h-[420px] overflow-y-auto p-2">
                            {log.length === 0 ? (
                                <p className="p-4 text-xs text-slate-500">
                                    No events yet. The server pushes heartbeat, online_list, and
                                    device.* events here automatically.
                                </p>
                            ) : (
                                <ul className="space-y-1">
                                    {log.map((entry, i) => (
                                        <li
                                            key={`${entry.ts}-${i}`}
                                            className="glass-subtle rounded-lg px-2 py-1.5 font-mono text-[11px]"
                                        >
                                            <div className="flex items-center gap-2">
                                                <span className="text-slate-500">
                                                    {new Date(entry.ts).toLocaleTimeString()}
                                                </span>
                                                <Badge
                                                    variant={
                                                        entry.ok
                                                            ? entry.type === "publish"
                                                                ? "info"
                                                                : "default"
                                                            : "danger"
                                                    }
                                                    className="text-[10px]"
                                                >
                                                    {entry.type}
                                                </Badge>
                                                {entry.topic && (
                                                    <span className="text-slate-400">{entry.topic}</span>
                                                )}
                                            </div>
                                            {entry.text && (
                                                <div className="mt-0.5 text-slate-400">{entry.text}</div>
                                            )}
                                            {entry.payload != null && (
                                                <pre className="mt-1 overflow-x-auto whitespace-pre-wrap break-all text-slate-500">
                                                    {typeof entry.payload === "string"
                                                        ? entry.payload
                                                        : JSON.stringify(entry.payload, null, 2)}
                                                </pre>
                                            )}
                                        </li>
                                    ))}
                                </ul>
                            )}
                        </div>
                    </CardContent>
                </Card>
            </div>
        </div>
    );
}