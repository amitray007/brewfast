# brewfast

**brewfast accelerates Homebrew cask installs whose download comes from a
throttled GitHub release asset.** GitHub throttles unauthenticated release-asset
downloads to a fraction of your line speed, so a large `.dmg` or `.pkg` behind a
`github.com/.../releases/download/...` URL can crawl for minutes. brewfast fetches
that asset with aria2's parallel connections, verifies its checksum against the
cask definition, drops the file into brew's download cache, and then hands off to
`brew` for the real install.

brew stays the source of truth. brewfast is a pre-fetch optimizer that sits in
front of it and never wedges it: if a cask isn't a slow GitHub-asset download,
brewfast steps aside and lets brew do its normal thing.

## Install

```sh
brew install amitray007/tap/brewfast
```

Requires [`aria2`](https://aria2.github.io/) for the parallel download path
(`brew install aria2`) and a working Homebrew install. Runs on macOS and Linux
(arm64 and x64); the slow-cask problem is macOS-centric in practice.

## Usage

Install (or upgrade) a cask through the fast path — the default command:

```sh
brewfast <cask>          # e.g. brewfast orpheus
```

Check whether a cask can be accelerated, without touching anything:

```sh
brewfast check <cask>    # read-only: accelerate-able / already-fast / no-checksum
```

Other commands:

```sh
brewfast doctor          # diagnose deps, wedged transactions, orphaned cache files (read-only)
brewfast upgrade         # self-update brewfast via its tap
brewfast --version
brewfast --help
```

### Key flags

All flags are global (they work on the default install command and subcommands):

| Flag | Effect |
|---|---|
| `--fallback` | Hand off to plain `brew` on any non-fatal snag instead of stopping. |
| `--any-host` | Accelerate downloads from hosts other than the recognized slow GitHub-asset hosts. |
| `--no-verify` | Skip checksum verification entirely (accepts the risk). |
| `--force` | Attempt the fast path even in cases the default would refuse. |
| `--yes` / `--no-input` | Never prompt; take the non-interactive path even in a terminal. |
| `--quiet` | Suppress the success/status line for scriptable consumers. |

## How it works

1. **Resolve.** brewfast asks brew for the cask's download URL and checksum
   (`brew info --json=v2 --cask <cask>`).
2. **Classify.** If the URL is a throttled GitHub release asset with a checksum,
   it's a candidate; otherwise brewfast steps aside (already-CDN-fast casks gain
   nothing, and it won't accelerate an unknown host unless you pass `--any-host`).
3. **Fetch in parallel.** It downloads the asset with aria2's multiple
   connections into brew's own download cache, using the exact cache path and
   filename brew expects.
4. **Verify.** The downloaded file's sha256 is checked against the cask's
   declared checksum (unless `--no-verify`), so integrity matches what brew would
   have enforced.
5. **Hand off.** brewfast invokes `brew` for the actual install. Because the
   asset is already in brew's cache and passes its checksum, brew skips its own
   slow download and proceeds straight to installing.

If anything unexpected happens along the way, `--fallback` lets brewfast defer to
plain `brew` rather than block the install.

## Distribution

brewfast is distributed through
[`amitray007/homebrew-tap`](https://github.com/amitray007/homebrew-tap) as a
Homebrew formula. Multi-arch tarballs and their `SHA256SUMS` are published to the
tap's GitHub Releases by this repo's release pipeline, and `Formula/brewfast.rb`
is auto-rendered and pushed to the tap on each release — it is generated, never
hand-edited. Do not open PRs or issues against the tap.

## License

See the repository for license details.
