import React, { useState, useEffect } from "react";
import { Shield, RefreshCw, Trash2, Plus, Lock, AlertCircle } from "lucide-react";
import { PolicyRule } from "../types/hearthd";
import { DoorBadge } from "./DoorBadge";
import { ConnectorsCard } from "./ConnectorsCard";

interface DoorsPanelProps {
  token: string | null;
}

export function DoorsPanel({ token }: DoorsPanelProps) {
  const [rules, setRules] = useState<PolicyRule[]>([]);
  const [egressStrict, setEgressStrict] = useState(false);
  const [loading, setLoading] = useState(false);
  const [locked, setLocked] = useState(false);
  const [corrupt, setCorrupt] = useState(false);
  const [errorMsg, setErrorMsg] = useState<string | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  // Form state
  const [lane, setLane] = useState<"credential" | "egress">("credential");
  const [pattern, setPattern] = useState("");
  const [verdict, setVerdict] = useState<"auto" | "ask" | "deny">("auto");
  const [submitting, setSubmitting] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [formNotes, setFormNotes] = useState<string[]>([]);

  const fetchRules = async () => {
    if (!token) return;
    setLoading(true);
    setErrorMsg(null);
    setDeleteError(null);
    try {
      const resp = await fetch("/v1/policy/rules", {
        headers: {
          Authorization: `Bearer ${token}`,
        },
      });

      if (resp.status === 423) {
        setLocked(true);
        setRules([]);
        return;
      }

      if (resp.status === 503) {
        setCorrupt(true);
        setRules([]);
        return;
      }

      setLocked(false);
      setCorrupt(false);

      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(err.error || resp.statusText);
      }

      const data = await resp.json();
      setEgressStrict(Boolean(data.egress_strict));
      if (Array.isArray(data.rules)) {
        setRules(data.rules);
      } else {
        setRules([]);
      }
    } catch (e: any) {
      setErrorMsg(e.message || "Failed to fetch door policy rules");
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    fetchRules();
  }, [token]);

  const handleAddRule = async (e: React.FormEvent) => {
    e.preventDefault();
    const trimmedPattern = pattern.trim();
    if (!token || !trimmedPattern || submitting) return;

    setSubmitting(true);
    setFormError(null);
    setFormNotes([]);

    try {
      const resp = await fetch("/v1/policy/rules", {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({
          lane,
          pattern: trimmedPattern,
          verdict,
        }),
      });

      if (resp.status === 423) {
        setLocked(true);
        setFormError("Custody locked or not configured — door policy is managed by the custos CLI");
        return;
      }

      if (resp.status === 503) {
        setCorrupt(true);
        setFormError("policy corrupt");
        return;
      }

      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        setFormError(err.error || resp.statusText);
        return;
      }

      const data = await resp.json();
      if (data && Array.isArray(data.notes)) {
        setFormNotes(data.notes);
      }
      setPattern("");
      await fetchRules();
    } catch (e: any) {
      setFormError(e.message || "Failed to add rule");
    } finally {
      setSubmitting(false);
    }
  };

  const handleDeleteRule = async (rule: PolicyRule) => {
    if (!token) return;

    const isCard = rule.source.includes("card");
    if (isCard || rule.always) {
      const reason =
        isCard && rule.always
          ? "This rule was minted by a custody card and marked always."
          : isCard
          ? "This rule was minted by a custody card."
          : "This rule is marked always.";
      const confirmed = window.confirm(
        `${reason} Are you sure you want to delete this rule?`
      );
      if (!confirmed) return;
    }

    setDeleteError(null);
    try {
      const encodedPattern = encodeURIComponent(rule.pattern);
      const encodedLane = encodeURIComponent(rule.lane);
      const resp = await fetch(
        `/v1/policy/rules/${encodedPattern}?lane=${encodedLane}`,
        {
          method: "DELETE",
          headers: {
            Authorization: `Bearer ${token}`,
          },
        }
      );

      if (resp.status === 423) {
        setLocked(true);
        return;
      }

      if (resp.status === 503) {
        setCorrupt(true);
        return;
      }

      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(err.error || resp.statusText);
      }

      await fetchRules();
    } catch (e: any) {
      setDeleteError(e.message || "Failed to delete rule");
    }
  };

  const lanes: Array<"credential" | "egress"> = ["credential", "egress"];

  return (
    <div className="flex-1 overflow-y-auto p-4 md:p-6 bg-bg text-ink max-w-4xl mx-auto w-full">
      <ConnectorsCard token={token} rules={rules} onRefreshRules={fetchRules} />
      <div className="flex items-center justify-between pb-4 mb-6 border-b border-line">
        <div className="flex items-center gap-3">
          <div className="p-2.5 rounded-xl bg-panel2 border border-line text-ember">
            <Shield className="w-6 h-6" />
          </div>
          <div>
            <div className="flex items-center gap-2">
              <h2 className="text-lg font-bold text-ink">Door policy</h2>
              {egressStrict && (
                <span
                  title="Strict egress enforcement is active"
                  className="px-2 py-0.5 rounded-full text-xs font-mono font-medium bg-warn/15 text-warn border border-warn/30"
                >
                  egress_strict
                </span>
              )}
            </div>
            <p className="text-xs text-ink3">Manage egress and credential access rules</p>
          </div>
        </div>
        <button
          type="button"
          onClick={fetchRules}
          disabled={loading}
          className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-medium bg-panel2 hover:bg-line text-ink2 hover:text-ink border border-line transition-colors disabled:opacity-50"
        >
          <RefreshCw className={`w-3.5 h-3.5 ${loading ? "animate-spin" : ""}`} />
          <span>Refresh</span>
        </button>
      </div>

      {locked ? (
        <div className="p-8 text-center rounded-2xl border border-dashed border-line bg-panel2/50 my-6">
          <Lock className="w-10 h-10 text-ink3 mx-auto mb-3 opacity-60" />
          <h3 className="text-sm font-semibold text-ink mb-1">Custody locked or not configured</h3>
          <p className="text-xs text-ink3 max-w-md mx-auto leading-relaxed">
            Custody locked or not configured — door policy is managed by the custos CLI
          </p>
        </div>
      ) : corrupt ? (
        <div className="p-4 rounded-xl border border-err/30 bg-err/10 text-err text-xs font-mono my-6 flex items-center gap-2">
          <AlertCircle className="w-4 h-4 shrink-0" />
          <span>policy corrupt</span>
        </div>
      ) : (
        <>
          {deleteError && (
            <div className="mb-4 p-3 rounded-xl border border-err/30 bg-err/10 text-err text-xs font-mono flex items-center justify-between">
              <span>{deleteError}</span>
              <button
                type="button"
                onClick={() => setDeleteError(null)}
                className="text-err hover:text-ink ml-2"
              >
                ×
              </button>
            </div>
          )}

          {errorMsg && (
            <div className="mb-4 p-4 rounded-xl border border-err/30 bg-err/10 text-err text-xs font-mono">
              {errorMsg}
            </div>
          )}

          {/* Add-rule form */}
          <div className="p-4 rounded-2xl border border-line bg-panel mb-6">
            <h3 className="text-sm font-semibold text-ink mb-3">Add policy rule</h3>
            <form onSubmit={handleAddRule} className="space-y-3">
              <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
                <div>
                  <label className="block text-xs font-medium text-ink2 mb-1">Lane</label>
                  <select
                    value={lane}
                    onChange={(e) => setLane(e.target.value as "credential" | "egress")}
                    className="w-full px-3 py-2 rounded-lg bg-panel2 border border-line text-sm text-ink focus:outline-hidden focus:border-ember"
                  >
                    <option value="credential">credential</option>
                    <option value="egress">egress</option>
                  </select>
                </div>

                <div>
                  <label className="block text-xs font-medium text-ink2 mb-1">Verdict</label>
                  <select
                    value={verdict}
                    onChange={(e) => setVerdict(e.target.value as "auto" | "ask" | "deny")}
                    className="w-full px-3 py-2 rounded-lg bg-panel2 border border-line text-sm text-ink focus:outline-hidden focus:border-ember"
                  >
                    <option value="auto">Open (auto)</option>
                    <option value="ask">Knock first (ask)</option>
                    <option value="deny">Sealed (deny)</option>
                  </select>
                </div>

                <div>
                  <label className="block text-xs font-medium text-ink2 mb-1">Pattern</label>
                  <input
                    type="text"
                    placeholder="e.g. api.anthropic.com or openai.com/v1/*"
                    value={pattern}
                    onChange={(e) => {
                      setPattern(e.target.value);
                      if (formError) setFormError(null);
                    }}
                    className="w-full px-3 py-2 rounded-lg bg-panel2 border border-line text-sm font-mono text-ink placeholder-ink3 focus:outline-hidden focus:border-ember"
                    spellCheck="false"
                    autoComplete="off"
                  />
                </div>
              </div>

              <p className="text-[11px] text-ink3">
                Matches credential hostnames/paths or egress URL prefixes. Printable ASCII (1–256 characters, no spaces).
              </p>

              <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 pt-1">
                <div className="flex-1">
                  {formError && (
                    <div className="text-xs text-err font-mono">
                      {formError}
                    </div>
                  )}
                </div>
                <button
                  type="submit"
                  disabled={submitting || !pattern.trim()}
                  className="min-h-[38px] px-4 py-1.5 rounded-lg bg-ember text-bg font-semibold text-xs hover:bg-ember2 transition-colors disabled:opacity-50 shrink-0 flex items-center justify-center gap-1.5"
                >
                  <Plus className="w-3.5 h-3.5" />
                  <span>{submitting ? "Adding…" : "Add rule"}</span>
                </button>
              </div>

              {formNotes.length > 0 && (
                <div className="mt-3 p-3 rounded-xl border border-line bg-panel2/60 text-xs space-y-1">
                  <span className="font-semibold text-ink block">Domination notes:</span>
                  <ul className="list-disc list-inside space-y-0.5 text-ink3 font-mono text-[11px]">
                    {formNotes.map((note, idx) => (
                      <li key={idx}>{note}</li>
                    ))}
                  </ul>
                </div>
              )}
            </form>
          </div>

          {/* Grouped rules by lane */}
          <div className="space-y-6">
            {lanes.map((laneKey) => {
              const laneRules = rules.filter((r) => r.lane === laneKey);
              return (
                <div key={laneKey} className="space-y-3">
                  <div className="flex items-center justify-between pb-2 border-b border-line">
                    <h3 className="text-xs font-semibold uppercase tracking-wider text-ink2">
                      {laneKey} rules
                    </h3>
                    <span className="text-[11px] font-mono text-ink3">
                      {laneRules.length} {laneRules.length === 1 ? "rule" : "rules"}
                    </span>
                  </div>

                  {laneRules.length === 0 ? (
                    <div className="p-6 text-center rounded-xl border border-dashed border-line bg-panel2/30 text-xs text-ink3">
                      No {laneKey} rules configured.
                    </div>
                  ) : (
                    <div className="grid gap-2">
                      {laneRules.map((rule, idx) => (
                        <div
                          key={`${rule.lane}-${rule.pattern}-${idx}`}
                          className="p-3 sm:p-4 rounded-xl border border-line bg-panel flex flex-col sm:flex-row sm:items-center justify-between gap-3"
                        >
                          <div className="flex flex-wrap items-center gap-2 min-w-0">
                            <span className="font-mono text-xs sm:text-sm text-ink break-all">
                              {rule.pattern}
                            </span>
                            <DoorBadge verdict={rule.verdict} />
                            {rule.always && (
                              <span
                                title="Immutable rule (always=true)"
                                className="px-1.5 py-0.5 rounded text-[11px] font-mono bg-panel2 border border-line text-ink3"
                              >
                                always
                              </span>
                            )}
                          </div>

                          <div className="flex items-center justify-between sm:justify-end gap-3 shrink-0 pt-2 sm:pt-0 border-t sm:border-t-0 border-line/40">
                            <span
                              title={`Source: ${rule.source}`}
                              className="text-xs text-ink3 font-mono truncate max-w-[160px] sm:max-w-[200px]"
                            >
                              {rule.source}
                            </span>
                            <button
                              type="button"
                              onClick={() => handleDeleteRule(rule)}
                              className="min-h-[32px] min-w-[32px] p-1.5 rounded-lg text-ink3 hover:text-err hover:bg-err/10 transition-colors flex items-center justify-center"
                              title={`Delete rule ${rule.pattern}`}
                              aria-label={`Delete rule ${rule.pattern}`}
                            >
                              <Trash2 className="w-4 h-4" />
                            </button>
                          </div>
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        </>
      )}
    </div>
  );
}

export default DoorsPanel;
