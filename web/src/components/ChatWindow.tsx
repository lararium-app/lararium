import React, { useRef, useEffect } from "react";
import { Copy, RotateCcw, Check, Square, Send, Paperclip, Mic } from "lucide-react";
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

export function ChatWindow({
  currentSession,
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
  const scrollEndRef = useRef<HTMLDivElement | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);

  // Auto-scroll on new events or streaming tokens
  useEffect(() => {
    scrollEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [events, streamingText, activeToolCalls, pendingApprovals, custosCards]);

  const handleCopyMessage = (text: string, id: string) => {
    navigator.clipboard.writeText(text).then(() => {
      setCopiedId(id);
      setTimeout(() => setCopiedId(null), 1500);
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

  const isEmpty = events.length === 0 && !isStreaming && pendingApprovals.length === 0;

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
          {isEmpty ? (
            <div className="flex flex-col items-center justify-center py-16 text-center max-w-md mx-auto">
              <div className="text-5xl mb-4">🔥</div>
              <h2 className="text-xl font-bold text-ink mb-2">Your hearth is warm.</h2>
              <p className="text-xs text-ink3 mb-8">Choose a suggestion or ask anything to begin.</p>
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-2 w-full">
                {SUGGESTIONS.map((sug) => (
                  <button
                    key={sug}
                    type="button"
                    onClick={() => {
                      setInputText(sug);
                      textareaRef.current?.focus();
                    }}
                    className="p-3 text-left rounded-xl bg-panel border border-line hover:border-ember/40 text-xs text-ink hover:text-ember transition-colors"
                  >
                    {sug}
                  </button>
                ))}
              </div>
            </div>
          ) : (
            events.map((ev, idx) => {
              if (ev.t === "msg" && ev.role === "user") {
                return (
                  <div key={idx} className="flex items-start justify-end gap-3 group">
                    <div className="max-w-2xl px-4 py-3 rounded-2xl bg-ember/15 border border-ember/25 text-ink text-sm leading-relaxed whitespace-pre-wrap">
                      {ev.text}
                    </div>
                    <div className="w-8 h-8 rounded-full bg-panel2 border border-line flex items-center justify-center text-xs font-bold text-ink3 shrink-0">
                      YOU
                    </div>
                  </div>
                );
              }

              if (ev.t === "msg" && ev.role === "assistant") {
                const msgId = `msg-${idx}`;
                return (
                  <div key={idx} className="flex items-start gap-3 group">
                    <div className="w-8 h-8 rounded-full bg-ember/20 border border-ember/30 flex items-center justify-center text-sm shrink-0">
                      🔥
                    </div>
                    <div className="max-w-2xl flex-1 min-w-0">
                      <div className="flex items-center gap-2 mb-1 text-xs text-ink3">
                        <span className="font-bold text-ink">Hearth</span>
                        {ev.created && (
                          <span>
                            {new Date(ev.created).toLocaleTimeString([], {
                              hour: "numeric",
                              minute: "2-digit",
                            })}
                          </span>
                        )}
                        {currentSession?.model_pin && (
                          <span className="font-mono text-[11px] px-1.5 py-0.2 rounded bg-panel2 border border-line">
                            {currentSession.model_pin}
                          </span>
                        )}
                      </div>
                      <div className="px-4 py-3 rounded-2xl bg-panel border border-line text-sm leading-relaxed text-ink shadow-2xs">
                        <MarkdownRenderer content={ev.text || ""} />
                      </div>
                      <div className="flex items-center gap-2 mt-1 text-xs opacity-0 group-hover:opacity-100 transition-opacity">
                        <button
                          type="button"
                          onClick={() => handleCopyMessage(ev.text || "", msgId)}
                          className="flex items-center gap-1 text-ink3 hover:text-ink transition-colors"
                        >
                          {copiedId === msgId ? (
                            <>
                              <Check className="w-3 h-3 text-ok" />
                              <span className="text-ok">Copied</span>
                            </>
                          ) : (
                            <>
                              <Copy className="w-3 h-3" />
                              <span>Copy</span>
                            </>
                          )}
                        </button>
                        <button
                          type="button"
                          onClick={() => handleRetry(idx)}
                          className="flex items-center gap-1 text-ink3 hover:text-ink transition-colors"
                        >
                          <RotateCcw className="w-3 h-3" />
                          <span>Retry</span>
                        </button>
                      </div>
                    </div>
                  </div>
                );
              }

              if (ev.t === "tool_call") {
                return (
                  <details
                    key={idx}
                    className="p-3 rounded-xl bg-panel border border-line text-xs font-mono text-ink2 group"
                  >
                    <summary className="cursor-pointer font-medium text-ink flex items-center gap-2 select-none">
                      <span>⚙ {ev.name} (approval)</span>
                    </summary>
                    <div className="mt-2 pt-2 border-t border-line text-ink3 break-words whitespace-pre-wrap">
                      args: {typeof ev.args === "string" ? ev.args : JSON.stringify(ev.args || {}, null, 2)}
                    </div>
                  </details>
                );
              }

              if (ev.t === "tool_result") {
                return (
                  <div
                    key={idx}
                    className={`p-2.5 rounded-lg border text-xs font-mono ${
                      ev.ok
                        ? "bg-ok/10 border-ok/30 text-ok"
                        : "bg-err/10 border-err/30 text-err"
                    }`}
                  >
                    → {ev.ok ? "ok" : "error"}: {ev.result_digest || ""}
                  </div>
                );
              }

              if (ev.t === "compact") {
                return (
                  <div key={idx} className="relative py-2 flex items-center justify-center">
                    <hr className="w-full border-line" />
                    <span className="absolute px-3 py-0.5 rounded-full bg-panel2 border border-line text-[11px] font-mono text-ink3">
                      compacted
                    </span>
                  </div>
                );
              }

              if (ev.t === "branch") {
                return (
                  <div
                    key={idx}
                    className="p-2.5 rounded-lg border border-line bg-panel2 text-xs font-mono text-ink3"
                  >
                    branched from {ev.from_session || "?"}
                  </div>
                );
              }

              return null;
            })
          )}

          {/* Active tool calls during streaming */}
          {activeToolCalls.map((tc) => (
            <div
              key={tc.call_id}
              className="p-3 rounded-xl bg-panel border border-line text-xs font-mono text-ink2"
            >
              <div className="flex items-center justify-between">
                <span className="font-medium text-ink">
                  ⚙ {tc.name} ({tc.status})
                </span>
                {tc.status === "running" && (
                  <span className="inline-block w-2 h-2 rounded-full bg-ember animate-pulse" />
                )}
              </div>
              {tc.args_summary && (
                <div className="mt-1 text-ink3 break-words">args: {tc.args_summary}</div>
              )}
            </div>
          ))}

          {/* Live In-flight Streaming Assistant Response */}
          {isStreaming && (
            <div className="flex items-start gap-3">
              <div className="w-8 h-8 rounded-full bg-ember/20 border border-ember/30 flex items-center justify-center text-sm shrink-0 animate-pulse">
                🔥
              </div>
              <div className="max-w-2xl flex-1 min-w-0">
                <div className="flex items-center gap-2 mb-1 text-xs text-ink3">
                  <span className="font-bold text-ink">Hearth</span>
                  <span className="text-ember font-mono text-[11px]">generating…</span>
                </div>
                <div className="px-4 py-3 rounded-2xl bg-panel border border-line text-sm leading-relaxed text-ink shadow-2xs">
                  <MarkdownRenderer content={streamingText} />
                  {!streamingText && (
                    <div className="flex items-center gap-1.5 py-1 text-ink3">
                      <span className="w-1.5 h-1.5 rounded-full bg-ember animate-bounce" />
                      <span className="w-1.5 h-1.5 rounded-full bg-ember animate-bounce delay-100" />
                      <span className="w-1.5 h-1.5 rounded-full bg-ember animate-bounce delay-200" />
                    </div>
                  )}
                </div>
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
      <div className="p-4 md:px-8 border-t border-line bg-bg2 shrink-0">
        <form onSubmit={handleSubmit} className="max-w-4xl mx-auto w-full">
          <div className="p-3 rounded-2xl bg-panel border border-line focus-within:border-ember/50 shadow-sm transition-colors">
            <textarea
              ref={textareaRef}
              rows={1}
              value={inputText}
              onChange={handleTextareaInput}
              onKeyDown={handleKeyDown}
              disabled={isStreaming}
              placeholder="Ask your hearth anything…"
              className="w-full bg-transparent text-sm text-ink placeholder-ink3 focus:outline-hidden resize-none leading-relaxed"
            />
            <div className="flex items-center justify-between pt-2 border-t border-line/60 mt-1">
              <div className="flex items-center gap-1.5">
                <button
                  type="button"
                  disabled
                  title="coming soon"
                  className="p-1.5 rounded-lg text-ink3 hover:text-ink disabled:opacity-40 cursor-not-allowed transition-colors"
                >
                  <Paperclip className="w-4 h-4" />
                </button>
                <button
                  type="button"
                  disabled
                  title="coming soon"
                  className="flex items-center gap-1 px-2 py-1 rounded-lg text-ink3 text-xs disabled:opacity-40 cursor-not-allowed transition-colors"
                >
                  <Mic className="w-3.5 h-3.5" />
                  <span>Voice</span>
                </button>
              </div>

              <div className="flex items-center gap-2">
                {isStreaming ? (
                  <button
                    type="button"
                    onClick={onCancelTurn}
                    className="flex items-center gap-1.5 px-3 py-1.5 rounded-xl bg-err/20 text-err border border-err/30 hover:bg-err/30 text-xs font-medium transition-colors"
                  >
                    <Square className="w-3 h-3 fill-current" />
                    <span>Stop</span>
                  </button>
                ) : (
                  <button
                    type="submit"
                    disabled={!inputText.trim()}
                    className="flex items-center gap-1.5 px-3.5 py-1.5 rounded-xl bg-ember text-bg font-semibold text-xs hover:bg-ember2 disabled:opacity-40 transition-colors shadow-2xs"
                  >
                    <span>Send</span>
                    <Send className="w-3 h-3" />
                  </button>
                )}
              </div>
            </div>
          </div>
          <p className="text-center text-[11px] text-ink3 mt-2">
            Hearth can make mistakes — verify before you act.
          </p>
        </form>
      </div>
    </div>
  );
}

export default ChatWindow;
