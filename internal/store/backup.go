package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Snapshot creates a consistent independent database, never a copy of live WAL files.
func (d *DB) Snapshot(ctx context.Context, path string) error {
	if _, err := d.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return err
	}
	copy, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer copy.Close()
	var result string
	if err := copy.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("snapshot integrity: %s", result)
	}
	return nil
}
