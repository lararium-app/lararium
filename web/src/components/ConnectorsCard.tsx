import { useState, useEffect, useCallback, useRef } from "react";
import { Plug, RefreshCw } from "lucide-react";

interface ConnectorCardData {
  name: string;
  state: string;
  tools: number;
  restarts: number;
  reason?: string;
}

interface ConnectorsCardProps {
  token: string | null;
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
export function ConnectorsCard({ token }: ConnectorsCardProps) {
  const [connectors, setConnectors] = useState<ConnectorCardData[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
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
        <div className="p-3 rounded-xl border border-err/30 bg-err/10 text-err text-xs font-mono">
          {error}
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
          {connectors.map((c) => (
            <li key={c.name} className="flex items-center gap-3 py-2 first:pt-0 last:pb-0">
              <span className="font-mono text-sm text-ink min-w-0 truncate">{c.name}</span>
              <span
                className={`px-2 py-0.5 rounded-full text-[11px] font-mono font-medium border ${stateClasses(c.state)}`}
              >
                {c.state}
              </span>
              <span className="text-xs text-ink3">
                {c.tools} tool{c.tools === 1 ? "" : "s"}
                {c.restarts > 0 && ` · ${c.restarts} restart${c.restarts === 1 ? "" : "s"}`}
              </span>
              {c.reason && c.reason !== "ok" && (
                <span className="ml-auto text-[11px] font-mono text-ink3 truncate max-w-[40%]" title={c.reason}>
                  {c.reason}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
