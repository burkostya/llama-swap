package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/mostlygeek/llama-swap/internal/store"

	"github.com/pressly/goose/v3"
)

func TestStore_MigrateActivitySessionAcrossForkAndUpstream(t *testing.T) {
	for _, shape := range []string{"empty", "base", "fork", "upstream"} {
		t.Run(shape, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "activity.sqlite")
			wantSession, wantSource := "", ""
			if shape != "empty" {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
				base, err := migrationFS.ReadFile("migrations/00001_create_activity.sql")
				if err != nil {
					t.Fatal(err)
				}
				files := fstest.MapFS{"00001_create_activity.sql": &fstest.MapFile{Data: base}}
				switch shape {
				case "fork":
					files["00002_add_activity_session_id.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nALTER TABLE activity ADD COLUMN session_id TEXT NOT NULL DEFAULT '';\n-- +goose Down\nALTER TABLE activity DROP COLUMN session_id;\n")}
					wantSession = "existing-session"
				case "upstream":
					source, err := migrationFS.ReadFile("migrations/00002_add_activity_source.sql")
					if err != nil {
						t.Fatal(err)
					}
					files["00002_add_activity_source.sql"] = &fstest.MapFile{Data: source}
					wantSource = "tailcat:existing-peer"
				}
				provider, err := goose.NewProvider(goose.DialectSQLite3, db, files)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := provider.Up(t.Context()); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO activity (ts_created, model_id, metadata_json) VALUES (1, 'existing', '{"keep":"value"}')`); err != nil {
					t.Fatal(err)
				}
				if shape == "fork" {
					if _, err := db.Exec(`UPDATE activity SET session_id = ?`, wantSession); err != nil {
						t.Fatal(err)
					}
				}
				if shape == "upstream" {
					if _, err := db.Exec(`UPDATE activity SET src = ?`, wantSource); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}

			for attempt := 0; attempt < 2; attempt++ {
				st, err := New(path)
				if err != nil {
					t.Fatalf("open attempt %d: %v", attempt, err)
				}
				t.Cleanup(func() { st.Close() })
				if attempt == 0 {
					if _, err := st.Activity().Insert(t.Context(), store.ActivityLogEntry{Model: "new", SessionID: "new-session", Src: "new-source"}); err != nil {
						t.Fatal(err)
					}
				}
				page, err := st.Activity().List(t.Context(), store.ActivityQuery{Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
				wantRows := 2
				if shape == "empty" {
					wantRows = 1
				}
				if len(page.Data) != wantRows {
					t.Fatalf("rows = %d, want %d", len(page.Data), wantRows)
				}
				for _, row := range page.Data {
					if row.Model == "existing" {
						if row.SessionID != wantSession || row.Src != wantSource || row.Metadata["keep"] != "value" || row.ID != 1 {
							t.Fatalf("existing row changed: %+v", row)
						}
					} else if row.Model != "new" || row.SessionID != "new-session" || row.Src != "new-source" {
						t.Fatalf("new row changed: %+v", row)
					}
				}
				var indexes int
				if err := st.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_activity_src_created_id'`).Scan(&indexes); err != nil || indexes != 1 {
					t.Fatalf("source index count = %d, err = %v", indexes, err)
				}
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
