/** @type {import('tailwindcss').Config} */
export default {
  darkMode: "class",
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        // Dark surface tokens aligned with the slate/zinc palette.
        surface: {
          DEFAULT: "#0b1120",
          raised: "#111827",
          subtle: "#1e293b",
          border: "#334155",
        },
      },
      fontFamily: {
        // Distinctive but conservative pairing for a control panel.
        sans: ["Inter", "system-ui", "sans-serif"],
        mono: ["JetBrains Mono", "ui-monospace", "monospace"],
      },
    },
  },
  plugins: [],
};
