import { useState } from "react";
import { Play, Pause, Trash2 } from "lucide-react";
import { FastiJob, FastiSchedule } from "../types/hearthd";

interface JobCardProps {
  job: FastiJob;
  token: string | null;
  onJobUpdated: (job: FastiJob) => void;
  onJobDeleted: (jobId: string) => void;
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

function formatSchedule(schedule: FastiSchedule): string {
  if (schedule.kind === "cron") {
    return `cron: ${schedule.expr || ""}`;
  }
  return `every ${schedule.every || ""}`;
}

export function JobCard({
  job,
  token,
  onJobUpdated,
  onJobDeleted,
  onError,
}: JobCardProps) {
  const [running, setRunning] = useState(false);
  const [actionPending, setActionPending] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const displayName = job.name || job.id;
  const schedText = formatSchedule(job.schedule);
  const nextDueRel = formatRelativeTime(job.next_due);
  const lastFireRel = formatRelativeTime(job.last_fire);

  const handleRunNow = async () => {
    if (!token || running) return;
    setRunning(true);
    try {
      const resp = await fetch(`/v1/fasti/jobs/${encodeURIComponent(job.id)}/run`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${token}`,
        },
      });

      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        onError(err.error || resp.statusText);
      }
    } catch (e: any) {
      onError(e.message || "Failed to trigger job");
    } finally {
      setRunning(false);
    }
  };

  const handleTogglePause = async () => {
    if (!token || actionPending) return;
    setActionPending(true);
    // Daemon rail: enabled:false pauses the job, enabled:true resumes it
    // (applyJobPatch -> PauseJob/ResumeJob). The paused flag is the state.
    const newEnabled = job.paused;

    try {
      const resp = await fetch(`/v1/fasti/jobs/${encodeURIComponent(job.id)}`, {
        method: "PATCH",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({ enabled: newEnabled }),
      });

      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        onError(err.error || resp.statusText);
        return;
      }

      const updated: FastiJob = await resp.json();
      onJobUpdated(updated);
    } catch (e: any) {
      onError(e.message || "Failed to update job");
    } finally {
      setActionPending(false);
    }
  };

  const handleDelete = async () => {
    if (!token || actionPending) return;
    setActionPending(true);
    setConfirmDelete(false);

    // Optimistic deletion
    onJobDeleted(job.id);

    try {
      const resp = await fetch(`/v1/fasti/jobs/${encodeURIComponent(job.id)}`, {
        method: "DELETE",
        headers: {
          Authorization: `Bearer ${token}`,
        },
      });

      if (!resp.ok && resp.status !== 204) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        onError(err.error || resp.statusText);
      }
    } catch (e: any) {
      onError(e.message || "Failed to delete job");
    } finally {
      setActionPending(false);
    }
  };

  return (
    <div className="rounded-2xl border border-line bg-panel p-4 transition-colors">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 mb-2">
        <div className="flex items-center gap-2 flex-wrap min-w-0">
          <span className="text-sm font-semibold text-ink font-mono truncate">
            {displayName}
          </span>
          <span className="px-2 py-0.5 rounded-md text-[11px] font-mono bg-panel2 text-ink2 border border-line">
            {schedText}
          </span>
          {!job.enabled && (
            <span className="px-2 py-0.5 rounded-full text-[11px] font-mono border bg-panel2 text-ink3 border-line">
              disabled
            </span>
          )}
          {job.paused && (
            <span className="px-2 py-0.5 rounded-full text-[11px] font-mono border bg-warn/15 text-warn border-warn/30">
              paused
            </span>
          )}
          {job.consecutive_skips >= 3 && (
            <span className="px-2 py-0.5 rounded-full text-[11px] font-mono border bg-err/15 text-err border-err/30">
              skipped x{job.consecutive_skips}
            </span>
          )}
        </div>

        {/* Relative time */}
        <div className="text-[11px] font-mono text-ink3 flex items-center gap-2.5 shrink-0">
          {nextDueRel && <span>next: {nextDueRel}</span>}
          {lastFireRel && <span>last: {lastFireRel}</span>}
        </div>
      </div>

      {/* Truncated Prompt with Full Title Attr */}
      <p
        className="text-xs text-ink2 truncate mb-3"
        title={job.prompt}
      >
        {job.prompt}
      </p>

      {/* Action Buttons */}
      <div className="flex items-center justify-between pt-2 border-t border-line">
        <div className="text-[11px] font-mono text-ink3 truncate max-w-[200px]">
          by: {job.created_by}
        </div>

        <div className="flex items-center gap-1.5">
          {confirmDelete ? (
            <div className="flex items-center gap-1.5 bg-panel2 px-2 py-1 rounded-lg border border-err/30">
              <span className="text-xs text-err">Delete?</span>
              <button
                type="button"
                onClick={handleDelete}
                className="px-2 py-0.5 text-xs rounded bg-err text-bg font-medium hover:opacity-90"
              >
                Confirm
              </button>
              <button
                type="button"
                onClick={() => setConfirmDelete(false)}
                className="px-2 py-0.5 text-xs rounded bg-panel hover:bg-line text-ink2"
              >
                Cancel
              </button>
            </div>
          ) : (
            <>
              <button
                type="button"
                onClick={handleRunNow}
                disabled={running}
                className="flex items-center gap-1 px-2.5 py-1 rounded-lg text-xs font-medium bg-panel2 hover:bg-line text-ink2 hover:text-ink border border-line transition-colors disabled:opacity-50"
                title="Run job now"
              >
                <Play className="w-3 h-3 text-ember" />
                <span>Run now</span>
              </button>

              <button
                type="button"
                onClick={handleTogglePause}
                disabled={actionPending}
                className="flex items-center gap-1 px-2.5 py-1 rounded-lg text-xs font-medium bg-panel2 hover:bg-line text-ink2 hover:text-ink border border-line transition-colors disabled:opacity-50"
                title={job.paused ? "Resume job" : "Pause job"}
              >
                {job.paused ? (
                  <Play className="w-3 h-3 text-ok" />
                ) : (
                  <Pause className="w-3 h-3 text-warn" />
                )}
                <span>{job.paused ? "Resume" : "Pause"}</span>
              </button>

              <button
                type="button"
                onClick={() => setConfirmDelete(true)}
                disabled={actionPending}
                className="p-1.5 rounded-lg text-xs text-ink3 hover:text-err hover:bg-panel2 transition-colors disabled:opacity-50"
                title="Delete job"
                aria-label="Delete job"
              >
                <Trash2 className="w-3.5 h-3.5" />
              </button>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
