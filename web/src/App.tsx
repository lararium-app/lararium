import { useState, useEffect, useCallback, useRef } from "react";
import { Session, SurfaceEvent, CustosCard } from "./types/hearthd";
import { Sidebar } from "./components/Sidebar";
import { Header } from "./components/Header";
import { ChatWindow } from "./components/ChatWindow";
import { DoorsPanel } from "./components/DoorsPanel";
import { CellMonitor } from "./components/CellMonitor";
import { MemoryViewer } from "./components/MemoryViewer";
import { KeysDrawer } from "./components/KeysDrawer";
import { ThemePopover } from "./components/ThemePopover";
import { useHearthStream } from "./hooks/useHearthStream";

export function App() {
  const [token, setToken] = useState<string | null>(null);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [currentSessionId, setCurrentSessionId] = useState<string | null>(null);
  const [events, setEvents] = useState<SurfaceEvent[]>([]);
  const [activeView, setActiveView] = useState<"chat" | "doors" | "cells" | "memory">("chat");
  const [statusText, setStatusText] = useState("hearth online");
  const [statusType, setStatusType] = useState<"ok" | "warn" | "err">("ok");

  // Feature flag for Cell & Memory placeholder panels
  const [showPreviewPanels, setShowPreviewPanels] = useState(false);

  // Custos custody cards
  const [custosCards, setCustosCards] = useState<CustosCard[]>([]);

  // Modals & Drawers
  const [keysOpen, setKeysOpen] = useState(false);
  const [themeOpen, setThemeOpen] = useState(false);
  const [mobileNavOpen, setMobileNavOpen] = useState(false);

  const catchUpTimerRef = useRef<number | null>(null);

  // 1. Token initialization from URL hash or sessionStorage
  useEffect(() => {
    let tok: string | null = null;
    const hash = window.location.hash;

    if (hash.startsWith("#token=")) {
      tok = hash.slice(7);
    } else if (hash.startsWith("#lar1_")) {
      tok = hash.slice(1);
    } else if (hash.startsWith("#") && hash.length > 1) {
      tok = hash.slice(1);
    }

    if (tok) {
      sessionStorage.setItem("lar_token", tok);
      window.history.replaceState(null, "", window.location.pathname);
    } else {
      tok = sessionStorage.getItem("lar_token");
    }

    if (tok) {
      setToken(tok);
      setStatusText("hearth online");
      setStatusType("ok");
    } else {
      setStatusText("open the URL printed by hearthd token create");
      setStatusType("warn");
    }
  }, []);

  // Transcript fetching
  const fetchTranscript = useCallback(
    async (sessId: string) => {
      if (!token || !sessId) return;
      try {
        const resp = await fetch(
          `/v1/sessions/${encodeURIComponent(sessId)}/events?after_seq=0`,
          {
            headers: {
              Authorization: `Bearer ${token}`,
            },
          }
        );
        if (resp.status === 401) {
          setStatusText("unauthorized — token revoked or wrong");
          setStatusType("err");
          return;
        }
        if (!resp.ok) {
          const err = await resp.json().catch(() => ({ error: resp.statusText }));
          throw new Error(err.error || resp.statusText);
        }

        const data = await resp.json();
        setEvents(Array.isArray(data.events) ? data.events : []);

        if (data.in_flight) {
          setStatusText("turn in flight…");
          setStatusType("warn");
          if (!catchUpTimerRef.current) {
            catchUpTimerRef.current = window.setInterval(() => {
              if (!document.hidden) {
                fetchTranscript(sessId);
              }
            }, 2000);
          }
        } else {
          if (catchUpTimerRef.current) {
            clearInterval(catchUpTimerRef.current);
            catchUpTimerRef.current = null;
          }
          setStatusText("hearth online");
          setStatusType("ok");
        }
      } catch (e: any) {
        setStatusText(`failed to load transcript: ${e.message}`);
        setStatusType("err");
      }
    },
    [token]
  );

  // Hook for streaming messages
  const {
    isStreaming,
    streamingText,
    activeToolCalls,
    pendingApprovals,
    sendMessage: streamSendMessage,
    cancelTurn: streamCancelTurn,
    resolveApproval: streamResolveApproval,
  } = useHearthStream({
    token,
    onTurnComplete: () => {
      if (currentSessionId) {
        fetchTranscript(currentSessionId);
      }
    },
    onError: (err) => {
      if (err.includes("unauthorized")) {
        setStatusText("unauthorized — token revoked or wrong");
        setStatusType("err");
      } else {
        setStatusText(`error: ${err}`);
        setStatusType("err");
      }
    },
  });

  // Sessions fetching
  const fetchSessions = useCallback(
    async (selectId?: string) => {
      if (!token) return;
      try {
        const resp = await fetch("/v1/sessions", {
          headers: {
            Authorization: `Bearer ${token}`,
          },
        });
        if (resp.status === 401) {
          setStatusText("unauthorized — token revoked or wrong");
          setStatusType("err");
          return;
        }
        if (!resp.ok) {
          const err = await resp.json().catch(() => ({ error: resp.statusText }));
          throw new Error(err.error || resp.statusText);
        }
        const data: Session[] = await resp.json();
        setSessions(data);
        setStatusText("hearth online");
        setStatusType("ok");

        if (data.length > 0) {
          const target = selectId || currentSessionId || data[0].id;
          const exists = data.some((s) => s.id === target);
          const chosen = exists ? target : data[0].id;
          setCurrentSessionId(chosen);
          fetchTranscript(chosen);
        } else if (!selectId) {
          // If no sessions, create one
          handleCreateSession();
        }
      } catch (e: any) {
        setStatusText(`failed to load sessions: ${e.message}`);
        setStatusType("err");
      }
    },
    [token, currentSessionId, fetchTranscript]
  );

  const handleCreateSession = async () => {
    if (!token) return;
    try {
      const resp = await fetch("/v1/sessions", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({}),
      });
      if (resp.status === 401) {
        setStatusText("unauthorized — token revoked or wrong");
        setStatusType("err");
        return;
      }
      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(err.error || resp.statusText);
      }
      const newSess: Session = await resp.json();
      setStatusText("hearth online");
      setStatusType("ok");
      await fetchSessions(newSess.id);
      setActiveView("chat");
    } catch (e: any) {
      setStatusText(`failed to create session: ${e.message}`);
      setStatusType("err");
    }
  };

  const handleSelectSession = (id: string) => {
    setCurrentSessionId(id);
    setActiveView("chat");
    fetchTranscript(id);
  };

  // Custos Cards fetching & SSE
  const fetchCustosCards = useCallback(async () => {
    if (!token) return;
    try {
      const resp = await fetch("/v1/custos/cards", {
        headers: {
          Authorization: `Bearer ${token}`,
        },
      });
      if (!resp.ok) return;
      const data = await resp.json();
      if (data && Array.isArray(data.cards)) {
        setCustosCards(data.cards);
      }
    } catch {
      // Ignored
    }
  }, [token]);

  useEffect(() => {
    if (!token) return;
    fetchSessions();
    fetchCustosCards();

    // Custos SSE Stream
    const ac = new AbortController();
    const listenCustosEvents = async () => {
      try {
        const resp = await fetch("/v1/custos/cards/events", {
          headers: {
            Accept: "text/event-stream",
            Authorization: `Bearer ${token}`,
          },
          signal: ac.signal,
        });
        if (!resp.ok || !resp.body) return;

        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";

        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          const parts = buffer.split("\n\n");
          buffer = parts.pop() || "";

          for (const part of parts) {
            if (!part.trim() || part.startsWith(":")) continue;
            let evType: string | null = null;
            let evData: any = null;

            for (const line of part.split("\n")) {
              if (line.startsWith("event:")) evType = line.slice(6).trim();
              else if (line.startsWith("data:")) {
                try {
                  evData = JSON.parse(line.slice(5).trim());
                } catch {}
              }
            }

            if (evType === "custos_card" && evData) {
              setCustosCards((prev) => {
                if (prev.some((c) => c.id === evData.id)) return prev;
                return [...prev, evData];
              });
            } else if (evType === "custos_gone" && evData) {
              setCustosCards((prev) => prev.filter((c) => c.id !== evData.id));
            }
          }
        }
      } catch (e: any) {
        if (e.name !== "AbortError") {
          setTimeout(fetchCustosCards, 3000);
        }
      }
    };

    listenCustosEvents();

    return () => {
      ac.abort();
      if (catchUpTimerRef.current) {
        clearInterval(catchUpTimerRef.current);
      }
    };
  }, [token, fetchSessions, fetchCustosCards]);

  const handleResolveCustosCard = async (
    id: string,
    verdict: "once" | "always" | "deny"
  ) => {
    if (!token) return;
    const resp = await fetch(
      `/v1/custos/cards/${encodeURIComponent(id)}/resolve`,
      {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({ verdict }),
      }
    );

    if (resp.status === 200) {
      setCustosCards((prev) => prev.filter((c) => c.id !== id));
      return;
    }

    const err = await resp.json().catch(() => ({ error: resp.statusText }));
    throw new Error(err.error || resp.statusText);
  };

  const handleSendMessage = (text: string) => {
    if (!currentSessionId) return;
    streamSendMessage(currentSessionId, text);
  };

  const handleCancelTurn = () => {
    if (!currentSessionId) return;
    streamCancelTurn(currentSessionId);
  };

  const handleResolveToolApproval = async (
    approvalId: string,
    verdict: "allow" | "deny"
  ) => {
    if (!currentSessionId) return;
    await streamResolveApproval(currentSessionId, approvalId, verdict);
  };

  const handleTogglePreviewPanels = () => {
    setShowPreviewPanels((prev) => {
      const next = !prev;
      if (!next && (activeView === "cells" || activeView === "memory")) {
        setActiveView("chat");
      }
      return next;
    });
  };

  const currentSession = sessions.find((s) => s.id === currentSessionId) || null;

  return (
    <div
      className="flex h-app max-h-app w-full overflow-hidden bg-bg text-ink"
      style={{ height: "var(--app-h, 100vh)", maxHeight: "var(--app-h, 100vh)" }}
    >
      <Sidebar
        sessions={sessions}
        currentSessionId={currentSessionId}
        activeView={activeView}
        statusText={statusText}
        statusType={statusType}
        showPreviewPanels={showPreviewPanels}
        onSelectSession={handleSelectSession}
        onCreateSession={handleCreateSession}
        onSelectView={setActiveView}
        onOpenKeys={() => setKeysOpen(true)}
        onToggleTheme={() => setThemeOpen(!themeOpen)}
        isOpenMobile={mobileNavOpen}
        onCloseMobile={() => setMobileNavOpen(false)}
      />

      <div className="flex-1 flex flex-col min-w-0 h-full overflow-hidden">
        <Header
          title={
            activeView === "chat"
              ? currentSession?.title || currentSession?.id || "Lararium"
              : activeView === "doors"
              ? "Door policy"
              : activeView === "cells"
              ? "Cells & Sandbox Monitor"
              : "Memory & Session Log Inspector"
          }
          modelPin={activeView === "chat" ? currentSession?.model_pin : undefined}
          onToggleMobileNav={() => setMobileNavOpen(!mobileNavOpen)}
          showPreviewPanels={showPreviewPanels}
          onTogglePreviewPanels={handleTogglePreviewPanels}
        />

        <main className="flex-1 flex flex-col min-h-0 overflow-hidden relative">
          {activeView === "chat" && (
            <ChatWindow
              currentSession={currentSession}
              events={events}
              isStreaming={isStreaming}
              streamingText={streamingText}
              activeToolCalls={activeToolCalls}
              pendingApprovals={pendingApprovals}
              custosCards={custosCards}
              onSendMessage={handleSendMessage}
              onCancelTurn={handleCancelTurn}
              onResolveToolApproval={handleResolveToolApproval}
              onResolveCustosCard={handleResolveCustosCard}
            />
          )}

          {activeView === "doors" && <DoorsPanel token={token} />}

          {activeView === "cells" && <CellMonitor />}

          {activeView === "memory" && <MemoryViewer />}

          <ThemePopover isOpen={themeOpen} onClose={() => setThemeOpen(false)} />
        </main>
      </div>

      <KeysDrawer
        isOpen={keysOpen}
        onClose={() => setKeysOpen(false)}
        token={token}
      />
    </div>
  );
}

export default App;
