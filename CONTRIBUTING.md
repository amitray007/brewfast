# Contributing to brewfast

Thanks for your interest in improving brewfast. This is a small, focused tool —
contributions that keep it small and focused are the most welcome.

## What brewfast is (and isn't)

brewfast accelerates Homebrew **cask** downloads that come from throttled GitHub
release assets. It downloads with `aria2`, verifies the checksum against the cask
definition, places the file in brew's cache, and hands off to `brew`. brew stays
the source of truth.

Out of scope (see the plan in `docs/plans/` for the full rationale):

- Accelerating formula/bottle downloads — those already come CDN-fast from ghcr.io.
- A global `HOMEBREW_CURL_PATH` shim that intercepts every brew download. This is a
  deliberate non-goal: it fails silently and machine-wide.
- A TUI. brewfast is a CLI; the only interactivity is the cask-name picker.

If you want to propose something outside this scope, open an issue first so we can
talk about it before you write code.

## Development

Requirements: Go 1.26+, and a working Homebrew install for manual testing.

```sh
git clone https://github.com/amitray007/brewfast
cd brewfast
go build ./...
go test ./...
go vet ./...
gofmt -l .   # must print nothing
```

The codebase is deliberately structured for testability without a real brew,
aria2, or network: each package exposes its side-effecting calls behind injected
function fields or small interfaces, and the pure logic (checksum verify, host
detection, name validation, stuck-transaction detection) is separately testable.
Please keep that pattern when you add code.

## Pull requests

1. Branch from `main` (`feat/...`, `fix/...`, `chore/...`).
2. Add or update tests for any behavior change. Bug fixes ship with a regression
   test.
3. Run the full check set above; all of it must be green.
4. Use [Conventional Commits](https://www.conventionalcommits.org/) for your commit
   messages (`feat:`, `fix:`, `chore:`, `docs:`, `refactor:`) — releases and the
   changelog are generated from them by release-please.
5. Open the PR against `main`. CI runs build, vet, test, gofmt, and a binary smoke
   test.

## Safety-sensitive areas

Some parts of brewfast guard real failure modes. If you touch these, be extra
careful and explain your reasoning in the PR:

- **`internal/accel`** — the checksum verification. A mismatch must stay fatal.
  Never let unverified bytes reach the handoff.
- **`internal/handoff`** — the signal-guarded brew child. brew runs in its own
  process group so an interrupt to brewfast can't leave a half-finished install.
- **`internal/brew`** — cask-name validation and the `--` argument terminator that
  stop argument injection into `brew`/`aria2c`.

## Reporting bugs

Open an issue with your OS/arch, `brewfast --version`, the exact command, and the
output. `brewfast doctor` output is very helpful for install/cache problems.

## Releases

Maintainers only. Releases are cut by merging the release-please PR, which builds
cross-platform binaries and pushes the rendered formula to `amitray007/homebrew-tap`.
