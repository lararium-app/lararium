// Package penatus implements the file-layer contract for Lararium hearth roots.
//
// Spec: docs/PENATUS-SPEC.md v0.1
//
// Sub-packages cover:
//
// §1 — Persona markdown files (frontmatter parsing, memory lines)
// §2 — Session transcripts (JSONL events, append-only log, tombstones)
// §3 — Compaction events (assembly, transitive resolution)
// §4 — Import/export helpers (session creation, ID generation)
package penatus
