import { useState, useRef, useCallback } from "react";
import { ApprovalRequest } from "../types/hearthd";

export interface ToolCallState {
  call_id: string;
  name: string;
  args_summary?: string;
  status: "running" | "done";
  ok?: boolean;
  approval?: string;
}

interface UseHearthStreamOptions {
  token: string | null;
  onTurnComplete?: () => void;
  onError?: (err: string) => void;
}

export function useHearthStream({ token, onTurnComplete, onError }: UseHearthStreamOptions) {
  const [isStreaming, setIsStreaming] = useState(false);
  const [streamingText, setStreamingText] = useState("");
  const [activeToolCalls, setActiveToolCalls] = useState<ToolCallState[]>([]);
  const [pendingApprovals, setPendingApprovals] = useState<ApprovalRequest[]>([]);
  const abortControllerRef = useRef<AbortController | null>(null);

  const resetStreamState = useCallback(() => {
    setIsStreaming(false);
    setStreamingText("");
    setActiveToolCalls([]);
    setPendingApprovals([]);
    abortControllerRef.current = null;
  }, []);

  const sendMessage = useCallback(
    async (sessionId: string, text: string) => {
      if (!token || !sessionId) return;
      if (isStreaming) return;

      setIsStreaming(true);
      setStreamingText("");
      setActiveToolCalls([]);
      setPendingApprovals([]);

      const ac = new AbortController();
      abortControllerRef.current = ac;

      try {
        const resp = await fetch(`/v1/sessions/${encodeURIComponent(sessionId)}/messages`, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Accept: "text/event-stream",
            Authorization: `Bearer ${token}`,
          },
          body: JSON.stringify({ text }),
          signal: ac.signal,
        });

        if (resp.status === 401) {
          throw new Error("unauthorized — token revoked or wrong");
        }
        if (!resp.ok) {
          const errData = await resp.json().catch(() => ({ error: resp.statusText }));
          throw new Error(errData.error || resp.statusText);
        }

        if (!resp.body) {
          throw new Error("Streaming unsupported by browser or connection");
        }

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

            let eventType: string | null = null;
            let eventData: any = null;

            for (const line of part.split("\n")) {
              if (line.startsWith("event:")) {
                eventType = line.slice(6).trim();
              } else if (line.startsWith("data:")) {
                try {
                  eventData = JSON.parse(line.slice(5).trim());
                } catch {
                  // Ignore JSON parse errors for non-JSON data
                }
              }
            }

            if (eventType && eventData) {
              switch (eventType) {
                case "delta":
                  if (typeof eventData.text === "string") {
                    setStreamingText((prev) => prev + eventData.text);
                  }
                  break;

                case "tool_start":
                  setActiveToolCalls((prev) => [
                    ...prev,
                    {
                      call_id: eventData.call_id,
                      name: eventData.name,
                      args_summary: eventData.args_summary,
                      status: "running",
                    },
                  ]);
                  break;

                case "tool_done":
                  setActiveToolCalls((prev) =>
                    prev.map((tc) =>
                      tc.call_id === eventData.call_id
                        ? {
                            ...tc,
                            status: "done",
                            ok: eventData.ok,
                            approval: eventData.approval,
                          }
                        : tc
                    )
                  );
                  break;

                case "approval_request":
                  setPendingApprovals((prev) => [
                    ...prev,
                    {
                      approval_id: eventData.approval_id,
                      name: eventData.name,
                      args_summary: eventData.args_summary,
                      session_id: sessionId,
                    },
                  ]);
                  break;

                case "turn_done":
                  setIsStreaming(false);
                  if (onTurnComplete) onTurnComplete();
                  break;

                case "error":
                  setIsStreaming(false);
                  if (onError) onError(eventData.error || "turn failed");
                  break;
              }
            }
          }
        }
      } catch (e: any) {
        if (e.name !== "AbortError") {
          const msg = e.message || "stream error";
          if (onError) onError(msg);
        }
      } finally {
        setIsStreaming(false);
        abortControllerRef.current = null;
        if (onTurnComplete) onTurnComplete();
      }
    },
    [token, isStreaming, onTurnComplete, onError]
  );

  const cancelTurn = useCallback(
    async (sessionId: string) => {
      if (abortControllerRef.current) {
        abortControllerRef.current.abort();
      }
      if (!token || !sessionId) return;
      try {
        await fetch(`/v1/sessions/${encodeURIComponent(sessionId)}/cancel`, {
          method: "POST",
          headers: {
            Authorization: `Bearer ${token}`,
          },
        });
      } catch {
        // Ignored
      } finally {
        resetStreamState();
      }
    },
    [token, resetStreamState]
  );

  const resolveApproval = useCallback(
    async (sessionId: string, approvalId: string, verdict: "allow" | "deny") => {
      if (!token || !sessionId) return;
      try {
        const resp = await fetch(
          `/v1/sessions/${encodeURIComponent(sessionId)}/approvals/${encodeURIComponent(approvalId)}`,
          {
            method: "POST",
            headers: {
              "Content-Type": "application/json",
              Authorization: `Bearer ${token}`,
            },
            body: JSON.stringify({ verdict, decision: verdict }),
          }
        );
        if (!resp.ok) {
          const err = await resp.json().catch(() => ({ error: resp.statusText }));
          throw new Error(err.error || resp.statusText);
        }
        setPendingApprovals((prev) => prev.filter((a) => a.approval_id !== approvalId));
      } catch (e: any) {
        if (onError) onError(`Approval failed: ${e.message}`);
        throw e;
      }
    },
    [token, onError]
  );

  return {
    isStreaming,
    streamingText,
    activeToolCalls,
    pendingApprovals,
    sendMessage,
    cancelTurn,
    resolveApproval,
    resetStreamState,
  };
}
