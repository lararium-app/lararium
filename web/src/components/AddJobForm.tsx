import { useState } from "react";
import { X } from "lucide-react";
import { FastiJob, FastiSchedule } from "../types/hearthd";

interface AddJobFormProps {
  token: string | null;
  onSuccess: (job: FastiJob) => void;
  onClose: () => void;
}

export function AddJobForm({ token, onSuccess, onClose }: AddJobFormProps) {
  const [name, setName] = useState("");
  const [scheduleKind, setScheduleKind] = useState<"cron" | "interval">("cron");
  const [scheduleExpr, setScheduleExpr] = useState("");
  const [scheduleEvery, setScheduleEvery] = useState("");
  const [prompt, setPrompt] = useState("");
  const [session, setSession] = useState("");
  const [announce, setAnnounce] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);

  const runeCount = [...prompt].length;

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!token || submitting) return;

    setFormError(null);
    const trimmedPrompt = prompt.trim();

    if (!trimmedPrompt) {
      setFormError("Prompt is required");
      return;
    }
    if ([...trimmedPrompt].length > 1000) {
      setFormError("Prompt exceeds 1000 runes");
      return;
    }
    if (trimmedPrompt.includes("\n") || trimmedPrompt.includes("\r")) {
      setFormError("Prompt must be a single paragraph (no newlines)");
      return;
    }

    if (scheduleKind === "cron") {
      const parts = scheduleExpr.trim().split(/\s+/).filter(Boolean);
      if (parts.length !== 5) {
        setFormError("Cron expression must have exactly 5 whitespace-separated fields");
        return;
      }
    } else {
      const everyTrimmed = scheduleEvery.trim();
      if (!/^[0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h)+$/.test(everyTrimmed)) {
        setFormError('Interval must match format like "10m", "1h", "30s"');
        return;
      }
    }

    setSubmitting(true);

    const payload: {
      name?: string;
      schedule: FastiSchedule;
      prompt: string;
      session?: string;
      announce?: string;
    } = {
      schedule:
        scheduleKind === "cron"
          ? { kind: "cron", expr: scheduleExpr.trim() }
          : { kind: "interval", every: scheduleEvery.trim() },
      prompt: trimmedPrompt,
    };

    if (name.trim()) {
      payload.name = name.trim();
    }
    if (session.trim()) {
      payload.session = session.trim();
    }
    if (announce.trim()) {
      payload.announce = announce.trim();
    }

    try {
      const resp = await fetch("/v1/fasti/jobs", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify(payload),
      });

      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        setFormError(err.error || resp.statusText);
        setSubmitting(false);
        return;
      }

      const created: FastiJob = await resp.json();
      onSuccess(created);
      onClose();
    } catch (e: any) {
      setFormError(e.message || "Failed to create job");
      setSubmitting(false);
    }
  };

  return (
    <div className="rounded-2xl border border-line bg-panel p-4 md:p-5 mb-6">
      <div className="flex items-center justify-between mb-4 pb-2 border-b border-line">
        <h3 className="text-sm font-semibold text-ink">New scheduled job</h3>
        <button
          type="button"
          onClick={onClose}
          className="text-ink3 hover:text-ink text-xs p-1"
          aria-label="Close form"
        >
          <X className="w-4 h-4" />
        </button>
      </div>

      <form onSubmit={handleSubmit} className="space-y-4">
        <div>
          <label className="block text-xs font-medium text-ink2 mb-1">
            Name (optional)
          </label>
          <input
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. Morning brief"
            className="w-full px-3 py-1.5 rounded-lg bg-panel2 border border-line text-sm text-ink placeholder-ink3 focus:outline-none focus:border-ember"
          />
        </div>

        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <div>
            <label className="block text-xs font-medium text-ink2 mb-1">
              Schedule kind
            </label>
            <div className="flex rounded-lg border border-line bg-panel2 p-0.5">
              <button
                type="button"
                onClick={() => setScheduleKind("cron")}
                className={`flex-1 py-1 rounded-md text-xs font-medium transition-colors ${
                  scheduleKind === "cron"
                    ? "bg-ember text-bg"
                    : "text-ink2 hover:text-ink"
                }`}
              >
                Cron
              </button>
              <button
                type="button"
                onClick={() => setScheduleKind("interval")}
                className={`flex-1 py-1 rounded-md text-xs font-medium transition-colors ${
                  scheduleKind === "interval"
                    ? "bg-ember text-bg"
                    : "text-ink2 hover:text-ink"
                }`}
              >
                Interval
              </button>
            </div>
          </div>

          <div>
            {scheduleKind === "cron" ? (
              <>
                <label className="block text-xs font-medium text-ink2 mb-1">
                  Cron expression
                </label>
                <input
                  type="text"
                  value={scheduleExpr}
                  onChange={(e) => setScheduleExpr(e.target.value)}
                  placeholder="0 7 * * *"
                  className="w-full px-3 py-1.5 rounded-lg bg-panel2 border border-line text-sm font-mono text-ink placeholder-ink3 focus:outline-none focus:border-ember"
                />
              </>
            ) : (
              <>
                <label className="block text-xs font-medium text-ink2 mb-1">
                  Every
                </label>
                <input
                  type="text"
                  value={scheduleEvery}
                  onChange={(e) => setScheduleEvery(e.target.value)}
                  placeholder="10m"
                  className="w-full px-3 py-1.5 rounded-lg bg-panel2 border border-line text-sm font-mono text-ink placeholder-ink3 focus:outline-none focus:border-ember"
                />
              </>
            )}
          </div>
        </div>

        <div>
          <div className="flex items-center justify-between mb-1">
            <label className="text-xs font-medium text-ink2">Prompt</label>
            <span
              className={`text-xs font-mono ${
                runeCount > 1000 ? "text-err" : "text-ink3"
              }`}
            >
              {runeCount}/1000 runes
            </span>
          </div>
          <textarea
            value={prompt}
            maxLength={1000}
            onChange={(e) => setPrompt(e.target.value)}
            placeholder="Instructions for the agent when the job fires (single paragraph without newlines)..."
            rows={3}
            className="w-full px-3 py-2 rounded-lg bg-panel2 border border-line text-sm text-ink placeholder-ink3 focus:outline-none focus:border-ember resize-none"
          />
        </div>

        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <div>
            <label className="block text-xs font-medium text-ink2 mb-1">
              Session (optional)
            </label>
            <input
              type="text"
              value={session}
              onChange={(e) => setSession(e.target.value)}
              placeholder="e.g. default"
              className="w-full px-3 py-1.5 rounded-lg bg-panel2 border border-line text-sm text-ink placeholder-ink3 focus:outline-none focus:border-ember"
            />
          </div>

          <div>
            <label className="block text-xs font-medium text-ink2 mb-1">
              Announce (optional)
            </label>
            <input
              type="text"
              value={announce}
              onChange={(e) => setAnnounce(e.target.value)}
              placeholder="channel"
              className="w-full px-3 py-1.5 rounded-lg bg-panel2 border border-line text-sm text-ink placeholder-ink3 focus:outline-none focus:border-ember"
            />
          </div>
        </div>

        {formError && (
          <div className="p-2.5 rounded-lg border border-err/30 bg-err/10 text-err text-xs font-mono">
            {formError}
          </div>
        )}

        <div className="flex justify-end gap-2 pt-2">
          <button
            type="button"
            onClick={onClose}
            className="px-3 py-1.5 rounded-lg text-xs font-medium text-ink2 hover:bg-panel2 transition-colors"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={submitting}
            className="px-4 py-1.5 rounded-lg text-xs font-medium bg-ember hover:bg-ember2 text-bg transition-colors disabled:opacity-50"
          >
            {submitting ? "Creating…" : "Create job"}
          </button>
        </div>
      </form>
    </div>
  );
}
