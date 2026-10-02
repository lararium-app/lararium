# Contributing to Lararium

Thanks for pulling up a chair. This document is the short version of how
contributions work here.

## Ground rules

1. **Bugs and ideas → issues.** Use the issue templates. Security issues
   go to the private channel in [SECURITY.md](SECURITY.md), never to
   issues.
2. **Specs before code.** Components whose behavior is frozen by a spec
   in `docs/` (PENATUS-SPEC, CELL-SPEC) must match the spec exactly. If
   you believe a spec is wrong, open an issue to change the spec *first* —
   code that silently diverges from a frozen spec will be closed, not
   merged.
3. **One concern per PR.** Conventional Commit titles
   (`feat: …`, `fix(cell): …`, `docs: …`). The PR title becomes the merge
   commit subject.
4. **CI must be green:** `go vet ./... && go build ./... && go test -race
   ./...`, and `gofmt -l` must report nothing on the files you touched.

## Development setup

- Go 1.24+ (the version in `go.mod` is authoritative)
- Linux for anything touching `internal/cell` (systemd-nspawn, cgroup2);
  everything else runs anywhere Go runs
- No secrets, endpoints, or personal infrastructure in tests — unit tests
  run hermetically with fake runners; anything needing a real hypervisor-
  class kernel is gated behind the cage suite and clearly marked

## Pull request checklist

Please keep the checklist in your PR description (deleting it slows triage):

- [ ] Conventional Commit title
- [ ] `go vet ./... && go test -race ./...` green
- [ ] `gofmt -l` clean on touched files
- [ ] Behavior matches (or an issue proposes changing) the frozen specs
- [ ] New behavior has a test; fixes have a regression test
- [ ] Docs updated where user-visible behavior changed

## AI-assisted contributions

You may use AI tools while contributing. What you submit, you own — in the
responsibility sense:

- You must be able to explain every line you propose, and defend it in
  review.
- Do not paste generated code you have not read.
- Disclose substantial AI assistance in the PR description (one line is
  fine). PR descriptions themselves should be written by a human.

Fabricated content — invented APIs, invented test results, invented
hardware behavior — is the fastest path to a closed PR. We verify.

## Style

- `gofmt` / `goimports`, `editorconfig` respected (see `.editorconfig`)
- Prefer the standard library; every dependency needs a reason stated in
  the PR
- Error strings: lowercase, no trailing punctuation, wrap with `%w` to
  keep the chain

## License

By contributing you confirm the contribution is licensed under the
project's AGPL-3.0 license (inbound = outbound). Commercial dual-licensing
of the project as a whole is reserved to the project owner.
