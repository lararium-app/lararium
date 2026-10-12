## 4.5 Whole-hearth backup & restore (amendment A4, PO sign-off 2026-10-09)

Transcribed verbatim from ratified draft rev 4 (four hostile review
rounds, converged 0 blockers). Draft-internal § refs renumbered to this
section (§2→§4.5.2, §3.x→§4.5.3.x); cross-spec refs qualified by spec
name; fold-maps and the scope table stay with the draft archive. The
V10.1 scoping sentence lives in CUSTOS-SPEC §4.2; the CLI shapes live in
SURFACE-SPEC §3A; the import-precedence sentence sits above in §4.

### 4.5.1 The contract (one sentence)

**A backup captures the whole hearth root — including its config — in
one file, and restore turns that file back into a running daemon with at
most one documented operator step (installing the config file on a new
machine via `extract-config`).** Disk death should cost the transcript
between the last backup and the crash — nothing else. Partial capture
stays the job of §4 export and KEYS-SPEC tooling; backup exists for
completeness + consistency + one-file recovery.

### 4.5.2 Bundle format `backup/1` (A4.1)

`<name>.lararium-backup` — ZIP (deflate), written mode 0600.

```
manifest.json          # strictly the FIRST zip entry
tree/…                 # hearth root contents, verbatim, minus exclusions
config/lararium.yaml   # config in force (bundled by default; --no-config opts out)
```

**manifest.json:**

```json
{ "format": "backup/1.0",
  "created": "2026-10-09T18:00:00Z",
  "product": "hearthd/0.7.3",
  "source": { "abs_path": "…", "hostname": "…" },
  "entries": [
    {"path": "tree/sessions/main/events.jsonl",
     "sha256": "…", "size": 1234, "mode": "0644"},
    {"path": "tree/archive/2026-09/", "dir": true, "mode": "0755"}
  ],
  "counts": { "sessions": 12, "memory_files": 34 } }
```

- `format` is a string `"backup/<major>.<minor>"` (rev-2 F10: an integer
  cannot carry minors). Parsers must tolerate unknown minor/fields.
- `entries` covers **every** zip member except `manifest.json`. Regular
  files: `sha256` + `size` + `mode`. Directories (incl. empty): `dir:
  true` + `mode`, no hash/size. No entry may appear in zip without being
  in the manifest, and vice versa (verify enforces both directions).
- `counts` reports only what a locked daemon can compute without
  unlocking (rev-2 F17): `sessions` = dirs under `tree/sessions/`,
  `memory_files` = `tree/memory/**/*.md` — recursive (the frozen §1
  tree puts memory in `people/ projects/ notes/`; rev-3 F5). No
  vault-derived numbers.
- `secrets` is **derived at verify/display time** from bundle-path
  patterns `tree/keys.json`, `tree/tokens.json`, `tree/*.token`,
  `tree/custos/vault.*`, `tree/custos/surrogates.age`,
  `tree/custos/fingerprints.json`, `tree/custos/snapshots/**`,
  `config/lararium.yaml` — patterns are law, matched against full
  bundle paths (rev-3 F7); never hand-listed in the manifest
  (rev-2 F13).
- No `hearth_id`: provenance = `created` + `source`.

**Inclusion law.** Included: persona files, `sessions/` (blobs too —
`--lean` is gone), `memory/`, `archive/`, `keys.json`, `tokens.json`,
`custos/` **including `snapshots/` in full** (DR restore keeps CUSTOS-SPEC §8.4
rollback history), `nuntius/`. Excluded, always: `memory/index.db`
(rule 5), `*.sock`, `*.lock`. Exclusion-list changes are format-minor.

**Tree-shape law.** Regular files + directories only. Symlinks,
hardlinks, devices, FIFOs anywhere in the root → backup **refuses**,
listing offenders. **Every** directory (empty or not) gets a zip dir
entry + manifest entry with its mode — modes round-trip for the whole
tree (rev-3 F14). Empty dirs too; tested (BK13). Paths stored
NFC-normalized (tested, BK14). On extraction, recorded modes are masked
`& 0777` **after stripping setuid/setgid** and are never re-joined with
the process umask (BK13 asserts a source `04755` restores as `0755`;
bundle-embedded privilege bits are not honored). Manifest and zip paths
are relative, `/`-separated; extraction maps `tree/<p>` → `<dir>/<p>`,
never extracts `manifest.json` or `config/` into the root (config only
via `extract-config`), rejects any entry whose normalized path escapes
`<dir>` (`..`, absolute, drive-shaped) or is not listed in the manifest
— refuse the whole bundle (zip-slip law; BK18).

