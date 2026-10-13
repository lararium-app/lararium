/** @type {import('tailwindcss').Config} */
const withAlpha = (variable) => ({ opacityValue }) => opacityValue === undefined ? `var(${variable})` : `color-mix(in srgb, var(${variable}) calc(${opacityValue} * 100%), transparent)`;

export default {
  content: [
    "./index.html",
    "./src/**/*.{js,ts,jsx,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        bg: withAlpha('--bg'),
        bg2: withAlpha('--bg2'),
        panel: withAlpha('--panel'),
        panel2: withAlpha('--panel2'),
        line: withAlpha('--line'),
        line2: withAlpha('--line2'),
        ink: withAlpha('--ink'),
        ink2: withAlpha('--ink2'),
        ink3: withAlpha('--ink3'),
        ember: withAlpha('--ember'),
        ember2: withAlpha('--ember2'),
        ok: withAlpha('--ok'),
        warn: withAlpha('--warn'),
        err: withAlpha('--err'),
        overlay: withAlpha('--overlay'),
        scrim: 'var(--scrim)',
      },
      borderRadius: {
        md: 'var(--radius-sm)',
        lg: 'var(--radius)',
        xl: 'var(--radius)',
        '2xl': 'var(--radius)',
      },
      boxShadow: {
        e1: 'var(--shadow-e1)',
        e2: 'var(--shadow-e2)',
        e3: 'var(--shadow-e3)',
      },
      transitionTimingFunction: {
        'ease-out-custom': 'var(--ease-out)',
        'ease-inout-custom': 'var(--ease-inout)',
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
