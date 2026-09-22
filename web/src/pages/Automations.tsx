import { useEffect, useState } from "react";
import {
    Zap,
    Plus,
    RefreshCw,
    Play,
    Trash2,
    Edit,
    CheckCircle2,
    AlertCircle,
    Clock,
    Activity,
    Bell,
    Radio,
    Globe,
    X,
    Loader2,
} from "lucide-react";
import {
    listRules,
    createRule,
    updateRule,
    deleteRule,
    testRule,
    type CreateRulePayload,
} from "@/api/automation";
import { formatDateTime, cn } from "@/lib/utils";
import type { AutomationRule, RuleAction } from "@/types";
import {
    Card,
    CardContent,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input, Label, Select } from "@/components/ui/input";

const PRESETS: Array<{
    label: string;
    rule: CreateRulePayload;
}> = [
    {
        label: "🌙 夜间人形入侵通知",
        rule: {
            name: "夜间人形入侵告警",
            trigger: "camera.motion",
            condition: {
                time_gte: "22:00",
                time_lte: "06:00",
                payload_eq: { label: "person" },
            },
            action: {
                type: "notify",
                title: "⚠️ 夜间人形入侵告警",
                body: "安防摄像头在夜间检测到可疑人员走动，请及时确认画面",
            },
            throttle: {
                cooldown_s: 60,
                dedup: true,
            },
        },
    },
    {
        label: "🚨 监控设备离线告警",
        rule: {
            name: "摄像头离线紧急通知",
            trigger: "camera.offline",
            action: {
                type: "notify",
                title: "🔴 监控摄像头离线",
                body: "检测到安防摄像头掉线，请检查供电或网络连接",
            },
            throttle: {
                cooldown_s: 300,
            },
        },
    },
    {
        label: "🛡️ 离家布防模式自动告警",
        rule: {
            name: "离家模式安防触发",
            trigger: "security.guard_mode",
            condition: {
                payload_eq: { mode: "armed_away" },
            },
            action: {
                type: "mqtt",
                topic: "home-datacenter/devices/1/command",
                payload: JSON.stringify({ cmd: "arm_security", status: "active" }),
                qos: 1,
            },
            throttle: {
                cooldown_s: 10,
            },
        },
    },
];

