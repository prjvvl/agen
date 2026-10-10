#!/usr/bin/env bash
# Install check (Linux/macOS): install a release tarball into a temp dir, agen
# up, run a task, agen down, uninstall --purge; fails if anything is left.
# Also checks the installer's safety rails (checksum, purge refusal).
set -euo pipefail
archive="${1:?usage: test-install.sh dist/agen_<v>_<os>_<arch>.tar.gz}"
here="$(cd "$(dirname "$0")" && pwd)"
base="$(mktemp -d)"; trap 'rm -rf "$base"' EXIT
export AGEN_INSTALL_DIR="$base/bin" AGEN_HOME="$base/home"
if sh "$here/install.sh" "$archive" --sha256 "$(printf '0%.0s' $(seq 64))" >/dev/null 2>&1; then echo "wrong checksum accepted"; exit 1; fi
[ ! -e "$AGEN_INSTALL_DIR/agen" ] || { echo "installed despite a wrong checksum"; exit 1; }
sh "$here/install.sh" "$archive" >/dev/null
agen="$AGEN_INSTALL_DIR/agen"
"$agen" up --listen 127.0.0.1:7395 --gateway-listen 127.0.0.1:0 >/dev/null 2>&1 &
for _ in $(seq 1 100); do [ -f "$AGEN_HOME/local.json" ] && break; sleep 0.3; done; sleep 2
(cd "$base" && "$agen" init >/dev/null && "$agen" deploy hello --replicas 2 >/dev/null)
[ "$("$agen" run hello hi)" = "Hello! Nice to meet you." ]
sleep 3
[ "$(pgrep -f "$AGEN_INSTALL_DIR/agen" | wc -l)" -ge 3 ] || { echo "expected agen + 2 hosts"; exit 1; }
"$agen" down >/dev/null; sleep 2
if pgrep -f "$AGEN_INSTALL_DIR/agen" >/dev/null; then echo "processes left after agen down"; exit 1; fi
# --purge refuses an AGEN_HOME that is not Agen's.
mkdir -p "$base/not-agen" && echo keep > "$base/not-agen/precious.txt"
if AGEN_HOME="$base/not-agen" sh "$here/install.sh" --uninstall --purge >/dev/null 2>&1; then echo "purge accepted a non-Agen dir"; exit 1; fi
[ -f "$base/not-agen/precious.txt" ] || { echo "purge deleted a non-Agen directory"; exit 1; }
sh "$here/install.sh" --uninstall --purge >/dev/null
[ ! -e "$AGEN_INSTALL_DIR" ] && [ ! -e "$AGEN_HOME" ] || { echo "files left"; exit 1; }
echo "install/up/down/uninstall OK"
