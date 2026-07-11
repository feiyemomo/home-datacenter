/** @type {import('tailwindcss').Config} */
export default {
  // Theme is driven by a `data-theme` attribute on <html> (set
  // by useTheme). We no longer rely on Tailwind's default `.dark`
  // class for the palette — the slate colors are themselves
  // backed by CSS variables that flip when the attribute changes,
  // so the existing `slate-XXX` classes just work in both modes.
  darkMode: ["selector", '[data-theme="dark"]'],
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        // Backwards-compatible: the four surface tokens. Light
        // mode uses near-white / slate-50; dark mode uses the
        // original navy / slate palette. See index.css for the
        // CSS variable bindings.
        surface: {
          DEFAULT: "rgb(var(--bg) / <alpha-value>)",
          raised: "rgb(var(--bg-raised) / <alpha-value>)",
          subtle: "rgb(var(--bg-subtle) / <alpha-value>)",
          border: "rgb(var(--border) / <alpha-value>)",
        },
        // Page foreground / muted text. Components use these
        // instead of `text-slate-100` / `text-slate-500` so the
        // color auto-flips.
        fg: {
          DEFAULT: "rgb(var(--fg) / <alpha-value>)",
          muted: "rgb(var(--fg-muted) / <alpha-value>)",
          subtle: "rgb(var(--fg-subtle) / <alpha-value>)",
          inverted: "rgb(var(--fg-inverted) / <alpha-value>)",
        },
      },
      // Slate palette -> CSS variables. We define the full range
      // (50-950) so the existing `slate-XXX` utility classes
      // resolve to the right color in either theme. Light mode
      // values are chosen to be a slate-leaning grayscale (not
      // pure white) so cards/borders still read as "elevated
      // surfaces" against the background.
      // The literal slate values are NOT imported from Tailwind
      // defaults; they're declared via the `colors` block below.
      // We use `colors: { ... }` to OVERRIDE the default slate.
      fontFamily: {
        sans: ["Inter", "system-ui", "sans-serif"],
        mono: ["JetBrains Mono", "ui-monospace", "monospace"],
      },
    },
    // Override the default `slate` palette so every `bg-slate-*`
    // / `text-slate-*` / `border-slate-*` class picks up theme
    // via CSS variables. Without this, slate-* is always the
    // dark palette baked into Tailwind's generated CSS.
    colors: {
      transparent: "transparent",
      current: "currentColor",
      black: "#000",
      white: "#fff",
      slate: {
        50: "rgb(var(--slate-50) / <alpha-value>)",
        100: "rgb(var(--slate-100) / <alpha-value>)",
        200: "rgb(var(--slate-200) / <alpha-value>)",
        300: "rgb(var(--slate-300) / <alpha-value>)",
        400: "rgb(var(--slate-400) / <alpha-value>)",
        500: "rgb(var(--slate-500) / <alpha-value>)",
        600: "rgb(var(--slate-600) / <alpha-value>)",
        700: "rgb(var(--slate-700) / <alpha-value>)",
        800: "rgb(var(--slate-800) / <alpha-value>)",
        900: "rgb(var(--slate-900) / <alpha-value>)",
        950: "rgb(var(--slate-950) / <alpha-value>)",
      },
    },
  },
  plugins: [],
};
