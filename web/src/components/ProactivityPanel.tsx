import { useState, useEffect, useCallback } from "react";
import { RefreshCw, Plus, AlertCircle, Clock, Sparkles } from "lucide-react";
import {
  FastiJob,
  FastiGoal,
  FastiHeartbeatState,
  FastiNoiseSnapshot,
  FastiStateResponse,
  FastiEvent,
} from "../types/hearthd";
import { AddJobForm } from "./AddJobForm";
import { JobCard } from "./JobCard";
import { GoalCard } from "./GoalCard";

interface ProactivityPanelProps {
  token: string | null;
}

export function ProactivityPanel({ token }: ProactivityPanelProps) {
  const [loading, setLoading] = useState(true);
  const [fastiDisabled, setFastiDisabled] = useState(false);
  const [jobs, setJobs] = useState<FastiJob[]>([]);
  const [goals, setGoals] = useState<FastiGoal[]>([]);
  const [heartbeat, setHeartbeat] = useState<FastiHeartbeatState | null>(null);
  const [noise, setNoise] = useState<FastiNoiseSnapshot | null>(null);
  const [toastError, setToastError] = useState<string | null>(null);
  const [showAddJob, setShowAddJob] = useState(false);

  // "disabled" only when the daemon says fasti is off (stop the rail);
  // "error" stays retryable so a transient blip never kills the stream.
  const fetchState = useCallback(async (): Promise<"ok" | "disabled" | "error"> => {
    if (!token) return "error";
    try {
      const resp = await fetch("/v1/fasti/state", {
        headers: {
          Authorization: `Bearer ${token}`,
        },
      });

      if (resp.status === 503) {
        setFastiDisabled(true);
        setLoading(false);
        return "disabled";
      }

      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        setToastError(err.error || resp.statusText);
        setLoading(false);
        return "error";
      }

      const data: FastiStateResponse = await resp.json();
      setFastiDisabled(false);
      setJobs(Array.isArray(data.jobs) ? data.jobs : []);
      setGoals(Array.isArray(data.goals) ? data.goals : []);
      setHeartbeat(data.heartbeat || null);
      setNoise(data.noise || null);
      setLoading(false);
      return "ok";
    } catch (e: any) {
      setToastError(e.message || "Failed to fetch proactivity state");
      setLoading(false);
      return "error";
    }
  }, [token]);

  // Handle SSE Fasti Events
  const handleFastiEvent = useCallback((ev: FastiEvent) => {
    if (ev.kind === "job") {
      if (ev.op === "upsert" && ev.row) {
        const job = ev.row as FastiJob;
        setJobs((prev) => {
          const idx = prev.findIndex((j) => j.id === job.id);
          if (idx >= 0) {
            const next = [...prev];
            next[idx] = job;
            return next;
          }
          return [job, ...prev];
        });
      } else if (ev.op === "delete" && ev.row) {
        const delId = typeof ev.row === "string" ? ev.row : ev.row.id;
        if (delId) {
          setJobs((prev) => prev.filter((j) => j.id !== delId));
        }
      }
    } else if (ev.kind === "goal") {
      if (ev.op === "upsert" && ev.row) {
        const goal = ev.row as FastiGoal;
        setGoals((prev) => {
          const idx = prev.findIndex((g) => g.id === goal.id);
          if (idx >= 0) {
            const next = [...prev];
            next[idx] = goal;
            return next;
          }
          return [goal, ...prev];
        });
      } else if (ev.op === "delete" && ev.row) {
        const delId = typeof ev.row === "string" ? ev.row : ev.row.id;
        if (delId) {
          setGoals((prev) => prev.filter((g) => g.id !== delId));
        }
      }
    } else if (ev.kind === "heartbeat" && ev.row) {
      if (ev.op === "upsert") {
        setHeartbeat(ev.row as FastiHeartbeatState);
      }
    }
  }, []);

  // SSE stream rail lifecycle: exactly 1 stream per mounted view with auto-reconnect and re-GET
  useEffect(() => {
    if (!token) return;

    let isMounted = true;
    let abortController: AbortController | null = null;
    let reconnectTimer: number | null = null;

    const connectStream = async () => {
      if (!isMounted) return;

      // Reconcile before opening; only a disabled daemon stops the rail.
      const stateStatus = await fetchState();
      if (!isMounted || stateStatus === "disabled") return;

      abortController = new AbortController();

      try {
        const resp = await fetch("/v1/fasti/events", {
          headers: {
            Accept: "text/event-stream",
            Authorization: `Bearer ${token}`,
          },
          signal: abortController.signal,
        });

        if (resp.status === 503) {
          if (isMounted) setFastiDisabled(true);
          return;
        }

        if (!resp.ok || !resp.body) {
          throw new Error("Stream connection failed");
        }

        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";

        while (isMounted) {
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
              if (line.startsWith("event:")) {
                evType = line.slice(6).trim();
              } else if (line.startsWith("data:")) {
                try {
                  evData = JSON.parse(line.slice(5).trim());
                } catch {
                  // ignore malformed data chunk
                }
              }
            }

            if ((evType === "fasti_event" || !evType) && evData) {
              handleFastiEvent(evData);
            }
          }
        }
      } catch (e: any) {
        if (e.name === "AbortError") return;
      }

      if (isMounted) {
        reconnectTimer = window.setTimeout(() => {
          if (isMounted) {
            connectStream();
          }
        }, 3000);
      }
    };

    connectStream();

    return () => {
      isMounted = false;
      if (abortController) abortController.abort();
      if (reconnectTimer !== null) window.clearTimeout(reconnectTimer);
    };
  }, [token, fetchState, handleFastiEvent]);

  // Noise meter computation
  const announceCount = noise?.announce_count ?? 0;
  const maxAnnounce = noise?.max_announce_per_day ?? 0;
  const streakCount = noise?.overflow_day_streak?.length ?? 0;
  // max 0 means "no cap"; never flag red before real data has arrived.
  const isNoiseOver =
    noise !== null && (streakCount > 0 || (maxAnnounce > 0 && announceCount >= maxAnnounce));
  const noisePct = maxAnnounce > 0 ? Math.min(100, Math.round((announceCount / maxAnnounce) * 100)) : 0;

  // Disabled State Card
  if (fastiDisabled) {
    return (
      <div className="flex-1 overflow-y-auto p-4 md:p-6 bg-bg text-ink max-w-4xl mx-auto w-full">
        <div className="p-8 md:p-12 text-center rounded-2xl border border-line bg-panel max-w-xl mx-auto my-12">
          <div className="w-12 h-12 rounded-2xl bg-panel2 border border-line text-ink3 flex items-center justify-center mx-auto mb-4">
            <Clock className="w-6 h-6" />
          </div>
          <h2 className="text-lg font-bold text-ink mb-2">Proactivity is off</h2>
          <p className="text-sm text-ink2 mb-6 leading-relaxed">
            The hearth daemon can run scheduled recurring jobs and autonomous multi-turn goals, but proactivity is currently disabled.
          </p>
          <div className="text-xs text-left bg-panel2 p-3.5 rounded-xl border border-line text-ink3 leading-relaxed">
            <span className="font-semibold text-ink2 block mb-1">To enable proactivity:</span>
            Add a <code className="font-mono text-ink text-[11px] bg-bg px-1.5 py-0.5 rounded border border-line">fasti:</code> block to the hearthd configuration file and restart the daemon.
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="flex-1 overflow-y-auto p-4 md:p-6 bg-bg text-ink max-w-4xl mx-auto w-full">
      {/* Header Row */}
      <div className="flex items-center justify-between pb-4 mb-6 border-b border-line">
        <div className="flex items-center gap-3">
          <div className="p-2.5 rounded-xl bg-panel2 border border-line text-ember">
            <Sparkles className="w-6 h-6" />
          </div>
          <div>
            <h2 className="text-lg font-bold text-ink">Proactivity</h2>
            <p className="text-xs text-ink3">Scheduled jobs, continuous goals, and heartbeat supervision</p>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={() => fetchState()}
            disabled={loading}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-medium bg-panel2 hover:bg-line text-ink2 hover:text-ink border border-line transition-colors disabled:opacity-50"
            title="Refresh status"
          >
            <RefreshCw className={`w-3.5 h-3.5 ${loading ? "animate-spin" : ""}`} />
            <span className="hidden sm:inline">Refresh</span>
          </button>
          <button
            type="button"
            onClick={() => setShowAddJob(!showAddJob)}
            className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-medium bg-ember hover:bg-ember2 text-bg transition-colors"
          >
            <Plus className="w-4 h-4" />
            <span>New job</span>
          </button>
        </div>
      </div>

      {/* Toast Error Banner */}
      {toastError && (
        <div className="p-3 mb-6 rounded-xl border border-err/30 bg-err/10 text-err text-xs font-mono flex items-center justify-between">
          <div className="flex items-center gap-2 min-w-0">
            <AlertCircle className="w-4 h-4 shrink-0" />
            <span className="truncate">{toastError}</span>
          </div>
          <button
            type="button"
            onClick={() => setToastError(null)}
            className="ml-3 hover:opacity-70 text-ink2"
            aria-label="Dismiss error"
          >
            ✕
          </button>
        </div>
      )}

      {/* Add-Job Form */}
      {showAddJob && (
        <AddJobForm
          token={token}
          onSuccess={(newJob) => {
            setJobs((prev) => {
              if (prev.some((j) => j.id === newJob.id)) return prev;
              return [newJob, ...prev];
            });
          }}
          onClose={() => setShowAddJob(false)}
        />
      )}

      {/* Status Strip Card */}
      <div className="rounded-2xl border border-line bg-panel p-4 mb-6 grid grid-cols-1 md:grid-cols-2 gap-4">
        {/* Heartbeat Status */}
        <div className="flex flex-col justify-between">
          <div className="flex items-center gap-2 mb-2">
            <span className="text-xs font-semibold text-ink uppercase tracking-wider">Heartbeat</span>
            <span
              className={`px-2 py-0.5 rounded-full text-[11px] font-mono font-medium border ${
                heartbeat?.enabled
                  ? "bg-ok/15 text-ok border-ok/30"
                  : "bg-panel2 text-ink3 border-line"
              }`}
            >
              {heartbeat?.enabled ? "enabled" : "disabled"}
            </span>
          </div>
          <div className="flex items-center gap-4 text-xs text-ink3">
            <span>
              Cadence:{" "}
              <code className="text-ink2 font-mono">{heartbeat?.cadence || "n/a"}</code>
            </span>
            <span>
              Quiet:{" "}
              <code className="text-ink2 font-mono">{heartbeat?.quiet || "none"}</code>
            </span>
          </div>
        </div>

        {/* Noise Budget Mini-Meter */}
        <div>
          <div className="flex items-center justify-between mb-1.5">
            <span className="text-xs font-semibold text-ink uppercase tracking-wider">Noise budget</span>
            <span
              className={`text-xs font-mono font-medium ${
                isNoiseOver ? "text-err" : "text-ink2"
              }`}
            >
              announces today: {announceCount} / max {maxAnnounce}
            </span>
          </div>
          <div className="h-2 w-full bg-panel2 rounded-full overflow-hidden border border-line">
            <div
              className={`h-full transition-all duration-300 ${
                isNoiseOver ? "bg-err" : "bg-ember"
              }`}
              style={{ width: `${noisePct}%` }}
            />
          </div>
          {streakCount > 0 && (
            <div className="mt-1.5 text-right">
              <span className="px-2 py-0.5 rounded-full text-[11px] font-mono border bg-err/15 text-err border-err/30">
                overflow streak: {streakCount}d
              </span>
            </div>
          )}
        </div>
      </div>

      {/* Jobs Section */}
      <div className="mb-8">
        <div className="flex items-center gap-2 mb-3">
          <h3 className="text-sm font-semibold text-ink uppercase tracking-wider">Scheduled jobs</h3>
          <span className="px-2 py-0.5 rounded-full text-xs font-mono bg-panel2 text-ink3 border border-line">
            {jobs.length}
          </span>
        </div>

        {jobs.length === 0 ? (
          <div className="p-8 text-center rounded-2xl border border-line bg-panel">
            <Clock className="w-8 h-8 text-ink3 mx-auto mb-2 opacity-50" />
            <p className="text-xs text-ink3">
              No scheduled jobs yet. Create one or ask the agent in chat.
            </p>
          </div>
        ) : (
          <div className="space-y-3">
            {jobs.map((job) => (
              <JobCard
                key={job.id}
                job={job}
                token={token}
                onJobUpdated={(updated) =>
                  setJobs((prev) => prev.map((j) => (j.id === updated.id ? updated : j)))
                }
                onJobDeleted={(deletedId) =>
                  setJobs((prev) => prev.filter((j) => j.id !== deletedId))
                }
                onError={(err) => setToastError(err)}
              />
            ))}
          </div>
        )}
      </div>

      {/* Goals Section */}
      <div className="mb-6">
        <div className="flex items-center gap-2 mb-3">
          <h3 className="text-sm font-semibold text-ink uppercase tracking-wider">Goals</h3>
          <span className="px-2 py-0.5 rounded-full text-xs font-mono bg-panel2 text-ink3 border border-line">
            {goals.length}
          </span>
        </div>

        {goals.length === 0 ? (
          <div className="p-8 text-center rounded-2xl border border-line bg-panel">
            <Sparkles className="w-8 h-8 text-ink3 mx-auto mb-2 opacity-50" />
            <p className="text-xs text-ink3">
              No active goals yet. Create one or ask the agent in chat.
            </p>
          </div>
        ) : (
          <div className="space-y-4">
            {goals.map((goal) => (
              <GoalCard
                key={goal.id}
                goal={goal}
                token={token}
                onGoalUpdated={(updated) =>
                  setGoals((prev) => prev.map((g) => (g.id === updated.id ? updated : g)))
                }
                onError={(err) => setToastError(err)}
              />
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

export default ProactivityPanel;
