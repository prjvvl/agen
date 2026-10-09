# Store schema

One logical schema, one migration file per dialect per version. Both the Rust
engine and the Go platform embed these files and apply any not yet recorded in
`schema_migrations`, holding a lock (Postgres advisory lock / SQLite
`BEGIN IMMEDIATE`). Never edit an applied migration; add a new version.

Conventions: IDs are ULID text, times are unix milliseconds (`*_ms`), JSON is
stored as text.
