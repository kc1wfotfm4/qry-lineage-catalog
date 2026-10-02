package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/kc1wfotfm4/qry-lineage-catalog/internal/store"
)

type testServer struct {
	t  *testing.T
	rt http.Handler
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &testServer{t: t, rt: NewRouter(st)}
}

func (s *testServer) request(method, path, body string) *httptest.ResponseRecorder {
	s.t.Helper()
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.rt.ServeHTTP(rec, req)
	return rec
}

func (s *testServer) register(t *testing.T, body string) map[string]any {
	t.Helper()
	rec := s.request(http.MethodPost, "/api/v1/assets", body)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("register status = %d body = %s", rec.Code, rec.Body.String())
	}
	var asset map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &asset); err != nil {
		t.Fatalf("decode asset: %v body=%s", err, rec.Body.String())
	}
	return asset
}

func registerBody(id, typ, owner, upstream, downstream string) string {
	return `{"id":` + jsonString(id) + `,"type":` + jsonString(typ) + `,"owner":` + jsonString(owner) +
		`,"upstream":` + upstream + `,"downstream":` + downstream + `}`
}

func jsonString(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, wantStatus, rec.Body.String())
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if envelope.Error.Code != wantCode {
		t.Fatalf("code = %q, want %q", envelope.Error.Code, wantCode)
	}
	if envelope.Error.Message == "" {
		t.Fatalf("message must not be empty")
	}
}

func assetList(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var assets []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &assets); err != nil {
		t.Fatalf("decode list: %v body=%s", err, rec.Body.String())
	}
	return assets
}

