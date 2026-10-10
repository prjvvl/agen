#!/usr/bin/env sh
# Regenerate code from spec/proto. CI runs this and fails if the tree changes.
set -eu
cd "$(dirname "$0")/.."
(cd spec/proto && buf lint && buf generate)
buf build spec/proto -o platform/internal/apidesc/agen.binpb
# Go cannot embed files outside its module: copy the SQL migrations in.
rm -rf platform/internal/store/migrations
mkdir -p platform/internal/store/migrations
cp -r spec/sql/sqlite spec/sql/postgres platform/internal/store/migrations/
# Bundle JSON Schemas for the Hub's bundle validation.
rm -rf platform/internal/bundle/schemas
mkdir -p platform/internal/bundle/schemas
cp spec/bundle/*.schema.json platform/internal/bundle/schemas/
# Example bundles for `agen init`.
rm -rf platform/internal/cli/examples
cp -r examples/bundles platform/internal/cli/examples
