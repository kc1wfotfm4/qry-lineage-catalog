// Package store owns the SQLite file and every write the service performs.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// Sentinel errors callers map onto the published HTTP codes.
var (
	ErrInvalidAsset  = errors.New("invalid asset")
	ErrLineageCycle  = errors.New("lineage cycle")
	ErrAssetNotFound = errors.New("asset not found")
)

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Asset is the stored representation of a registered catalog entry.
type Asset struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Owner      string   `json:"owner"`
	Upstream   []string `json:"upstream"`
	Downstream []string `json:"downstream"`
}

// UpstreamAsset is an ancestor asset annotated with its shortest distance from
// the query target.
type UpstreamAsset struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Owner      string   `json:"owner"`
	Upstream   []string `json:"upstream"`
	Downstream []string `json:"downstream"`
	Distance   int      `json:"distance"`
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	dsn := buildDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func buildDSN(path string) string {
	query := url.Values{}
	query.Set("_pragma", "busy_timeout(5000)")
	query.Set("_pragma", "foreign_keys(1)")
	query.Set("_txlock", "immediate")
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return path + "?" + query.Encode()
	}
	return "file:" + path + "?" + query.Encode()
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS service_metadata (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS assets (
	id    TEXT PRIMARY KEY,
	type  TEXT NOT NULL,
	owner TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS edges (
	upstream_id    TEXT NOT NULL,
	downstream_id  TEXT NOT NULL,
	upstream_pos   INTEGER NOT NULL DEFAULT 0,
	downstream_pos INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (upstream_id, downstream_id)
);

CREATE INDEX IF NOT EXISTS idx_edges_downstream ON edges (downstream_id);
`

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// loadAssets hydrates full assets (both dependency lists) from the rows the
// query produced. Rows must select id, type, owner.
func loadAssets(ctx context.Context, q queryer, rows *sql.Rows) ([]*Asset, error) {
	ids := []string{}
	result := []*Asset{}
	assets := map[string]*Asset{}
	for rows.Next() {
		asset := &Asset{Upstream: []string{}, Downstream: []string{}}
		if err := rows.Scan(&asset.ID, &asset.Type, &asset.Owner); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, asset.ID)
		assets[asset.ID] = asset
		result = append(result, asset)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := fillEdges(ctx, q, assets, ids); err != nil {
		return nil, err
	}
	return result, nil
}

func fillEdges(ctx context.Context, q queryer, assets map[string]*Asset, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	query := "SELECT downstream_id, upstream_id, downstream_pos FROM edges WHERE downstream_id IN (" + placeholders + ") ORDER BY downstream_pos, upstream_id"
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var downstreamID, upstreamID string
		var pos int
		if err := rows.Scan(&downstreamID, &upstreamID, &pos); err != nil {
			rows.Close()
			return err
		}
		if asset, ok := assets[downstreamID]; ok {
			asset.Upstream = append(asset.Upstream, upstreamID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	query = "SELECT upstream_id, downstream_id, upstream_pos FROM edges WHERE upstream_id IN (" + placeholders + ") ORDER BY upstream_pos, downstream_id"
	rows, err = q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var upstreamID, downstreamID string
		var pos int
		if err := rows.Scan(&upstreamID, &downstreamID, &pos); err != nil {
			rows.Close()
			return err
		}
		if asset, ok := assets[upstreamID]; ok {
			asset.Downstream = append(asset.Downstream, downstreamID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
}

// ListAssets returns every asset, ordered by id ascending.
func (s *Store) ListAssets(ctx context.Context) ([]*Asset, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, type, owner FROM assets ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	return loadAssets(ctx, s.db, rows)
}

// ListAssetsByType returns assets whose type matches exactly, ordered by id.
func (s *Store) ListAssetsByType(ctx context.Context, assetType string) ([]*Asset, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, type, owner FROM assets WHERE type = ? ORDER BY id ASC", assetType)
	if err != nil {
		return nil, err
	}
	return loadAssets(ctx, s.db, rows)
}

// Upstream returns every reachable ancestor of id, each annotated with the
// shortest distance. Results are ordered by distance then id. maxDepth 0 means
// unlimited; otherwise traversal stops at maxDepth (which must be 1..10).
func (s *Store) Upstream(ctx context.Context, id string, maxDepth int) (*UpstreamAsset, []*UpstreamAsset, error) {
	target, err := s.getAsset(ctx, s.db, id)
	if err != nil {
		return nil, nil, err
	}

	distances := map[string]int{}
	frontier := []string{id}
	for distance := 1; len(frontier) > 0 && (maxDepth == 0 || distance <= maxDepth); distance++ {
		next := []string{}
		query, args := inQuery(
			"SELECT downstream_id, upstream_id FROM edges WHERE downstream_id IN (",
			frontier,
		)
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var downstreamID, upstreamID string
			if err := rows.Scan(&downstreamID, &upstreamID); err != nil {
				rows.Close()
				return nil, nil, err
			}
			if upstreamID == id {
				continue
			}
			if _, seen := distances[upstreamID]; seen || upstreamID == id {
				continue
			}
			distances[upstreamID] = distance
			next = append(next, upstreamID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rows.Close()
		frontier = next
	}

	ancestorIDs := make([]string, 0, len(distances))
	for ancestorID := range distances {
		ancestorIDs = append(ancestorIDs, ancestorID)
	}
	sort.Slice(ancestorIDs, func(i, j int) bool {
		if distances[ancestorIDs[i]] != distances[ancestorIDs[j]] {
			return distances[ancestorIDs[i]] < distances[ancestorIDs[j]]
		}
		return ancestorIDs[i] < ancestorIDs[j]
	})

	ancestors := make([]*UpstreamAsset, 0, len(ancestorIDs))
	if len(ancestorIDs) > 0 {
		query, args := inQuery("SELECT id, type, owner FROM assets WHERE id IN (", ancestorIDs)
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, nil, err
		}
		loaded, err := loadAssets(ctx, s.db, rows)
		if err != nil {
			return nil, nil, err
		}
		byID := make(map[string]*Asset, len(loaded))
		for _, asset := range loaded {
			byID[asset.ID] = asset
		}
		for _, ancestorID := range ancestorIDs {
			asset := byID[ancestorID]
			ancestors = append(ancestors, &UpstreamAsset{
				ID:         asset.ID,
				Type:       asset.Type,
				Owner:      asset.Owner,
				Upstream:   asset.Upstream,
				Downstream: asset.Downstream,
				Distance:   distances[ancestorID],
			})
		}
	}
	return &UpstreamAsset{ID: target.ID, Type: target.Type, Owner: target.Owner, Upstream: target.Upstream, Downstream: target.Downstream}, ancestors, nil
}

func (s *Store) getAsset(ctx context.Context, q queryer, id string) (*Asset, error) {
	rows, err := q.QueryContext(ctx, "SELECT id, type, owner FROM assets WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	assets, err := loadAssets(ctx, q, rows)
	if err != nil {
		return nil, err
	}
	if len(assets) == 0 {
		return nil, ErrAssetNotFound
	}
	return assets[0], nil
}

func inQuery(prefix string, ids []string) (string, []any) {
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return prefix + placeholders + ")", args
}

// UpsertAsset registers a new asset or fully replaces an existing asset's
// fields and both dependency lists in a single transaction. Nothing is written
// when validation fails or the new lineage would contain a cycle.
func (s *Store) UpsertAsset(ctx context.Context, asset *Asset) (bool, error) {
	if asset == nil || strings.TrimSpace(asset.ID) == "" || strings.TrimSpace(asset.Type) == "" || strings.TrimSpace(asset.Owner) == "" {
		return false, ErrInvalidAsset
	}
	asset.Upstream = dedupe(asset.Upstream)
	asset.Downstream = dedupe(asset.Downstream)
	for _, ref := range asset.Upstream {
		if strings.TrimSpace(ref) == "" {
			return false, ErrInvalidAsset
		}
	}
	for _, ref := range asset.Downstream {
		if strings.TrimSpace(ref) == "" {
			return false, ErrInvalidAsset
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var existed int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(1) FROM assets WHERE id = ?", asset.ID).Scan(&existed); err != nil {
		return false, err
	}

	for _, list := range [][]string{asset.Upstream, asset.Downstream} {
		if len(list) == 0 {
			continue
		}
		query, args := inQuery("SELECT COUNT(1) FROM assets WHERE id IN (", list)
		var found int
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&found); err != nil {
			return false, err
		}
		if found != len(list) {
			return false, ErrInvalidAsset
		}
	}

	if err := ensureNoCycle(ctx, tx, asset); err != nil {
		return false, err
	}

	if existed == 0 {
		if _, err := tx.ExecContext(ctx, "INSERT INTO assets (id, type, owner) VALUES (?, ?, ?)", asset.ID, asset.Type, asset.Owner); err != nil {
			return false, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, "UPDATE assets SET type = ?, owner = ? WHERE id = ?", asset.Type, asset.Owner, asset.ID); err != nil {
			return false, err
		}
	}

	if err := replaceEdges(ctx, tx, asset); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return existed == 0, nil
}

func dedupe(values []string) []string {
	seen := map[string]struct{}{}
	result := []string{}
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// ensureNoCycle checks whether the lineage after applying the new dependency
// lists would let asset.ID reach itself through upstream edges.
func ensureNoCycle(ctx context.Context, tx *sql.Tx, asset *Asset) error {
	direct := map[string]bool{}
	for _, upstreamID := range asset.Upstream {
		direct[upstreamID] = true
	}
	if direct[asset.ID] {
		return ErrLineageCycle
	}

	neighbors := func(node string) ([]string, error) {
		if node == asset.ID {
			return asset.Upstream, nil
		}
		rows, err := tx.QueryContext(ctx, "SELECT upstream_id FROM edges WHERE downstream_id = ? ORDER BY upstream_id", node)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		result := []string{}
		for rows.Next() {
			var upstreamID string
			if err := rows.Scan(&upstreamID); err != nil {
				return nil, err
			}
			result = append(result, upstreamID)
		}
		return result, rows.Err()
	}

	visited := map[string]bool{asset.ID: true}
	frontier := append([]string{}, asset.Upstream...)
	for len(frontier) > 0 {
		node := frontier[0]
		frontier = frontier[1:]
		if node == asset.ID {
			return ErrLineageCycle
		}
		if visited[node] {
			continue
		}
		visited[node] = true
		next, err := neighbors(node)
		if err != nil {
			return err
		}
		for _, upstreamID := range next {
			if upstreamID == asset.ID {
				return ErrLineageCycle
			}
			if !visited[upstreamID] {
				frontier = append(frontier, upstreamID)
			}
		}
	}
	return nil
}

// replaceEdges deletes every edge touching asset.ID and inserts the two new
// dependency lists, preserving list positions owned by the other endpoint when
// that endpoint's own list is not being replaced.
func replaceEdges(ctx context.Context, tx *sql.Tx, asset *Asset) error {
	preservedUpstreamPos := map[string]int{}
	preservedDownstreamPos := map[string]int{}

	rows, err := tx.QueryContext(ctx,
		"SELECT upstream_id, downstream_id, upstream_pos, downstream_pos FROM edges WHERE upstream_id = ? OR downstream_id = ?",
		asset.ID, asset.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var upstreamID, downstreamID string
		var upstreamPos, downstreamPos int
		if err := rows.Scan(&upstreamID, &downstreamID, &upstreamPos, &downstreamPos); err != nil {
			rows.Close()
			return err
		}
		if downstreamID == asset.ID {
			preservedUpstreamPos[upstreamID] = upstreamPos
		}
		if upstreamID == asset.ID {
			preservedDownstreamPos[downstreamID] = downstreamPos
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if _, err := tx.ExecContext(ctx, "DELETE FROM edges WHERE upstream_id = ? OR downstream_id = ?", asset.ID, asset.ID); err != nil {
		return err
	}

	for pos, upstreamID := range asset.Upstream {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO edges (upstream_id, downstream_id, upstream_pos, downstream_pos) VALUES (?, ?, ?, ?)",
			upstreamID, asset.ID, preservedUpstreamPos[upstreamID], pos); err != nil {
			return err
		}
	}
	for pos, downstreamID := range asset.Downstream {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO edges (upstream_id, downstream_id, upstream_pos, downstream_pos) VALUES (?, ?, ?, ?)",
			asset.ID, downstreamID, pos, preservedDownstreamPos[downstreamID]); err != nil {
			return err
		}
	}
	return nil
}
