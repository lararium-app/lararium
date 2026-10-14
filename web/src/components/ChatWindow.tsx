import React, { useRef, useEffect } from "react";
import { Copy, RotateCcw, Check, Square, Send, ChevronRight, Paperclip, Mic } from "lucide-react";
import { Session, SurfaceEvent, ApprovalRequest, CustosCard } from "../types/hearthd";
import { MarkdownRenderer } from "./MarkdownRenderer";
import { ToolApprovalCard, CustosApprovalCard } from "./ApprovalCard";
import { ToolCallState } from "../hooks/useHearthStream";

interface ChatWindowProps {
  currentSession: Session | null;
  events: SurfaceEvent[];
  isStreaming: boolean;
  streamingText: string;
  activeToolCalls: ToolCallState[];
  pendingApprovals: ApprovalRequest[];
  custosCards: CustosCard[];
  onSendMessage: (text: string) => void;
  onCancelTurn: () => void;
  onResolveToolApproval: (approvalId: string, verdict: "allow" | "deny") => Promise<void>;
  onResolveCustosCard: (id: string, verdict: "once" | "always" | "deny") => Promise<void>;
}

const SUGGESTIONS = [
  "Summarize today's notifications",
  "What did the door sensors see?",
  "Draft a weekly digest",
  "What are my provider keys doing?",
];

function formatDigest(digest?: string): string {
  if (!digest) return "";
  if (digest.length <= 13) return digest;
  return `${digest.slice(0, 6)}…${digest.slice(-6)}`;
}

function formatEventTime(ev: SurfaceEvent): string {
  const raw = ev.created || ev.time || ev.ts;
  if (!raw) return "";
  try {
    const d = new Date(raw);
    if (!isNaN(d.getTime())) {
      return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
    }
  } catch {}
  return "";
}

function getToolStateLabel(call: SurfaceEvent, result?: SurfaceEvent): string {
  if (result) {
    if (result.ok === false) return "failed";
    const resApproval = (result as any)?.approval;
    if (resApproval?.decision === "denied") {
      return resApproval.reason === "timeout" ? "timed out" : "denied at door";
    }
    return "approved at door";
  }
  const rawApproval = (call as any).approval;
  if (typeof rawApproval === "string") {
    if (rawApproval.startsWith("denied")) return "denied at door";
    if (rawApproval.startsWith("approved")) return "approved at door";
  }
  return "approved at door";
}

type RenderItem =
  | { kind: "msg"; event: SurfaceEvent; index: number }
  | { kind: "tool"; call: SurfaceEvent; result?: SurfaceEvent; index: number }
  | { kind: "compact"; event: SurfaceEvent; index: number }
  | { kind: "branch"; event: SurfaceEvent; index: number }
  | { kind: "orphan_result"; event: SurfaceEvent; index: number };

function processEvents(events: SurfaceEvent[]): RenderItem[] {
  const callResultMap = new Map<number, SurfaceEvent>();
  const consumedResults = new Set<number>();

  for (let i = 0; i < events.length; i++) {
    const ev = events[i];
    if (ev.t === "tool_result") {
      let matchedCallIdx = -1;
      if (ev.call_id) {
        for (let j = i - 1; j >= 0; j--) {
          if (events[j].t === "tool_call" && events[j].call_id === ev.call_id && !callResultMap.has(j)) {
            matchedCallIdx = j;
            break;
          }
        }
      }
      if (matchedCallIdx === -1) {
        for (let j = i - 1; j >= 0; j--) {
          if (events[j].t === "tool_call" && !callResultMap.has(j)) {
            matchedCallIdx = j;
            break;
          }
        }
      }
      if (matchedCallIdx !== -1) {
        callResultMap.set(matchedCallIdx, ev);
        consumedResults.add(i);
      }
    }
  }

  const items: RenderItem[] = [];
  for (let i = 0; i < events.length; i++) {
    const ev = events[i];
    if (ev.t === "msg") {
      items.push({ kind: "msg", event: ev, index: i });
    } else if (ev.t === "tool_call") {
      items.push({ kind: "tool", call: ev, result: callResultMap.get(i), index: i });
    } else if (ev.t === "tool_result") {
      if (!consumedResults.has(i)) {
        items.push({ kind: "orphan_result", event: ev, index: i });
      }
    } else if (ev.t === "compact") {
      items.push({ kind: "compact", event: ev, index: i });
    } else if (ev.t === "branch") {
      items.push({ kind: "branch", event: ev, index: i });
    }
  }
  return items;
}

