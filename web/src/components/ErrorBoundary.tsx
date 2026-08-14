import { Component, type ReactNode } from "react";
import { AlertTriangle, RefreshCw } from "lucide-react";
import { reportClientError } from "@/lib/errorReport";

interface Props {
    /** Optional tag describing where the boundary is mounted (e.g. "page", "live-video"). */
    context?: string;
    /** Fallback rendered on error. Defaults to a themed retry card. */
    fallback?: (reset: () => void) => ReactNode;
    children: ReactNode;
}

interface State {
    error: Error | null;
}

/**
 * ErrorBoundary — catches render errors in the subtree, reports them to
 * the backend, and renders a retry card instead of a blank white screen.
 *
 * React error boundaries are the only mechanism that can catch errors
 * thrown during render / in lifecycle methods (window "error" handlers
 * miss them). Without a boundary, a single bad render unmounts the whole
 * app tree and leaves a frozen page. The boundary reports the error to
 * /system/client-errors (context "render.error") so it shows up in the
 * log pane, then gives the user a retry button that remounts the subtree.
 *
 * v1.8.41: new — closes the "render crash → invisible to server" gap.
 *
 * Usage:
 *   <ErrorBoundary context="dashboard">
 *     <Dashboard />
 *   </ErrorBoundary>
 */
export class ErrorBoundary extends Component<Props, State> {
    state: State = { error: null };

    static getDerivedStateFromError(error: Error): State {
        return { error };
    }

    componentDidCatch(error: Error): void {
        // Report the render failure to the backend. The context tag lets
        // the operator distinguish "dashboard page crashed" from "live
        // video card crashed" in the log pane.
        reportClientError({
            level: "critical",
            context: this.props.context ? `render.error.${this.props.context}` : "render.error",
            message: error.message || "render error",
            stack: error.stack,
        });
    }

    private reset = (): void => {
        this.setState({ error: null });
    };

    render(): ReactNode {
        if (this.state.error) {
            if (this.props.fallback) {
                return this.props.fallback(this.reset);
            }
            return (
                <div className="flex min-h-[240px] flex-col items-center justify-center gap-3 rounded-2xl border border-[rgb(var(--border)/0.4)] bg-[rgb(var(--glass-bg)/0.6)] p-6 text-center">
                    <AlertTriangle className="h-8 w-8 text-[rgb(var(--accent-danger))]" />
                    <p className="text-sm font-medium text-fg">界面渲染出错</p>
                    <p className="max-w-md text-xs text-fg-muted">
                        {this.state.error.message || "渲染过程中发生未知错误"}
                    </p>
                    <button
                        type="button"
                        onClick={this.reset}
                        className="mt-2 inline-flex items-center gap-1.5 rounded-lg bg-[rgb(var(--accent-primary)/0.2)] px-4 py-2 text-xs font-medium text-[rgb(var(--accent-primary))] transition-colors hover:bg-[rgb(var(--accent-primary)/0.3)]"
                    >
                        <RefreshCw className="h-3 w-3" />
                        重试
                    </button>
                </div>
            );
        }
        return this.props.children;
    }
}

export default ErrorBoundary;