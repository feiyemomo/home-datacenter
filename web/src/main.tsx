import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import App from "./App";
import { applyThemeEarly } from "./hooks/useTheme";
import "./index.css";

// Set the data-theme attribute on <html> before React mounts so
// the first paint uses the persisted theme. The hook will
// re-apply it on every subsequent render, but doing it here
// avoids the dark->light flash on slow devices.
applyThemeEarly();

ReactDOM.createRoot(document.getElementById("root")!).render(
    <React.StrictMode>
        <BrowserRouter>
            <App />
        </BrowserRouter>
    </React.StrictMode>,
);