export function ChatWindow({
  currentSession: _currentSession,
  events,
  isStreaming,
  streamingText,
  activeToolCalls,
  pendingApprovals,
  custosCards,
  onSendMessage,
  onCancelTurn,
  onResolveToolApproval,
  onResolveCustosCard,
}: ChatWindowProps) {
  const [inputText, setInputText] = React.useState("");
  const [copiedId, setCopiedId] = React.useState<string | null>(null);
  const [copiedDigestId, setCopiedDigestId] = React.useState<string | null>(null);
  // Optimistic user bubble: the transcript only surfaces the question on the
  // post-turn refetch, so keep it visible while the answer streams.
  const [pendingUser, setPendingUser] = React.useState<string | null>(null);
  const scrollEndRef = useRef<HTMLDivElement | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);

  // Auto-scroll on new events or streaming tokens
  useEffect(() => {
    scrollEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [events, streamingText, activeToolCalls, pendingApprovals, custosCards, pendingUser]);

  // Drop the optimistic bubble once the real user message lands in the transcript.
  React.useEffect(() => {
    if (pendingUser && events.some((ev) => ev.t === "msg" && ev.role === "user" && ev.text === pendingUser)) {
      setPendingUser(null);
    }
  }, [events, pendingUser]);

  // Safety net: the question left no transcript trace (error/cancel) — fade the bubble.
  React.useEffect(() => {
    if (pendingUser && !isStreaming) {
      const id = window.setTimeout(() => setPendingUser(null), 1500);
      return () => window.clearTimeout(id);
    }
  }, [pendingUser, isStreaming]);

  const handleCopyMessage = (text: string, id: string) => {
    navigator.clipboard.writeText(text).then(() => {
      setCopiedId(id);
      setTimeout(() => setCopiedId(null), 1500);
    }).catch(() => {});
  };

  const handleCopyDigest = (digest: string, id: string, e: React.MouseEvent) => {
    e.preventDefault();
    e.stopPropagation();
    navigator.clipboard.writeText(digest).then(() => {
      setCopiedDigestId(id);
      setTimeout(() => setCopiedDigestId(null), 1500);
    }).catch(() => {});
  };

  const handleRetry = (eventIndex: number) => {
    // Find preceding user prompt
    for (let i = eventIndex - 1; i >= 0; i--) {
      if (events[i].t === "msg" && events[i].role === "user" && events[i].text) {
        onSendMessage(events[i].text!);
        return;
      }
    }
  };

  const handleSubmit = (e?: React.FormEvent) => {
    if (e) e.preventDefault();
    const trimmed = inputText.trim();
    if (!trimmed || isStreaming) return;
    setInputText("");
    if (textareaRef.current) {
      textareaRef.current.style.height = "auto";
    }
    setPendingUser(trimmed);
    onSendMessage(trimmed);
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault();
      handleSubmit();
    }
  };

  const handleTextareaInput = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    setInputText(e.target.value);
    e.target.style.height = "auto";
    const nextHeight = Math.min(e.target.scrollHeight, 200);
    e.target.style.height = `${nextHeight}px`;
  };

  const hasConversation = events.some((ev) => ev.t === "msg" || ev.t === "tool_call" || ev.t === "tool_result");
  const showEmptyHero = !hasConversation && !isStreaming && pendingApprovals.length === 0;

  return (
    <div className="flex-1 flex flex-col h-full min-w-0 bg-bg overflow-hidden">
      {/* Custos Cards pinned to top of stream */}
      {custosCards.length > 0 && (
        <div className="px-4 pt-3 max-w-4xl w-full mx-auto">
          {custosCards.map((card) => (
            <CustosApprovalCard
              key={card.id}
              card={card}
              onResolve={onResolveCustosCard}
            />
          ))}
        </div>
      )}

      {/* Transcript Scroll Area */}
      <div className="flex-1 overflow-y-auto px-4 py-4 md:px-8 space-y-4">
        <div className="max-w-4xl mx-auto w-full space-y-4">
          {showEmptyHero ? (
            <div className="space-y-4">
              {events.map((ev, idx) => {
                const timeStr = formatEventTime(ev);
                if (ev.t === "branch") {
                  return (
                    <div key={`meta-${idx}`} className="relative py-2 flex items-center justify-center">
                      <div className="absolute inset-0 flex items-center" aria-hidden="true">
                        <div className="w-full border-t border-line" />
                      </div>
                      <span className="relative px-3 bg-bg text-xs text-ink3 select-none">
                        — branched{ev.from_session ? <> from <span className="font-mono">{ev.from_session}</span></> : ""}{timeStr ? ` · ${timeStr}` : ""} —
                      </span>
                    </div>
                  );
                }
                if (ev.t === "compact") {
                  return (
                    <div key={`meta-${idx}`} className="relative py-2 flex items-center justify-center">
                      <div className="absolute inset-0 flex items-center" aria-hidden="true">
                        <div className="w-full border-t border-line" />
                      </div>
                      <span className="relative px-3 bg-bg text-xs text-ink3 select-none">
                        — compacted{timeStr ? ` · ${timeStr}` : ""} —
                      </span>
                    </div>
                  );
                }
                return null;
              })}

              <div className="empty-hero flex flex-col items-center justify-center py-6 sm:py-10 lg:py-16 text-center max-w-md mx-auto">
                <div className="empty-hero-flame text-ember mb-3 sm:mb-4">
                  <svg viewBox="0 0 24 24" fill="currentColor" className="w-12 h-12" aria-hidden="true">
                    <path d="M8.5 14.5A2.5 2.5 0 0 0 11 12c0-1.38-.5-2-1-3-1.072-2.143-.224-4.054 2-6 .5 2.5 2 4.9 4 6.5 2 1.6 3 3.5 3 5.5a7 7 0 1 1-14 0c0-1.153.433-2.294 1-3a2.5 2.5 0 0 0 2.5 2.5z"/>
                  </svg>
                </div>
                <h2 className="empty-hero-title text-2xl font-brand text-ink mb-1 sm:mb-2">Your hearth is warm.</h2>
                <p className="empty-hero-subtitle text-xs text-ink3 mb-3 sm:mb-6 lg:mb-8">Choose a suggestion or ask anything to begin.</p>
                <div className="empty-hero-grid grid grid-cols-1 sm:grid-cols-2 gap-2 w-full">
                  {SUGGESTIONS.map((sug) => (
                    <button
                      key={sug}
                      type="button"
                      onClick={() => {
                        setInputText(sug);
                        textareaRef.current?.focus();
                      }}
                      className="min-h-[44px] p-3 text-left rounded-lg bg-panel border border-line hover:bg-panel2 text-sm text-ink transition-colors flex items-center"
                    >
                      {sug}
                    </button>
                  ))}
                </div>
                <div className="text-xs text-ink3 mt-4">
                  Doors: loopback only · Memory: empty
                </div>
              </div>
            </div>
          ) : (
            processEvents(events).map((item) => {
              if (item.kind === "msg" && item.event.role === "user") {
                return (
                  <div key={`msg-${item.index}`} className="flex justify-end">
                    <div className="max-w-[80%] px-4 py-3 rounded-xl bg-[var(--ember-dim)] border border-ember/25 text-ink text-sm sm:text-[15px] leading-relaxed whitespace-pre-wrap">
                      {item.event.text}
                    </div>
                  </div>
                );
              }

              if (item.kind === "msg" && item.event.role === "assistant") {
                const msgId = `msg-${item.index}`;
                return (
                  <div key={msgId} className="flex flex-col items-start max-w-[85%] group">
                    <div className="w-full px-4 py-3 rounded-xl bg-panel border border-line shadow-e1 text-sm sm:text-[15px] leading-relaxed text-ink">
                      <MarkdownRenderer content={item.event.text || ""} />
                    </div>
                    <div className="flex items-center gap-2 mt-1.5 px-1 text-xs opacity-0 group-hover:opacity-100 [@media(hover:none)]:opacity-100 transition-opacity">
                      <button
                        type="button"
                        onClick={() => handleCopyMessage(item.event.text || "", msgId)}
                        className="flex items-center gap-1 text-xs text-ink3 hover:text-ink transition-colors"
                        title="Copy message"
                      >
                        {copiedId === msgId ? (
                          <>
                            <Check className="w-3.5 h-3.5 text-ok" />
                            <span className="text-ok">Copied</span>
                          </>
                        ) : (
                          <>
                            <Copy className="w-3.5 h-3.5" />
                            <span>Copy</span>
                          </>
                        )}
                      </button>
                      <button
                        type="button"
                        onClick={() => handleRetry(item.index)}
                        className="flex items-center gap-1 text-xs text-ink3 hover:text-ink transition-colors"
                        title="Retry turn"
                      >
                        <RotateCcw className="w-3.5 h-3.5" />
                        <span>Retry</span>
                      </button>
                    </div>
                  </div>
                );
              }

              if (item.kind === "tool") {
                const toolId = item.call.call_id || `tool-${item.index}`;
                const fullDigest = item.result?.result_digest || "";
                const shortDigest = formatDigest(fullDigest);
                const isError = item.result && item.result.ok === false;
                const stateLabel = getToolStateLabel(item.call, item.result);

                return (
                  <details
                    key={toolId}
                    className="group border-l-2 border-line2 pl-3 py-1 text-ink2"
                  >
                    <summary className="cursor-pointer list-none [&::-webkit-details-marker]:hidden flex items-center gap-2 select-none text-xs">
                      <ChevronRight className="w-3.5 h-3.5 text-ink3 shrink-0 transition-transform duration-200 group-open:rotate-90" />
                      <span className="font-mono text-[13px] text-ink">
                        {item.call.name} · {stateLabel}
                      </span>
                      <span
                        className={`w-1.5 h-1.5 rounded-full shrink-0 ${
                          isError ? "bg-err" : "bg-ok"
                        }`}
                      />
                      {shortDigest && (
                        <span className="font-mono text-ink3 text-xs">
                          {shortDigest}
                        </span>
                      )}
                      {fullDigest && (
                        <button
                          type="button"
                          onClick={(e) => handleCopyDigest(fullDigest, toolId, e)}
                          className="p-1 rounded text-ink3 hover:text-ink transition-colors ml-auto shrink-0"
                          title="Copy digest"
                          aria-label={copiedDigestId === toolId ? "Copied" : "Copy digest"}
                        >
                          {copiedDigestId === toolId ? (
                            <Check className="w-3 h-3 text-ok" />
                          ) : (
                            <Copy className="w-3 h-3" />
                          )}
                        </button>
                      )}
                    </summary>
                    <div className="mt-2 space-y-2 text-xs">
                      {item.call.args !== undefined && (
                        <pre className="bg-panel rounded-lg p-2 font-mono text-ink2 overflow-x-auto whitespace-pre-wrap break-words">
                          {typeof item.call.args === "string"
                            ? item.call.args
                            : JSON.stringify(item.call.args || {}, null, 2)}
                        </pre>
                      )}
                      {fullDigest && (
                        <div className="font-mono text-ink3 break-words px-1">
                          {fullDigest}
                        </div>
                      )}
                    </div>
                  </details>
                );
              }

              if (item.kind === "orphan_result") {
                const fullDigest = item.event.result_digest || "";
                const shortDigest = formatDigest(fullDigest);
                const isError = item.event.ok === false;
                const orphanId = item.event.call_id || `res-${item.index}`;

                return (
                  <div
                    key={orphanId}
                    className="border-l-2 border-line2 pl-3 py-1 text-ink2 flex items-center gap-2 text-xs"
                  >
                    <span className={`w-1.5 h-1.5 rounded-full shrink-0 ${isError ? "bg-err" : "bg-ok"}`} />
                    <span className="font-mono text-[13px] text-ink">
                      result · {isError ? "failed" : "approved at door"}
                    </span>
                    {shortDigest && (
                      <span className="font-mono text-ink3 text-xs">
                        {shortDigest}
                      </span>
                    )}
                    {fullDigest && (
                      <button
                        type="button"
                        onClick={(e) => handleCopyDigest(fullDigest, orphanId, e)}
                        className="p-1 rounded text-ink3 hover:text-ink transition-colors ml-auto shrink-0"
                        title="Copy digest"
                        aria-label={copiedDigestId === orphanId ? "Copied" : "Copy digest"}
                      >
                        {copiedDigestId === orphanId ? (
                          <Check className="w-3 h-3 text-ok" />
                        ) : (
                          <Copy className="w-3 h-3" />
                        )}
                      </button>
                    )}
                  </div>
                );
              }

              if (item.kind === "compact") {
                const timeStr = formatEventTime(item.event);
                return (
                  <div key={`compact-${item.index}`} className="relative py-2 flex items-center justify-center">
                    <div className="absolute inset-0 flex items-center" aria-hidden="true">
                      <div className="w-full border-t border-line" />
                    </div>
                    <span className="relative px-3 bg-bg text-xs text-ink3 select-none">
                      — compacted{timeStr ? ` · ${timeStr}` : ""} —
                    </span>
                  </div>
                );
              }

              if (item.kind === "branch") {
                const timeStr = formatEventTime(item.event);
                return (
                  <div key={`branch-${item.index}`} className="relative py-2 flex items-center justify-center">
                    <div className="absolute inset-0 flex items-center" aria-hidden="true">
                      <div className="w-full border-t border-line" />
                    </div>
                    <span className="relative px-3 bg-bg text-xs text-ink3 select-none">
                      — branched{item.event.from_session ? <> from <span className="font-mono">{item.event.from_session}</span></> : ""}{timeStr ? ` · ${timeStr}` : ""} —
                    </span>
                  </div>
                );
              }

              return null;
            })
          )}

          {/* Optimistic user question — visible during the turn, deduped when the
              server transcript catches up (see effect above). */}
          {pendingUser && !events.some((ev) => ev.t === "msg" && ev.role === "user" && ev.text === pendingUser) && (
            <div className="flex justify-end">
              <div className="max-w-[80%] px-4 py-3 rounded-xl bg-[var(--ember-dim)] border border-ember/25 text-ink text-sm sm:text-[15px] leading-relaxed whitespace-pre-wrap">
                {pendingUser}
              </div>
            </div>
          )}

          {/* Active tool calls during streaming */}
          {activeToolCalls.map((tc) => (
            <div
              key={tc.call_id}
              className="border-l-2 border-line2 pl-3 py-1 text-ink2 flex items-center gap-2 text-xs"
            >
              <ChevronRight className="w-3.5 h-3.5 text-line2 shrink-0" />
              <span className="font-mono text-[13px] text-ink">
                {tc.name} · {tc.status || "running"}
              </span>
              <span className="w-1.5 h-1.5 rounded-full bg-ember animate-pulse shrink-0" />
              {tc.args_summary && (
                <span className="font-mono text-ink3 text-xs truncate">
                  {tc.args_summary}
                </span>
              )}
            </div>
          ))}

          {/* Live In-flight Streaming Assistant Response */}
          {isStreaming && (
            <div className="flex flex-col items-start max-w-[85%]">
              <div className="w-full px-4 py-3 rounded-xl bg-panel border border-line shadow-e1 text-sm sm:text-[15px] leading-relaxed text-ink">
                <MarkdownRenderer content={streamingText} isStreaming={true} />
              </div>
            </div>
          )}

          {/* Pending Tool Approvals */}
          {pendingApprovals.map((req) => (
            <ToolApprovalCard
              key={req.approval_id}
              request={req}
              onResolve={onResolveToolApproval}
            />
          ))}

          <div ref={scrollEndRef} />
        </div>
      </div>

      {/* Composer Area */}
      <div className="p-3 sm:p-4 md:px-8 border-t border-line bg-bg2 shrink-0">
        <form onSubmit={handleSubmit} className="max-w-4xl mx-auto w-full">
          <div className="bg-panel rounded-xl shadow-e2 p-2 border border-transparent focus-within:border-ember focus-within:shadow-[0_0_0_3px_var(--ember-glow)] transition-all">
            <div className="flex items-center gap-2">
              <button
                type="button"
                disabled
                title="coming soon"
                aria-label="Attach file"
                className="w-8 h-8 flex items-center justify-center rounded-lg text-ink3 hover:text-ink transition-colors cursor-not-allowed shrink-0"
              >
                <Paperclip className="w-4 h-4" />
              </button>
              <button
                type="button"
                disabled
                title="coming soon"
                aria-label="Voice input"
                className="w-8 h-8 flex items-center justify-center rounded-lg text-ink3 hover:text-ink transition-colors cursor-not-allowed shrink-0"
              >
                <Mic className="w-4 h-4" />
              </button>

              <textarea
                ref={textareaRef}
                rows={1}
                value={inputText}
                onChange={handleTextareaInput}
                onKeyDown={handleKeyDown}
                disabled={isStreaming}
                placeholder="Ask your hearth anything…"
                className="flex-1 bg-transparent text-sm sm:text-base text-ink placeholder-ink3 focus:outline-hidden focus-visible:outline-none resize-none leading-relaxed py-1.5 px-1 min-h-[36px] max-h-[200px]"
              />

              {isStreaming ? (
                <button
                  type="button"
                  onClick={onCancelTurn}
                  className="flex items-center justify-center gap-1.5 px-3.5 h-11 sm:h-10 rounded-lg bg-err/20 text-err border border-err/30 hover:bg-err/30 text-xs font-medium transition-colors shrink-0 cursor-pointer"
                  aria-label="Stop"
                >
                  <Square className="w-3.5 h-3.5 fill-current" />
                  <span className="hidden sm:inline">Stop</span>
                </button>
              ) : (
                <button
                  type="submit"
                  disabled={!inputText.trim()}
                  className="flex items-center justify-center gap-1.5 px-4 h-11 sm:h-10 rounded-lg bg-ember hover:bg-ember2 text-bg font-medium text-xs transition-colors shrink-0 cursor-pointer disabled:cursor-not-allowed"
                  aria-label="Send"
                >
                  <span>Send</span>
                  <Send className="w-3.5 h-3.5" />
                </button>
              )}
            </div>
          </div>
          <p className="text-center text-xs text-ink3 mt-2">
            Hearth can make mistakes — verify before you act.
          </p>
        </form>
      </div>
    </div>
  );
}

export default ChatWindow;
