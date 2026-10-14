package sqlite

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mostlygeek/llama-swap/internal/store"

	"github.com/pressly/goose/v3"
)

func TestStore_MigrateActivitySessionAcrossForkAndUpstream(t *testing.T) {
	for _, shape := range []string{"empty", "base", "fork-v2", "upstream-v2", "fork-v3", "upstream-v3"} {
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
				case "fork-v2", "fork-v3":
					files["00002_add_activity_session_id.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nALTER TABLE activity ADD COLUMN session_id TEXT NOT NULL DEFAULT '';\n-- +goose Down\nALTER TABLE activity DROP COLUMN session_id;\n")}
					wantSession = "existing-session"
					if shape == "fork-v3" {
						// The published fork version 3 reconciled src and session_id,
						// but did not create upstream's cache table.
						files["00003_reconcile_activity.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nALTER TABLE activity ADD COLUMN src TEXT NOT NULL DEFAULT '';\nCREATE INDEX idx_activity_src_created_id ON activity (src, ts_created DESC, id DESC);\n")}
						wantSource = "tailcat:existing-peer"
					}
				case "upstream-v2", "upstream-v3":
					source, err := migrationFS.ReadFile("migrations/00002_add_activity_source.sql")
					if err != nil {
						t.Fatal(err)
					}
					files["00002_add_activity_source.sql"] = &fstest.MapFile{Data: source}
					wantSource = "tailcat:existing-peer"
					if shape == "upstream-v3" {
						cache, err := migrationFS.ReadFile("migrations/00003_create_cache.sql")
						if err != nil {
							t.Fatal(err)
						}
						files["00003_create_cache.sql"] = &fstest.MapFile{Data: cache}
					}
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
				if strings.HasPrefix(shape, "fork-") {
					if _, err := db.Exec(`UPDATE activity SET session_id = ?`, wantSession); err != nil {
						t.Fatal(err)
					}
				}
				if wantSource != "" {
					if _, err := db.Exec(`UPDATE activity SET src = ?`, wantSource); err != nil {
						t.Fatal(err)
					}
				}
				if shape == "upstream-v3" {
					if _, err := db.Exec(`INSERT INTO cache (key, data, ts_created) VALUES ('legacy', ?, 1)`, []byte("keep-cache")); err != nil {
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
					if err := st.Cache().Set(t.Context(), store.CacheEntry{Key: "new", Data: []byte("new-cache")}); err != nil {
						t.Fatal(err)
					}
				}
				cached, found, err := st.Cache().Get(t.Context(), "new")
				if err != nil || !found || !bytes.Equal(cached.Data, []byte("new-cache")) {
					t.Fatalf("new cache attempt %d: entry=%+v found=%t err=%v", attempt, cached, found, err)
				}
				if shape == "upstream-v3" {
					cached, found, err := st.Cache().Get(t.Context(), "legacy")
					if err != nil || !found || !bytes.Equal(cached.Data, []byte("keep-cache")) {
						t.Fatalf("legacy cache changed: entry=%+v found=%t err=%v", cached, found, err)
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
				if err := st.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_cache_expiry'`).Scan(&indexes); err != nil || indexes != 1 {
					t.Fatalf("cache index count = %d, err = %v", indexes, err)
				}
				var version int
				if err := st.db.QueryRow(`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied`).Scan(&version); err != nil || version != 4 {
					t.Fatalf("migration version = %d, err = %v", version, err)
				}
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
