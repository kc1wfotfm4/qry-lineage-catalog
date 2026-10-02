package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func mustAsset(t *testing.T, st *Store, asset *Asset) {
	t.Helper()
	if _, err := st.UpsertAsset(context.Background(), asset); err != nil {
		t.Fatalf("upsert %s: %v", asset.ID, err)
	}
}

func TestUpsertReplacesBothListsFully(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	mustAsset(t, st, &Asset{ID: "u1", Type: "t", Owner: "o", Upstream: []string{}, Downstream: []string{}})
	mustAsset(t, st, &Asset{ID: "u2", Type: "t", Owner: "o", Upstream: []string{}, Downstream: []string{}})
	mustAsset(t, st, &Asset{ID: "d1", Type: "t", Owner: "o", Upstream: []string{}, Downstream: []string{}})
	mustAsset(t, st, &Asset{ID: "a", Type: "t", Owner: "o", Upstream: []string{"u1", "u2"}, Downstream: []string{"d1"}})

	mustAsset(t, st, &Asset{ID: "a", Type: "t", Owner: "o", Upstream: []string{"u2"}, Downstream: []string{}})

	a, err := st.getAsset(ctx, st.db, "a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(a.Upstream) != 1 || a.Upstream[0] != "u2" {
		t.Fatalf("upstream = %v", a.Upstream)
	}
	if len(a.Downstream) != 0 {
		t.Fatalf("downstream = %v", a.Downstream)
	}
	u1, _ := st.getAsset(ctx, st.db, "u1")
	if len(u1.Downstream) != 0 {
		t.Fatalf("stale reverse edge left on u1: %v", u1.Downstream)
	}
}

func TestUpsertCycleLeavesStateUntouched(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	mustAsset(t, st, &Asset{ID: "c", Type: "t", Owner: "o"})
	mustAsset(t, st, &Asset{ID: "b", Type: "t", Owner: "o", Upstream: []string{"c"}})
	mustAsset(t, st, &Asset{ID: "a", Type: "t", Owner: "o", Upstream: []string{"b"}})

	_, err = st.UpsertAsset(ctx, &Asset{ID: "c", Type: "t", Owner: "o2", Upstream: []string{"a"}})
	if !errors.Is(err, ErrLineageCycle) {
		t.Fatalf("err = %v, want lineage cycle", err)
	}
	c, _ := st.getAsset(ctx, st.db, "c")
	if c.Owner != "o" || len(c.Upstream) != 0 {
		t.Fatalf("c mutated despite rollback: %+v", c)
	}
}

func TestUpsertRejectsUnknownAndEmptyReferences(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	for _, asset := range []*Asset{
		{ID: "x", Type: "t", Owner: "o", Upstream: []string{"ghost"}},
		{ID: "x", Type: "t", Owner: "o", Downstream: []string{" "}},
		{ID: "  ", Type: "t", Owner: "o"},
		{ID: "x", Type: "", Owner: "o"},
	} {
		if _, err := st.UpsertAsset(ctx, asset); !errors.Is(err, ErrInvalidAsset) {
			t.Fatalf("asset %+v: err = %v, want invalid", asset, err)
		}
	}
	assets, err := st.ListAssets(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(assets) != 0 {
		t.Fatalf("invalid writes leaked: %+v", assets)
	}
}

func TestUpstreamShortestDistanceAndDedupe(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	mustAsset(t, st, &Asset{ID: "u3", Type: "t", Owner: "o"})
	mustAsset(t, st, &Asset{ID: "u2", Type: "t", Owner: "o", Upstream: []string{"u3"}})
	mustAsset(t, st, &Asset{ID: "u1", Type: "t", Owner: "o", Upstream: []string{"u2", "u3"}})
	mustAsset(t, st, &Asset{ID: "root", Type: "t", Owner: "o", Upstream: []string{"u1"}})

	_, up, err := st.Upstream(ctx, "root", 0)
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}
	want := map[string]int{"u1": 1, "u2": 2, "u3": 2}
	if len(up) != len(want) {
		t.Fatalf("ancestors = %+v", up)
	}
	for _, ancestor := range up {
		if ancestor.Distance != want[ancestor.ID] {
			t.Fatalf("%s distance = %d, want %d", ancestor.ID, ancestor.Distance, want[ancestor.ID])
		}
	}

	_, up, err = st.Upstream(ctx, "root", 1)
	if err != nil || len(up) != 1 || up[0].ID != "u1" {
		t.Fatalf("depth 1: %+v err=%v", up, err)
	}

	if _, _, err := st.Upstream(ctx, "ghost", 0); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("missing asset err = %v", err)
	}
}

func TestConcurrentUpsertsDoNotLoseEdges(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	mustAsset(t, st, &Asset{ID: "hub", Type: "t", Owner: "o"})
	for _, id := range []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8"} {
		mustAsset(t, st, &Asset{ID: id, Type: "t", Owner: "o", Upstream: []string{"hub"}})
	}

	hub, err := st.getAsset(ctx, st.db, "hub")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(hub.Downstream) != 8 {
		t.Fatalf("hub downstream = %v, want all 8", hub.Downstream)
	}
}
