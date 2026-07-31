#!/usr/bin/env bash
#
# render-formula.sh — render the Homebrew formula for brewfast from the
# placeholder template and a SHA256SUMS file.
#
# Usage:
#   scripts/render-formula.sh <version> <sha256sums-path> <output-formula-path>
#
# Substitutes {{VERSION}}, {{TAG}} (= brewfast-v<version>), and the four
# per-arch checksum placeholders ({{DARWIN_ARM64_SHA256}}, {{DARWIN_X64_SHA256}},
# {{LINUX_ARM64_SHA256}}, {{LINUX_X64_SHA256}}) drawn from SHA256SUMS, whose
# entries are the reproducible tarballs named brewfast-<version>-<os>-<arch>.tar.gz.
#
# Fails loudly if a checksum is missing or if any {{...}} placeholder survives
# substitution — a half-rendered formula must never reach the tap.
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "Usage: $0 <version> <sha256sums-path> <output-formula-path>" >&2
  exit 2
fi

VERSION="$1"
SUMS_PATH="$2"
OUTPUT_PATH="$3"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE_PATH="${SCRIPT_DIR}/brewfast-formula.template.rb"

if [[ ! -f "$TEMPLATE_PATH" ]]; then
  echo "error: template not found at $TEMPLATE_PATH" >&2
  exit 1
fi
if [[ ! -f "$SUMS_PATH" ]]; then
  echo "error: SHA256SUMS not found at $SUMS_PATH" >&2
  exit 1
fi

# Validate the version looks like a semver (optionally with a pre-release tail),
# mirroring the renderer in the ccstack pipeline.
if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "error: invalid release version: $VERSION" >&2
  exit 1
fi

TAG="brewfast-v${VERSION}"

# Look up a checksum for a given tarball basename from SHA256SUMS. The file
# format is the standard `<64-hex-sha>  <filename>` produced by sha256sum.
sha_for() {
  local filename="$1" line hash name
  while IFS= read -r line; do
    [[ -z "${line// }" ]] && continue
    hash="${line%% *}"
    name="${line##* }"
    if [[ "$name" == "$filename" ]]; then
      if [[ ! "$hash" =~ ^[a-f0-9]{64}$ ]]; then
        echo "error: malformed sha256 for $filename in $SUMS_PATH: $hash" >&2
        exit 1
      fi
      printf '%s' "$hash"
      return 0
    fi
  done < "$SUMS_PATH"
  echo "error: missing checksum for $filename in $SUMS_PATH" >&2
  exit 1
}

DARWIN_ARM64_SHA256="$(sha_for "brewfast-${VERSION}-darwin-arm64.tar.gz")"
DARWIN_X64_SHA256="$(sha_for "brewfast-${VERSION}-darwin-x64.tar.gz")"
LINUX_ARM64_SHA256="$(sha_for "brewfast-${VERSION}-linux-arm64.tar.gz")"
LINUX_X64_SHA256="$(sha_for "brewfast-${VERSION}-linux-x64.tar.gz")"

rendered="$(cat "$TEMPLATE_PATH")"
rendered="${rendered//\{\{VERSION\}\}/$VERSION}"
rendered="${rendered//\{\{TAG\}\}/$TAG}"
rendered="${rendered//\{\{DARWIN_ARM64_SHA256\}\}/$DARWIN_ARM64_SHA256}"
rendered="${rendered//\{\{DARWIN_X64_SHA256\}\}/$DARWIN_X64_SHA256}"
rendered="${rendered//\{\{LINUX_ARM64_SHA256\}\}/$LINUX_ARM64_SHA256}"
rendered="${rendered//\{\{LINUX_X64_SHA256\}\}/$LINUX_X64_SHA256}"

if [[ "$rendered" == *'{{'* ]]; then
  echo "error: rendered formula contains an unresolved placeholder:" >&2
  printf '%s\n' "$rendered" | grep -n '{{' >&2 || true
  exit 1
fi

mkdir -p "$(dirname "$OUTPUT_PATH")"
printf '%s\n' "$rendered" > "$OUTPUT_PATH"
echo "Rendered $OUTPUT_PATH"
