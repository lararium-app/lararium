import { DoorPolicy } from "../types/hearthd";

interface DoorBadgeProps {
  status: DoorPolicy["status"];
  className?: string;
}

export function DoorBadge({ status, className = "" }: DoorBadgeProps) {
  let colorClasses = "bg-panel2 text-ink2 border-line";

  switch (status) {
    case "open":
      colorClasses = "bg-ok/15 text-ok border-ok/30";
      break;
    case "closed":
      colorClasses = "bg-panel2 text-ink3 border-line";
      break;
    case "guarded":
      colorClasses = "bg-warn/15 text-warn border-warn/30";
      break;
    case "restricted":
      colorClasses = "bg-err/15 text-err border-err/30";
      break;
  }

  return (
    <span
      className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-mono font-medium border ${colorClasses} ${className}`}
    >
      {status}
    </span>
  );
}

export default DoorBadge;
