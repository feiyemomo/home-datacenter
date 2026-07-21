import { useEffect, useRef, useState, type ReactNode } from "react";
import { NavLink } from "react-router-dom";
import {
    Activity,
    LayoutDashboard,
    HardDrive,
    Radio,
    User as UserIcon,
    UserCog,
    Menu,
    X,
    LogOut,
    Server,
    Camera as CameraIcon,
    Sun,
    Moon,
    Monitor,
    Network as NetworkIcon,
    Check,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { useAuth } from "@/hooks/useAuth";
import { useTheme, type Theme } from "@/hooks/useTheme";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";

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
    { to: "/devices", label: "设备", icon: <HardDrive size={18} /> },
    {
        to: "/users",
        label: "用户",
        icon: <UserCog size={18} />,
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

/** Left navigation rail with liquid glass style */
export function Sidebar({ open, onClose }: SidebarProps) {
    const { isAdmin, logout, user } = useAuth();
    const items = NAV_ITEMS.filter((i) => !i.adminOnly || isAdmin);

    return (
        <>
            {/* Mobile backdrop */}
            {open && (
                <div
                    className="fixed inset-0 z-30 bg-black/40 backdrop-blur-sm md:hidden animate-fade-in"
                    onClick={onClose}
                    aria-hidden
                />
            )}

            <aside
                className={cn(
                    "fixed inset-y-0 left-0 z-40 flex w-64 flex-col glass-strong transition-all duration-500 ease-out",
                    "md:static md:translate-x-0",
                    open ? "translate-x-0" : "-translate-x-full",
                )}
            >
                {/* Brand */}
                <div className="flex h-16 items-center gap-3 px-5">
                    <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-gradient-to-br from-[rgb(var(--accent-primary)/0.6)] to-[rgb(var(--accent-warm)/0.4)] text-white glass-glow">
                        <Server size={18} />
                    </div>
                    <div className="flex flex-col leading-tight">
                        <span className="text-sm font-semibold text-fg">
                            家庭数据中心
                        </span>
                        <span className="text-[10px] tracking-widest text-fg-subtle">
                            控制面板
                        </span>
                    </div>
                    <button
                        className="ml-auto text-fg-subtle hover:text-fg transition-colors md:hidden"
                        onClick={onClose}
                        aria-label="关闭导航"
                    >
                        <X size={20} />
                    </button>
                </div>

                {/* Divider */}
                <div className="mx-4 h-px bg-gradient-to-r from-transparent via-[rgb(var(--border)/0.5)] to-transparent" />

                {/* Nav links */}
                <nav className="flex-1 space-y-1 overflow-y-auto p-3">
                    {items.map((item, i) => (
                        <NavLink
                            key={item.to}
                            to={item.to}
                            onClick={onClose}
                            className={({ isActive }) =>
                                cn(
                                    "group flex items-center gap-3 rounded-xl px-3 py-2.5 text-sm font-medium transition-all duration-300 ease-out",
                                    "animate-slide-in",
                                    isActive
                                        ? "glass text-[rgb(var(--accent-primary))] glass-glow"
                                        : "text-fg-muted hover:bg-[rgb(var(--bg-subtle)/0.3)] hover:text-fg",
                                )
                            }
                            style={{ animationDelay: `${i * 50}ms` }}
                        >
                            {item.icon}
                            <span>{item.label}</span>
                            {item.adminOnly && (
                                <Badge variant="info" className="ml-auto text-[10px]">
                                    管理员
                                </Badge>
                            )}
                        </NavLink>
                    ))}
                </nav>

                {/* Footer */}
                <div className="p-3">
                    <div className="mb-2 flex items-center gap-2 rounded-xl glass-subtle px-3 py-2.5">
                        <div className="flex h-9 w-9 items-center justify-center rounded-full bg-gradient-to-br from-[rgb(var(--accent-primary)/0.3)] to-[rgb(var(--accent-warm)/0.2)] text-xs font-semibold text-[rgb(var(--accent-primary))]">
                            {user?.name?.charAt(0)?.toUpperCase() ?? "?"}
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
                            <Badge variant="success" className="text-[10px]">
                                <Activity size={10} /> 管理员
                            </Badge>
                        )}
                    </div>
                    <Button
                        variant="ghost"
                        size="sm"
                        className="w-full justify-start text-fg-muted hover:text-[rgb(var(--accent-danger))]"
                        onClick={logout}
                    >
                        <LogOut size={16} />
                        退出登录
                    </Button>
                </div>
            </aside>
        </>
    );
}

