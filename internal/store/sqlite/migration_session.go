package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3"
)

// Versions 2 and 3 were published independently by upstream and this fork.
// Goose records version numbers, not contents: a fork database may skip the
// source column in version 2 or the cache table in version 3. Reconcile both
// shapes in version 4 without rewriting published upstream SQL migrations.
func activitySessionMigration() *goose.Migration {
	return goose.NewGoMigration(4, &goose.GoFunc{RunTx: migrateActivitySession},
		&goose.GoFunc{RunTx: func(context.Context, *sql.Tx) error {
			return fmt.Errorf("activity session reconciliation cannot be downgraded; restore a pre-upgrade backup")
		}})
}

func migrateActivitySession(ctx context.Context, tx *sql.Tx) error {
	for _, column := range []string{"src", "session_id"} {
		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('activity') WHERE name = ?`, column).Scan(&count); err != nil {
			return fmt.Errorf("inspect activity column %s: %w", column, err)
		}
		if count == 0 {
			// Column names are from the fixed list above, never user input.
			if _, err := tx.ExecContext(ctx, `ALTER TABLE activity ADD COLUMN `+column+` TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("add activity column %s: %w", column, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_activity_src_created_id
		ON activity (src, ts_created DESC, id DESC)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cache (
		key TEXT PRIMARY KEY,
		data BLOB NOT NULL,
		ttl_seconds INTEGER NOT NULL DEFAULT 0,
		ts_created INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_cache_expiry ON cache (ts_created) WHERE ttl_seconds > 0;`)
	return err
}
