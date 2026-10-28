package sqlite

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/store"
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

func TestActivity_PromptPoints(t *testing.T) {
	ctx := context.Background()
	st, err := New("")
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
	st, err := New("")
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