interface LayoutProps {
    children: ReactNode;
}

/**
 * ThemeMenu — 3-state theme picker (light / dark / system) with a
 * glass dropdown. Replaces the old binary Sun/Moon toggle so the
 * operator can opt into "system" (follow OS prefers-color-scheme).
 *
 * The dropdown closes on outside-click, Escape, or option pick.
 * The icon reflects the *resolved* theme (what's actually applied
 * to <html>), while the highlighted option reflects the user's
 * choice (so "system" stays highlighted even if it resolves to
 * dark on this OS).
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
            >
                <ActiveIcon size={16} />
            </Button>
            {open && (
                <div
                    role="menu"
                    className="absolute right-0 top-full mt-1 min-w-[150px] overflow-hidden rounded-xl glass-strong p-1 shadow-xl z-[100] animate-fade-in ring-1 ring-[rgb(var(--border)/0.4)]"
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
                                    "flex w-full items-center gap-2 rounded-lg px-2.5 py-1.5 text-xs transition-colors",
                                    active
                                        ? "bg-[rgb(var(--accent-primary)/0.15)] text-[rgb(var(--accent-primary))]"
                                        : "text-fg-muted hover:bg-[rgb(var(--bg-subtle)/0.5)] hover:text-fg",
                                )}
                            >
                                <Icon size={13} />
                                <span className="flex-1 text-left">{opt.label}</span>
                                {active && <Check size={12} />}
                            </button>
                        );
                    })}
                </div>
            )}
        </div>
    );
}

/** App shell with liquid glass layout */
export function Layout({ children }: LayoutProps) {
    const [sidebarOpen, setSidebarOpen] = useState(false);
    const { resolved } = useTheme();

    return (
        <div className="relative flex h-screen overflow-hidden bg-surface">
            {/* Ambient background orbs */}
            <div className="orb orb-warm" style={{ width: 400, height: 400, top: -100, right: -100 }} />
            <div className="orb orb-cool" style={{ width: 350, height: 350, bottom: -80, left: -80 }} />
            <div className="orb orb-accent" style={{ width: 250, height: 250, top: "40%", left: "30%" }} />

            <Sidebar open={sidebarOpen} onClose={() => setSidebarOpen(false)} />

            <div className="relative flex flex-1 flex-col overflow-hidden">
                {/* Top header — z-40 so dropdowns inside (ThemeMenu)
                 * can stack above main content but below the mobile
                 * sidebar backdrop (z-30 is mobile backdrop; we use
                 * z-40 on the header so the theme dropdown's z-[100]
                 * cleanly rises above any card content below). */}
                <header className="relative z-40 flex h-16 shrink-0 items-center gap-3 glass-strong px-4 md:px-6 transition-all duration-500 ease-out">
                    <button
                        className="text-fg-subtle hover:text-fg transition-colors md:hidden"
                        onClick={() => setSidebarOpen(true)}
                        aria-label="打开导航"
                    >
                        <Menu size={22} />
                    </button>

                    <div className="flex items-center gap-2">
                        <h1 className="text-sm font-semibold text-fg">
                            家庭数据中心
                        </h1>
                        <Badge variant="outline" className="hidden sm:inline-flex">
                            <span className="mr-1 inline-block h-1.5 w-1.5 rounded-full bg-fg-subtle" />
                            {resolved === "dark" ? "暗色" : "亮色"}
                        </Badge>
                    </div>

                    <div className="ml-auto flex items-center gap-3">
                        <a
                            href="/health"
                            target="_blank"
                            rel="noreferrer"
                            className="hidden text-xs text-fg-subtle hover:text-fg transition-colors sm:inline"
                            title="后端健康检查"
                        >
                            健康检查
                        </a>
                        <ThemeMenu />
                    </div>
                </header>

                {/* Main scroll area */}
                <main className="relative flex-1 overflow-y-auto p-4 md:p-6">
                    {children}
                </main>
            </div>
        </div>
    );
}

export default Layout;
