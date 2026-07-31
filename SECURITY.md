# Security Policy

## Reporting a vulnerability

Please report security issues privately using GitHub's
[private vulnerability reporting](https://github.com/amitray007/brewfast/security/advisories/new)
rather than a public issue. Include the version, the affected command, and a
reproduction if you have one. We aim to respond within a few days.

Please do not open a public issue for a vulnerability until it has been addressed.

## Trust model — what brewfast does and does not guarantee

brewfast downloads a binary and hands it to an installer, so its trust model is
worth stating plainly:

- **Integrity, not authenticity.** brewfast verifies the downloaded file's SHA-256
  against the checksum in the Homebrew cask definition, and both the URL and the
  checksum come from that same cask. So a mismatch reliably catches a corrupt or
  truncated download, but the check provides no more authenticity than plain
  `brew` already does — brewfast inherits, and does not widen, brew's trust in the
  cask and the tap. This is the same trust model as Homebrew itself.
- **A checksum mismatch is always fatal.** The file is discarded and never
  installed, even under `--fallback`. Only `--no-verify` skips checking entirely,
  and it prints an unmissable warning naming the cask and URL.
- **HTTPS is required.** brewfast refuses to download an asset over a non-`https`
  URL and passes `--check-certificate=true` to `aria2c`.
- **`--no-verify` is a foot-gun by design.** It exists for casks that declare no
  checksum. Using it means you accept unverified bytes. Prefer `--fallback` (which
  hands off to plain `brew`) when you are unsure.
- **Cask names are validated.** brewfast validates a cask name against Homebrew's
  token grammar and uses a `--` argument terminator on every `brew`/`aria2c`
  invocation, so a crafted name cannot be parsed as a flag or injected as an
  argument.

If you believe any of these guarantees can be bypassed, that is a security issue —
please report it privately as above.

## Supported versions

brewfast is pre-1.0/early. Security fixes land on the latest release only.
