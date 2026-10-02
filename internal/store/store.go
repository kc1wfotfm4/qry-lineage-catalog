// Package store owns the SQLite file and every write the service performs.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sort"

	_ "modernc.org/sqlite"
)

// Sentinel errors describe the business failures the HTTP layer maps to status codes.
var (
	ErrInvalidAsset  = errors.New("the asset or one of its references is invalid")
	ErrLineageCycle  = errors.New("registering this asset would create a dependency cycle")
	ErrAssetNotFound = errors.New("the requested asset does not exist")
)

// Asset is the full asset object returned by every read and write.
type Asset struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Owner      string   `json:"owner"`
	Upstream   []string `json:"upstream"`
	Downstream []string `json:"downstream"`
}

// UpstreamAsset is an asset reached while walking upstream, annotated with its shortest
// distance from the query target.
type UpstreamAsset struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Owner      string   `json:"owner"`
	Upstream   []string `json:"upstream"`
	Downstream []string `json:"downstream"`
	Distance   int      `json:"distance"`
}

// Store wraps the SQLite handle so callers never touch database/sql directly.
type Store struct {
	db *sql.DB
}

// Open prepares the database file and the schema this service needs.
func Open(path string) (*Store, error) {
	query := url.Values{"_pragma": {"foreign_keys(on)"}}.Encode()
	db, err := sql.Open("sqlite", "file:"+path+"?"+query)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection serializes all access, so write transactions can never contend on
	// SQLite's one-writer lock and every read sees a fully committed state.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Ping reports whether the storage layer is usable.
func (s *Store) Ping() error { return s.db.Ping() }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// PutAsset registers a new asset or fully replaces the fields and both dependency lists of an
// existing one. All writes happen in one immediate transaction: validation, cycle detection and
// edge replacement are atomic, so a rejected registration leaves every existing relation intact.
// created reports whether the asset was newly registered (true) or updated (false).
func (s *Store) PutAsset(asset Asset) (result Asset, created bool, err error) {
	for _, ref := range append(append([]string{}, asset.Upstream...), asset.Downstream...) {
		if ref == "" {
			return Asset{}, false, ErrInvalidAsset
		}
	}
	asset.Upstream = dedupeNonEmpty(asset.Upstream)
	asset.Downstream = dedupeNonEmpty(asset.Downstream)
	if asset.ID == "" || asset.Type == "" || asset.Owner == "" {
		return Asset{}, false, ErrInvalidAsset
	}

	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return Asset{}, false, err
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	var existed bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM assets WHERE id = ?)`, asset.ID).Scan(&existed); err != nil {
		return Asset{}, false, err
	}

	for _, ref := range append(append([]string{}, asset.Upstream...), asset.Downstream...) {
		var found bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM assets WHERE id = ?)`, ref).Scan(&found); err != nil {
			return Asset{}, false, err
		}
		if !found {
			return Asset{}, false, ErrInvalidAsset
		}
	}

	down := map[string]map[string]bool{}
	addEdge := func(from, to string) {
		if down[from] == nil {
			down[from] = map[string]bool{}
		}
		down[from][to] = true
	}
	edgeRows, err := tx.Query(`SELECT from_id, to_id FROM asset_refs`)
	if err != nil {
		return Asset{}, false, err
	}
	for edgeRows.Next() {
		var from, to string
		if err = edgeRows.Scan(&from, &to); err != nil {
			edgeRows.Close()
			return Asset{}, false, err
		}
		addEdge(from, to)
	}
	if err = edgeRows.Err(); err != nil {
		edgeRows.Close()
		return Asset{}, false, err
	}
	edgeRows.Close()

	down[asset.ID] = map[string]bool{}
	for _, upstream := range asset.Upstream {
		addEdge(upstream, asset.ID)
	}
	for _, downstream := range asset.Downstream {
		addEdge(asset.ID, downstream)
	}

	if reaches(down, asset.ID, asset.ID) {
		return Asset{}, false, ErrLineageCycle
	}

	if _, err = tx.Exec(
		`INSERT INTO assets (id, type, owner) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET type = excluded.type, owner = excluded.owner`,
		asset.ID, asset.Type, asset.Owner); err != nil {
		return Asset{}, false, err
	}
	if _, err = tx.Exec(`DELETE FROM asset_refs WHERE owner_id = ?`, asset.ID); err != nil {
		return Asset{}, false, err
	}
	for _, upstream := range asset.Upstream {
		if _, err = tx.Exec(
			`INSERT OR IGNORE INTO asset_refs (owner_id, direction, from_id, to_id) VALUES (?, ?, ?, ?)`,
			asset.ID, "upstream", upstream, asset.ID); err != nil {
			return Asset{}, false, err
		}
	}
	for _, downstream := range asset.Downstream {
		if _, err = tx.Exec(
			`INSERT OR IGNORE INTO asset_refs (owner_id, direction, from_id, to_id) VALUES (?, ?, ?, ?)`,
			asset.ID, "downstream", asset.ID, downstream); err != nil {
			return Asset{}, false, err
		}
	}

	if err = tx.Commit(); err != nil {
		return Asset{}, false, err
	}
	committed = true
	return asset, !existed, nil
}

