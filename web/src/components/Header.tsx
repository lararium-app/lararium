import { useState } from "react";
import { Menu, Share2, Check, Lock, AlertTriangle, Layers } from "lucide-react";

interface HeaderProps {
  title: string;
  modelPin?: string;
  onToggleMobileNav: () => void;
  showPreviewPanels: boolean;
  onTogglePreviewPanels: () => void;
}

export function Header({
  title,
  modelPin,
  onToggleMobileNav,
  showPreviewPanels,
  onTogglePreviewPanels,
}: HeaderProps) {
  const [copied, setCopied] = useState(false);

  const isLoopback =
    typeof window !== "undefined" &&
    (window.location.hostname === "localhost" ||
      window.location.hostname === "127.0.0.1" ||
      window.location.hostname === "[::1]" ||
      window.location.hostname === "::1" ||
      window.location.hostname === "");
  const isHttps = typeof window !== "undefined" && window.location.protocol === "https:";

  const handleShare = () => {
    if (navigator.share) {
      navigator.share({ title: document.title, url: window.location.href }).catch(() => {});
    } else {
      navigator.clipboard.writeText(window.location.href).then(() => {
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      }).catch(() => {});
    }
  };

  return (
    <header className="h-14 border-b border-line bg-bg2 px-4 flex items-center justify-between shrink-0 z-20">
      <div className="flex items-center gap-3 min-w-0">
        <button
          type="button"
          onClick={onToggleMobileNav}
          className="min-h-[40px] min-w-[40px] flex items-center justify-center p-2 rounded-lg text-ink2 hover:text-ink hover:bg-panel lg:hidden transition-colors"
          title="Toggle navigation"
          aria-label="Toggle navigation"
        >
          <Menu className="w-4 h-4" />
        </button>
        <span className="text-base font-medium font-brand text-ink truncate">{title}</span>
        {modelPin && (
          <span className="hidden sm:inline-flex items-center px-2 py-0.5 rounded-md text-xs font-mono bg-panel text-ink2">
            {modelPin}
          </span>
        )}
      </div>

      <div className="flex items-center gap-2">
        {/* Transport chip */}
        <span className="hidden sm:inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md text-xs font-mono bg-panel text-ink2">
          {isLoopback ? (
            <>
              <Lock className="w-3 h-3 text-ok" />
              <span>loopback</span>
            </>
          ) : isHttps ? (
            <>
              <Lock className="w-3 h-3 text-ok" />
              <span>https</span>
            </>
          ) : (
            <>
              <AlertTriangle className="w-3 h-3 text-warn" />
              <span>unencrypted</span>
            </>
          )}
        </span>

        {/* Feature Flag Toggle for Preview Panels */}
        <button
          type="button"
          onClick={onTogglePreviewPanels}
          className="min-h-[40px] min-w-[40px] flex items-center justify-center p-2 xl:px-2.5 xl:py-1 rounded-lg text-xs font-medium text-ink3 hover:text-ink hover:bg-panel2 transition-colors gap-1.5"
          title="Toggle preview views (Cells & Memory inspectors)"
          aria-label={`Preview views: ${showPreviewPanels ? "On" : "Off"}`}
        >
          <Layers className="w-3.5 h-3.5" />
          <span
            className={`w-1.5 h-1.5 rounded-full ${
              showPreviewPanels ? "bg-ok" : "bg-ink3"
            }`}
          />
          <span className="hidden xl:inline">Preview</span>
        </button>

        {/* Share Button */}
        <button
          type="button"
          onClick={handleShare}
          className="min-h-[40px] min-w-[40px] flex items-center justify-center p-2 lg:px-3 lg:py-1 rounded-lg text-xs font-medium bg-ember hover:bg-ember2 text-bg transition-colors"
          title="Share"
          aria-label={copied ? "Copied" : "Share"}
        >
          {copied ? (
            <>
              <Check className="w-3.5 h-3.5 text-bg" />
              <span className="text-bg hidden lg:inline ml-1.5">Copied</span>
            </>
          ) : (
            <>
              <Share2 className="w-3.5 h-3.5" />
              <span className="hidden lg:inline ml-1.5">Share</span>
            </>
          )}
        </button>
      </div>
    </header>
  );
}

export default Header;
