package sqlite

import (
	"context"
	"io/fs"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/pressly/goose/v3"
)

// Every sort key the store package accepts must map to a SQL column here, and
// nothing outside that whitelist may be sortable.
func TestActivity_SortColumnsCoverStoreKeys(t *testing.T) {
	keys := store.ActivitySortKeys()
	for _, key := range keys {
		if _, ok := activitySortColumns[key]; !ok {
			t.Errorf("store sort key %q has no SQL column", key)
		}
	}
	if len(activitySortColumns) != len(keys) {
		t.Errorf("activitySortColumns has %d entries, store has %d keys", len(activitySortColumns), len(keys))
	}
	for key := range activitySortColumns {
		if !store.ValidActivitySortKey(key) {
			t.Errorf("SQL sort column key %q is not a store sort key", key)
		}
	}
}

func TestStore_ActivityTaskRouteRoundTrip(t *testing.T) {
	st, err := New("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	in := store.ActivityLogEntry{
		Timestamp: time.Unix(100, 0),
		Model:     "m",
		ReqPath:   "/v1/chat/completions",
		Task:      "chat",
		Route:     "peer",
		PeerID:    "cloud",
		Tokens:    store.TokenMetrics{InputTokens: 3, OutputTokens: 9, ReasoningTokens: 6},
	}
	if _, err := st.Activity().Insert(ctx, in); err != nil {
		t.Fatal(err)
	}
	page, err := st.Activity().List(ctx, store.ActivityQuery{Limit: 10, Page: 1})
	if err != nil || len(page.Data) != 1 {
		t.Fatalf("list = %+v, %v", page, err)
	}
	got := page.Data[0]
	if got.Task != "chat" || got.Route != "peer" || got.PeerID != "cloud" || got.Tokens.ReasoningTokens != 6 {
		t.Errorf("entry = %+v", got)
	}
}

