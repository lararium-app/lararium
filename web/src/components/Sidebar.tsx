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

function formatSessionRelativeTime(createdStr?: string) {
  if (!createdStr) return "";
  const date = new Date(createdStr);
  if (isNaN(date.getTime())) return "";
  const now = new Date();

  const todayStart = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const yesterdayStart = todayStart - 86400000;

  const pad = (n: number) => n.toString().padStart(2, "0");
  const timeStr = `${pad(date.getHours())}:${pad(date.getMinutes())}`;

  const t = date.getTime();
  if (t >= todayStart) return `Today ${timeStr}`;
  if (t >= yesterdayStart) return `Yesterday ${timeStr}`;
  const monthNames = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
  return `${monthNames[date.getMonth()]} ${date.getDate()} ${timeStr}`;
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
          className="fixed inset-0 bg-scrim animate-[fade-in_200ms_var(--ease-out)] z-30 lg:hidden"
          style={{ height: "var(--app-h, 100vh)" }}
          onClick={onCloseMobile}
        />
      )}

      <aside
        ref={drawerRef}
        aria-hidden={!isDesktop && !isOpenMobile ? "true" : undefined}
        aria-label="Navigation sidebar"
        className={`fixed lg:static top-0 bottom-0 left-0 w-64 h-app lg:h-full max-h-app bg-bg2 border-r border-line z-[35] flex flex-col transition-transform duration-[var(--dur-base)] ease-[var(--ease-out)] lg:translate-x-0 ${
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
            <span className="text-ember">
              <svg viewBox="0 0 24 24" fill="currentColor" className="w-5 h-5" aria-hidden="true">
                <path d="M8.5 14.5A2.5 2.5 0 0 0 11 12c0-1.38-.5-2-1-3-1.072-2.143-.224-4.054 2-6 .5 2.5 2 4.9 4 6.5 2 1.6 3 3.5 3 5.5a7 7 0 1 1-14 0c0-1.153.433-2.294 1-3a2.5 2.5 0 0 0 2.5 2.5z" />
              </svg>
            </span>
            <div>
              <h1 className="font-bold text-ink text-sm tracking-tight font-brand">Lararium</h1>
              <div className="text-xs text-ink3">house hearth · v0.8</div>
            </div>
          </div>
          <button
            type="button"
            onClick={() => {
              onCreateSession();
              onCloseMobile();
            }}
            className="w-full min-h-[40px] flex items-center justify-center gap-2 py-2 px-3 rounded-lg bg-ember hover:bg-ember2 text-bg text-xs font-medium transition-colors"
          >
            <Plus className="w-4 h-4" />
            <span>New session</span>
          </button>
        </div>

        {/* View Switcher Navigation */}
        <div className="p-2 border-b border-line flex flex-col gap-1">
          <button
            type="button"
            onClick={() => {
              onSelectView("chat");
              onCloseMobile();
            }}
            className={`w-full flex items-center gap-2 h-10 rounded-lg px-2 text-sm transition-colors ${
              activeView === "chat"
                ? "bg-[var(--ember-dim)] text-ink border-l-2 border-ember font-medium"
                : "text-ink2 hover:bg-panel2 border-l-2 border-transparent"
            }`}
          >
            <MessageSquare className="w-4 h-4" />
            <span>Hearth</span>
          </button>
          <button
            type="button"
            onClick={() => {
              onSelectView("doors");
              onCloseMobile();
            }}
            className={`w-full flex items-center gap-2 h-10 rounded-lg px-2 text-sm transition-colors ${
              activeView === "doors"
                ? "bg-[var(--ember-dim)] text-ink border-l-2 border-ember font-medium"
                : "text-ink2 hover:bg-panel2 border-l-2 border-transparent"
            }`}
          >
            <Shield className="w-4 h-4" />
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
                className={`w-full flex items-center gap-2 h-10 rounded-lg px-2 text-sm transition-colors ${
                  activeView === "cells"
                    ? "bg-[var(--ember-dim)] text-ink border-l-2 border-ember font-medium"
                    : "text-ink2 hover:bg-panel2 border-l-2 border-transparent"
                }`}
              >
                <Cpu className="w-4 h-4" />
                <span>Cells</span>
              </button>
              <button
                type="button"
                onClick={() => {
                  onSelectView("memory");
                  onCloseMobile();
                }}
                className={`w-full flex items-center gap-2 h-10 rounded-lg px-2 text-sm transition-colors ${
                  activeView === "memory"
                    ? "bg-[var(--ember-dim)] text-ink border-l-2 border-ember font-medium"
                    : "text-ink2 hover:bg-panel2 border-l-2 border-transparent"
                }`}
              >
                <Database className="w-4 h-4" />
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
              className="w-full pl-8 pr-3 py-1.5 rounded-lg bg-panel2 border-none text-base lg:text-sm text-ink placeholder-ink3"
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
                  <div className="px-2 text-xs uppercase tracking-wide text-ink3 font-medium">
                    {gKey}
                  </div>
                  {list.map((s) => {
                    const isActive = s.id === currentSessionId && activeView === "chat";
                    const relTime = formatSessionRelativeTime(s.created);
                    const displayTitle = s.title || (relTime ? `Untitled · ${relTime}` : "Untitled");
                    return (
                      <button
                        key={s.id}
                        type="button"
                        onClick={() => {
                          onSelectSession(s.id);
                          onCloseMobile();
                        }}
                        className={`w-full min-h-[40px] flex flex-col justify-center text-left px-2.5 py-2 rounded-lg text-xs transition-colors gap-0.5 ${
                          isActive
                            ? "bg-[var(--ember-dim)] text-ink font-medium"
                            : "text-ink2 hover:text-ink hover:bg-panel2"
                        }`}
                      >
                        <div className="flex items-center justify-between w-full">
                          <span className="truncate flex-1">{displayTitle}</span>
                          {isActive && (s.kind === "side" || s.parent) && (
                            <span className="ml-1 text-xs px-1.5 py-0.5 rounded bg-panel2 text-ink3 border border-line">
                              branched
                            </span>
                          )}
                        </div>
                        {(s.preview || s.last_message) && (
                          <span className="text-xs text-ink3 truncate">
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
            <span className="text-xs text-ink3 truncate max-w-[110px]" title={statusText}>
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
              className="min-h-[40px] min-w-[40px] flex items-center justify-center p-2 rounded-lg text-ink3 hover:text-ink hover:bg-panel transition-colors"
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
              className="min-h-[40px] min-w-[40px] flex items-center justify-center p-2 rounded-lg text-ink3 hover:text-ink hover:bg-panel transition-colors"
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
