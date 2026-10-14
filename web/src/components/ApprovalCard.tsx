import { useState } from "react";
import { Shield, AlertTriangle } from "lucide-react";
import { ApprovalRequest, CustosCard } from "../types/hearthd";

interface ToolApprovalCardProps {
  request: ApprovalRequest;
  onResolve: (approvalId: string, verdict: "allow" | "deny") => Promise<void>;
}

export function ToolApprovalCard({ request, onResolve }: ToolApprovalCardProps) {
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleDecision = async (verdict: "allow" | "deny") => {
    if (submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      await onResolve(request.approval_id, verdict);
    } catch (e: any) {
      setError(e.message || "Failed to submit decision");
      setSubmitting(false);
    }
  };

  return (
    <div
      role="dialog"
      aria-labelledby={`tool-approval-${request.approval_id}`}
      className="my-3 p-3 rounded-xl border border-line bg-panel shadow-e2"
    >
      <div className="flex items-start gap-3">
        <div className="p-2 rounded-lg bg-ember/10 text-ember shrink-0">
          <AlertTriangle className="w-5 h-5" />
        </div>
        <div className="flex-1 min-w-0">
          <div className="flex items-center justify-between mb-1">
            <h4
              id={`tool-approval-${request.approval_id}`}
              className="font-medium text-ink text-sm"
            >
              Tool Approval Required
            </h4>
            <span className="text-xs px-2 py-0.5 rounded font-mono bg-panel2 text-ink2 border border-line">
              {request.name}
            </span>
          </div>
          <div className="bg-panel2 rounded-lg p-2 text-xs font-mono text-ink2 mb-3 break-words whitespace-pre-wrap">
            {request.args_summary}
          </div>

          {error && (
            <div className="text-xs text-err mb-2">{error}</div>
          )}

          <div className="flex items-center gap-2">
            <button
              type="button"
              disabled={submitting}
              onClick={() => handleDecision("allow")}
              className="min-h-[44px] px-4 py-2 rounded-lg text-xs font-medium bg-ember hover:bg-ember2 text-bg transition-colors flex items-center justify-center disabled:cursor-not-allowed"
            >
              Allow
            </button>
            <button
              type="button"
              disabled={submitting}
              onClick={() => handleDecision("deny")}
              className="min-h-[44px] px-4 py-2 rounded-lg text-xs font-medium text-ink3 hover:text-err transition-colors flex items-center justify-center disabled:cursor-not-allowed"
            >
              Deny
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

interface CustosApprovalCardProps {
  card: CustosCard;
  onResolve: (id: string, verdict: "once" | "always" | "deny") => Promise<void>;
}

export function CustosApprovalCard({ card, onResolve }: CustosApprovalCardProps) {
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleResolve = async (verdict: "once" | "always" | "deny") => {
    if (submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      await onResolve(card.id, verdict);
    } catch (e: any) {
      setError(e.message || "Failed to resolve custody card");
      setSubmitting(false);
    }
  };

  const metaParts = [];
  if (card.cell) metaParts.push(`cell: ${card.cell}`);
  if (card.cred) metaParts.push(`cred: ${card.cred}`);

  return (
    <div
      role="dialog"
      aria-labelledby={`custos-card-${card.id}`}
      className="mb-3 p-3 rounded-xl border border-line bg-panel shadow-e2"
    >
      <div className="flex items-start gap-3">
        <div className="p-2 rounded-lg bg-ember/15 text-ember shrink-0">
          <Shield className="w-5 h-5" />
        </div>
        <div className="flex-1 min-w-0">
          <div className="flex items-center justify-between mb-1">
            <div className="flex items-center gap-2 flex-wrap">
              <h4 id={`custos-card-${card.id}`} className="font-semibold text-ink text-sm">
                Custody Request
              </h4>
              {metaParts.length > 0 && (
                <span className="text-xs text-ink3">
                  — {metaParts.join(" · ")}
                </span>
              )}
            </div>
            {card.expires_in_s > 0 && (
              <span className="text-xs text-ink3 font-mono">
                expires in {card.expires_in_s}s
              </span>
            )}
          </div>

          <div className="space-y-1.5 text-xs mb-3">
            {card.dest && (
              <div className="text-ink2">
                <span className="text-ink3 font-medium">Destination:</span>{" "}
                <span className="font-mono text-ink">{card.dest}</span>
              </div>
            )}
            {card.tool && (
              <div className="text-ink2">
                <span className="text-ink3 font-medium">Tool:</span>{" "}
                <span className="font-mono text-ink">{card.tool}</span>
              </div>
            )}
            {card.review && (
              <div className="p-2 rounded bg-panel2 border border-line text-ink leading-relaxed">
                {card.review}
              </div>
            )}
          </div>

          {error && (
            <div className="text-xs text-err mb-2 font-mono">{error}</div>
          )}

          <div className="flex items-center gap-2 flex-wrap">
            <button
              type="button"
              disabled={submitting}
              onClick={() => handleResolve("once")}
              className="min-h-[44px] px-3.5 py-2 rounded-lg text-xs font-medium bg-ember hover:bg-ember2 text-bg transition-colors flex items-center justify-center disabled:cursor-not-allowed"
            >
              Allow once
            </button>
            <button
              type="button"
              disabled={submitting}
              onClick={() => handleResolve("always")}
              className="min-h-[44px] px-3.5 py-2 rounded-lg text-xs font-medium text-ink3 hover:text-ink transition-colors flex items-center justify-center disabled:cursor-not-allowed"
            >
              Always
            </button>
            <button
              type="button"
              disabled={submitting}
              onClick={() => handleResolve("deny")}
              className="min-h-[44px] px-3.5 py-2 rounded-lg text-xs font-medium text-ink3 hover:text-err transition-colors flex items-center justify-center disabled:cursor-not-allowed"
            >
              Deny
            </button>
            <button
              type="button"
              disabled
              title="not supported by hearthd yet"
              className="min-h-[44px] px-3.5 py-2 rounded-lg text-xs font-medium text-ink3 cursor-not-allowed flex items-center justify-center"
            >
              Edit Parameters
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

export default ToolApprovalCard;
