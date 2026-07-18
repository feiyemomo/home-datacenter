import { useState, type ReactNode } from "react";
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
    Network as NetworkIcon,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { useAuth } from "@/hooks/useAuth";
import { useTheme } from "@/hooks/useTheme";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";

interface NavItem {
    to: string;
    label: string;
    icon: ReactNode;
    adminOnly?: boolean;
}

const NAV_ITEMS: NavItem[] = [
    { to: "/dashboard", label: "Dashboard", icon: <LayoutDashboard size={18} /> },
    { to: "/cameras", label: "Cameras", icon: <CameraIcon size={18} /> },
    { to: "/network", label: "Network", icon: <NetworkIcon size={18} /> },
    { to: "/devices", label: "Devices", icon: <HardDrive size={18} /> },
    {
        to: "/users",
        label: "Users",
        icon: <UserCog size={18} />,
        adminOnly: true,
    },
    {
        to: "/mqtt",
        label: "MQTT Debug",
        icon: <Radio size={18} />,
        adminOnly: true,
    },
    { to: "/profile", label: "Profile", icon: <UserIcon size={18} /> },
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
                            Home Datacenter
                        </span>
                        <span className="text-[10px] uppercase tracking-widest text-fg-subtle">
                            Control Panel
                        </span>
                    </div>
                    <button
                        className="ml-auto text-fg-subtle hover:text-fg transition-colors md:hidden"
                        onClick={onClose}
                        aria-label="Close navigation"
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
                                    ADMIN
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
                                {user?.name ?? "unknown"}
                            </p>
                            <p className="text-[10px] text-fg-muted">
                                {isAdmin ? "administrator" : "user"}
                            </p>
                        </div>
                        {isAdmin && (
                            <Badge variant="success" className="text-[10px]">
                                <Activity size={10} /> admin
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
                        Sign out
                    </Button>
                </div>
            </aside>
        </>
    );
}

interface LayoutProps {
    children: ReactNode;
}

/** App shell with liquid glass layout */
export function Layout({ children }: LayoutProps) {
    const [sidebarOpen, setSidebarOpen] = useState(false);
    const [theme, setTheme] = useTheme();

    return (
        <div className="relative flex h-screen overflow-hidden bg-surface">
            {/* Ambient background orbs */}
            <div className="orb orb-warm" style={{ width: 400, height: 400, top: -100, right: -100 }} />
            <div className="orb orb-cool" style={{ width: 350, height: 350, bottom: -80, left: -80 }} />
            <div className="orb orb-accent" style={{ width: 250, height: 250, top: "40%", left: "30%" }} />

            <Sidebar open={sidebarOpen} onClose={() => setSidebarOpen(false)} />

            <div className="relative flex flex-1 flex-col overflow-hidden">
                {/* Top header */}
                <header className="flex h-16 shrink-0 items-center gap-3 glass-strong px-4 md:px-6 transition-all duration-500 ease-out">
                    <button
                        className="text-fg-subtle hover:text-fg transition-colors md:hidden"
                        onClick={() => setSidebarOpen(true)}
                        aria-label="Open navigation"
                    >
                        <Menu size={22} />
                    </button>

                    <div className="flex items-center gap-2">
                        <h1 className="text-sm font-semibold text-fg">
                            Home Datacenter
                        </h1>
                        <Badge variant="outline" className="hidden sm:inline-flex">
                            <span className="mr-1 inline-block h-1.5 w-1.5 rounded-full bg-fg-subtle" />
                            {theme}
                        </Badge>
                    </div>

                    <div className="ml-auto flex items-center gap-3">
                        <a
                            href="/health"
                            target="_blank"
                            rel="noreferrer"
                            className="hidden text-xs text-fg-subtle hover:text-fg transition-colors sm:inline"
                        >
                            /health
                        </a>
                        <Button
                            size="icon"
                            variant="ghost"
                            onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
                            aria-label={
                                theme === "dark"
                                    ? "Switch to light theme"
                                    : "Switch to dark theme"
                            }
                            title={
                                theme === "dark"
                                    ? "Switch to light theme"
                                    : "Switch to dark theme"
                            }
                        >
                            {theme === "dark" ? <Sun size={16} /> : <Moon size={16} />}
                        </Button>
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
