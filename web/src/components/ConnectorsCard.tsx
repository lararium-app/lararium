import { useState, useEffect, useCallback, useRef } from "react";
import { Plug, RefreshCw } from "lucide-react";
import { PolicyRule } from "../types/hearthd";

interface ConnectorCardData {
  name: string;
  state: string;
  tools: number;
  restarts: number;
  reason?: string;
}

interface ConnectorsCardProps {
  token: string | null;
  rules?: PolicyRule[];
  onRefreshRules?: () => void;
}

// State → pill styling. Mirrors the closed `state` set of MCP-SPEC §7.
function stateClasses(state: string): string {
  switch (state) {
    case "ready":
      return "bg-ok/15 text-ok border-ok/30";
    case "failed":
      return "bg-err/15 text-err border-err/30";
    case "backoff":
    case "starting":
    case "restarting":
      return "bg-warn/15 text-warn border-warn/30";
    default:
      return "bg-panel2 text-ink3 border-line";
  }
}

const POLL_MS = 15000;

// ConnectorsCard lists the MCP rail inventory served by
// GET /v1/mcp/connectors. Rail tools join model turns automatically
// once a connector is ready (issue #83); this card is the operator's
// view into what the rail is doing.
export function ConnectorsCard({ token, rules, onRefreshRules }: ConnectorsCardProps) {
  const [connectors, setConnectors] = useState<ConnectorCardData[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [untrusting, setUntrusting] = useState<string | null>(null);
  const [untrustError, setUntrustError] = useState<string | null>(null);
  const timer = useRef<number | null>(null);

  const fetchConnectors = useCallback(async () => {
    if (!token) return;
    setLoading(true);
    try {
      const resp = await fetch("/v1/mcp/connectors", {
        headers: { Authorization: `Bearer ${token}` },
      });
      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(err.error || resp.statusText);
      }
      const data = await resp.json();
      setConnectors(Array.isArray(data.connectors) ? data.connectors : []);
      setError(null);
    } catch (e: any) {
      setError(e.message || "Failed to load connectors");
    } finally {
      setLoading(false);
    }
  }, [token]);

  const handleUntrust = async (connectorName: string) => {
    if (!token || untrusting) return;
    setUntrusting(connectorName);
    setUntrustError(null);
    try {
      const encodedPattern = encodeURIComponent(`${connectorName}/*`);
      const resp = await fetch(
        `/v1/policy/rules/${encodedPattern}?lane=credential`,
        {
          method: "DELETE",
          headers: {
            Authorization: `Bearer ${token}`,
          },
        }
      );
      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(err.error || resp.statusText);
      }
      if (onRefreshRules) {
        onRefreshRules();
      }
    } catch (e: any) {
      setUntrustError(e.message || `Failed to untrust ${connectorName}`);
    } finally {
      setUntrusting(null);
    }
  };

  useEffect(() => {
    if (!token) return;
    fetchConnectors();
    timer.current = window.setInterval(fetchConnectors, POLL_MS);
    return () => {
      if (timer.current !== null) window.clearInterval(timer.current);
    };
  }, [token, fetchConnectors]);

  return (
    <div className="rounded-2xl border border-line bg-panel p-4 mb-6">
      <div className="flex items-center justify-between mb-3">
        <div className="flex items-center gap-2.5">
          <div className="p-2 rounded-lg bg-panel2 border border-line text-ember">
            <Plug className="w-4 h-4" />
          </div>
          <div>
            <h3 className="text-sm font-semibold text-ink">MCP connectors</h3>
            <p className="text-xs text-ink3">Tools offered to model turns per connector</p>
          </div>
        </div>
        <button
          type="button"
          onClick={fetchConnectors}
          disabled={loading}
          className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-medium bg-panel2 hover:bg-line text-ink2 hover:text-ink border border-line transition-colors disabled:opacity-50"
        >
          <RefreshCw className={`w-3.5 h-3.5 ${loading ? "animate-spin" : ""}`} />
          <span>Refresh</span>
        </button>
      </div>

      {error && (
        <div className="p-3 mb-3 rounded-xl border border-err/30 bg-err/10 text-err text-xs font-mono">
          {error}
        </div>
      )}

      {untrustError && (
        <div className="p-3 mb-3 rounded-xl border border-err/30 bg-err/10 text-err text-xs font-mono">
          {untrustError}
        </div>
      )}

      {!error && connectors === null && (
        <p className="text-xs text-ink3 font-mono">loading…</p>
      )}

      {!error && connectors !== null && connectors.length === 0 && (
        <p className="text-xs text-ink3">
          No MCP connectors configured — declare them under{" "}
          <code className="font-mono text-ink2">custos.connectors:</code> in lararium.yaml.
        </p>
      )}

      {!error && connectors !== null && connectors.length > 0 && (
        <ul className="divide-y divide-line">
          {connectors.map((c) => {
            const isTrusted = rules?.some(
              (r) => r.lane === "credential" && r.pattern === `${c.name}/*` && r.verdict === "auto"
            );
            return (
              <li key={c.name} className="flex items-center gap-3 py-2 first:pt-0 last:pb-0">
                <span className="font-mono text-sm text-ink min-w-0 truncate">{c.name}</span>
                <span
                  className={`px-2 py-0.5 rounded-full text-[11px] font-mono font-medium border ${stateClasses(c.state)}`}
                >
                  {c.state}
                </span>
                {isTrusted && (
                  <span className="px-2 py-0.5 rounded-full text-[11px] font-mono font-medium border bg-ok/15 text-ok border-ok/30">
                    trusted
                  </span>
                )}
                <span className="text-xs text-ink3">
                  {c.tools} tool{c.tools === 1 ? "" : "s"}
                  {c.restarts > 0 && ` · ${c.restarts} restart${c.restarts === 1 ? "" : "s"}`}
                </span>
                {isTrusted ? (
                  <button
                    type="button"
                    disabled={untrusting === c.name}
                    onClick={() => handleUntrust(c.name)}
                    className="ml-auto px-2 py-1 rounded text-xs font-medium text-ink3 hover:text-err border border-line hover:border-err/30 transition-colors disabled:opacity-50"
                  >
                    {untrusting === c.name ? "Untrusting…" : "Untrust"}
                  </button>
                ) : c.reason && c.reason !== "ok" ? (
                  <span className="ml-auto text-[11px] font-mono text-ink3 truncate max-w-[40%]" title={c.reason}>
                    {c.reason}
                  </span>
                ) : null}
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
