/** @type {import('tailwindcss').Config} */
export default {
  content: [
    "./index.html",
    "./src/**/*.{js,ts,jsx,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        bg: "var(--bg)",
        bg2: "var(--bg2)",
        panel: "var(--panel)",
        panel2: "var(--panel2)",
        line: "var(--line)",
        line2: "var(--line2)",
        ink: "var(--ink)",
        ink2: "var(--ink2)",
        ink3: "var(--ink3)",
        ember: "var(--ember)",
        ember2: "var(--ember2)",
        ok: "var(--ok)",
        warn: "var(--warn)",
        err: "var(--err)",
      },
      fontFamily: {
        brand: ["var(--font-brand)", "Georgia", "serif"],
        body: ["var(--font-body)", "sans-serif"],
        mono: ["var(--font-mono)", "monospace"],
      },
      height: {
        app: "var(--app-h, 100vh)",
      },
      maxHeight: {
        app: "var(--app-h, 100vh)",
      },
    },
  },
  plugins: [],
};
