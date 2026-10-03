## Description

What does this change, and why? Link the issue it closes with `Closes #N`.

## Checklist

Keep this list in the description — it structures the review.

- [ ] PR title is a Conventional Commit (`feat: …`, `fix(cell): …`, `docs: …`)
- [ ] `go vet ./...` clean
- [ ] `go build ./...` clean
- [ ] `go test -race ./...` green
- [ ] `gofmt -l` reports nothing on touched files
- [ ] Behavior matches the frozen specs in `docs/` (or a linked issue proposes the change)
- [ ] Tests added/updated for behavior changes
- [ ] User-visible changes documented

## AI assistance

Was substantial AI assistance used in writing this code? (yes/no — one line)
