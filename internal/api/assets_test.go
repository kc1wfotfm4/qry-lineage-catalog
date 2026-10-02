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

func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st)
}

func doRequest(t *testing.T, router http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, target, reader)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

const validAsset = `{"id":"a1","type":"table","owner":"alice","upstream":[],"downstream":[]}`

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

func expectErrorCode(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", recorder.Code, status, recorder.Body.String())
	}
	body := decodeBody(t, recorder)
	errorBody, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error object: %s", recorder.Body.String())
	}
	if errorBody["code"] != code {
		t.Fatalf("code = %v, want %s", errorBody["code"], code)
	}
	for _, forbidden := range []string{"SELECT", "INSERT", "DELETE", ".go:", "store_test", "/opt/"} {
		if bytes.Contains(recorder.Body.Bytes(), []byte(forbidden)) {
			t.Fatalf("response leaks internals (%s): %s", forbidden, recorder.Body.String())
		}
	}
}

func TestPostAssetCreatesAndUpdates(t *testing.T) {
	router := newTestRouter(t)

	created := doRequest(t, router, http.MethodPost, "/api/v1/assets", validAsset)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", created.Code)
	}
	asset := decodeBody(t, created)
	if asset["id"] != "a1" || asset["type"] != "table" || asset["owner"] != "alice" {
		t.Fatalf("unexpected asset: %s", created.Body.String())
	}
	upstream, ok := asset["upstream"].([]any)
	if !ok || len(upstream) != 0 {
		t.Fatalf("upstream must be an empty array: %s", created.Body.String())
	}

	updated := doRequest(t, router, http.MethodPost, "/api/v1/assets",
		`{"id":"a1","type":"job","owner":"bob","upstream":[],"downstream":[]}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200", updated.Code)
	}
	asset = decodeBody(t, updated)
	if asset["type"] != "job" || asset["owner"] != "bob" {
		t.Fatalf("update body = %s", updated.Body.String())
	}
}

func TestPostAssetValidationErrors(t *testing.T) {
	router := newTestRouter(t)
	doRequest(t, router, http.MethodPost, "/api/v1/assets", validAsset)
	doRequest(t, router, http.MethodPost, "/api/v1/assets", validAsset)

	bodies := map[string]string{
		"not json":            `{not json`,
		"empty":               ``,
		"json array":          `["a"]`,
		"missing id":          `{"type":"t","owner":"o","upstream":[],"downstream":[]}`,
		"blank type":          `{"id":"x","type":"","owner":"o","upstream":[],"downstream":[]}`,
		"wrong owner type":    `{"id":"x","type":"t","owner":7,"upstream":[],"downstream":[]}`,
		"missing upstream":    `{"id":"x","type":"t","owner":"o","downstream":[]}`,
		"upstream wrong type": `{"id":"x","type":"t","owner":"o","upstream":"a1","downstream":[]}`,
		"downstream null":     `{"id":"x","type":"t","owner":"o","upstream":[],"downstream":null}`,
		"empty ref":           `{"id":"x","type":"t","owner":"o","upstream":[""],"downstream":[]}`,
		"unknown upstream":    `{"id":"x","type":"t","owner":"o","upstream":["ghost"],"downstream":[]}`,
	}
	invalidRequest := map[string]bool{"not json": true, "empty": true, "json array": true}
	for name, body := range bodies {
		recorder := doRequest(t, router, http.MethodPost, "/api/v1/assets", body)
		if invalidRequest[name] {
			expectErrorCode(t, recorder, http.StatusBadRequest, "invalid_request")
		} else {
			expectErrorCode(t, recorder, http.StatusBadRequest, "invalid_asset")
		}
	}

	// A rejected asset must never be written.
	listing := doRequest(t, router, http.MethodGet, "/api/v1/assets", "")
	var listed []map[string]any
	if err := json.Unmarshal(listing.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(listed) != 1 || listed[0]["id"] != "a1" {
		t.Fatalf("rejected writes leaked: %s", listing.Body.String())
	}
}

func TestListAssetsFilterAndOrder(t *testing.T) {
	router := newTestRouter(t)
	post := func(body string) {
		t.Helper()
		if recorder := doRequest(t, router, http.MethodPost, "/api/v1/assets", body); recorder.Code != http.StatusCreated && recorder.Code != http.StatusOK {
			t.Fatalf("seed failed: %s", recorder.Body.String())
		}
	}
	post(`{"id":"z","type":"job","owner":"o","upstream":[],"downstream":[]}`)
	post(`{"id":"a","type":"table","owner":"o","upstream":[],"downstream":[]}`)
	post(`{"id":"m","type":"table","owner":"o","upstream":[],"downstream":[]}`)

	all := doRequest(t, router, http.MethodGet, "/api/v1/assets", "")
	var allAssets []map[string]any
	if err := json.Unmarshal(all.Body.Bytes(), &allAssets); err != nil {
		t.Fatalf("decode all: %v", err)
	}
	if len(allAssets) != 3 || allAssets[0]["id"] != "a" || allAssets[2]["id"] != "z" {
		t.Fatalf("unexpected order: %s", all.Body.String())
	}

	tables := doRequest(t, router, http.MethodGet, "/api/v1/assets?type=table", "")
	var tableAssets []map[string]any
	if err := json.Unmarshal(tables.Body.Bytes(), &tableAssets); err != nil {
		t.Fatalf("decode tables: %v", err)
	}
	if len(tableAssets) != 2 || tableAssets[0]["id"] != "a" || tableAssets[1]["id"] != "m" {
		t.Fatalf("type filter: %s", tables.Body.String())
	}
}

func TestUpstreamGraphDepthAndOrder(t *testing.T) {
	router := newTestRouter(t)
	post := func(body string) {
		t.Helper()
		if recorder := doRequest(t, router, http.MethodPost, "/api/v1/assets", body); recorder.Code > 201 {
			t.Fatalf("seed failed: %s", recorder.Body.String())
		}
	}
	// a <- b, a <- d, b <- c, d <- c, c <- e
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		post(`{"id":"` + id + `","type":"t","owner":"o","upstream":[],"downstream":[]}`)
	}
	post(`{"id":"a","type":"t","owner":"o","upstream":["b","d"],"downstream":[]}`)
	post(`{"id":"b","type":"t","owner":"o","upstream":["c"],"downstream":[]}`)
	post(`{"id":"d","type":"t","owner":"o","upstream":["c"],"downstream":[]}`)
	post(`{"id":"c","type":"t","owner":"o","upstream":["e"],"downstream":[]}`)

	response := doRequest(t, router, http.MethodGet, "/api/v1/assets/a/upstream", "")
	body := decodeBody(t, response)
	if body["id"] != "a" {
		t.Fatalf("missing target id: %s", response.Body.String())
	}
	nodes, ok := body["upstream"].([]any)
	if !ok || len(nodes) != 4 {
		t.Fatalf("upstream = %s", response.Body.String())
	}
	want := []struct {
		id       string
		distance float64
	}{{"b", 1}, {"d", 1}, {"c", 2}, {"e", 3}}
	for i, w := range want {
		node := nodes[i].(map[string]any)
		if node["id"] != w.id || node["distance"] != w.distance {
			t.Fatalf("position %d = %v, want %s@%v", i, node, w.id, w.distance)
		}
	}

	direct := doRequest(t, router, http.MethodGet, "/api/v1/assets/a/upstream?depth=1", "")
	directBody := decodeBody(t, direct)
	directNodes := directBody["upstream"].([]any)
	if len(directNodes) != 2 {
		t.Fatalf("depth 1 = %s", direct.Body.String())
	}
}

func TestUpstreamDepthValidation(t *testing.T) {
	router := newTestRouter(t)
	doRequest(t, router, http.MethodPost, "/api/v1/assets", validAsset)

	for _, depth := range []string{"0", "11", "1.5", "abc", "-1", "10"} {
		recorder := doRequest(t, router, http.MethodGet, "/api/v1/assets/a1/upstream?depth="+depth, "")
		if depth == "10" {
			if recorder.Code != http.StatusOK {
				t.Fatalf("depth=%s should be accepted, got %d", depth, recorder.Code)
			}
			continue
		}
		expectErrorCode(t, recorder, http.StatusBadRequest, "invalid_depth")
	}
}

func TestUpstreamUnknownAsset(t *testing.T) {
	router := newTestRouter(t)
	expectErrorCode(t,
		doRequest(t, router, http.MethodGet, "/api/v1/assets/ghost/upstream", ""),
		http.StatusNotFound, "asset_not_found")
	// Unknown asset wins over depth validation: the path resource must exist first.
	expectErrorCode(t,
		doRequest(t, router, http.MethodGet, "/api/v1/assets/ghost/upstream?depth=3", ""),
		http.StatusNotFound, "asset_not_found")
}

func TestPostAssetRejectsCycleWithoutMutation(t *testing.T) {
	router := newTestRouter(t)
	post := func(body string) {
		t.Helper()
		if recorder := doRequest(t, router, http.MethodPost, "/api/v1/assets", body); recorder.Code > 201 {
			t.Fatalf("seed failed: %s", recorder.Body.String())
		}
	}
	post(`{"id":"a","type":"t","owner":"o","upstream":[],"downstream":[]}`)
	post(`{"id":"b","type":"t","owner":"o","upstream":[],"downstream":[]}`)
	post(`{"id":"c","type":"t","owner":"o","upstream":[],"downstream":[]}`)
	post(`{"id":"a","type":"t","owner":"o","upstream":[],"downstream":["b"]}`)
	post(`{"id":"b","type":"t","owner":"o","upstream":["a"],"downstream":["c"]}`)
	post(`{"id":"c","type":"t","owner":"o","upstream":["b"],"downstream":[]}`)

	// Closing a -> b -> c -> a.
	expectErrorCode(t,
		doRequest(t, router, http.MethodPost, "/api/v1/assets",
			`{"id":"c","type":"t","owner":"o","upstream":["b"],"downstream":["a"]}`),
		http.StatusConflict, "lineage_cycle")

	// c's lists must remain as previously committed.
	response := doRequest(t, router, http.MethodGet, "/api/v1/assets?type=t", "")
	var assets []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &assets); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, asset := range assets {
		if asset["id"] == "c" {
			downstream := asset["downstream"].([]any)
			if len(downstream) != 0 {
				t.Fatalf("cycle write leaked into c: %s", response.Body.String())
			}
		}
	}

	// An upstream walk must not observe a rejected edge.
	upstream := doRequest(t, router, http.MethodGet, "/api/v1/assets/a/upstream", "")
	upstreamBody := decodeBody(t, upstream)
	if nodes := upstreamBody["upstream"].([]any); len(nodes) != 0 {
		t.Fatalf("a should have no reachable upstream, got %s", upstream.Body.String())
	}
}

func TestBaselineRoutesStillPublished(t *testing.T) {
	router := newTestRouter(t)
	if recorder := doRequest(t, router, http.MethodGet, "/healthz", ""); recorder.Code != http.StatusOK {
		t.Fatalf("healthz = %d", recorder.Code)
	}
	expectErrorCode(t,
		doRequest(t, router, http.MethodGet, "/missing", ""),
		http.StatusNotFound, "route_not_found")
}
