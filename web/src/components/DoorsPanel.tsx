import { useState, useEffect } from "react";
import { Shield, RefreshCw } from "lucide-react";
import { DoorPolicy } from "../types/hearthd";
import { DoorBadge } from "./DoorBadge";

interface DoorsPanelProps {
  token: string | null;
}

export function DoorsPanel({ token }: DoorsPanelProps) {
  const [doors, setDoors] = useState<DoorPolicy[]>([]);
  const [loading, setLoading] = useState(false);
  const [unavailable, setUnavailable] = useState(false);
  const [errorMsg, setErrorMsg] = useState<string | null>(null);

  const fetchDoors = async () => {
    if (!token) return;
    setLoading(true);
    setErrorMsg(null);
    setUnavailable(false);
    try {
      const resp = await fetch("/v1/doors", {
        headers: {
          Authorization: `Bearer ${token}`,
        },
      });

      if (resp.status === 404) {
        setUnavailable(true);
        setDoors([]);
        return;
      }

      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(err.error || resp.statusText);
      }

      const data = await resp.json();
      if (Array.isArray(data)) {
        setDoors(data);
      } else if (data && Array.isArray(data.doors)) {
        setDoors(data.doors);
      } else {
        setDoors([]);
      }
    } catch (e: any) {
      setErrorMsg(e.message || "Failed to fetch doors");
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    fetchDoors();
  }, [token]);

  return (
    <div className="flex-1 overflow-y-auto p-4 md:p-6 bg-bg text-ink max-w-4xl mx-auto w-full">
      <div className="flex items-center justify-between pb-4 mb-6 border-b border-line">
        <div className="flex items-center gap-3">
          <div className="p-2.5 rounded-xl bg-panel2 border border-line text-ember">
            <Shield className="w-6 h-6" />
          </div>
          <div>
            <h2 className="text-lg font-bold text-ink">Doors & Policy Dashboard</h2>
            <p className="text-xs text-ink3">Manage door access controls, security policies, and capabilities</p>
          </div>
        </div>
        <button
          type="button"
          onClick={fetchDoors}
          disabled={loading}
          className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-medium bg-panel2 hover:bg-line text-ink2 hover:text-ink border border-line transition-colors disabled:opacity-50"
        >
          <RefreshCw className={`w-3.5 h-3.5 ${loading ? "animate-spin" : ""}`} />
          <span>Refresh</span>
        </button>
      </div>

      {unavailable ? (
        <div className="p-8 text-center rounded-2xl border border-dashed border-line bg-panel2/50 my-6">
          <Shield className="w-10 h-10 text-ink3 mx-auto mb-3 opacity-60" />
          <h3 className="text-sm font-semibold text-ink mb-1">Doors API not available yet</h3>
          <p className="text-xs text-ink3 max-w-sm mx-auto leading-relaxed">
            The daemon returned 404 for <code className="font-mono bg-panel px-1 py-0.5 rounded text-ember">/v1/doors</code>.
            Backend support for live door policies is pending implementation in hearthd.
          </p>
        </div>
      ) : errorMsg ? (
        <div className="p-4 rounded-xl border border-err/30 bg-err/10 text-err text-xs font-mono">
          {errorMsg}
        </div>
      ) : doors.length === 0 ? (
        <div className="p-8 text-center rounded-2xl border border-dashed border-line bg-panel2/50 my-6">
          <p className="text-xs text-ink3">No door policies configured.</p>
        </div>
      ) : (
        <div className="grid gap-3">
          {doors.map((door) => (
            <div
              key={door.name}
              className="p-4 rounded-xl border border-line bg-panel flex items-center justify-between"
            >
              <div>
                <div className="flex items-center gap-2 mb-1">
                  <span className="font-semibold text-sm text-ink">{door.name}</span>
                  <DoorBadge status={door.status} />
                </div>
                {door.description && (
                  <p className="text-xs text-ink3">{door.description}</p>
                )}
              </div>
              {door.allowed_tools && door.allowed_tools.length > 0 && (
                <div className="text-xs text-ink3 font-mono">
                  {door.allowed_tools.join(", ")}
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

export default DoorsPanel;
