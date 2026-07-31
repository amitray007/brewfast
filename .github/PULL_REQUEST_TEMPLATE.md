<!--
Thanks for contributing! A few reminders:
- Use a Conventional Commit title (feat: / fix: / chore: / docs: / refactor:).
  Releases and the changelog are generated from commit messages.
- Behavior changes need tests. Bug fixes ship with a regression test.
- If you touched internal/accel, internal/handoff, or internal/brew, explain the
  safety reasoning (see CONTRIBUTING.md → Safety-sensitive areas).
-->

## What this changes

<!-- One or two sentences on the behavior change. -->

## Why

<!-- The problem being solved. Link an issue if there is one. -->

## Checklist

- [ ] `go test ./...` passes
- [ ] `go vet ./...` and `gofmt -l .` are clean
- [ ] Tests added/updated for the change
- [ ] Scope stays within what brewfast is for (see CONTRIBUTING.md)
