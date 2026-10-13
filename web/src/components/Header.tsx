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
          className="min-h-[44px] min-w-[44px] flex items-center justify-center p-2 rounded-lg text-ink2 hover:text-ink hover:bg-panel lg:hidden transition-colors"
          title="Toggle navigation"
          aria-label="Toggle navigation"
        >
          <Menu className="w-5 h-5" />
        </button>
        <span className="font-semibold text-sm text-ink truncate font-brand">{title}</span>
        {modelPin && (
          <span className="hidden sm:inline-flex items-center px-2 py-0.5 rounded text-xs font-mono bg-panel text-ink2 border border-line">
            {modelPin}
          </span>
        )}
      </div>

      <div className="flex items-center gap-2">
        {/* Transport chip */}
        <span className="hidden sm:inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-mono bg-panel border border-line text-ink2">
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
          className={`min-h-[44px] min-w-[44px] flex items-center justify-center p-2 lg:px-2.5 lg:py-1 rounded-lg text-xs font-medium border transition-colors ${
            showPreviewPanels
              ? "bg-warn/15 text-warn border-warn/30 hover:bg-warn/20"
              : "bg-panel text-ink3 border-line hover:text-ink hover:bg-panel2"
          }`}
          title="Toggle preview views (Cells & Memory inspectors)"
          aria-label={`Preview views: ${showPreviewPanels ? "On" : "Off"}`}
        >
          <Layers className="w-3.5 h-3.5" />
          <span className="hidden lg:inline ml-1.5">Preview Views: {showPreviewPanels ? "On" : "Off"}</span>
        </button>

        {/* Share Button */}
        <button
          type="button"
          onClick={handleShare}
          className="min-h-[44px] min-w-[44px] flex items-center justify-center p-2 lg:px-3 lg:py-1 rounded-lg text-xs font-medium bg-panel hover:bg-panel2 text-ink border border-line transition-colors shadow-2xs"
          title="Share"
          aria-label={copied ? "Copied" : "Share"}
        >
          {copied ? (
            <>
              <Check className="w-3.5 h-3.5 text-ok" />
              <span className="text-ok hidden lg:inline ml-1.5">Copied</span>
            </>
          ) : (
            <>
              <Share2 className="w-3.5 h-3.5 text-ink2" />
              <span className="hidden lg:inline ml-1.5">Share</span>
            </>
          )}
        </button>
      </div>
    </header>
  );
}

export default Header;
