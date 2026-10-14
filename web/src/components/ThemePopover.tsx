import { useState, useEffect, useRef } from "react";
import { Palette, X } from "lucide-react";

interface ThemePopoverProps {
  isOpen: boolean;
  onClose: () => void;
}

export function ThemePopover({ isOpen, onClose }: ThemePopoverProps) {
  const [theme, setThemeState] = useState("warm");
  const [base, setBaseState] = useState("dark");
  const [hue, setHueState] = useState("25");

  const popoverRef = useRef<HTMLDivElement | null>(null);
  const previousActiveElementRef = useRef<HTMLElement | null>(null);

  useEffect(() => {
    const savedTheme = sessionStorage.getItem("lararium.theme") || "warm";
    const savedHue = sessionStorage.getItem("lararium.accent-hue") || "25";
    const savedBase = sessionStorage.getItem("lararium.base") || "dark";

    setThemeState(savedTheme);
    setHueState(savedHue);
    setBaseState(savedBase);

    document.documentElement.dataset.theme = savedTheme;
    document.documentElement.dataset.base = savedBase;
    if (savedTheme === "custom") {
      document.documentElement.style.setProperty("--accent-h", savedHue);
    }
  }, []);

  useEffect(() => {
    if (isOpen) {
      previousActiveElementRef.current = document.activeElement as HTMLElement | null;
      const timer = setTimeout(() => {
        const focusable = popoverRef.current?.querySelector<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
        );
        focusable?.focus();
      }, 50);
      return () => clearTimeout(timer);
    } else if (previousActiveElementRef.current) {
      previousActiveElementRef.current.focus();
      previousActiveElementRef.current = null;
    }
  }, [isOpen]);

  useEffect(() => {
    if (!isOpen) return;

    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
        return;
      }

      if (e.key === "Tab") {
        const popover = popoverRef.current;
        if (!popover) return;
        const focusables = popover.querySelectorAll<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
        );
        if (focusables.length === 0) return;
        const first = focusables[0];
        const last = focusables[focusables.length - 1];

        if (e.shiftKey) {
          if (document.activeElement === first) {
            last.focus();
            e.preventDefault();
          }
        } else {
          if (document.activeElement === last) {
            first.focus();
            e.preventDefault();
          }
        }
      }
    };

    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, [isOpen, onClose]);

  const changeTheme = (newTheme: string) => {
    setThemeState(newTheme);
    document.documentElement.dataset.theme = newTheme;
    sessionStorage.setItem("lararium.theme", newTheme);
    if (newTheme === "custom") {
      document.documentElement.style.setProperty("--accent-h", hue);
      document.documentElement.dataset.base = base;
    }
  };

  const changeBase = (newBase: string) => {
    setBaseState(newBase);
    document.documentElement.dataset.base = newBase;
    sessionStorage.setItem("lararium.base", newBase);
  };

  const changeHue = (newHue: string) => {
    setHueState(newHue);
    sessionStorage.setItem("lararium.accent-hue", newHue);
    if (theme === "custom") {
      document.documentElement.style.setProperty("--accent-h", newHue);
    }
  };

  if (!isOpen) return null;

  return (
    <>
      {/* Mobile backdrop */}
      <div
        className="fixed inset-0 bg-scrim animate-[fade-in_var(--dur-fast)_var(--ease-out)] z-[45] lg:hidden"
        onClick={onClose}
      />

      {/* Popover / Sheet */}
      <div
        ref={popoverRef}
        role="dialog"
        aria-modal="true"
        aria-label="Theme"
        className="fixed inset-x-0 bottom-0 rounded-t-xl bg-panel shadow-e3 p-4 pb-[max(1rem,env(safe-area-inset-bottom))] lg:absolute lg:inset-auto lg:bottom-14 lg:left-4 lg:w-64 lg:rounded-xl lg:p-4 border border-line z-[46] text-xs"
      >
        <div className="flex items-center justify-between pb-2 mb-3 border-b border-line">
          <div className="flex items-center gap-1.5 font-semibold text-ink">
            <Palette className="w-4 h-4 text-ember" />
            <span>Theme</span>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="text-ink3 hover:text-ink transition-colors p-1"
            aria-label="Close"
          >
            <X className="w-4 h-4" />
          </button>
        </div>

        <div className="grid grid-cols-3 gap-1.5 mb-3">
          {(["warm", "neutral", "custom"] as const).map((t) => (
            <button
              key={t}
              type="button"
              onClick={() => changeTheme(t)}
              className={`py-1.5 px-2 rounded-lg font-medium capitalize text-center transition-colors ${
                theme === t
                  ? "bg-ember text-bg font-semibold"
                  : "bg-panel2 text-ink2 hover:text-ink hover:bg-line2"
              }`}
            >
              {t}
            </button>
          ))}
        </div>

        {theme === "custom" && (
          <div className="space-y-3 pt-2 border-t border-line">
            <div>
              <div className="flex items-center justify-between text-ink3 mb-1">
                <span>Accent hue</span>
                <span className="font-mono text-ink">{hue}°</span>
              </div>
              <input
                type="range"
                min="0"
                max="360"
                value={hue}
                onChange={(e) => changeHue(e.target.value)}
                className="w-full h-1.5 bg-panel2 rounded-lg appearance-none cursor-pointer accent-ember"
              />
            </div>

            <div className="flex items-center justify-between">
              <span className="text-ink3">Base mode</span>
              <div className="grid grid-cols-2 gap-1 bg-panel2 p-0.5 rounded-lg border border-line">
                <button
                  type="button"
                  onClick={() => changeBase("dark")}
                  className={`px-2 py-0.5 rounded text-xs font-medium transition-colors ${
                    base === "dark" ? "bg-panel text-ink shadow-xs" : "text-ink3 hover:text-ink"
                  }`}
                >
                  Dark
                </button>
                <button
                  type="button"
                  onClick={() => changeBase("light")}
                  className={`px-2 py-0.5 rounded text-xs font-medium transition-colors ${
                    base === "light" ? "bg-panel text-ink shadow-xs" : "text-ink3 hover:text-ink"
                  }`}
                >
                  Light
                </button>
              </div>
            </div>
          </div>
        )}
      </div>
    </>
  );
}

export default ThemePopover;
