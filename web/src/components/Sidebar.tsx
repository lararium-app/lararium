import { useState, useMemo, useEffect, useRef } from "react";
import { Plus, Search, Key, Palette, MessageSquare, Shield, Cpu, Database } from "lucide-react";
import { Session } from "../types/hearthd";

interface SidebarProps {
  sessions: Session[];
  currentSessionId: string | null;
  activeView: "chat" | "doors" | "cells" | "memory";
  statusText: string;
  statusType: "ok" | "warn" | "err";
  showPreviewPanels: boolean;
  onSelectSession: (id: string) => void;
  onCreateSession: () => void;
  onSelectView: (view: "chat" | "doors" | "cells" | "memory") => void;
  onOpenKeys: () => void;
  onToggleTheme: () => void;
  isOpenMobile: boolean;
  onCloseMobile: () => void;
}

function getSessionGroup(createdStr: string) {
  if (!createdStr) return "Older";
  const date = new Date(createdStr);
  if (isNaN(date.getTime())) return "Older";
  const now = new Date();

  const todayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const yesterdayStart = todayStart - 86400000;
  const prev7Start = todayStart - 7 * 86400000;

  const t = date.getTime();
  if (t >= todayStart) return "Today";
  if (t >= yesterdayStart) return "Yesterday";
  if (t >= prev7Start) return "Previous 7 days";
  return "Older";
}

