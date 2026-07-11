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

/** Left navigation rail. On mobile it slides in as an overlay. */
export function Sidebar({ open, onClose }: SidebarProps) {
    const { isAdmin, logout, user } = useAuth();

    const items = NAV_ITEMS.filter((i) => !i.adminOnly || isAdmin);

    return (
        <>
            {/* Mobile backdrop */}
            {open && (
                <div
                    className="fixed inset-0 z-30 bg-black/60 backdrop-blur-sm md:hidden"
                    onClick={onClose}
                    aria-hidden
                />
            )}

            <aside
                className={cn(
                    "fixed inset-y-0 left-0 z-40 flex w-64 flex-col border-r border-surface-border bg-surface-raised transition-transform duration-200",
                    "md:static md:translate-x-0",
                    open ? "translate-x-0" : "-translate-x-full",
                )}
            >
                {/* Brand */}
                <div className="flex h-16 items-center gap-2 border-b border-surface-border px-5">
                    <div className="flex h-9 w-9 items-center justify-center rounded-lg bg-gradient-to-br from-sky-500 to-indigo-600 text-white shadow-lg shadow-sky-500/20">
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
                        className="ml-auto text-fg-subtle hover:text-fg md:hidden"
                        onClick={onClose}
                        aria-label="Close navigation"
                    >
                        <X size={20} />
                    </button>
                </div>

                {/* Nav links */}
                <nav className="flex-1 space-y-1 overflow-y-auto p-3">
                    {items.map((item) => (
                        <NavLink
                            key={item.to}
                            to={item.to}
                            onClick={onClose}
                            className={({ isActive }) =>
                                cn(
                                    "group flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-colors",
                                    isActive
                                        ? "bg-sky-500/10 text-sky-700 ring-1 ring-inset ring-sky-500/30 dark:text-sky-300"
                                        : "text-fg-muted hover:bg-surface-subtle hover:text-fg",
                                )
                            }
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

                {/* Footer / user + logout */}
                <div className="border-t border-surface-border p-3">
                    <div className="mb-2 flex items-center gap-2 rounded-lg bg-surface-subtle/60 px-3 py-2">
                        <div className="flex h-8 w-8 items-center justify-center rounded-full bg-sky-500/10 text-xs font-semibold text-sky-700 dark:text-sky-300 ring-1 ring-inset ring-sky-500/20">
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
                        className="w-full justify-start text-fg-muted hover:text-rose-600 dark:hover:text-rose-300"
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

/** App shell: sidebar + header + scrollable main area. */
export function Layout({ children }: LayoutProps) {
    const [sidebarOpen, setSidebarOpen] = useState(false);
    const [theme, setTheme] = useTheme();

    return (
        <div className="flex h-screen overflow-hidden bg-surface">
            <Sidebar open={sidebarOpen} onClose={() => setSidebarOpen(false)} />

            <div className="flex flex-1 flex-col overflow-hidden">
                {/* Top header */}
                <header className="flex h-16 shrink-0 items-center gap-3 border-b border-surface-border bg-surface-raised/70 px-4 backdrop-blur md:px-6">
                    <button
                        className="text-fg-subtle hover:text-fg md:hidden"
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
                            className="hidden text-xs text-fg-subtle hover:text-fg sm:inline"
                        >
                            /health
                        </a>
                        {/* Theme toggle. The Sun/Moon glyph
                         * hints at the destination of the next
                         * click ("click to go to the other one"),
                         * which is the more useful affordance
                         * than showing the current state. */}
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
                <main className="flex-1 overflow-y-auto p-4 md:p-6">{children}</main>
            </div>
        </div>
    );
}

export default Layout;