**Verify law.** `hearthd backup verify <file>`: (a) every manifest entry
hashes/sizes/matches, both directions; (b) `format` major == 1; (c)
parse every `tree/sessions/*/events.jsonl`: findings carry a class.
`tamper` = mismatch against the manifest (hash, size, membership).
`quarantine` = logical damage the bytes themselves record — seq
gap/duplicate, or unparseable lines in a file that nonetheless matches
its hash (damage predates the backup; the frozen §2 rule quarantines
and continues past exactly this). Content-level damage can never be
tamper (a tampered byte breaks the hash); manifest mismatch can never
be quarantine (rev-3 F6 pinned). Output schema: header
`#columns: class path detail`, one row per finding, exit 0 iff no rows;
findings go to **stdout** and nothing else does — the derived-secrets
list and the disk-safety warning print to **stderr** so stdout stays
machine-parseable (rev-4 F4). **Restore classifies**: `tamper` → refuse;
`quarantine` → warn loudly, proceed — a DR tool must be able to restore
a sick hearth exactly as it was sick (rev-2 F6).

**Secrets honesty.** The bundle contains every secret in on-disk form
(`vault.key` plaintext = live-disk exposure; `keys.json` plaintext is
frozen KEYS-SPEC design). create/verify/`--help` print: *a bundle is
your disk — store it where your disk would be unsafe.*


### 4.5.3 Consistency: taking a bundle while things run

