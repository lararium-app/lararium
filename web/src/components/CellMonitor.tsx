import { Cpu, AlertCircle } from "lucide-react";
import { CellStatus } from "../types/hearthd";

interface CellMonitorProps {
  cells?: CellStatus[];
}

export function CellMonitor({ cells = [] }: CellMonitorProps) {
  return (
    <div className="flex-1 overflow-y-auto p-4 md:p-6 bg-bg text-ink max-w-4xl mx-auto w-full">
      <div className="flex items-center justify-between pb-4 mb-6 border-b border-line">
        <div className="flex items-center gap-3">
          <div className="p-2.5 rounded-xl bg-panel2 border border-line text-ember">
            <Cpu className="w-6 h-6" />
          </div>
          <div>
            <div className="flex items-center gap-2">
              <h2 className="text-lg font-bold text-ink">Cells & Sandbox Monitor</h2>
              <span className="px-2 py-0.5 text-xs font-semibold rounded-full bg-warn/15 text-warn border border-warn/30">
                Preview — backend pending
              </span>
            </div>
            <p className="text-xs text-ink3">Inspect and supervise sandbox execution environments</p>
          </div>
        </div>
      </div>

      <div className="p-4 rounded-xl border border-warn/30 bg-warn/10 text-warn text-xs mb-6 flex items-start gap-2.5">
        <AlertCircle className="w-4 h-4 shrink-0 mt-0.5" />
        <div className="leading-relaxed">
          <strong>Preview — backend pending:</strong> No backend endpoint exists for Cell sandbox monitoring yet.
          When hearthd adds cell supervision APIs, active sandboxes and containerized execution metrics will appear here.
        </div>
      </div>

      {cells.length === 0 ? (
        <div className="p-12 text-center rounded-2xl border border-dashed border-line bg-panel2/50 my-6">
          <Cpu className="w-10 h-10 text-ink3 mx-auto mb-3 opacity-60" />
          <h3 className="text-sm font-semibold text-ink mb-1">No Sandbox Cells Active</h3>
          <p className="text-xs text-ink3 max-w-md mx-auto leading-relaxed">
            Live cell instrumentation is pending backend support. No live processes are running.
          </p>
        </div>
      ) : (
        <div className="grid gap-3">
          {cells.map((cell) => (
            <div
              key={cell.id}
              className="p-4 rounded-xl border border-line bg-panel flex items-center justify-between"
            >
              <div>
                <div className="flex items-center gap-2 mb-1">
                  <span className="font-semibold text-sm text-ink">{cell.name}</span>
                  <span className="px-2 py-0.5 rounded text-xs font-mono bg-panel2 text-ink2 border border-line">
                    {cell.state}
                  </span>
                </div>
                <div className="text-xs text-ink3 font-mono">ID: {cell.id}</div>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

export default CellMonitor;