export default function Automations() {
    const [rules, setRules] = useState<AutomationRule[]>([]);
    const [loading, setLoading] = useState(true);
    const [refreshing, setRefreshing] = useState(false);
    const [feedback, setFeedback] = useState<{ type: "success" | "error"; text: string } | null>(null);

    // Modal state
    const [modalOpen, setModalOpen] = useState(false);
    const [editingRule, setEditingRule] = useState<AutomationRule | null>(null);
    const [testingId, setTestingId] = useState<number | null>(null);
    const [submitting, setSubmitting] = useState(false);

    // Form fields
    const [formName, setFormName] = useState("");
    const [formTrigger, setFormTrigger] = useState("camera.motion");
    const [formTimeGte, setFormTimeGte] = useState("");
    const [formTimeLte, setFormTimeLte] = useState("");
    const [formActionType, setFormActionType] = useState<"notify" | "mqtt" | "webhook">("notify");
    const [formNotifyTitle, setFormNotifyTitle] = useState("");
    const [formNotifyBody, setFormNotifyBody] = useState("");
    const [formMqttTopic, setFormMqttTopic] = useState("home-datacenter/devices/1/command");
    const [formMqttPayload, setFormMqttPayload] = useState('{"cmd":"alert"}');
    const [formWebhookUrl, setFormWebhookUrl] = useState("");
    const [formCooldown, setFormCooldown] = useState(60);

    const loadRules = async () => {
        try {
            const data = await listRules();
            setRules(data);
        } catch (err: any) {
            setFeedback({ type: "error", text: err?.message || "加载自动化规则失败" });
        } finally {
            setLoading(false);
            setRefreshing(false);
        }
    };

    useEffect(() => {
        loadRules();
    }, []);

    const handleRefresh = () => {
        setRefreshing(true);
        loadRules();
    };

    const handleToggle = async (rule: AutomationRule) => {
        try {
            const next = !rule.enabled;
            await updateRule(rule.id, { enabled: next });
            setRules((prev) =>
                prev.map((r) => (r.id === rule.id ? { ...r, enabled: next } : r))
            );
            setFeedback({
                type: "success",
                text: `规则 "${rule.name}" 已${next ? "启用" : "停用"}`,
            });
        } catch (err: any) {
            setFeedback({ type: "error", text: err?.message || "切换规则状态失败" });
        }
    };

    const handleTest = async (rule: AutomationRule) => {
        setTestingId(rule.id);
        try {
            const res = await testRule(rule.id);
            setFeedback({
                type: "success",
                text: `测试触发成功：规则 "${res.name}" 执行动作 [${res.action}] 完成`,
            });
        } catch (err: any) {
            setFeedback({ type: "error", text: err?.message || "测试触发执行失败" });
        } finally {
            setTestingId(null);
        }
    };

    const handleDelete = async (rule: AutomationRule) => {
        if (!window.confirm(`确定要删除自动化规则 "${rule.name}" 吗？此操作不可恢复。`)) {
            return;
        }
        try {
            await deleteRule(rule.id);
            setRules((prev) => prev.filter((r) => r.id !== rule.id));
            setFeedback({ type: "success", text: `规则 "${rule.name}" 已删除` });
        } catch (err: any) {
            setFeedback({ type: "error", text: err?.message || "删除规则失败" });
        }
    };

    const openCreateModal = () => {
        setEditingRule(null);
        setFormName("");
        setFormTrigger("camera.motion");
        setFormTimeGte("");
        setFormTimeLte("");
        setFormActionType("notify");
        setFormNotifyTitle("AI 检测安防告警");
        setFormNotifyBody("监控摄像头检测到人形移动");
        setFormMqttTopic("home-datacenter/devices/1/command");
        setFormMqttPayload('{"cmd":"alert"}');
        setFormWebhookUrl("");
        setFormCooldown(60);
        setModalOpen(true);
    };

    const openEditModal = (rule: AutomationRule) => {
        setEditingRule(rule);
        setFormName(rule.name);
        setFormTrigger(rule.trigger);
        setFormTimeGte(rule.condition?.time_gte || "");
        setFormTimeLte(rule.condition?.time_lte || "");
        setFormActionType((rule.action.type as any) || "notify");
        setFormNotifyTitle(rule.action.title || "");
        setFormNotifyBody(rule.action.body || "");
        setFormMqttTopic(rule.action.topic || "home-datacenter/devices/1/command");
        setFormMqttPayload(rule.action.payload || "{}");
        setFormWebhookUrl(rule.action.url || "");
        setFormCooldown(rule.throttle?.cooldown_s ?? 60);
        setModalOpen(true);
    };

    const applyPreset = (preset: typeof PRESETS[0]) => {
        const r = preset.rule;
        setFormName(r.name);
        setFormTrigger(r.trigger);
        setFormTimeGte(r.condition?.time_gte || "");
        setFormTimeLte(r.condition?.time_lte || "");
        setFormActionType((r.action.type as any) || "notify");
        setFormNotifyTitle(r.action.title || "");
        setFormNotifyBody(r.action.body || "");
        setFormMqttTopic(r.action.topic || "home-datacenter/devices/1/command");
        setFormMqttPayload(r.action.payload || "{}");
        setFormWebhookUrl(r.action.url || "");
        setFormCooldown(r.throttle?.cooldown_s ?? 60);
    };

    const handleSubmit = async (e: React.FormEvent) => {
        e.preventDefault();
        if (!formName.trim()) {
            setFeedback({ type: "error", text: "请输入规则名称" });
            return;
        }

        const action: RuleAction = { type: formActionType };
        if (formActionType === "notify") {
            action.title = formNotifyTitle;
            action.body = formNotifyBody;
        } else if (formActionType === "mqtt") {
            action.topic = formMqttTopic;
            action.payload = formMqttPayload;
            action.qos = 1;
        } else if (formActionType === "webhook") {
            action.url = formWebhookUrl;
            action.method = "POST";
        }

        const condition: any = {};
        if (formTimeGte) condition.time_gte = formTimeGte;
        if (formTimeLte) condition.time_lte = formTimeLte;

        setSubmitting(true);
        try {
            if (editingRule) {
                const updated = await updateRule(editingRule.id, {
                    name: formName,
                    trigger: formTrigger,
                    condition,
                    action,
                    throttle: { cooldown_s: Number(formCooldown) },
                });
                setRules((prev) =>
                    prev.map((r) => (r.id === editingRule.id ? updated : r))
                );
                setFeedback({ type: "success", text: `规则 "${updated.name}" 已保存` });
            } else {
                const created = await createRule({
                    name: formName,
                    trigger: formTrigger,
                    condition,
                    action,
                    throttle: { cooldown_s: Number(formCooldown) },
                });
                setRules((prev) => [created, ...prev]);
                setFeedback({ type: "success", text: `新规则 "${created.name}" 已创建` });
            }
            setModalOpen(false);
        } catch (err: any) {
            setFeedback({ type: "error", text: err?.message || "保存规则失败" });
        } finally {
            setSubmitting(false);
        }
    };

    return (
        <div className="space-y-6 animate-fade-in p-2 md:p-4">
            {/* Header banner */}
            <div className="flex flex-col md:flex-row md:items-center justify-between gap-4">
                <div>
                    <h1 className="text-2xl font-bold tracking-tight text-fg flex items-center gap-2">
                        <Zap className="text-[rgb(var(--accent-primary))]" size={26} />
                        智能联动与自动化
                    </h1>
                    <p className="text-sm text-fg-muted mt-1">
                        实时事件总线驱动：联动 AI 目标检测、安防布撤防模式、设备状态与 MQTT 物联网指令
                    </p>
                </div>
                <div className="flex items-center gap-3">
                    <Button
                        variant="outline"
                        size="sm"
                        onClick={handleRefresh}
                        disabled={refreshing}
                        className="gap-1.5"
                    >
                        <RefreshCw size={14} className={refreshing ? "animate-spin" : ""} />
                        刷新
                    </Button>
                    <Button
                        size="sm"
                        onClick={openCreateModal}
                        className="gap-1.5 bg-gradient-to-r from-[rgb(var(--accent-primary))] to-[rgb(var(--accent-warm))] text-white shadow-md hover:brightness-110"
                    >
                        <Plus size={16} />
                        新建联动规则
                    </Button>
                </div>
            </div>

            {/* Notification alert banner */}
            {feedback && (
                <div
                    className={cn(
                        "flex items-center justify-between p-3.5 rounded-xl border text-sm transition-all",
                        feedback.type === "success"
                            ? "bg-emerald-500/10 border-emerald-500/20 text-emerald-600 dark:text-emerald-400"
                            : "bg-red-500/10 border-red-500/20 text-red-600 dark:text-red-400"
                    )}
                >
                    <div className="flex items-center gap-2">
                        {feedback.type === "success" ? (
                            <CheckCircle2 size={16} />
                        ) : (
                            <AlertCircle size={16} />
                        )}
                        <span>{feedback.text}</span>
                    </div>
                    <button
                        onClick={() => setFeedback(null)}
                        className="text-fg-subtle hover:text-fg transition-colors"
                    >
                        <X size={14} />
                    </button>
                </div>
            )}

            {/* Rules list */}
            {loading ? (
                <div className="flex flex-col items-center justify-center p-12 text-fg-muted space-y-3">
                    <Loader2 size={32} className="animate-spin text-[rgb(var(--accent-primary))]" />
                    <p className="text-sm">正在加载自动化规则...</p>
                </div>
            ) : rules.length === 0 ? (
                <Card className="p-8 text-center border-dashed">
                    <CardContent className="space-y-4 pt-4">
                        <div className="w-12 h-12 rounded-full bg-[rgb(var(--accent-primary)/0.1)] text-[rgb(var(--accent-primary))] flex items-center justify-center mx-auto">
                            <Zap size={24} />
                        </div>
                        <div className="space-y-1">
                            <h3 className="font-semibold text-fg">暂无自动化规则</h3>
                            <p className="text-sm text-fg-muted max-w-sm mx-auto">
                                创建您的第一条规则，当摄像头 AI 检测到异动或设备状态发生变化时自动执行指定动作。
                            </p>
                        </div>
                        <Button onClick={openCreateModal} size="sm" className="gap-2">
                            <Plus size={15} /> 立即新建
                        </Button>
                    </CardContent>
                </Card>
            ) : (
                <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
                    {rules.map((rule) => {
                        const isTesting = testingId === rule.id;
                        return (
                            <Card
                                key={rule.id}
                                className={cn(
                                    "relative transition-all duration-200 hover:shadow-lg border",
                                    !rule.enabled && "opacity-60 bg-[rgb(var(--bg-subtle)/0.2)]"
                                )}
                            >
                                <CardHeader className="pb-3 flex flex-row items-start justify-between space-y-0">
                                    <div className="space-y-1 min-w-0 flex-1 pr-2">
                                        <CardTitle className="text-base font-semibold truncate text-fg flex items-center gap-2">
                                            <span>{rule.name}</span>
                                        </CardTitle>
                                        <div className="flex flex-wrap items-center gap-1.5 pt-1">
                                            <Badge variant="outline" className="text-[10px] px-1.5 py-0 font-mono">
                                                {rule.trigger}
                                            </Badge>
                                            {rule.action.type === "notify" && (
                                                <Badge variant="info" className="text-[10px] px-1.5 py-0 gap-1">
                                                    <Bell size={10} /> 系统通知
                                                </Badge>
                                            )}
                                            {rule.action.type === "mqtt" && (
                                                <Badge variant="info" className="text-[10px] px-1.5 py-0 gap-1">
                                                    <Radio size={10} /> MQTT 下发
                                                </Badge>
                                            )}
                                            {rule.action.type === "webhook" && (
                                                <Badge variant="info" className="text-[10px] px-1.5 py-0 gap-1">
                                                    <Globe size={10} /> Webhook
                                                </Badge>
                                            )}
                                        </div>
                                    </div>
                                    <button
                                        onClick={() => handleToggle(rule)}
                                        className={cn(
                                            "relative inline-flex h-5 w-10 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none",
                                            rule.enabled
                                                ? "bg-[rgb(var(--accent-primary))]"
                                                : "bg-slate-300 dark:bg-slate-700"
                                        )}
                                        title={rule.enabled ? "点击停用" : "点击启用"}
                                    >
                                        <span
                                            className={cn(
                                                "pointer-events-none inline-block h-4 w-4 transform rounded-full bg-white shadow ring-0 transition duration-200 ease-in-out",
                                                rule.enabled ? "translate-x-5" : "translate-x-0"
                                            )}
                                        />
                                    </button>
                                </CardHeader>

                                <CardContent className="space-y-3 text-xs text-fg-muted">
                                    {/* Condition & Action description */}
                                    <div className="rounded-lg bg-[rgb(var(--bg-subtle)/0.4)] p-2.5 space-y-1.5">
                                        <div className="flex items-center justify-between">
                                            <span className="text-fg-subtle">触发条件：</span>
                                            <span className="font-mono text-fg font-medium">
                                                {rule.condition?.time_gte && rule.condition?.time_lte
                                                    ? `时间 ${rule.condition.time_gte}-${rule.condition.time_lte}`
                                                    : "全时段生效"}
                                            </span>
                                        </div>
                                        <div className="flex items-center justify-between">
                                            <span className="text-fg-subtle">执行目标：</span>
                                            <span className="truncate max-w-[180px] text-fg font-medium">
                                                {rule.action.type === "notify"
                                                    ? rule.action.title || "推送通知"
                                                    : rule.action.type === "mqtt"
                                                    ? rule.action.topic
                                                    : rule.action.url}
                                            </span>
                                        </div>
                                        {rule.throttle?.cooldown_s ? (
                                            <div className="flex items-center justify-between">
                                                <span className="text-fg-subtle">防抖冷却：</span>
                                                <span className="text-fg">{rule.throttle.cooldown_s} 秒</span>
                                            </div>
                                        ) : null}
                                    </div>

                                    {/* Stats line */}
                                    <div className="flex items-center justify-between pt-1 text-[11px] text-fg-subtle">
                                        <span className="flex items-center gap-1">
                                            <Activity size={12} />
                                            已触发 {rule.fire_count} 次
                                        </span>
                                        <span className="flex items-center gap-1">
                                            <Clock size={12} />
                                            {rule.last_fire_at
                                                ? formatDateTime(rule.last_fire_at * 1000)
                                                : "从未触发"}
                                        </span>
                                    </div>

                                    {/* Action toolbar */}
                                    <div className="flex items-center justify-end gap-2 pt-2 border-t border-border/40">
                                        <Button
                                            variant="ghost"
                                            size="sm"
                                            className="h-7 px-2 text-xs gap-1 hover:bg-[rgb(var(--accent-primary)/0.1)] hover:text-[rgb(var(--accent-primary))]"
                                            onClick={() => handleTest(rule)}
                                            disabled={isTesting}
                                        >
                                            <Play size={12} className={isTesting ? "animate-spin" : ""} />
                                            <span>{isTesting ? "测试中" : "测试触发"}</span>
                                        </Button>
                                        <Button
                                            variant="ghost"
                                            size="sm"
                                            className="h-7 px-2 text-xs gap-1"
                                            onClick={() => openEditModal(rule)}
                                        >
                                            <Edit size={12} />
                                            <span>编辑</span>
                                        </Button>
                                        <Button
                                            variant="ghost"
                                            size="sm"
                                            className="h-7 px-2 text-xs gap-1 text-red-500 hover:text-red-600 hover:bg-red-500/10"
                                            onClick={() => handleDelete(rule)}
                                        >
                                            <Trash2 size={12} />
                                            <span>删除</span>
                                        </Button>
                                    </div>
                                </CardContent>
                            </Card>
                        );
                    })}
                </div>
            )}

            {/* Modal for Create / Edit Rule */}
            {modalOpen && (
                <div className="fixed inset-0 z-50 flex items-center justify-center p-4 bg-black/50 backdrop-blur-sm animate-fade-in">
                    <div className="w-full max-w-lg rounded-2xl border border-border/70 bg-card p-6 shadow-2xl space-y-5 max-h-[90vh] overflow-y-auto">
                        <div className="flex items-center justify-between pb-3 border-b border-border/50">
                            <h3 className="text-lg font-bold text-fg flex items-center gap-2">
                                <Zap className="text-[rgb(var(--accent-primary))]" size={20} />
                                {editingRule ? "编辑自动化规则" : "新建自动化联动规则"}
                            </h3>
                            <button
                                onClick={() => setModalOpen(false)}
                                className="text-fg-subtle hover:text-fg"
                            >
                                <X size={18} />
                            </button>
                        </div>

                        {/* Presets picker for new rule */}
                        {!editingRule && (
                            <div className="space-y-2">
                                <Label className="text-xs text-fg-muted">快速套用推荐预置模板：</Label>
                                <div className="flex flex-wrap gap-2">
                                    {PRESETS.map((p, idx) => (
                                        <button
                                            key={idx}
                                            type="button"
                                            onClick={() => applyPreset(p)}
                                            className="text-xs px-2.5 py-1.5 rounded-lg border border-border/60 bg-[rgb(var(--bg-subtle)/0.3)] hover:bg-[rgb(var(--accent-primary)/0.1)] hover:border-[rgb(var(--accent-primary)/0.4)] transition-all text-fg"
                                        >
                                            {p.label}
                                        </button>
                                    ))}
                                </div>
                            </div>
                        )}

                        <form onSubmit={handleSubmit} className="space-y-4">
                            {/* Rule Name */}
                            <div className="space-y-1.5">
                                <Label className="text-xs font-medium">规则名称 *</Label>
                                <Input
                                    value={formName}
                                    onChange={(e) => setFormName(e.target.value)}
                                    placeholder="例如：夜间阳台人形移动告警"
                                    required
                                />
                            </div>

                            {/* Trigger */}
                            <div className="space-y-1.5">
                                <Label className="text-xs font-medium">触发事件源 (Trigger) *</Label>
                                <Select
                                    value={formTrigger}
                                    onChange={(e) => setFormTrigger(e.target.value)}
                                >
                                    <option value="camera.motion">camera.motion (摄像头 AI 检测 / 运动)</option>
                                    <option value="camera.offline">camera.offline (摄像头离线掉线)</option>
                                    <option value="camera.online">camera.online (摄像头上线恢复)</option>
                                    <option value="security.guard_mode">security.guard_mode (安防布撤防模式切换)</option>
                                    <option value="device.status">device.status (设备状态变化)</option>
                                </Select>
                            </div>

                            {/* Condition Time Range */}
                            <div className="grid grid-cols-2 gap-3">
                                <div className="space-y-1.5">
                                    <Label className="text-xs font-medium">生效起始时间 (可选)</Label>
                                    <Input
                                        type="time"
                                        value={formTimeGte}
                                        onChange={(e) => setFormTimeGte(e.target.value)}
                                        placeholder="22:00"
                                    />
                                </div>
                                <div className="space-y-1.5">
                                    <Label className="text-xs font-medium">生效截止时间 (可选)</Label>
                                    <Input
                                        type="time"
                                        value={formTimeLte}
                                        onChange={(e) => setFormTimeLte(e.target.value)}
                                        placeholder="06:00"
                                    />
                                </div>
                            </div>

                            {/* Action Type */}
                            <div className="space-y-1.5">
                                <Label className="text-xs font-medium">联动执行动作 *</Label>
                                <Select
                                    value={formActionType}
                                    onChange={(e) => setFormActionType(e.target.value as any)}
                                >
                                    <option value="notify">发送系统推送通知 (notify)</option>
                                    <option value="mqtt">发布 MQTT 物联网指令 (mqtt)</option>
                                    <option value="webhook">调用外部 Webhook URL (webhook)</option>
                                </Select>
                            </div>

                            {/* Dynamic Action Fields */}
                            {formActionType === "notify" && (
                                <div className="space-y-3 p-3 rounded-xl bg-[rgb(var(--bg-subtle)/0.3)] border border-border/40">
                                    <div className="space-y-1">
                                        <Label className="text-xs">通知标题</Label>
                                        <Input
                                            value={formNotifyTitle}
                                            onChange={(e) => setFormNotifyTitle(e.target.value)}
                                            placeholder="告警标题"
                                        />
                                    </div>
                                    <div className="space-y-1">
                                        <Label className="text-xs">通知正文</Label>
                                        <Input
                                            value={formNotifyBody}
                                            onChange={(e) => setFormNotifyBody(e.target.value)}
                                            placeholder="告警详细描述文本"
                                        />
                                    </div>
                                </div>
                            )}

                            {formActionType === "mqtt" && (
                                <div className="space-y-3 p-3 rounded-xl bg-[rgb(var(--bg-subtle)/0.3)] border border-border/40">
                                    <div className="space-y-1">
                                        <Label className="text-xs">MQTT Topic *</Label>
                                        <Input
                                            value={formMqttTopic}
                                            onChange={(e) => setFormMqttTopic(e.target.value)}
                                            placeholder="home-datacenter/devices/1/command"
                                        />
                                    </div>
                                    <div className="space-y-1">
                                        <Label className="text-xs">指令 Payload (JSON) *</Label>
                                        <Input
                                            value={formMqttPayload}
                                            onChange={(e) => setFormMqttPayload(e.target.value)}
                                            placeholder='{"cmd":"alert"}'
                                        />
                                    </div>
                                </div>
                            )}

                            {formActionType === "webhook" && (
                                <div className="space-y-3 p-3 rounded-xl bg-[rgb(var(--bg-subtle)/0.3)] border border-border/40">
                                    <div className="space-y-1">
                                        <Label className="text-xs">Webhook URL *</Label>
                                        <Input
                                            type="url"
                                            value={formWebhookUrl}
                                            onChange={(e) => setFormWebhookUrl(e.target.value)}
                                            placeholder="https://api.example.com/webhook"
                                        />
                                    </div>
                                </div>
                            )}

                            {/* Cooldown */}
                            <div className="space-y-1.5">
                                <Label className="text-xs font-medium">防抖冷却时长 (秒)</Label>
                                <Input
                                    type="number"
                                    min="0"
                                    value={formCooldown}
                                    onChange={(e) => setFormCooldown(Number(e.target.value))}
                                />
                                <p className="text-[11px] text-fg-muted">
                                    规则触发后在此秒数内忽略重复触发，防止事件风暴轰炸。
                                </p>
                            </div>

                            {/* Buttons */}
                            <div className="flex items-center justify-end gap-3 pt-3 border-t border-border/40">
                                <Button
                                    type="button"
                                    variant="outline"
                                    size="sm"
                                    onClick={() => setModalOpen(false)}
                                    disabled={submitting}
                                >
                                    取消
                                </Button>
                                <Button
                                    type="submit"
                                    size="sm"
                                    disabled={submitting}
                                    className="gap-2 bg-[rgb(var(--accent-primary))] text-white"
                                >
                                    {submitting && <Loader2 size={14} className="animate-spin" />}
                                    <span>{editingRule ? "保存修改" : "确认创建"}</span>
                                </Button>
                            </div>
                        </form>
                    </div>
                </div>
            )}
        </div>
    );
}
