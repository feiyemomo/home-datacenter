import { useState, type ReactNode } from "react";
import { NavLink } from "react-router-dom";
import {
    Activity,
    LayoutDashboard,
    HardDrive,
    Radio,
    User as UserIcon,
    Menu,
    X,
    LogOut,
    Server,
    Camera as CameraIcon,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { useAuth } from "@/hooks/useAuth";
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
                    "fixed inset-y-0 left-0 z-40 flex w-64 flex-col border-r border-slate-800 bg-surface-raised transition-transform duration-200",
                    "md:static md:translate-x-0",
                    open ? "translate-x-0" : "-translate-x-full",
                )}
            >
                {/* Brand */}
                <div className="flex h-16 items-center gap-2 border-b border-slate-800 px-5">
                    <div className="flex h-9 w-9 items-center justify-center rounded-lg bg-gradient-to-br from-sky-500 to-indigo-600 text-white shadow-lg shadow-sky-500/20">
                        <Server size={18} />
                    </div>
                    <div className="flex flex-col leading-tight">
                        <span className="text-sm font-semibold text-slate-100">
                            Home Datacenter
                        </span>
                        <span className="text-[10px] uppercase tracking-widest text-slate-500">
                            Control Panel
                        </span>
                    </div>
                    <button
                        className="ml-auto text-slate-400 hover:text-slate-100 md:hidden"
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
                                    "group flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors",
                                    isActive
                                        ? "bg-sky-500/10 text-sky-300 ring-1 ring-inset ring-sky-500/30"
                                        : "text-slate-400 hover:bg-slate-800 hover:text-slate-100",
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
                <div className="border-t border-slate-800 p-3">
                    <div className="mb-2 flex items-center gap-2 rounded-md bg-slate-900/60 px-3 py-2">
                        <div className="flex h-8 w-8 items-center justify-center rounded-full bg-slate-700 text-xs font-semibold text-slate-200">
                            {user?.name?.charAt(0)?.toUpperCase() ?? "?"}
                        </div>
                        <div className="min-w-0 flex-1">
                            <p className="truncate text-xs font-medium text-slate-200">
                                {user?.name ?? "unknown"}
                            </p>
                            <p className="text-[10px] text-slate-500">
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
                        className="w-full justify-start text-slate-400 hover:text-rose-300"
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

    return (
        <div className="flex h-screen overflow-hidden bg-surface">
            <Sidebar open={sidebarOpen} onClose={() => setSidebarOpen(false)} />

            <div className="flex flex-1 flex-col overflow-hidden">
                {/* Top header */}
                <header className="flex h-16 shrink-0 items-center gap-3 border-b border-slate-800 bg-surface-raised/70 px-4 backdrop-blur md:px-6">
                    <button
                        className="text-slate-400 hover:text-slate-100 md:hidden"
                        onClick={() => setSidebarOpen(true)}
                        aria-label="Open navigation"
                    >
                        <Menu size={22} />
                    </button>

                    <div className="flex items-center gap-2">
                        <h1 className="text-sm font-semibold text-slate-200">
                            Home Datacenter
                        </h1>
                        <Badge variant="outline" className="hidden sm:inline-flex">
                            <span className="mr-1 inline-block h-1.5 w-1.5 rounded-full bg-slate-500" />
                            dark
                        </Badge>
                    </div>

                    <div className="ml-auto flex items-center gap-3">
                        <a
                            href="/health"
                            target="_blank"
                            rel="noreferrer"
                            className="hidden text-xs text-slate-500 hover:text-slate-300 sm:inline"
                        >
                            /health
                        </a>
                    </div>
                </header>

                {/* Main scroll area */}
                <main className="flex-1 overflow-y-auto p-4 md:p-6">{children}</main>
            </div>
        </div>
    );
}

export default Layout;
