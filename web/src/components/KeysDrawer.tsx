import React, { useState, useEffect, useCallback } from "react";
import { Key, X, AlertTriangle } from "lucide-react";
import { KeyRow } from "../types/hearthd";

interface KeysDrawerProps {
  isOpen: boolean;
  onClose: () => void;
  token: string | null;
}

export function KeysDrawer({ isOpen, onClose, token }: KeysDrawerProps) {
  const [keys, setKeys] = useState<KeyRow[]>([]);
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [statusMsg, setStatusMsg] = useState("");
  const [isError, setIsError] = useState(false);
  const [loading, setLoading] = useState(false);

  const isLoopback =
    typeof window !== "undefined" &&
    (window.location.hostname === "localhost" ||
      window.location.hostname === "127.0.0.1" ||
      window.location.hostname === "[::1]" ||
      window.location.hostname === "::1" ||
      window.location.hostname === "");
  const showTransportWarn =
    typeof window !== "undefined" &&
    window.location.protocol !== "https:" &&
    !isLoopback;

  const loadKeys = useCallback(async () => {
    if (!token) return;
    setLoading(true);
    try {
      const resp = await fetch("/v1/keys", {
        headers: {
          Authorization: `Bearer ${token}`,
        },
      });
      if (resp.status === 401) {
        setStatusMsg("unauthorized — token revoked or wrong");
        setIsError(true);
        return;
      }
      if (!resp.ok) {
        const err = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(err.error || resp.statusText);
      }
      const data = await resp.json();
      setKeys(Array.isArray(data) ? data : []);
    } catch (e: any) {
      setStatusMsg(`failed to load keys: ${e.message}`);
      setIsError(true);
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => {
    if (isOpen) {
      setStatusMsg("");
      setIsError(false);
      loadKeys();
    }
  }, [isOpen, loadKeys]);

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault();
    const trimmedName = name.trim();
    if (!trimmedName || !value) return;

    setStatusMsg("");
    setIsError(false);

    try {
      const resp = await fetch(`/v1/keys/${encodeURIComponent(trimmedName)}`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({ key: value }),
      });

      if (resp.status === 204 || resp.status === 200) {
        setValue("");
        setName("");
        setStatusMsg(`saved ${trimmedName}`);
        setIsError(false);
        await loadKeys();
        return;
      }

      const err = await resp.json().catch(() => ({ error: resp.statusText }));
      if (err.error === "unknown provider") {
        if (window.confirm(`"${trimmedName}" is not a configured provider. Save it anyway?`)) {
          const forceResp = await fetch(
            `/v1/keys/${encodeURIComponent(trimmedName)}?force=true`,
            {
              method: "PUT",
              headers: {
                "Content-Type": "application/json",
                Authorization: `Bearer ${token}`,
              },
              body: JSON.stringify({ key: value }),
            }
          );
          if (forceResp.status === 204 || forceResp.status === 200) {
            setValue("");
            setName("");
            setStatusMsg(`saved ${trimmedName}`);
            setIsError(false);
            await loadKeys();
            return;
          }
          const forceErr = await forceResp.json().catch(() => ({ error: forceResp.statusText }));
          throw new Error(forceErr.error || forceResp.statusText);
        }
        return;
      }
      throw new Error(err.error || resp.statusText);
    } catch (e: any) {
      setStatusMsg(e.message || "Failed to save key");
      setIsError(true);
    }
  };

  const handleRemove = async (keyName: string) => {
    if (!window.confirm(`Remove the key for ${keyName}?`)) return;
    try {
      const resp = await fetch(`/v1/keys/${encodeURIComponent(keyName)}`, {
        method: "DELETE",
        headers: {
          Authorization: `Bearer ${token}`,
        },
      });
      if (resp.status === 204 || resp.status === 200) {
        setStatusMsg(`removed ${keyName}`);
        setIsError(false);
        await loadKeys();
        return;
      }
      const err = await resp.json().catch(() => ({ error: resp.statusText }));
      throw new Error(err.error || resp.statusText);
    } catch (e: any) {
      setStatusMsg(e.message || "Failed to remove key");
      setIsError(true);
    }
  };

  if (!isOpen) return null;

  return (
    <>
      <div
        className="fixed inset-0 bg-black/60 backdrop-blur-xs z-40 transition-opacity"
        onClick={onClose}
      />
      <aside
        className="fixed top-0 right-0 bottom-0 w-full sm:w-96 bg-bg2 border-l border-line p-6 z-50 flex flex-col shadow-2xl overflow-y-auto"
        aria-label="Provider keys"
      >
        <div className="flex items-center justify-between pb-4 mb-4 border-b border-line">
          <div className="flex items-center gap-2">
            <Key className="w-5 h-5 text-ember" />
            <h2 className="text-base font-bold text-ink">Provider keys</h2>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="p-1 rounded-lg text-ink3 hover:text-ink hover:bg-panel transition-colors"
            title="Close"
          >
            <X className="w-5 h-5" />
          </button>
        </div>

        {showTransportWarn && (
          <div className="p-3 rounded-lg border border-warn/30 bg-warn/10 text-warn text-xs mb-4 flex items-start gap-2">
            <AlertTriangle className="w-4 h-4 shrink-0 mt-0.5" />
            <div className="leading-relaxed">
              This page is served over plain HTTP from a non-local address: keys typed
              here cross the network unencrypted. Use <code className="font-mono bg-panel px-1 py-0.5 rounded">hearthd keys set</code> on
              the daemon host, or connect over HTTPS or loopback, instead.
            </div>
          </div>
        )}

        <p className="text-xs text-ink3 leading-relaxed mb-4">
          Keys are stored on the daemon host (mode 0600) and never sent back to this page.
          Values show as a display fingerprint only.
        </p>

        <div className="flex-1 mb-6 overflow-y-auto">
          {loading && keys.length === 0 ? (
            <div className="text-xs text-ink3 py-4 text-center">Loading keys…</div>
          ) : keys.length === 0 ? (
            <div className="text-xs text-ink3 py-4 text-center border border-dashed border-line rounded-lg">
              No keys found
            </div>
          ) : (
            <ul className="space-y-2">
              {keys.map((k) => (
                <li
                  key={k.name}
                  className="p-2.5 rounded-lg border border-line bg-panel flex items-center justify-between text-xs"
                >
                  <div className="min-w-0 pr-2">
                    <div className="font-semibold text-ink truncate">{k.name}</div>
                    <div className="text-ink3 font-mono text-[11px] truncate">
                      {k.status === "missing"
                        ? "missing"
                        : `${k.status}${k.sha256_8 ? ` · ${k.sha256_8}` : ""}`}
                    </div>
                  </div>
                  {k.status === "set (keys.json)" && (
                    <button
                      type="button"
                      onClick={() => handleRemove(k.name)}
                      className="px-2 py-1 rounded text-[11px] text-err hover:bg-err/10 transition-colors"
                    >
                      remove
                    </button>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>

        <form onSubmit={handleSave} className="space-y-3 pt-4 border-t border-line">
          <div>
            <label className="block text-xs font-medium text-ink2 mb-1">Provider Name</label>
            <input
              type="text"
              placeholder="e.g. openai, anthropic"
              value={name}
              onChange={(e) => setName(e.target.value)}
              className="w-full px-3 py-2 rounded-lg bg-panel border border-line text-base text-ink placeholder-ink3 focus:outline-hidden focus:border-ember"
              autoComplete="off"
              spellCheck="false"
            />
          </div>
          <div>
            <label className="block text-xs font-medium text-ink2 mb-1">API Key</label>
            <input
              type="password"
              placeholder="api key value"
              value={value}
              onChange={(e) => setValue(e.target.value)}
              className="w-full px-3 py-2 rounded-lg bg-panel border border-line text-base text-ink placeholder-ink3 focus:outline-hidden focus:border-ember font-mono"
              autoComplete="new-password"
            />
          </div>
          <button
            type="submit"
            className="w-full min-h-[44px] py-2 rounded-lg bg-ember text-bg font-semibold text-xs hover:bg-ember2 transition-colors disabled:opacity-50 flex items-center justify-center"
            disabled={!name.trim() || !value}
          >
            Save Key
          </button>
        </form>

        {statusMsg && (
          <div
            className={`mt-3 text-xs p-2.5 rounded-lg ${
              isError ? "bg-err/10 text-err border border-err/30" : "bg-ok/10 text-ok border border-ok/30"
            }`}
          >
            {statusMsg}
          </div>
        )}
      </aside>
    </>
  );
}

export default KeysDrawer;