// ListAssets returns assets ordered by id, optionally restricted to an exact type match.
// An empty typeFilter returns every asset.
func (s *Store) ListAssets(typeFilter string) ([]Asset, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	query := `SELECT id, type, owner FROM assets`
	args := []any{}
	if typeFilter != "" {
		query += ` WHERE type = ?`
		args = append(args, typeFilter)
	}
	query += ` ORDER BY id ASC`

	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	assets := map[string]*Asset{}
	ids := []string{}
	for rows.Next() {
		asset := &Asset{Upstream: []string{}, Downstream: []string{}}
		if err := rows.Scan(&asset.ID, &asset.Type, &asset.Owner); err != nil {
			rows.Close()
			return nil, err
		}
		assets[asset.ID] = asset
		ids = append(ids, asset.ID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	refRows, err := tx.Query(`SELECT owner_id, direction, from_id, to_id FROM asset_refs`)
	if err != nil {
		return nil, err
	}
	defer refRows.Close()
	for refRows.Next() {
		var ownerID, direction, from, to string
		if err := refRows.Scan(&ownerID, &direction, &from, &to); err != nil {
			return nil, err
		}
		asset := assets[ownerID]
		if asset == nil {
			continue
		}
		if direction == "upstream" {
			asset.Upstream = append(asset.Upstream, from)
		} else {
			asset.Downstream = append(asset.Downstream, to)
		}
	}
	if err := refRows.Err(); err != nil {
		return nil, err
	}

	result := make([]Asset, 0, len(ids))
	for _, id := range ids {
		result = append(result, *assets[id])
	}
	return result, nil
}

// Upstream returns every distinct asset reachable from targetID by upstream edges, annotated with
// the shortest distance (direct references are distance 1). maxDepth 0 means no limit; otherwise
// only assets at distance <= maxDepth are returned. Results are ordered by distance then id.
func (s *Store) Upstream(targetID string, maxDepth int) ([]UpstreamAsset, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	assets := map[string]Asset{}
	up := map[string][]string{}

	var targetType, targetOwner string
	findErr := tx.QueryRow(`SELECT type, owner FROM assets WHERE id = ?`, targetID).
		Scan(&targetType, &targetOwner)
	if errors.Is(findErr, sql.ErrNoRows) {
		return nil, ErrAssetNotFound
	}
	if findErr != nil {
		return nil, findErr
	}

	rows, err := tx.Query(`SELECT id, type, owner FROM assets`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var asset Asset
		if err := rows.Scan(&asset.ID, &asset.Type, &asset.Owner); err != nil {
			rows.Close()
			return nil, err
		}
		asset.Upstream = []string{}
		asset.Downstream = []string{}
		assets[asset.ID] = asset
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	refRows, err := tx.Query(`SELECT owner_id, direction, from_id, to_id FROM asset_refs`)
	if err != nil {
		return nil, err
	}
	for refRows.Next() {
		var ownerID, direction, from, to string
		if err := refRows.Scan(&ownerID, &direction, &from, &to); err != nil {
			refRows.Close()
			return nil, err
		}
		asset := assets[ownerID]
		if direction == "upstream" {
			asset.Upstream = append(asset.Upstream, from)
			up[to] = append(up[to], from)
		} else {
			asset.Downstream = append(asset.Downstream, to)
		}
		assets[ownerID] = asset
	}
	if err := refRows.Err(); err != nil {
		refRows.Close()
		return nil, err
	}
	refRows.Close()

	distances := map[string]int{}
	frontier := []string{targetID}
	for depth := 1; len(frontier) > 0 && (maxDepth == 0 || depth <= maxDepth); depth++ {
		next := []string{}
		for _, node := range frontier {
			for _, ancestor := range up[node] {
				if _, seen := distances[ancestor]; seen {
					continue
				}
				if ancestor == targetID {
					continue
				}
				distances[ancestor] = depth
				next = append(next, ancestor)
			}
		}
		frontier = next
	}

	result := make([]UpstreamAsset, 0, len(distances))
	for id, distance := range distances {
		asset := assets[id]
		result = append(result, UpstreamAsset{
			ID:         asset.ID,
			Type:       asset.Type,
			Owner:      asset.Owner,
			Upstream:   asset.Upstream,
			Downstream: asset.Downstream,
			Distance:   distance,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Distance != result[j].Distance {
			return result[i].Distance < result[j].Distance
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

// reaches reports whether a walk from start reaches target using the given adjacency map.
func reaches(adjacency map[string]map[string]bool, start, target string) bool {
	visited := map[string]bool{start: true}
	frontier := []string{start}
	for len(frontier) > 0 {
		node := frontier[0]
		frontier = frontier[1:]
		for next := range adjacency[node] {
			if next == target {
				return true
			}
			if !visited[next] {
				visited[next] = true
				frontier = append(frontier, next)
			}
		}
	}
	return false
}

func dedupeNonEmpty(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

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

CREATE TABLE IF NOT EXISTS asset_refs (
	owner_id  TEXT NOT NULL,
	direction TEXT NOT NULL CHECK (direction IN ('upstream', 'downstream')),
	from_id   TEXT NOT NULL,
	to_id     TEXT NOT NULL,
	PRIMARY KEY (owner_id, direction, from_id, to_id),
	FOREIGN KEY (owner_id) REFERENCES assets(id) ON DELETE CASCADE,
	FOREIGN KEY (from_id) REFERENCES assets(id),
	FOREIGN KEY (to_id) REFERENCES assets(id)
);

CREATE INDEX IF NOT EXISTS idx_asset_refs_edge ON asset_refs (from_id, to_id);
`
