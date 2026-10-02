package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustPut(t *testing.T, st *Store, asset Asset) bool {
	t.Helper()
	_, created, err := st.PutAsset(asset)
	if err != nil {
		t.Fatalf("put %s: %v", asset.ID, err)
	}
	return created
}

func TestOpenCreatesUsableStore(t *testing.T) {
	st := openTestStore(t)
	if err := st.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestPutCreatesThenUpdates(t *testing.T) {
	st := openTestStore(t)

	created := mustPut(t, st, Asset{ID: "a1", Type: "table", Owner: "alice",
		Upstream: []string{}, Downstream: []string{}})
	if !created {
		t.Fatal("first registration must report created")
	}

	assets, err := st.ListAssets("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("len = %d, want 1", len(assets))
	}
	if assets[0].Upstream == nil || assets[0].Downstream == nil {
		t.Fatal("dependency lists must serialize as arrays, not null")
	}

	created = mustPut(t, st, Asset{ID: "a1", Type: "job", Owner: "bob"})
	if created {
		t.Fatal("repeat registration must report update")
	}
	assets, _ = st.ListAssets("")
	if got := assets[0]; got.Type != "job" || got.Owner != "bob" {
		t.Fatalf("updated asset = %+v", got)
	}
}

func TestPutReplacesBothDependencyLists(t *testing.T) {
	st := openTestStore(t)
	mustPut(t, st, Asset{ID: "a", Type: "t", Owner: "o"})
	mustPut(t, st, Asset{ID: "b", Type: "t", Owner: "o"})
	mustPut(t, st, Asset{ID: "c", Type: "t", Owner: "o"})
	mustPut(t, st, Asset{ID: "d", Type: "t", Owner: "o"})

	mustPut(t, st, Asset{ID: "a", Type: "t", Owner: "o", Upstream: []string{"b", "c"}})
	mustPut(t, st, Asset{ID: "a", Type: "t", Owner: "o", Upstream: []string{"c"}, Downstream: []string{"d"}})

	assets, err := st.ListAssets("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := assets[0]
	if !reflect.DeepEqual(got.Upstream, []string{"c"}) || !reflect.DeepEqual(got.Downstream, []string{"d"}) {
		t.Fatalf("lists not fully replaced: %+v", got)
	}

	upstream, err := st.Upstream("a", 0)
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}
	if len(upstream) != 1 || upstream[0].ID != "c" {
		t.Fatalf("old edge to b survived: %+v", upstream)
	}
}

func TestPutRejectsUnknownReferenceWithoutWriting(t *testing.T) {
	st := openTestStore(t)
	mustPut(t, st, Asset{ID: "a", Type: "t", Owner: "o"})

	if _, _, err := st.PutAsset(Asset{ID: "b", Type: "t", Owner: "o", Upstream: []string{"ghost"}}); !errors.Is(err, ErrInvalidAsset) {
		t.Fatalf("err = %v, want ErrInvalidAsset", err)
	}
	if _, _, err := st.PutAsset(Asset{ID: "b", Type: "t", Owner: "o", Downstream: []string{"ghost"}}); !errors.Is(err, ErrInvalidAsset) {
		t.Fatalf("err = %v, want ErrInvalidAsset", err)
	}
	if assets, _ := st.ListAssets(""); len(assets) != 1 {
		t.Fatalf("rejected registration wrote data: %+v", assets)
	}
}

func TestPutRejectsMalformedAsset(t *testing.T) {
	st := openTestStore(t)
	cases := []Asset{
		{ID: "", Type: "t", Owner: "o"},
		{ID: "x", Type: "", Owner: "o"},
		{ID: "x", Type: "t", Owner: ""},
		{ID: "x", Type: "t", Owner: "o", Upstream: []string{""}},
	}
	for _, tc := range cases {
		if _, _, err := st.PutAsset(tc); !errors.Is(err, ErrInvalidAsset) {
			t.Fatalf("%+v: err = %v, want ErrInvalidAsset", tc, err)
		}
	}
}

func TestPutRejectsCycleAndKeepsState(t *testing.T) {
	st := openTestStore(t)
	// d <- c <- b <- a, plus a -> d
	mustPut(t, st, Asset{ID: "a", Type: "t", Owner: "o"})
	mustPut(t, st, Asset{ID: "b", Type: "t", Owner: "o"})
	mustPut(t, st, Asset{ID: "c", Type: "t", Owner: "o"})
	mustPut(t, st, Asset{ID: "d", Type: "t", Owner: "o"})
	mustPut(t, st, Asset{ID: "a", Type: "t", Owner: "o", Downstream: []string{"b"}})
	mustPut(t, st, Asset{ID: "b", Type: "t", Owner: "o", Upstream: []string{"a"}, Downstream: []string{"c"}})
	mustPut(t, st, Asset{ID: "c", Type: "t", Owner: "o", Upstream: []string{"b"}, Downstream: []string{"d"}})
	mustPut(t, st, Asset{ID: "d", Type: "t", Owner: "o", Upstream: []string{"c"}})

	// Closing the loop d -> a must fail, including via self reference on update.
	if _, _, err := st.PutAsset(Asset{ID: "d", Type: "t", Owner: "o",
		Upstream: []string{"c"}, Downstream: []string{"a"}}); !errors.Is(err, ErrLineageCycle) {
		t.Fatalf("err = %v, want ErrLineageCycle", err)
	}
	if _, _, err := st.PutAsset(Asset{ID: "d", Type: "t", Owner: "o",
		Upstream: []string{"c", "d"}}); !errors.Is(err, ErrLineageCycle) {
		t.Fatalf("self loop err = %v, want ErrLineageCycle", err)
	}

	assets, _ := st.ListAssets("")
	got := assets[0] // ids sorted: a first
	if got.ID != "a" {
		t.Fatalf("unexpected asset order: %s", got.ID)
	}
	upstream, err := st.Upstream("d", 0)
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}
	if len(upstream) != 3 {
		t.Fatalf("state changed after rejected cycle: %+v", upstream)
	}
}

