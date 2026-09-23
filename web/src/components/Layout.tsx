import { useEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import {
    Activity,
    LayoutDashboard,
    Radio,
    User as UserIcon,
    UserCog,
    Menu,
    X,
    LogOut,
    Server,
    Camera as CameraIcon,
    History,
    Sun,
    Moon,
    Monitor,
    Network as NetworkIcon,
    Check,
    Zap,
    Shield,
    ShieldAlert,
    ShieldCheck,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { useAuth } from "@/hooks/useAuth";
import { useTheme, type Theme } from "@/hooks/useTheme";
import { useWebSocket } from "@/hooks/useWebSocket";
import { getSecurityGuard, setSecurityGuard } from "@/api/security";
import type { SecurityMode } from "@/types";
import { Button } from "@/components/ui/button";

interface NavItem {
    to: string;
    label: string;
    icon: ReactNode;
    adminOnly?: boolean;
}

const NAV_ITEMS: NavItem[] = [
    { to: "/dashboard", label: "仪表盘", icon: <LayoutDashboard size={18} /> },
    { to: "/cameras", label: "摄像头", icon: <CameraIcon size={18} /> },
    { to: "/network", label: "网络", icon: <NetworkIcon size={18} /> },
    {
        to: "/logs",
        label: "日志",
        icon: <History size={18} />,
        adminOnly: true,
    },
    {
        to: "/users",
        label: "用户",
        icon: <UserCog size={18} />,
        adminOnly: true,
    },
    {
        to: "/automations",
        label: "智能联动",
        icon: <Zap size={18} />,
        adminOnly: true,
    },
    {
        to: "/mqtt",
        label: "MQTT 调试",
        icon: <Radio size={18} />,
        adminOnly: true,
    },
    { to: "/profile", label: "个人中心", icon: <UserIcon size={18} /> },
];

interface SidebarProps {
    open: boolean;
    onClose: () => void;
}

/** Left navigation rail with enhanced liquid glass style */
export function Sidebar({ open, onClose }: SidebarProps) {
    const { isAdmin, logout, user } = useAuth();
    const items = NAV_ITEMS.filter((i) => !i.adminOnly || isAdmin);

    return (
        <>
            {/* Mobile backdrop */}
            {open && (
                <div
                    className="fixed inset-0 z-30 bg-black/30 backdrop-blur-md md:hidden animate-fade-in"
                    onClick={onClose}
                    aria-hidden
                />
            )}

            <aside
                className={cn(
                    "fixed inset-y-0 left-0 z-40 flex w-64 flex-col glass-subtle border-r border-[rgb(var(--glass-border)/0.1)] transition-all duration-500 cubic-bezier(0.32, 0.72, 0, 1)",
                    "md:static md:translate-x-0",
                    open ? "translate-x-0" : "-translate-x-full",
                )}
            >
                {/* Brand */}
                <div className="flex h-16 items-center gap-3 px-5">
                    <div className="relative flex h-10 w-10 items-center justify-center rounded-xl bg-gradient-to-br from-[rgb(var(--accent-warm)/0.7)] to-[rgb(var(--accent-primary)/0.5)] text-white shadow-lg shadow-[rgb(var(--accent-warm)/0.15)]">
                        <Server size={18} />
                        <div className="absolute inset-0 rounded-xl bg-gradient-to-t from-transparent to-white/20" />
                    </div>
                    <div className="flex flex-col leading-tight">
                        <span className="text-sm font-semibold tracking-tight text-fg">
                            家庭数据中心
                        </span>
                        <span className="text-[10px] tracking-widest text-fg-subtle/70 uppercase">
                            Control Panel
                        </span>
                    </div>
                    <button
                        className="ml-auto rounded-lg p-1.5 text-fg-subtle transition-all hover:bg-[rgb(var(--bg-subtle)/0.5)] hover:text-fg md:hidden"
                        onClick={onClose}
                        aria-label="关闭导航"
                    >
                        <X size={18} />
                    </button>
                </div>

                {/* Divider */}
                <div className="mx-4 h-px bg-gradient-to-r from-transparent via-[rgb(var(--border)/0.4)] to-transparent" />

                {/* Nav links */}
                <nav className="flex-1 space-y-1 overflow-y-auto p-3 stagger-children">
                    {items.map((item) => (
                        <NavLink
                            key={item.to}
                            to={item.to}
                            onClick={onClose}
                            className={({ isActive }) =>
                                cn(
                                    "group relative flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-all duration-300 ease-out",
                                    isActive
                                        ? "glass-subtle bg-[rgb(var(--accent-primary)/0.08)] text-[rgb(var(--accent-primary))] shadow-[0_0_12px_rgb(var(--accent-primary)/0.08)]"
                                        : "text-fg-muted hover:bg-[rgb(var(--bg-subtle)/0.4)] hover:text-fg",
                                )
                            }
                        >
                            {({ isActive }) => (
                                <>
                                    {/* Active indicator glow */}
                                    {isActive && (
                                        <div className="absolute left-0 top-1/2 h-6 w-1 -translate-y-1/2 rounded-r-full bg-gradient-to-b from-[rgb(var(--accent-warm))] to-[rgb(var(--accent-primary))] shadow-[0_0_8px_rgb(var(--accent-warm)/0.5)]" />
                                    )}
                                    <span className={cn(
                                        "transition-transform duration-300",
                                        isActive ? "scale-105" : "group-hover:scale-105"
                                    )}>
                                        {item.icon}
                                    </span>
                                    <span className="relative z-10">{item.label}</span>
                                    {item.adminOnly && (
                                        <span className="ml-auto text-[8px] text-fg-subtle/70">
                                            管理员
                                        </span>
                                    )}
                                </>
                            )}
                        </NavLink>
                    ))}
                </nav>

                {/* Footer */}
                <div className="p-3">
                    <div className="relative mb-2 flex items-center gap-2.5 rounded-xl px-3 py-2.5 transition-all hover:bg-[rgb(var(--bg-subtle)/0.3)]">
                        <div className="relative flex h-9 w-9 items-center justify-center rounded-full bg-gradient-to-br from-[rgb(var(--accent-primary)/0.4)] to-[rgb(var(--accent-warm)/0.3)] text-xs font-semibold text-[rgb(var(--accent-primary))] shadow-inner ring-1 ring-[rgb(var(--accent-warm)/0.2)]">
                            {user?.name?.charAt(0)?.toUpperCase() ?? "?"}
                            <div className="absolute inset-0 rounded-full bg-gradient-to-t from-transparent to-white/20" />
                        </div>
                        <div className="min-w-0 flex-1">
                            <p className="truncate text-xs font-medium text-fg">
                                {user?.name ?? "未知"}
                            </p>
                            <p className="text-[10px] text-fg-muted">
                                {isAdmin ? "管理员" : "普通用户"}
                            </p>
                        </div>
                        {isAdmin && (
                            <span className="inline-flex items-center gap-1 text-[8px] text-fg-subtle/70">
                                <span className="h-1.5 w-1.5 rounded-full bg-[rgb(var(--accent-warm))]" />
                                管理员
                            </span>
                        )}
                    </div>
                    <Button
                        variant="ghost"
                        size="sm"
                        className="w-full justify-start gap-2 text-fg-muted transition-all hover:bg-[rgb(var(--accent-danger)/0.08)] hover:text-[rgb(var(--accent-danger))]"
                        onClick={logout}
                    >
                        <LogOut size={15} />
                        <span>退出登录</span>
                    </Button>
                </div>
            </aside>
        </>
    );
}

interface LayoutProps {
    children?: ReactNode;
}

/**
 * ThemeMenu — 3-state theme picker (light / dark / system) with a
 * glass dropdown. Replaces the old binary Sun/Moon toggle so the
 * operator can opt into "system" (follow OS prefers-color-scheme).
 */
function ThemeMenu() {
    const { theme, resolved, setTheme } = useTheme();
    const [open, setOpen] = useState(false);
    const ref = useRef<HTMLDivElement>(null);

    useEffect(() => {
        if (!open) return;
        const onClick = (e: MouseEvent) => {
            if (ref.current && !ref.current.contains(e.target as Node)) {
                setOpen(false);
            }
        };
        const onKey = (e: KeyboardEvent) => {
            if (e.key === "Escape") setOpen(false);
        };
        window.addEventListener("mousedown", onClick);
        window.addEventListener("keydown", onKey);
        return () => {
            window.removeEventListener("mousedown", onClick);
            window.removeEventListener("keydown", onKey);
        };
    }, [open]);

    const options: { value: Theme; label: string; icon: typeof Sun }[] = [
        { value: "light", label: "亮色", icon: Sun },
        { value: "dark", label: "暗色", icon: Moon },
        { value: "system", label: "跟随系统", icon: Monitor },
    ];

    const ActiveIcon = resolved === "dark" ? Moon : Sun;
    const themeLabel = theme === "light" ? "亮色" : theme === "dark" ? "暗色" : "跟随系统";
    const resolvedLabel = resolved === "dark" ? "暗色" : "亮色";

    return (
        <div ref={ref} className="relative z-50">
            <Button
                size="icon"
                variant="ghost"
                onClick={() => setOpen((v) => !v)}
                aria-label={`主题：${themeLabel}`}
                title={`主题：${themeLabel}（当前生效：${resolvedLabel}）`}
                aria-expanded={open}
                aria-haspopup="menu"
                className="transition-all hover:bg-[rgb(var(--bg-subtle)/0.5)]"
            >
                <ActiveIcon size={16} />
            </Button>
            {open && (
                <div
                    role="menu"
                    className="absolute right-0 top-full mt-2 min-w-[160px] overflow-hidden rounded-xl glass p-1.5 shadow-2xl animate-scale-in ring-1 ring-[rgb(var(--border)/0.3)]"
                >
                    {options.map((opt) => {
                        const Icon = opt.icon;
                        const active = theme === opt.value;
                        return (
                            <button
                                key={opt.value}
                                role="menuitemradio"
                                aria-checked={active}
                                onClick={() => {
                                    setTheme(opt.value);
                                    setOpen(false);
                                }}
                                className={cn(
                                    "flex w-full items-center gap-2.5 rounded-lg px-3 py-2 text-xs transition-all",
                                    active
                                        ? "bg-gradient-to-r from-[rgb(var(--accent-warm)/0.12)] to-[rgb(var(--accent-primary)/0.1)] text-[rgb(var(--accent-primary))] shadow-sm"
                                        : "text-fg-muted hover:bg-[rgb(var(--bg-subtle)/0.5)] hover:text-fg",
                                )}
                            >
                                <Icon size={13} className={active ? "animate-pulse" : ""} />
                                <span className="flex-1 text-left">{opt.label}</span>
                                {active && <Check size={12} className="text-[rgb(var(--accent-success))]" />}
                            </button>
                        );
                    })}
                </div>
            )}
        </div>
    );
}

/**
 * SecurityGuardMenu — 3-state home security arming switcher.
 * Synchronizes with backend and WebSocket in real-time.
 */
function SecurityGuardMenu() {
    const [mode, setMode] = useState<SecurityMode>("armed_away");
    const [open, setOpen] = useState(false);
    const [loading, setLoading] = useState(false);
    const ref = useRef<HTMLDivElement>(null);
    const { lastMessage } = useWebSocket();

    useEffect(() => {
        getSecurityGuard()
            .then((res) => {
                if (res?.mode) setMode(res.mode);
            })
            .catch(() => {});
    }, []);

    useEffect(() => {
        if (!lastMessage) return;
        if (lastMessage.type === "security.guard_mode") {
            const payload = lastMessage.payload as any;
            if (payload?.mode) {
                setMode(payload.mode);
            }
        }
    }, [lastMessage]);

    useEffect(() => {
        if (!open) return;
        const onClick = (e: MouseEvent) => {
            if (ref.current && !ref.current.contains(e.target as Node)) {
                setOpen(false);
            }
        };
        const onKey = (e: KeyboardEvent) => {
            if (e.key === "Escape") setOpen(false);
        };
        window.addEventListener("mousedown", onClick);
        window.addEventListener("keydown", onKey);
        return () => {
            window.removeEventListener("mousedown", onClick);
            window.removeEventListener("keydown", onKey);
        };
    }, [open]);

    const handleSelect = async (newMode: SecurityMode) => {
        if (newMode === mode) {
            setOpen(false);
            return;
        }
        setLoading(true);
        try {
            const res = await setSecurityGuard(newMode);
            setMode(res.mode);
        } catch (e) {
            console.error("failed to switch security guard mode", e);
        } finally {
            setLoading(false);
            setOpen(false);
        }
    };

    const MODES: Array<{
        value: SecurityMode;
        label: string;
        desc: string;
        color: string;
        icon: typeof Shield;
    }> = [
        {
            value: "armed_away",
            label: "离家布防",
            desc: "全警戒，AI 异动触发即时强提醒与联动",
            color: "text-rose-500 bg-rose-500/10 border-rose-500/30",
            icon: ShieldAlert,
        },
        {
            value: "armed_home",
            label: "在家守护",
            desc: "监控重点外围，忽略室内移动",
            color: "text-amber-500 bg-amber-500/10 border-amber-500/30",
            icon: Shield,
        },
        {
            value: "disarmed",
            label: "撤防免打扰",
            desc: "在家免打扰，告警静音不弹窗",
            color: "text-emerald-500 bg-emerald-500/10 border-emerald-500/30",
            icon: ShieldCheck,
        },
    ];

    const current = MODES.find((m) => m.value === mode) || MODES[0];
    const CurrentIcon = current.icon;

    return (
        <div ref={ref} className="relative z-50">
            <button
                onClick={() => setOpen((v) => !v)}
                disabled={loading}
                className={cn(
                    "flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium border transition-all duration-200 shadow-sm hover:brightness-105",
                    current.color
                )}
                title={`当前安防模式：${current.label}（点击切换）`}
            >
                <CurrentIcon size={14} className={loading ? "animate-spin" : ""} />
                <span>{current.label}</span>
            </button>
            {open && (
                <div
                    role="menu"
                    className="absolute right-0 top-full mt-2 w-64 overflow-hidden rounded-xl glass p-2 shadow-2xl animate-scale-in ring-1 ring-[rgb(var(--border)/0.3)] space-y-1"
                >
                    <div className="px-2 py-1 text-[11px] font-semibold text-fg-subtle border-b border-border/40 mb-1">
                        切换家庭安防模式
                    </div>
                    {MODES.map((m) => {
                        const Icon = m.icon;
                        const active = m.value === mode;
                        return (
                            <button
                                key={m.value}
                                onClick={() => handleSelect(m.value)}
                                className={cn(
                                    "w-full flex items-start gap-2.5 p-2 rounded-lg text-left transition-all text-xs",
                                    active
                                        ? "bg-[rgb(var(--accent-primary)/0.12)] text-[rgb(var(--accent-primary))]"
                                        : "hover:bg-[rgb(var(--bg-subtle)/0.5)] text-fg"
                                )}
                            >
                                <Icon size={16} className="mt-0.5 shrink-0" />
                                <div className="min-w-0 flex-1">
                                    <div className="font-medium flex items-center justify-between">
                                        <span>{m.label}</span>
                                        {active && <Check size={13} />}
                                    </div>
                                    <div className="text-[10px] text-fg-muted mt-0.5 leading-tight">
                                        {m.desc}
                                    </div>
                                </div>
                            </button>
                        );
                    })}
                </div>
            )}
        </div>
    );
}

function FallAlertBanner() {
    const { lastMessage, subscribe } = useWebSocket();
    const [fallAlert, setFallAlert] = useState<{ camera_slug?: string; time?: string } | null>(null);
    const nav = useNavigate();

    useEffect(() => {
        subscribe("camera.fall_detected");
    }, [subscribe]);

    useEffect(() => {
        if (!lastMessage) return;
        if (lastMessage.topic === "camera.fall_detected" || (lastMessage as any).type === "camera.fall_detected") {
            const p = lastMessage.payload || {};
            setFallAlert({
                camera_slug: (p as any).camera_slug || "安防监控",
                time: new Date().toLocaleTimeString(),
            });
        }
    }, [lastMessage]);

    if (!fallAlert) return null;

    return createPortal(
        <div className="fixed top-5 left-1/2 -translate-x-1/2 z-[10000] w-[92%] max-w-lg animate-bounce-in shadow-2xl">
            <div className="rounded-2xl border-2 border-red-500 bg-red-950/95 backdrop-blur-md p-4 text-white shadow-[0_0_35px_rgba(239,68,68,0.6)] flex items-start gap-3">
                <div className="p-2 rounded-xl bg-red-600/30 text-red-400 shrink-0 animate-pulse">
                    <ShieldAlert size={26} />
                </div>
                <div className="min-w-0 flex-1">
                    <h4 className="text-sm font-bold text-red-200 flex items-center gap-1.5">
                        <span>🚨 紧急安全告警：检测到人员摔倒！</span>
                    </h4>
                    <p className="text-xs text-red-100/90 mt-1">
                        设备【{fallAlert.camera_slug}】于 {fallAlert.time} 检测到人员异常跌倒，请立即确认现场安全！
                    </p>
                    <div className="flex items-center gap-2 mt-3">
                        <button
                            type="button"
                            onClick={() => {
                                setFallAlert(null);
                                nav("/cameras");
                            }}
                            className="px-3 py-1.5 rounded-lg bg-red-600 hover:bg-red-500 text-xs font-semibold text-white shadow-md transition-all"
                        >
                            前往查看监控画面
                        </button>
                        <button
                            type="button"
                            onClick={() => setFallAlert(null)}
                            className="px-2.5 py-1.5 rounded-lg bg-white/10 hover:bg-white/20 text-xs text-white/80 transition-all"
                        >
                            我知道了
                        </button>
                    </div>
                </div>
                <button
                    type="button"
                    onClick={() => setFallAlert(null)}
                    className="text-white/60 hover:text-white p-1"
                >
                    <X size={16} />
                </button>
            </div>
        </div>,
        document.body
    );
}

/** App shell with enhanced liquid glass layout */
export function Layout({ children }: LayoutProps) {
    const [sidebarOpen, setSidebarOpen] = useState(false);
    const location = useLocation();

    return (
        <div className="relative flex h-screen overflow-hidden bg-surface">
            <FallAlertBanner />
            {/* Ambient background orbs - enhanced with more depth */}
            <div className="orb orb-warm" style={{ width: 550, height: 550, top: -180, right: -150, animationDelay: "0s" }} />
            <div className="orb orb-cool" style={{ width: 450, height: 450, bottom: -120, left: -120, animationDelay: "-8s" }} />
            <div className="orb orb-accent" style={{ width: 250, height: 250, top: "40%", left: "30%", animationDelay: "-15s" }} />

            <Sidebar open={sidebarOpen} onClose={() => setSidebarOpen(false)} />

            <div className="relative z-10 flex flex-1 flex-col overflow-hidden">
                {/* Top header */}
                <header className="relative z-40 flex h-14 shrink-0 items-center gap-3 glass-subtle px-4 md:px-6 transition-all duration-500 ease-out">
                    <button
                        className="rounded-lg p-2 text-fg-subtle transition-all hover:bg-[rgb(var(--bg-subtle)/0.5)] hover:text-fg md:hidden"
                        onClick={() => setSidebarOpen(true)}
                        aria-label="打开导航"
                    >
                        <Menu size={20} />
                    </button>

                    <div className="flex items-center gap-2">
                        <h1 className="text-sm font-medium tracking-tight text-fg">
                            家庭数据中心
                        </h1>
                    </div>

                    <div className="ml-auto flex items-center gap-2">
                        <SecurityGuardMenu />
                        <a
                            href="/health"
                            target="_blank"
                            rel="noreferrer"
                            className="inline-flex items-center justify-center rounded-lg p-2 text-fg-muted transition-all hover:bg-[rgb(var(--bg-subtle)/0.5)] hover:text-fg"
                            title="后端健康检查"
                        >
                            <Activity size={16} />
                        </a>
                        <ThemeMenu />
                    </div>
                </header>

                {/* Main scroll area */}
                <main className="relative z-10 flex-1 overflow-y-auto p-5 md:p-8">
                    <div key={location.pathname} className="relative z-10 animate-slide-up">
                        {children ?? <Outlet />}
                    </div>
                </main>
            </div>
        </div>
    );
}

export default Layout;
