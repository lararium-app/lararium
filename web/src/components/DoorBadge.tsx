import { PolicyRule } from "../types/hearthd";

interface DoorBadgeProps {
  verdict: PolicyRule["verdict"] | string;
  className?: string;
}

export function DoorBadge({ verdict, className = "" }: DoorBadgeProps) {
  let colorClasses = "bg-panel2 text-ink2 border-line";
  let label = verdict;

  switch (verdict) {
    case "auto":
      colorClasses = "bg-ok/15 text-ok border-ok/30";
      label = "Open";
      break;
    case "ask":
      colorClasses = "bg-warn/15 text-warn border-warn/30";
      label = "Knock first";
      break;
    case "deny":
      colorClasses = "bg-err/15 text-err border-err/30";
      label = "Sealed";
      break;
  }

  return (
    <span
      title={verdict}
      className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-mono font-medium border ${colorClasses} ${className}`}
    >
      {label}
    </span>
  );
}

export default DoorBadge;
