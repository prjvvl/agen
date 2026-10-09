#!/usr/bin/env sh
# Build the Agen C ABI static library the Go SDK links against.
# Windows (MinGW/cgo) needs the GNU target; Linux/macOS use the host target.
set -eu
cd "$(dirname "$0")/.."
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) cargo build -p agen-ffi --release --target x86_64-pc-windows-gnu ;;
  *) cargo build -p agen-ffi --release ;;
esac