func TestRegisterCreatesAndReturnsAsset(t *testing.T) {
	srv := newTestServer(t)
	rec := srv.request(http.MethodPost, "/api/v1/assets", registerBody("a", "table", "alice", `["u1"]`, `["d1"]`))
	_ = rec
	// references must exist first
	srv.register(t, registerBody("u1", "table", "bob", `[]`, `[]`))
	srv.register(t, registerBody("d1", "job", "carol", `[]`, `[]`))
	rec = srv.request(http.MethodPost, "/api/v1/assets", registerBody("a", "table", "alice", `["u1"]`, `["d1"]`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 body=%s", rec.Code, rec.Body.String())
	}
	var asset map[string]any
	json.Unmarshal(rec.Body.Bytes(), &asset)
	if asset["id"] != "a" || asset["type"] != "table" || asset["owner"] != "alice" {
		t.Fatalf("unexpected asset: %v", asset)
	}
	if len(asset["upstream"].([]any)) != 1 || len(asset["downstream"].([]any)) != 1 {
		t.Fatalf("dependency lists missing: %v", asset)
	}

	// re-register updates and returns 200
	rec = srv.request(http.MethodPost, "/api/v1/assets", registerBody("a", "view", "alice2", `[]`, `[]`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &asset)
	if asset["type"] != "view" || asset["owner"] != "alice2" {
		t.Fatalf("update not reflected: %v", asset)
	}
	// old edges must be gone: u1 no longer lists a as downstream
	rec = srv.request(http.MethodGet, "/api/v1/assets", "")
	assets := assetList(t, rec)
	for _, item := range assets {
		if item["id"] == "u1" {
			if len(item["downstream"].([]any)) != 0 {
				t.Fatalf("stale edge left on u1: %v", item)
			}
		}
	}
}

func TestListFiltersByTypeAndOrdersByID(t *testing.T) {
	srv := newTestServer(t)
	srv.register(t, registerBody("b", "table", "o", `[]`, `[]`))
	srv.register(t, registerBody("a", "job", "o", `[]`, `[]`))
	srv.register(t, registerBody("c", "table", "o", `[]`, `[]`))

	rec := srv.request(http.MethodGet, "/api/v1/assets", "")
	assets := assetList(t, rec)
	if len(assets) != 3 || assets[0]["id"] != "a" || assets[1]["id"] != "b" || assets[2]["id"] != "c" {
		t.Fatalf("unexpected ordering: %v", assets)
	}

	rec = srv.request(http.MethodGet, "/api/v1/assets?type=table", "")
	assets = assetList(t, rec)
	if len(assets) != 2 || assets[0]["id"] != "b" || assets[1]["id"] != "c" {
		t.Fatalf("type filter failed: %v", assets)
	}

	rec = srv.request(http.MethodGet, "/api/v1/assets?type=nope", "")
	if assets = assetList(t, rec); len(assets) != 0 {
		t.Fatalf("unknown type must return empty array: %v", assets)
	}
}

func TestUpstreamLineageDistancesAndDepth(t *testing.T) {
	srv := newTestServer(t)
	// u2 <- u1 <- root ; u2 <- root (diamond)
	for _, body := range []string{
		registerBody("u2", "table", "o", `[]`, `[]`),
		registerBody("u1", "table", "o", `["u2"]`, `[]`),
		registerBody("root", "table", "o", `["u1","u2"]`, `[]`),
	} {
		srv.register(t, body)
	}

	rec := srv.request(http.MethodGet, "/api/v1/assets/root/upstream", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		ID       string `json:"id"`
		Upstream []struct {
			ID       string `json:"id"`
			Distance int    `json:"distance"`
		} `json:"upstream"`
	}
	json.Unmarshal(rec.Body.Bytes(), &envelope)
	if envelope.ID != "root" {
		t.Fatalf("id = %q", envelope.ID)
	}
	if len(envelope.Upstream) != 2 {
		t.Fatalf("want 2 unique ancestors, got %d: %+v", len(envelope.Upstream), envelope.Upstream)
	}
	if envelope.Upstream[0].ID != "u1" || envelope.Upstream[0].Distance != 1 {
		t.Fatalf("nearest first: %+v", envelope.Upstream)
	}
	if envelope.Upstream[1].ID != "u2" || envelope.Upstream[1].Distance != 1 {
		t.Fatalf("u2 shortest distance is 1: %+v", envelope.Upstream)
	}

	rec = srv.request(http.MethodGet, "/api/v1/assets/root/upstream?depth=1", "")
	json.Unmarshal(rec.Body.Bytes(), &envelope)
	if len(envelope.Upstream) != 2 {
		t.Fatalf("depth 1 must show only direct: %+v", envelope.Upstream)
	}

	// separate chain for depth limiting: c3 <- c2 <- c1
	srv.register(t, registerBody("c3", "table", "o", `[]`, `[]`))
	srv.register(t, registerBody("c2", "table", "o", `["c3"]`, `[]`))
	srv.register(t, registerBody("c1", "table", "o", `["c2"]`, `[]`))
	rec = srv.request(http.MethodGet, "/api/v1/assets/missing/upstream", "")
	assertErrorCode(t, rec, http.StatusNotFound, "asset_not_found")

	rec = srv.request(http.MethodGet, "/api/v1/assets/c1/upstream?depth=1", "")
	json.Unmarshal(rec.Body.Bytes(), &envelope)
	if len(envelope.Upstream) != 1 || envelope.Upstream[0].ID != "c2" {
		t.Fatalf("depth 1 on c1: %+v", envelope.Upstream)
	}
	rec = srv.request(http.MethodGet, "/api/v1/assets/c1/upstream?depth=2", "")
	json.Unmarshal(rec.Body.Bytes(), &envelope)
	if len(envelope.Upstream) != 2 || envelope.Upstream[1].ID != "c3" {
		t.Fatalf("depth 2 reaches c3: %+v", envelope.Upstream)
	}
}

func TestDepthValidation(t *testing.T) {
	srv := newTestServer(t)
	srv.register(t, registerBody("a", "t", "o", `[]`, `[]`))
	for _, depth := range []string{"0", "11", "-1", "1.5", "abc", ""} {
		rec := srv.request(http.MethodGet, "/api/v1/assets/a/upstream?depth="+depth, "")
		assertErrorCode(t, rec, http.StatusBadRequest, "invalid_depth")
	}
}

func TestRegistrationValidationErrors(t *testing.T) {
	srv := newTestServer(t)
	srv.register(t, registerBody("a", "t", "o", `[]`, `[]`))

	cases := map[string]string{
		"missing field":    `{"id":"x","type":"t","owner":"o","upstream":[]}`,
		"empty id":         registerBody("  ", "t", "o", `[]`, `[]`),
		"wrong type":       `{"id":7,"type":"t","owner":"o","upstream":[],"downstream":[]}`,
		"wrong array type": `{"id":"x","type":"t","owner":"o","upstream":["a"],"downstream":"no"}`,
		"empty dep id":     registerBody("x", "t", "o", `[""]`, `[]`),
		"unknown dep":      registerBody("x", "t", "o", `["ghost"]`, `[]`),
		"malformed json":   `{"id":`,
		"trailing tokens":  registerBody("x", "t", "o", `[]`, `[]`) + `{"id":"y"}`,
		"array not object": `[1,2,3]`,
	}
	for name, body := range cases {
		rec := srv.request(http.MethodPost, "/api/v1/assets", body)
		wantCode := "invalid_asset"
		if name == "malformed json" || name == "trailing tokens" {
			wantCode = "invalid_request"
		}
		assertErrorCode(t, rec, http.StatusBadRequest, wantCode)
	}

	// nothing was written for invalid new asset x
	rec := srv.request(http.MethodGet, "/api/v1/assets/x/upstream", "")
	assertErrorCode(t, rec, http.StatusNotFound, "asset_not_found")
}

func TestCyclesAreRejectedAtomically(t *testing.T) {
	srv := newTestServer(t)
	srv.register(t, registerBody("c", "t", "o", `[]`, `[]`))
	srv.register(t, registerBody("b", "t", "o", `["c"]`, `[]`))
	srv.register(t, registerBody("a", "t", "o", `["b"]`, `[]`))

	// self loop
	rec := srv.request(http.MethodPost, "/api/v1/assets", registerBody("a", "t", "o", `["a"]`, `[]`))
	assertErrorCode(t, rec, http.StatusConflict, "lineage_cycle")

	// indirect cycle c -> a
	rec = srv.request(http.MethodPost, "/api/v1/assets", registerBody("c", "t", "o", `["a"]`, `[]`))
	assertErrorCode(t, rec, http.StatusConflict, "lineage_cycle")

	// existing relationships unchanged
	rec = srv.request(http.MethodGet, "/api/v1/assets/c/upstream", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var envelope struct {
		Upstream []struct{ ID string } `json:"upstream"`
	}
	json.Unmarshal(rec.Body.Bytes(), &envelope)
	if len(envelope.Upstream) != 0 {
		t.Fatalf("c must keep no upstreams after failed cycle write, got %+v", envelope.Upstream)
	}
	rec = srv.request(http.MethodGet, "/api/v1/assets/a/upstream?depth=1", "")
	json.Unmarshal(rec.Body.Bytes(), &envelope)
	if len(envelope.Upstream) != 1 || envelope.Upstream[0].ID != "b" {
		t.Fatalf("a must keep its original edge set, got %+v", envelope.Upstream)
	}
}

func TestDownstreamEdgeSymmetry(t *testing.T) {
	srv := newTestServer(t)
	srv.register(t, registerBody("a", "t", "o", `[]`, `[]`))
	srv.register(t, registerBody("b", "t", "o", `[]`, `["a"]`))

	rec := srv.request(http.MethodGet, "/api/v1/assets", "")
	assets := assetList(t, rec)
	for _, item := range assets {
		switch item["id"] {
		case "a":
			if d := item["downstream"].([]any); len(d) != 0 {
				t.Fatalf("a downstream: %v", d)
			}
			if u := item["upstream"].([]any); len(u) != 1 || u[0] != "b" {
				t.Fatalf("a should list b as upstream: %v", u)
			}
		case "b":
			if d := item["downstream"].([]any); len(d) != 1 || d[0] != "a" {
				t.Fatalf("b downstream: %v", d)
			}
		}
	}
}
