import { Database, AlertCircle } from "lucide-react";

export function MemoryViewer() {
  return (
    <div className="flex-1 overflow-y-auto p-4 md:p-6 bg-bg text-ink max-w-4xl mx-auto w-full">
      <div className="flex items-center justify-between pb-4 mb-6 border-b border-line">
        <div className="flex items-center gap-3">
          <div className="p-2.5 rounded-xl bg-panel2 border border-line text-ember">
            <Database className="w-6 h-6" />
          </div>
          <div>
            <div className="flex items-center gap-2">
              <h2 className="text-lg font-bold text-ink">Memory & Session Log Inspector</h2>
              <span className="px-2 py-0.5 text-xs font-semibold rounded-full bg-warn/15 text-warn border border-warn/30">
                Preview — backend pending
              </span>
            </div>
            <p className="text-xs text-ink3">Inspect long-term memory records, vector embeddings, and compaction logs</p>
          </div>
        </div>
      </div>

      <div className="p-4 rounded-xl border border-warn/30 bg-warn/10 text-warn text-xs mb-6 flex items-start gap-2.5">
        <AlertCircle className="w-4 h-4 shrink-0 mt-0.5" />
        <div className="leading-relaxed">
          <strong>Preview — backend pending:</strong> No backend endpoint exists for direct memory inspection yet.
          When hearthd exposes memory search and compaction telemetry, indexed entities and historical recall logs will be available here.
        </div>
      </div>

      <div className="p-12 text-center rounded-2xl border border-dashed border-line bg-panel2/50 my-6">
        <Database className="w-10 h-10 text-ink3 mx-auto mb-3 opacity-60" />
        <h3 className="text-sm font-semibold text-ink mb-1">Memory Inspection Pending</h3>
        <p className="text-xs text-ink3 max-w-md mx-auto leading-relaxed">
          Long-term memory indexing and memory search endpoints are not yet exposed by the hearthd surface.
        </p>
      </div>
    </div>
  );
}

export default MemoryViewer;
