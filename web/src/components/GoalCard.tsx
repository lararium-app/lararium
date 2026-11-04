import { useState } from "react";
import { Play, Pause, Check } from "lucide-react";
import { FastiGoal } from "../types/hearthd";

interface GoalCardProps {
  goal: FastiGoal;
  token: string | null;
  onGoalUpdated: (goal: FastiGoal) => void;
  onError: (err: string) => void;
}

function formatRelativeTime(isoStr?: string): string {
  if (!isoStr) return "";
  const date = new Date(isoStr);
  if (isNaN(date.getTime())) return "";
  const now = Date.now();
  const diffMs = date.getTime() - now;
  const absSec = Math.floor(Math.abs(diffMs) / 1000);

  if (diffMs > 0) {
    if (absSec < 60) return "in <1m";
    const mins = Math.floor(absSec / 60);
    if (mins < 60) return `in ${mins}m`;
    const hours = Math.floor(mins / 60);
    if (hours < 24) return `in ${hours}h`;
    const days = Math.floor(hours / 24);
    return `in ${days}d`;
  } else {
    if (absSec < 60) return "just now";
    const mins = Math.floor(absSec / 60);
    if (mins < 60) return `${mins}m ago`;
    const hours = Math.floor(mins / 60);
    if (hours < 24) return `${hours}h ago`;
    const days = Math.floor(hours / 24);
    return `${days}d ago`;
  }
}

function goalStatusBadge(status: string): string {
  switch (status) {
    case "active":
      return "bg-ember/15 text-ember border-ember/30";
    case "paused":
      return "bg-panel2 text-ink3 border-line";
    case "done":
      return "bg-ok/15 text-ok border-ok/30";
    case "failed":
    case "stale":
      return "bg-err/15 text-err border-err/30";
    default:
      return "bg-panel2 text-ink2 border-line";
  }
}

export function GoalCard({
  goal,
  token,
  onGoalUpdated,
  onError,
}: GoalCardProps) {
  const [actionPending, setActionPending] = useState(false);
  const [confirmClose, setConfirmClose] = useState(false);

  const nextDueRel = formatRelativeTime(goal.next_due);
  const secondaryText = goal.next_hint || goal.progress_prose;

  const handleToggleStatus = async () => {
    if (!token || actionPending) return;
    setActionPending(true);
    const nextStatus = goal.status === "paused" ? "active" : "paused";

    try {
      const resp = await fetch(`/v1/fasti/goals/${encodeURIComponent(goal.id)}`, {
        method: "PATCH",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({ status: nextStatus }),
      });

      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        onError(err.error || resp.statusText);
        return;
      }

      const updated: FastiGoal = await resp.json();
      onGoalUpdated(updated);
    } catch (e: any) {
      onError(e.message || "Failed to update goal");
    } finally {
      setActionPending(false);
    }
  };

  const handleForceClose = async () => {
    if (!token || actionPending) return;
    setActionPending(true);
    setConfirmClose(false);

    try {
      const resp = await fetch(`/v1/fasti/goals/${encodeURIComponent(goal.id)}/close`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({ mode: "user_force" }),
      });

      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        onError(err.error || resp.statusText);
        return;
      }

      const updated: FastiGoal = await resp.json();
      onGoalUpdated(updated);
    } catch (e: any) {
      onError(e.message || "Failed to close goal");
    } finally {
      setActionPending(false);
    }
  };

  return (
    <div className="rounded-2xl border border-line bg-panel p-4">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 mb-3">
        <div className="flex items-center gap-2 flex-wrap">
          <span className="text-sm font-semibold text-ink font-brand">
            {goal.title}
          </span>
          <span
            className={`px-2 py-0.5 rounded-full text-[11px] font-mono font-medium border ${goalStatusBadge(
              goal.status
            )}`}
          >
            {goal.status}
          </span>
          <span className="px-2 py-0.5 rounded-md text-[11px] font-mono bg-panel2 text-ink2 border border-line">
            turn {goal.turn_count}/{goal.turn_budget}
          </span>
        </div>

        {nextDueRel && (
          <span className="text-[11px] font-mono text-ink3">
            next: {nextDueRel}
          </span>
        )}
      </div>

      {/* Requirements Checklist */}
      {goal.requirements && goal.requirements.length > 0 && (
        <div className="mb-3 p-3 rounded-xl bg-panel2/60 border border-line space-y-1.5">
          <div className="text-[11px] uppercase tracking-wider text-ink3 font-semibold mb-1">
            Requirements
          </div>
          <ul className="space-y-1">
            {goal.requirements.map((req, idx) => (
              <li
                key={idx}
                className="flex items-start gap-2 text-xs leading-relaxed"
              >
                <span
                  className={`mt-0.5 w-3.5 h-3.5 rounded flex items-center justify-center shrink-0 border ${
                    req.done
                      ? "bg-ok text-bg border-ok"
                      : "bg-panel border-line text-transparent"
                  }`}
                  aria-hidden="true"
                >
                  <Check className="w-2.5 h-2.5 stroke-[3]" />
                </span>
                <span
                  className={
                    req.done
                      ? "text-ink3 line-through"
                      : "text-ink"
                  }
                >
                  {req.text}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}

      {/* Secondary Text (Next Hint or Progress Prose) */}
      {secondaryText && (
        <p className="text-xs text-ink3 italic mb-3 bg-panel2/30 p-2.5 rounded-lg border border-line/50">
          {secondaryText}
        </p>
      )}

      {/* Goal Actions */}
      <div className="flex items-center justify-between pt-2 border-t border-line">
        <span className="text-[11px] font-mono text-ink3">
          ID: {goal.id}
        </span>

        <div className="flex items-center gap-2">
          {/* Pause / Resume for active/paused */}
          {(goal.status === "active" || goal.status === "paused") && (
            <button
              type="button"
              onClick={handleToggleStatus}
              disabled={actionPending}
              className="flex items-center gap-1 px-2.5 py-1 rounded-lg text-xs font-medium bg-panel2 hover:bg-line text-ink2 hover:text-ink border border-line transition-colors disabled:opacity-50"
            >
              {goal.status === "paused" ? (
                <>
                  <Play className="w-3 h-3 text-ok" />
                  <span>Resume</span>
                </>
              ) : (
                <>
                  <Pause className="w-3 h-3 text-warn" />
                  <span>Pause</span>
                </>
              )}
            </button>
          )}

          {/* Close button with confirm dialog (open goals only; done/failed/stale are terminal) */}
          {(goal.status === "active" || goal.status === "paused") && (
            <>
              {confirmClose ? (
                <div className="flex items-center gap-1.5 bg-panel2 px-2 py-1 rounded-lg border border-err/30">
                  <span className="text-xs text-err">Force close?</span>
                  <button
                    type="button"
                    onClick={handleForceClose}
                    className="px-2 py-0.5 text-xs rounded bg-err text-bg font-medium hover:opacity-90"
                  >
                    Confirm
                  </button>
                  <button
                    type="button"
                    onClick={() => setConfirmClose(false)}
                    className="px-2 py-0.5 text-xs rounded bg-panel hover:bg-line text-ink2"
                  >
                    Cancel
                  </button>
                </div>
              ) : (
                <button
                  type="button"
                  onClick={() => setConfirmClose(true)}
                  disabled={actionPending}
                  className="px-2.5 py-1 rounded-lg text-xs font-medium text-ink3 hover:text-err border border-line hover:border-err/30 transition-colors disabled:opacity-50"
                >
                  Close
                </button>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  );
}
