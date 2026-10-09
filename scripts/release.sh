#!/usr/bin/env bash
# Builds a release archive for the host platform (or GOOS/GOARCH + a Rust
# TARGET for cross builds): dist/agen_<version>_<os>_<arch>.{tar.gz,zip}
# containing agen (with the web UI) and agen-host.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
version="${VERSION:-$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo dev)}"
goos="${GOOS:-$(go env GOOS)}"; goarch="${GOARCH:-$(go env GOARCH)}"
exe=""; [ "$goos" = windows ] && exe=".exe"
out="$root/dist/agen_${version}_${goos}_${goarch}"
rm -rf "$out" && mkdir -p "$out"

"$root/scripts/build-web.sh"
(cd "$root/platform" && GOWORK=off CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X github.com/prjvvl/agen/platform/internal/version.Version=$version" \
  -o "$out/agen$exe" ./cmd/agen)
if [ -n "${TARGET:-}" ]; then
  (cd "$root" && cargo build --release -p agen-host --target "$TARGET")
  cp "$root/target/$TARGET/release/agen-host$exe" "$out/"
else
  (cd "$root" && cargo build --release -p agen-host)
  cp "$root/target/release/agen-host$exe" "$out/"
fi
cp "$root/LICENSE" "$root/README.md" "$out/"
cd "$root/dist"
name="$(basename "$out")"
if [ "$goos" = windows ]; then
  archive="$name.zip"
  (command -v zip >/dev/null && zip -qr "$archive" "$name") || powershell -NoProfile -Command "Compress-Archive -Force -Path '$name' -DestinationPath '$archive'"
else
  archive="$name.tar.gz"
  tar -czf "$archive" "$name"
fi
# <archive>.sha256 ("<hex>  <file>"): install.sh / install.ps1 verify it.
if command -v sha256sum >/dev/null; then sha256sum "$archive" > "$archive.sha256"; else shasum -a 256 "$archive" > "$archive.sha256"; fi
# Relative to the repo root, so the path works for any shell (Git Bash's
# /c/... paths do not work in PowerShell).
echo "dist/$archive"
