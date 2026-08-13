import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import App from "./App";
import { applyThemeEarly } from "./hooks/useTheme";
import { reportClientError } from "./lib/errorReport";
import "./index.css";

// Set the data-theme attribute on <html> before React mounts so
// the first paint uses the persisted theme. The hook will
// re-apply it on every subsequent render, but doing it here
// avoids the dark->light flash on slow devices.
applyThemeEarly();

// Global client-side error reporting (v1.8.25). Uncaught JS errors and
// unhandled promise rejections are forwarded to the backend so they
// surface in the system log pane — previously they were invisible to
// the server, making remote diagnosis impossible.
function installGlobalErrorReporting(): void {
    window.addEventListener("error", (event) => {
        // Resource-load errors (e.g. a 404 <video>/<img>) do NOT populate
        // event.message or event.error — the failing element is event.target.
        if (event.message) {
            reportClientError({
                level: "normal",
                context: "window.onerror",
                message: event.message,
                stack: event.error instanceof Error ? event.error.stack : undefined,
            });
        } else {
            const el = event.target as HTMLElement | null;
            const tag = el?.tagName?.toLowerCase() ?? "element";
            const src = el?.getAttribute?.("src") ?? "";
            reportClientError({
                level: "normal",
                context: `resource:${tag}`,
                message: `资源加载失败 (${tag}): ${src.slice(0, 300)}`,
            });
        }
    });

    window.addEventListener("unhandledrejection", (event) => {
        const reason = event.reason;
        reportClientError({
            level: "normal",
            context: "unhandledrejection",
            message:
                reason instanceof Error ? reason.message : String(reason),
            stack: reason instanceof Error ? reason.stack : undefined,
        });
    });
}

installGlobalErrorReporting();

ReactDOM.createRoot(document.getElementById("root")!).render(
    <React.StrictMode>
        <BrowserRouter>
            <App />
        </BrowserRouter>
    </React.StrictMode>,
);