// TestStore_ActivityTaskBackfill checks migration 00004 labels rows recorded
// before the task column existed, and leaves reasoning tokens unknown.
func TestStore_ActivityTaskBackfill(t *testing.T) {
	st, err := New("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	migrations, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, st.db, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 3); err != nil {
		t.Fatalf("down to 3: %v", err)
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/embeddings", "/v1/audio/speech", "/upstream/m/props"} {
		if _, err := st.db.ExecContext(ctx, `INSERT INTO activity (ts_created, model_id, req_path) VALUES (1, 'm', ?)`, path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}

	page, err := st.Activity().List(ctx, store.ActivityQuery{Limit: 10, Page: 1, Sort: "id", Order: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"/v1/chat/completions": "chat",
		"/v1/embeddings":       "embedding",
		"/v1/audio/speech":     "speech",
		"/upstream/m/props":    "",
	}
	if len(page.Data) != len(want) {
		t.Fatalf("got %d rows, want %d", len(page.Data), len(want))
	}
	for _, e := range page.Data {
		if e.Task != want[e.ReqPath] {
			t.Errorf("%s task = %q, want %q", e.ReqPath, e.Task, want[e.ReqPath])
		}
		if e.Tokens.ReasoningTokens != -1 {
			t.Errorf("%s reasoning = %d, want -1", e.ReqPath, e.Tokens.ReasoningTokens)
		}
	}
}

func TestActivity_PromptPoints(t *testing.T) {
	ctx := context.Background()
	st, err := New("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	short := store.PromptPoint{Model: "m1", InputTokens: 21, CachedTokens: 35814, PromptPerSecond: 30.2}
	long := store.PromptPoint{Model: "m2", InputTokens: 1560, CachedTokens: 13013, PromptPerSecond: 313.4}
	other := store.PromptPoint{Model: "m1", InputTokens: 63, CachedTokens: -1, PromptPerSecond: 64.3}
	insert := func(p store.PromptPoint, session string) {
		t.Helper()
		_, err := st.Activity().Insert(ctx, store.ActivityLogEntry{
			Model: p.Model, SessionID: session,
			Tokens: store.TokenMetrics{InputTokens: p.InputTokens, CachedTokens: p.CachedTokens, PromptPerSecond: p.PromptPerSecond},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(short, "shared")
	insert(long, "shared")
	insert(other, "other")
	for i := 0; i < 30; i++ {
		insert(short, "shared")
	}
	for _, invalid := range []store.PromptPoint{
		{Model: "m1", InputTokens: 0, PromptPerSecond: 50},
		{Model: "m1", InputTokens: -1, PromptPerSecond: 50},
		{Model: "m1", InputTokens: 10, PromptPerSecond: 0},
		{Model: "m1", InputTokens: 10, PromptPerSecond: -1},
	} {
		insert(invalid, "shared")
	}

	tests := []struct {
		name  string
		query store.ActivityStatsQuery
		want  map[store.PromptPoint]int
	}{
		{"all", store.ActivityStatsQuery{}, map[store.PromptPoint]int{short: 31, long: 1, other: 1}},
		{"model", store.ActivityStatsQuery{Model: "m1"}, map[store.PromptPoint]int{short: 31, other: 1}},
		{"session", store.ActivityStatsQuery{SessionID: "shared"}, map[store.PromptPoint]int{short: 31, long: 1}},
		{"both", store.ActivityStatsQuery{Model: "m2", SessionID: "shared"}, map[store.PromptPoint]int{long: 1}},
		{"empty", store.ActivityStatsQuery{SessionID: "missing"}, map[store.PromptPoint]int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats, err := st.Activity().Stats(ctx, tt.query)
			if err != nil {
				t.Fatal(err)
			}
			if stats.PromptPoints == nil {
				t.Fatal("PromptPoints is nil")
			}
			got := map[store.PromptPoint]int{}
			for _, p := range stats.PromptPoints {
				got[p]++
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("points = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestActivity_ValidPromptPoint(t *testing.T) {
	for _, speed := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, -1} {
		if validPromptPoint(store.PromptPoint{InputTokens: 1, PromptPerSecond: speed}) {
			t.Errorf("speed %v accepted", speed)
		}
	}
	if !validPromptPoint(store.PromptPoint{InputTokens: 1, CachedTokens: -1, PromptPerSecond: 1}) {
		t.Error("valid point rejected")
	}
}

func TestActivity_InfinitePromptSpeed(t *testing.T) {
	ctx := context.Background()
	st, err := New("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.Activity().Insert(ctx, store.ActivityLogEntry{
		Model: "m", Tokens: store.TokenMetrics{InputTokens: 10, PromptPerSecond: math.Inf(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := st.Activity().Stats(ctx, store.ActivityStatsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.PromptPoints) != 0 || stats.PromptHistogram != nil {
		t.Fatalf("infinite speed included in statistics: %+v", stats)
	}
}

func TestActivity_GenerationAndDraftPoints(t *testing.T) {
	ctx := context.Background()
	st, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	entries := []store.ActivityLogEntry{
		{Model: "m1", SessionID: "shared", Tokens: store.TokenMetrics{InputTokens: 10, TokensPerSecond: 20, DraftTokens: 10, DraftAccTokens: 0}},
		{Model: "m2", SessionID: "shared", Tokens: store.TokenMetrics{InputTokens: 100, TokensPerSecond: 30, DraftTokens: 10, DraftAccTokens: 5}},
		{Model: "m1", SessionID: "other", Tokens: store.TokenMetrics{InputTokens: 1000, TokensPerSecond: 40, DraftTokens: 10, DraftAccTokens: 12}},
	}
	entries = append(entries, entries[0]) // Coincident requests must not be deduplicated.
	for _, tokens := range []store.TokenMetrics{
		{InputTokens: 0, TokensPerSecond: 50, DraftTokens: 10, DraftAccTokens: 5},
		{InputTokens: -1, TokensPerSecond: 50, DraftTokens: 10, DraftAccTokens: 5},
		{InputTokens: 10, TokensPerSecond: 0, DraftTokens: 0},
		{InputTokens: 10, TokensPerSecond: -1, DraftTokens: -1},
		{InputTokens: 10, DraftTokens: 10, DraftAccTokens: -1},
	} {
		entries = append(entries, store.ActivityLogEntry{Model: "m1", SessionID: "shared", Tokens: tokens})
	}
	for _, entry := range entries {
		if _, err := st.Activity().Insert(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name    string
		query   store.ActivityStatsQuery
		indexes []int
	}{
		{"all", store.ActivityStatsQuery{}, []int{0, 1, 2, 3}},
		{"model", store.ActivityStatsQuery{Model: "m1"}, []int{0, 2, 3}},
		{"session", store.ActivityStatsQuery{SessionID: "shared"}, []int{0, 1, 3}},
		{"both", store.ActivityStatsQuery{Model: "m2", SessionID: "shared"}, []int{1}},
		{"empty", store.ActivityStatsQuery{SessionID: "missing"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stats, err := st.Activity().Stats(ctx, tt.query)
			if err != nil {
				t.Fatal(err)
			}
			if stats.GenerationPoints == nil || stats.DraftPoints == nil {
				t.Fatal("point arrays must not be nil")
			}
			wantGen, gotGen := map[store.GenerationPoint]int{}, map[store.GenerationPoint]int{}
			wantDraft, gotDraft := map[store.DraftPoint]int{}, map[store.DraftPoint]int{}
			for _, i := range tt.indexes {
				e := entries[i]
				wantGen[store.GenerationPoint{Model: e.Model, InputTokens: e.Tokens.InputTokens, TokensPerSecond: e.Tokens.TokensPerSecond}]++
				wantDraft[store.DraftPoint{Model: e.Model, InputTokens: e.Tokens.InputTokens, DraftTokens: e.Tokens.DraftTokens, DraftAccTokens: e.Tokens.DraftAccTokens}]++
			}
			for _, p := range stats.GenerationPoints {
				gotGen[p]++
			}
			for _, p := range stats.DraftPoints {
				gotDraft[p]++
			}
			if !reflect.DeepEqual(gotGen, wantGen) {
				t.Fatalf("generation points = %v, want %v", gotGen, wantGen)
			}
			if !reflect.DeepEqual(gotDraft, wantDraft) {
				t.Fatalf("draft points = %v, want %v", gotDraft, wantDraft)
			}
			if tt.name == "all" {
				if stats.GenerationHistogram.Min != 20 || stats.GenerationHistogram.Max != 50 {
					t.Fatalf("generation histogram changed: %+v", stats.GenerationHistogram)
				}
				if stats.DraftHistogram.Min != 0 || stats.DraftHistogram.Max != 120 {
					t.Fatalf("draft histogram changed: %+v", stats.DraftHistogram)
				}
			}
		})
	}
}

func TestActivity_InfiniteGenerationSpeed(t *testing.T) {
	ctx := context.Background()
	st, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, speed := range []float64{math.Inf(1), math.Inf(-1)} {
		if _, err := st.Activity().Insert(ctx, store.ActivityLogEntry{Model: "m", Tokens: store.TokenMetrics{InputTokens: 10, TokensPerSecond: speed}}); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := st.Activity().Stats(ctx, store.ActivityStatsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.GenerationPoints) != 0 {
		t.Fatalf("infinite speed included: %+v", stats.GenerationPoints)
	}
}
