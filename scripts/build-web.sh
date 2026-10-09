#!/usr/bin/env bash
# Builds the web UI into platform/internal/ui/dist (embedded by the Hub).
set -euo pipefail
cd "$(dirname "$0")/../web"
[ -d node_modules ] || npm ci --no-audit --no-fund
npm run build
touch ../platform/internal/ui/dist/.keep
