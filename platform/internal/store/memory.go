package store

import (
	"context"
	"database/sql"
	"errors"
)

// MemoryBlock is durable, named text an agent reads and updates across
// runs and conversations (x-agen/config.json "memory"), namespace-scoped.
type MemoryBlock struct {
	Namespace   string
	ID          string
	Label       string
	Description string
	Value       string
	// LimitChars: 0 means no limit.
	LimitChars int64
	// Version is bumped on every update; the engine's memory_update tool
	// checks it for compare-and-set.
	Version   int64
	UpdatedMs int64
}

const memoryBlockCols = "namespace, id, label, description, value, limit_chars, version, updated_ms"

func scanMemoryBlock(r interface{ Scan(...any) error }) (MemoryBlock, error) {
	var b MemoryBlock
	err := r.Scan(&b.Namespace, &b.ID, &b.Label, &b.Description, &b.Value, &b.LimitChars, &b.Version, &b.UpdatedMs)
	return b, err
}

// SetMemoryBlock replaces a block's value outright, creating it if it does
// not exist yet. Bumps the version.
func (s *Store) SetMemoryBlock(ctx context.Context, b MemoryBlock) (MemoryBlock, error) {
	now := NowMs()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO memory_blocks (namespace, id, label, description, value, limit_chars, version, updated_ms) VALUES ($1, $2, $3, $4, $5, $6, 1, $7) "+
			"ON CONFLICT (namespace, id) DO UPDATE SET label = excluded.label, description = excluded.description, "+
			"value = excluded.value, limit_chars = excluded.limit_chars, version = memory_blocks.version + 1, updated_ms = excluded.updated_ms",
		b.Namespace, b.ID, b.Label, b.Description, b.Value, b.LimitChars, now)
	if err != nil {
		return b, err
	}
	return s.GetMemoryBlock(ctx, b.Namespace, b.ID)
}

// GetMemoryBlock returns a block, or ErrNotFound if it has never been written.
func (s *Store) GetMemoryBlock(ctx context.Context, namespace, id string) (MemoryBlock, error) {
	b, err := scanMemoryBlock(s.db.QueryRowContext(ctx, "SELECT "+memoryBlockCols+" FROM memory_blocks WHERE namespace = $1 AND id = $2", namespace, id))
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

// ListMemoryBlocks lists a namespace's memory blocks, newest-updated first.
// "" lists every namespace's (an unrestricted caller with no namespace
// named).
func (s *Store) ListMemoryBlocks(ctx context.Context, namespace string) ([]MemoryBlock, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+memoryBlockCols+" FROM memory_blocks WHERE ($1 = '' OR namespace = $1) ORDER BY updated_ms DESC, id DESC", namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryBlock
	for rows.Next() {
		b, err := scanMemoryBlock(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