1. **Daemon running (common case).** `hearthd backup create --out <p>`:
   the CLI resolves `--out` to an absolute path and invokes the control
   socket verb `backup <out_abs> [--no-config]` (S10.1 protocol amended:
   single request line, ack, progress lines, done/error; rev-4 F3: the
   flag travels as part of the request). The daemon: acquires flocks
   (§4.5.3.3) **and** its single-writer lock (the P3 serialization point);
   performs a fast local **copy of the included tree into a staging dir
   (0700) on the hearth root's own filesystem** (rev-2 F8: staging never
   rides `--out`'s filesystem — lock hold time is local-disk copy only);
   releases all locks; then compresses staging → temp file next to
   `<out_abs>` (parent-fsync'd, cross-device copy fine) → renames to
   `<out_abs>` (mode 0600) → cleans staging. Free-space precondition
   (rev-3 F8, rev-4 F2): before any lock — let `Σ` = Σ(tree size), the
   declared worst case (no compression estimate, no margin math). Root
   fs needs ≥ `Σ` for staging; `--out` fs needs ≥ `Σ` for the output.
   **If both live on one filesystem** (`statfs.f_fsid` equal), require
   ≥ `2Σ` — staging and output coexist there before cleanup. Short →
   refuse with the numbers and the rule printed (BK12).
   Quiesced during copy: turns, heartbeat, memory writes, SSE
   persistence (single-writer lock) + custos mutations + CLI `keys
   set/rm` (flocks). A turn in flight lands **whole or not at all**
   (deltas never persist, frozen §2); `tokens.json` rotation lands whole
   before or whole after (whole-file writer).
   **Writer inventory (informative — cannot be normative):** today's
   writers — events (`appendLine`), memory (temp+rename), keys/tokens
   (`writePrivate`), custody state root (flock protocol) — are all
   whole-file, safe to copy under the locks. "Every writer quiesces
   under lock L" holds only by discipline: any future hearth-root writer
   outside an existing lock inherits this inventory duty (same class of
   law as P3 ownership and the CUSTOS-SPEC §4.2 call-site enumeration).
2. **Daemon stopped.** CLI runs the identical walk after acquiring the
   same flocks directly. If `hearthd.sock` exists but does not answer a
   ping, the CLI **refuses** with the exact operator step ("confirm the
   daemon process is dead, remove `hearthd.sock`, retry") — rev-2 F7: a
   live-but-slow daemon must never be walked without the in-process
   lock; a stale socket is a one-command operator fix, and disaster
   recovery stays fully operator-reachable.
3. **Lock law.** Acquire in order: `backup.lock`, `custos.lock`,
   `keys.lock` (flock). For **create**: all non-blocking (`LOCK_NB`) —
   any holder → immediate busy refusal, exit non-zero (rev-2 F16: no
   10 s blocking, so "second backup refuses" is observable); within a
   restore the same, and ordering prevents deadlock because no other
   lock-taker in the system holds more than one of the three.
   `backup.lock` (the A4.0 tree addition) serializes every backup AND
   every `--replace` restore, running or stopped. Timeouts/errors:
   non-zero exit; staging + temp files removed. Because every custos
   mutation holds `custos.lock` across its whole protocol (CUSTOS-SPEC §4.2) and the
   walk holds it across the **whole tree**, vault+MAC+surrogates — and
   their relation to `keys.json` — are one coherent instant.
4. **Output law.** `--out <p>`; default `./hearth-<UTC ts>.lararium-backup`
   (cwd). An `--out` inside the hearth root → refuse (self-inclusion).


### 4.5.4 Restore (A4.2, S10.1, V10.1)

```
hearthd backup create [--out <path>] [--no-config]
hearthd backup list <file>          # TSV: path<TAB>size<TAB>sha256<TAB>mode (dirs: size/sha256 = "-")
hearthd backup verify <file>
hearthd backup extract-config <file> <path>
hearthd restore <file> --to <dir>             # fresh root; <dir> must not exist
hearthd restore <file> --replace <dir> --yes [--no-safety]
```

Stdout data / stderr errors / exit per frozen conventions. `list` emits
`#columns: path size sha256 mode` then one row per manifest entry
(rev-2 F11: schema frozen here). `extract-config` writes the bundled
yaml **mode 0600**, refuses an existing `<path>`, and errors clearly on
`--no-config` bundles (rev-2 F19). **No REST endpoint, no web verb, ever
in v1** — stated so future PRs must amend, not creep.

Restore law:

1. **Verify first, always** — full §4.5.2 verify (DR classification: tamper
   → refuse, quarantine → warn+proceed). Version law on `format`: major
   unknown → refuse with migration pointer; major known, minor newer →
   proceed, drop unknown manifest fields, report each drop (frozen §4
   rule 2 honored — rev-1 blocker); `product` newer than binary → warn.
2. **`--to` (fresh root).** No lock on the target (none exists yet);
   mutual exclusion is structural: stage into unique sibling
   `<dir>.staging-<ts>` (0700), fill, fsync, then single
   `rename(staging → dir)` — POSIX refuses to rename onto an existing
   non-empty dir, so a race loses cleanly. Refuse if `<dir>` exists.
3. **`--replace`.** Requires `--yes`; refuses while the target's
   `hearthd.sock` answers ping. **Safety bundle law:** by default the
   CLI first bundles the existing root →
   `<dir>.pre-restore-<ts>.lararium-backup`. If that safety bundle
   **fails** (source too damaged — the disaster case), the CLI does not
   abandon the operator: it prints the failure reason and the exact
   retry command, `restore … --no-safety`, which proceeds with no
   safety copy (rev-3 F10 reworded: `--no-safety` exists precisely so a
   damaged source cannot lock out recovery; the two-step is a
   confirmation, not a block). Free-space check before any lock:
   staged tree + (safety bundle if requested) — renames cost no space,
   old tree is never double-counted (rev-2 F18).
4. **Swap protocol (`--replace`).** Acquire all three flocks of §4.5.3.3
   (`backup.lock` first; create-and-open semantics — a root that never
   ran a backup gets `backup.lock` created `O_CREAT` 0600; rev-2 F4 /
   rev-3 F4: the swap moves custody + keys state too, so it takes every
   lock a writer could hold, LOCK_NB). **Swap marker:** staging is
   filled and fsynced, then the CLI writes a **parent-directory
   dotfile** `.restore-<ts>.json` recording
   `{staging, pre_swap, target, ts, safety}` and fsyncs the parent;
   rename-1 (`dir → pre_swap`); rename-2 (`staging → dir`); fsync
   parent; **then delete the marker** — last visible act. A marker
   existing IS the interrupt window; no marker means no swap is or was
   mid-flight here, so leftover `.pre-swap-*`/`.staging-*` siblings are
   **inert forever** (rev-3 F1/F2: sibling heuristics clobbered fresh
   `--to` restores and locked out DR). A markerless staging dir is
   garbage by construction (contents complete ⇒ marker would exist) —
   CLI may report it, never resumes it. Recovery table (evaluated by
   every `hearthd` / `hearthd restore` invocation against its own root
   path before doing anything — one glob over `.restore-*.json`):

   | state found | meaning | action |
   |---|---|---|
   | no marker | nothing interrupted | normal operation; siblings ignored |
   | marker, `pre_swap` absent (whether or not `dir` and staging exist) | died before rename-1 — swap never touched the root (pre_swap's absence PROVES rename-1 never ran, so row order can never misfire: this row matches exactly the pre-rename-1 world, where `dir` + staging legitimately coexist; rev-4 F1) | delete staging + marker, proceed normally (staging holds bundle bytes only; deleting the bundle's copy is safe because a complete bundle never exists on this filesystem anyway — self-inclusion is refused, §4.5.3.4) |
   | marker, `dir` absent, staging + `pre_swap` present | died in the rename window | **complete**: rename staging→dir, fsync parent, report, delete marker |
   | marker, `dir` present, staging absent | died after rename-2, before marker delete | delete marker; done (pre_swap remains as pre-restore root) |
   | marker, `dir` + staging present, `pre_swap` present | impossible — pre_swap + dir together means rename-1 was undone by an operator; staging present means rename-2 hadn't run | refuse, print marker contents, print `rm <marker>` escape hatch |
   | marker, `dir` absent, staging absent (`pre_swap` either) | operator deleted staging mid-flight, or crash before staging completed — no claimable payload exists | refuse, print marker contents + `rm <marker>` escape; never resurrect silently (rev-4 F9: stated explicitly) |

   After a completed swap the pre-swap sibling **is** the pre-restore
   root: CLI prints its path and leaves deletion to the operator (v1 —
   silent auto-deletion of an old tree is banned).
   **Lock-handoff note (verified):** flock follows the *inode*, so
   across rename-1 the restore keeps its lock on the moved-away
   `backup.lock`. Safe by construction: all tree mutation finishes
   before rename-1; whatever opens the new root's `backup.lock`
   afterwards cannot overlap it. Implementers must not copy the lock or
   hold two fds. (BK8 kill-tests every row.)
5. Restored files get recorded modes; restored root dir 0700.
6. **First boot after restore** is a normal boot: `index.db` rebuilt
   (rule 5); custos first-unlock recovery (CUSTOS-SPEC §8.1a) runs blind against
   restored state — the bundle's vault/MAC/surrogates/WAL are one
   coherent generation set (§4.5.3.3), so recovery is a no-op or ordinary
   dangling-intent closure, exactly as on a machine that merely crashed.

### 4.5.5 PO decisions

- **D1 — CLI home. DECIDED (rev-4 F7 status aligned with the amendment’s §0/§2 law):**
  new `SURFACE-SPEC §3A`; the `--help` registry line lands with
  implementation.
- **D2 — extension. DECIDED:** `.lararium-backup` + content type
  `application/vnd.lararium.backup+zip` registered in this spec.
- **D3 — encryption at rest — DECIDED:** v1 plaintext parity; age
  wrapper = future amendment.
- **D4 — config — DECIDED:** bundled by default (keys.json already
  ships; one-step DR wins); never auto-installed outside `<dir>`;
  `extract-config` is the documented step.
- **D5 — snapshots — DECIDED:** included in full (restore keeps CUSTOS-SPEC §8.4
  history; size is KBs).
- **D6 — reminders. DECIDED:** no v1 scheduler and no new endpoint —
  a heartbeat standing order makes the *agent* nag the operator to run
  the CLI manually (text prompt only; the agent never triggers backups,
  rev-3 F12). Scheduled native backups: deferred.


### 4.5.6 Verification suite (every §4.5.2–§4.5.4 claim → ≥1 test)

- **BK1** create→verify→`--to`→boot→old session answers + §5 catch-up.
- **BK2** Exclusions/inclusions by name: `index.db`/`*.sock`/`*.lock`
  absent; snapshots + config present by default; `--no-config` drops
  config; `manifest.json` is zip entry index 0; verify's secret-
  derivation display lists every §4.5.2 pattern match (keys.json,
  vault.*, snapshots/**, config) and nothing else (rev-3 F9).
- **BK3** Tamper: byte flip in member / in manifest / extra zip entry /
  missing manifest entry → verify non-zero; restore refuses. Seq gap +
  seq duplicate fixtures → verify non-zero **with class `quarantine`**;
  restore **proceeds with warning** on those two (and refuses tamper) —
  tests the §4.5.2/§4.5.4 classification boundary both ways.
- **BK4** Mid-turn: long turn + socket backup → restored root: every
  events.jsonl seq-contiguous; in-flight turn whole-or-absent; all
  pre-backup turns present.
- **BK5** Custody coherence: vault-mutation hammer + backup loop → every
  bundle is one generation (vault+MAC+surrogates+registry parse
  together); held-lock case → busy refusal, no leftovers.
- **BK6** Refusals: `--to` existing; `--replace` live daemon; `--replace`
  w/o `--yes`; `--out` in root; symlink/device in tree (listed);
  corrupt source + `--replace` → refuses until `--no-safety`; socket
  present-but-dead → refuses with operator-step text.
- **BK7** Version law: `backup/2.0` → refuse + pointer; `backup/1.9`
  with unknown fields → proceeds + reports drops; newer `product` →
  warn, proceed.
- **BK8** Swap kill matrix: SIGKILL at each step — pre-stage,
  **post-marker/pre-rename-1** (rev-4 F8: the row-2 auto-clean state),
  post-rename-1, post-rename-2, post-completion/pre-marker-delete →
  crash-point table yields exactly one complete root (old or new) +
  safety bundle bootable; each table row exercised, including "operator
  deleted dir after success → refuses".
- **BK9** Concurrency: second create refuses **immediately** (LOCK_NB),
  socket path AND stopped path; `--replace` during backup → immediate
  refusal.
- **BK10** `backup list` TSV: exact `#columns` line, dirs carry `-`,
  round-trip parse.
- **BK11** Post-restore first boot: index rebuilt; CUSTOS-SPEC §8.1a clean;
  `audit verify` green; snapshots listable.
- **BK12** Free-space preflight (rev-3 F8): `--out` fs artificially
  full → create refuses before any lock or partial temp exists;
  `--replace` on space-short fs → refuses pre-lock (the rev-2 BK18
  space-refusal scenario folded here; suite renumbered, rev-3 F13,
  rev-4 F6).
- **BK13** Modes + shape: restored `keys.json`/`vault.key` 0600, dirs
  round-trip recorded modes, root 0700, bundle 0600, staging 0700 while
  alive; source `04755` file restores `0755` (setuid stripped, rev-3
  F11); **every directory — empty included — byte-identical mode after
  restore**; dir manifest entries have no sha/size and verify passes.
- **BK15** Counts (rev-4 F5): a root with 3 sessions and `memory/`
  holding `MEMORY.md` + `projects/notes.md` + a nested non-`.md` file
  → manifest `counts.sessions == 3`, `counts.memory_files == 2` (glob is
  recursive, extension-filtered); counts survive round-trip; the
  offline-path and daemon-path counts agree on the same root.
- **BK14** NFC: a source path in NFD form is stored NFC; verify passes
  on the bundle; restored root path is NFC.
- **BK18** Zip-slip: crafted bundles (`../x`, `/x`, `a/../../x`, drive
  style, unlisted entry) → verify AND restore refuse; target root
  subtree unchanged after refusal.
- **BK16** extract-config: writes 0600 yaml, refuses existing path,
  clear error on `--no-config` bundle; fresh-root restore stdout
  contains the exact extract-config command.
- **BK17** Offline stopped-daemon: persona files, `memory/`, `archive/`,
  `nuntius/` byte-identical after round-trip.

Suite in `cmd/hearthd`, temp roots, stub router, no model calls.


### 4.5.7 Implementation sketch (sizing only)

`internal/backup/` (manifest/writer/reader/verify+classification, lock
law, staging copy, swap + crash-point table) · control-socket `backup`
verb · `cmd/hearthd` verbs · BK suite. Two slices: (1) bundle core +
create + verify, (2) restore + swap + kill matrix. Custos daemon: zero
code (V10.1 = docs edit).