func TestListAssetsFiltersTypeAndSortsByID(t *testing.T) {
	st := openTestStore(t)
	mustPut(t, st, Asset{ID: "z", Type: "job", Owner: "o"})
	mustPut(t, st, Asset{ID: "a", Type: "table", Owner: "o"})
	mustPut(t, st, Asset{ID: "m", Type: "table", Owner: "o"})

	all, err := st.ListAssets("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := []string{all[0].ID, all[1].ID, all[2].ID}; !reflect.DeepEqual(got, []string{"a", "m", "z"}) {
		t.Fatalf("order = %v", got)
	}

	tables, err := st.ListAssets("table")
	if err != nil {
		t.Fatalf("list table: %v", err)
	}
	if len(tables) != 2 || tables[0].ID != "a" || tables[1].ID != "m" {
		t.Fatalf("filter result = %+v", tables)
	}
}

func TestUpstreamShortestDistanceOrderAndDepth(t *testing.T) {
	st := openTestStore(t)
	// b and d both feed a; c feeds b and d; e feeds c.
	mustPut(t, st, Asset{ID: "a", Type: "t", Owner: "o"})
	mustPut(t, st, Asset{ID: "b", Type: "t", Owner: "o"})
	mustPut(t, st, Asset{ID: "c", Type: "t", Owner: "o"})
	mustPut(t, st, Asset{ID: "d", Type: "t", Owner: "o"})
	mustPut(t, st, Asset{ID: "e", Type: "t", Owner: "o"})
	mustPut(t, st, Asset{ID: "a", Type: "t", Owner: "o", Upstream: []string{"b", "d"}})
	mustPut(t, st, Asset{ID: "b", Type: "t", Owner: "o", Upstream: []string{"c"}})
	mustPut(t, st, Asset{ID: "d", Type: "t", Owner: "o", Upstream: []string{"c"}})
	mustPut(t, st, Asset{ID: "c", Type: "t", Owner: "o", Upstream: []string{"e"}})

	all, err := st.Upstream("a", 0)
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}
	want := []UpstreamAsset{
		{ID: "b", Distance: 1}, {ID: "d", Distance: 1},
		{ID: "c", Distance: 2}, {ID: "e", Distance: 3},
	}
	if len(all) != len(want) {
		t.Fatalf("len = %d, want %d (%+v)", len(all), len(want), all)
	}
	for i, w := range want {
		if all[i].ID != w.ID || all[i].Distance != w.Distance {
			t.Fatalf("position %d = %s@%d, want %s@%d", i, all[i].ID, all[i].Distance, w.ID, w.Distance)
		}
	}

	direct, err := st.Upstream("a", 1)
	if err != nil {
		t.Fatalf("upstream depth 1: %v", err)
	}
	if len(direct) != 2 || direct[0].ID != "b" || direct[1].ID != "d" {
		t.Fatalf("depth 1 = %+v", direct)
	}
}

func TestUpstreamUnknownAsset(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.Upstream("ghost", 0); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("err = %v, want ErrAssetNotFound", err)
	}
}

func TestMutualDeclarationsSurviveReRegistration(t *testing.T) {
	st := openTestStore(t)
	mustPut(t, st, Asset{ID: "a", Type: "t", Owner: "o"})
	mustPut(t, st, Asset{ID: "b", Type: "t", Owner: "o"})
	// a declares b downstream; b independently declares a upstream.
	mustPut(t, st, Asset{ID: "a", Type: "t", Owner: "o", Downstream: []string{"b"}})
	mustPut(t, st, Asset{ID: "b", Type: "t", Owner: "o", Upstream: []string{"a"}})

	// Re-registering a must not erase b's own declaration.
	mustPut(t, st, Asset{ID: "a", Type: "t", Owner: "o", Downstream: []string{"b"}})
	got, err := st.ListAssets("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !reflect.DeepEqual(got[0].Upstream, []string{}) || !reflect.DeepEqual(got[0].Downstream, []string{"b"}) {
		t.Fatalf("a changed unexpectedly: %+v", got[0])
	}
	if !reflect.DeepEqual(got[1].Upstream, []string{"a"}) {
		t.Fatalf("b lost its declaration: %+v", got[1])
	}
}
