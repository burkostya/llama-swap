package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3"
)

// Versions 2 through 4 were published independently by upstream and this fork.
// Reconcile both shapes at version 4 and again at version 6, because a database
// that already recorded either version 4 cannot rerun that migration.
func activitySessionMigration(version int64) *goose.Migration {
	return goose.NewGoMigration(version, &goose.GoFunc{RunTx: migrateActivitySession},
		&goose.GoFunc{RunTx: func(context.Context, *sql.Tx) error {
			return fmt.Errorf("activity session reconciliation cannot be downgraded; restore a pre-upgrade backup")
		}})
}

func migrateActivitySession(ctx context.Context, tx *sql.Tx) error {
	for _, col := range []struct{ name, definition string }{
		{"src", "TEXT NOT NULL DEFAULT ''"}, {"session_id", "TEXT NOT NULL DEFAULT ''"},
		{"task", "TEXT NOT NULL DEFAULT ''"}, {"route", "TEXT NOT NULL DEFAULT ''"}, {"peer_id", "TEXT NOT NULL DEFAULT ''"}, {"reasoning_tokens", "INTEGER NOT NULL DEFAULT -1"},
	} {
		column := col.name
		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('activity') WHERE name = ?`, column).Scan(&count); err != nil {
			return fmt.Errorf("inspect activity column %s: %w", column, err)
		}
		if count == 0 {
			// Column names are from the fixed list above, never user input.
			if _, err := tx.ExecContext(ctx, `ALTER TABLE activity ADD COLUMN `+column+` `+col.definition); err != nil {
				return fmt.Errorf("add activity column %s: %w", column, err)
			}
			if column == "task" {
				if _, err := tx.ExecContext(ctx, `UPDATE activity SET task = CASE req_path
    WHEN '/v1/chat/completions' THEN 'chat'
    WHEN '/v1/responses' THEN 'chat'
    WHEN '/v1/completions' THEN 'chat'
    WHEN '/v1/messages' THEN 'chat'
    WHEN '/completion' THEN 'chat'
    WHEN '/infill' THEN 'chat'
    WHEN '/v/chat/completions' THEN 'chat'
    WHEN '/v/responses' THEN 'chat'
    WHEN '/v/completions' THEN 'chat'
    WHEN '/v/messages' THEN 'chat'
    WHEN '/v1/embeddings' THEN 'embedding'
    WHEN '/v/embeddings' THEN 'embedding'
    WHEN '/rerank' THEN 'rerank'
    WHEN '/reranking' THEN 'rerank'
    WHEN '/v1/rerank' THEN 'rerank'
    WHEN '/v1/reranking' THEN 'rerank'
    WHEN '/v/rerank' THEN 'rerank'
    WHEN '/v/reranking' THEN 'rerank'
    WHEN '/v1/messages/count_tokens' THEN 'token_count'
    WHEN '/v/messages/count_tokens' THEN 'token_count'
    WHEN '/v1/images/generations' THEN 'image_generation'
    WHEN '/sdapi/v1/txt2img' THEN 'image_generation'
    WHEN '/v1/images/edits' THEN 'image_edit'
    WHEN '/sdapi/v1/img2img' THEN 'image_edit'
    WHEN '/v1/audio/speech' THEN 'speech'
    WHEN '/v1/audio/transcriptions' THEN 'transcription'
    WHEN '/v1/systemone' THEN 'decision'
    WHEN '/audioapi/v1/tasks/run' THEN 'audio'
    ELSE ''
END;`); err != nil {
					return fmt.Errorf("backfill task: %w", err)
				}
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