export function Sidebar({
  sessions,
  currentSessionId,
  activeView,
  statusText,
  statusType,
  showPreviewPanels,
  onSelectSession,
  onCreateSession,
  onSelectView,
  onOpenKeys,
  onToggleTheme,
  isOpenMobile,
  onCloseMobile,
}: SidebarProps) {
  const [searchQuery, setSearchQuery] = useState("");
  const [isDesktop, setIsDesktop] = useState(() =>
    typeof window !== "undefined" ? window.innerWidth >= 1024 : false
  );

  const drawerRef = useRef<HTMLElement | null>(null);
  const previousActiveElementRef = useRef<HTMLElement | null>(null);

  useEffect(() => {
    if (typeof window === "undefined") return;
    const updateDesktop = () => {
      const match = window.innerWidth >= 1024;
      setIsDesktop(match);
      if (match) {
        onCloseMobile();
      }
    };
    updateDesktop();
    window.addEventListener("resize", updateDesktop);
    return () => window.removeEventListener("resize", updateDesktop);
  }, [onCloseMobile]);

  useEffect(() => {
    if (isDesktop) return;
    if (isOpenMobile) {
      previousActiveElementRef.current = document.activeElement as HTMLElement | null;
      const timer = setTimeout(() => {
        const focusable = drawerRef.current?.querySelector<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
        );
        focusable?.focus();
      }, 50);
      return () => clearTimeout(timer);
    } else if (previousActiveElementRef.current) {
      previousActiveElementRef.current.focus();
      previousActiveElementRef.current = null;
    }
  }, [isOpenMobile, isDesktop]);

  useEffect(() => {
    if (!isOpenMobile || isDesktop) return;

    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onCloseMobile();
        return;
      }

      if (e.key === "Tab") {
        const drawer = drawerRef.current;
        if (!drawer) return;
        const focusables = drawer.querySelectorAll<HTMLElement>(
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
  }, [isOpenMobile, isDesktop, onCloseMobile]);

  const filteredSessions = useMemo(() => {
    const q = searchQuery.trim().toLowerCase();
    if (!q) return sessions;
    return sessions.filter(
      (s) =>
        s.id.toLowerCase().includes(q) ||
        (s.title && s.title.toLowerCase().includes(q))
    );
  }, [sessions, searchQuery]);

  const groups = useMemo(() => {
    const res: Record<string, Session[]> = {
      Today: [],
      Yesterday: [],
      "Previous 7 days": [],
      Older: [],
    };
    for (const s of filteredSessions) {
      const g = getSessionGroup(s.created);
      res[g].push(s);
    }
    return res;
  }, [filteredSessions]);

  const groupKeys = ["Today", "Yesterday", "Previous 7 days", "Older"] as const;

  return (
    <>
      {/* Mobile backdrop */}
      {isOpenMobile && (
        <div
          className="fixed inset-0 bg-black/60 z-30 lg:hidden"
          style={{ height: "var(--app-h, 100vh)" }}
          onClick={onCloseMobile}
        />
      )}

      <aside
        ref={drawerRef}
        aria-hidden={!isDesktop && !isOpenMobile ? "true" : undefined}
        aria-label="Navigation sidebar"
        className={`fixed lg:static top-0 bottom-0 left-0 w-64 h-app lg:h-full max-h-app bg-bg2 border-r border-line z-35 flex flex-col transition-transform duration-200 ease-in-out lg:translate-x-0 ${
          isOpenMobile ? "translate-x-0" : "-translate-x-full"
        }`}
        style={{
          height: isDesktop ? "100%" : "var(--app-h, 100vh)",
          maxHeight: isDesktop ? "100%" : "var(--app-h, 100vh)",
        }}
      >
        {/* Brand & New Session */}
        <div className="p-4 border-b border-line space-y-3">
          <div className="flex items-center gap-2.5">
            <span className="text-2xl">🔥</span>
            <div>
              <h1 className="font-bold text-ink text-sm tracking-tight font-brand">Lararium</h1>
              <div className="text-[11px] text-ink3">house hearth · v0.8</div>
            </div>
          </div>
          <button
            type="button"
            onClick={() => {
              onCreateSession();
              onCloseMobile();
            }}
            className="w-full min-h-[44px] flex items-center justify-center gap-2 py-2 px-3 rounded-xl bg-panel hover:bg-panel2 border border-line text-xs font-semibold text-ink transition-colors shadow-2xs"
          >
            <Plus className="w-4 h-4 text-ember" />
            <span>New session</span>
          </button>
        </div>

        {/* View Switcher Navigation */}
        <div className="p-2 border-b border-line grid grid-cols-2 gap-1 text-xs">
          <button
            type="button"
            onClick={() => {
              onSelectView("chat");
              onCloseMobile();
            }}
            className={`min-h-[44px] flex items-center gap-2 px-2.5 py-1.5 rounded-lg font-medium transition-colors ${
              activeView === "chat"
                ? "bg-panel text-ember shadow-2xs border border-line"
                : "text-ink3 hover:text-ink hover:bg-panel/50"
            }`}
          >
            <MessageSquare className="w-3.5 h-3.5" />
            <span>Hearth</span>
          </button>
          <button
            type="button"
            onClick={() => {
              onSelectView("doors");
              onCloseMobile();
            }}
            className={`min-h-[44px] flex items-center gap-2 px-2.5 py-1.5 rounded-lg font-medium transition-colors ${
              activeView === "doors"
                ? "bg-panel text-ember shadow-2xs border border-line"
                : "text-ink3 hover:text-ink hover:bg-panel/50"
            }`}
          >
            <Shield className="w-3.5 h-3.5" />
            <span>Doors</span>
          </button>
          {showPreviewPanels && (
            <>
              <button
                type="button"
                onClick={() => {
                  onSelectView("cells");
                  onCloseMobile();
                }}
                className={`min-h-[44px] flex items-center gap-2 px-2.5 py-1.5 rounded-lg font-medium transition-colors ${
                  activeView === "cells"
                    ? "bg-panel text-ember shadow-2xs border border-line"
                    : "text-ink3 hover:text-ink hover:bg-panel/50"
                }`}
              >
                <Cpu className="w-3.5 h-3.5" />
                <span>Cells</span>
              </button>
              <button
                type="button"
                onClick={() => {
                  onSelectView("memory");
                  onCloseMobile();
                }}
                className={`min-h-[44px] flex items-center gap-2 px-2.5 py-1.5 rounded-lg font-medium transition-colors ${
                  activeView === "memory"
                    ? "bg-panel text-ember shadow-2xs border border-line"
                    : "text-ink3 hover:text-ink hover:bg-panel/50"
                }`}
              >
                <Database className="w-3.5 h-3.5" />
                <span>Memory</span>
              </button>
            </>
          )}
        </div>

        {/* Session Search */}
        <div className="p-3 border-b border-line">
          <div className="relative">
            <Search className="w-3.5 h-3.5 text-ink3 absolute left-2.5 top-3" />
            <input
              type="text"
              placeholder="Search sessions…"
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              className="w-full pl-8 pr-3 py-1.5 rounded-lg bg-panel border border-line text-base text-ink placeholder-ink3 focus:outline-hidden focus:border-ember"
            />
          </div>
        </div>

        {/* Sessions List */}
        <div className="flex-1 overflow-y-auto p-2 space-y-4">
          {filteredSessions.length === 0 ? (
            <div className="text-center py-6 text-xs text-ink3">No sessions found</div>
          ) : (
            groupKeys.map((gKey) => {
              const list = groups[gKey];
              if (list.length === 0) return null;
              return (
                <div key={gKey} className="space-y-1">
                  <div className="px-2 text-[10px] font-semibold uppercase tracking-wider text-ink3">
                    {gKey}
                  </div>
                  {list.map((s) => {
                    const isActive = s.id === currentSessionId && activeView === "chat";
                    return (
                      <button
                        key={s.id}
                        type="button"
                        onClick={() => {
                          onSelectSession(s.id);
                          onCloseMobile();
                        }}
                        className={`w-full min-h-[44px] flex flex-col justify-center text-left px-2.5 py-2 rounded-lg text-xs transition-colors gap-0.5 ${
                          isActive
                            ? "bg-panel text-ink border border-line shadow-2xs font-medium"
                            : "text-ink2 hover:text-ink hover:bg-panel/50"
                        }`}
                      >
                        <div className="flex items-center justify-between w-full">
                          <span className="truncate flex-1">{s.title || s.id}</span>
                          {(s.kind === "side" || s.parent) && (
                            <span className="ml-1 text-[10px] px-1.5 py-0.2 rounded bg-panel2 text-ink3 border border-line">
                              branched
                            </span>
                          )}
                        </div>
                        {(s.preview || s.last_message) && (
                          <span className="text-[11px] text-ink3 truncate">
                            {s.preview || s.last_message}
                          </span>
                        )}
                      </button>
                    );
                  })}
                </div>
              );
            })
          )}
        </div>

        {/* Footer actions & status */}
        <div className="p-3 border-t border-line flex items-center justify-between text-xs bg-bg2">
          <div className="flex items-center gap-2">
            <span
              className={`w-2 h-2 rounded-full ${
                statusType === "ok"
                  ? "bg-ok"
                  : statusType === "warn"
                  ? "bg-warn"
                  : "bg-err"
              }`}
            />
            <span className="text-[11px] text-ink3 truncate max-w-[110px]" title={statusText}>
              {statusText}
            </span>
          </div>
          <div className="flex items-center gap-1">
            <button
              type="button"
              onClick={() => {
                onOpenKeys();
                onCloseMobile();
              }}
              className="min-h-[44px] min-w-[44px] flex items-center justify-center p-2 rounded-lg text-ink3 hover:text-ink hover:bg-panel transition-colors"
              title="Provider Keys"
              aria-label="Provider Keys"
            >
              <Key className="w-4 h-4" />
            </button>
            <button
              type="button"
              onClick={() => {
                onToggleTheme();
                onCloseMobile();
              }}
              className="min-h-[44px] min-w-[44px] flex items-center justify-center p-2 rounded-lg text-ink3 hover:text-ink hover:bg-panel transition-colors"
              title="Theme Settings"
              aria-label="Theme Settings"
            >
              <Palette className="w-4 h-4" />
            </button>
          </div>
        </div>
      </aside>
    </>
  );
}

export default Sidebar;
