// Copyright 2026 Aeneas Rekkas
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
)

// CollectionSnapshot is a read-only view of a shared collection and, when it
// has a membership row, of one project inside it. Counts use the same
// definitions as CollectionStats.
type CollectionSnapshot struct {
	// Shared and Legacy are mutually exclusive; when Legacy is set all other
	// fields are zero.
	Shared           bool
	Legacy           bool
	Dimensions       int
	VectorStorage    string
	Project          *ProjectSnapshot
	Projects         []ProjectSummary
	UniqueVectors    int
	ChunkReferences  int
	SharedReferences int
	DatabaseBytes    int64
	ReclaimableBytes int64
}

// ProjectSnapshot holds one project's membership counts and all of its
// project_meta values.
type ProjectSnapshot struct {
	Path   string
	Files  int
	Chunks int
	Meta   map[string]string
}

// ProjectSummary describes one project membership of a collection.
// LastIndexedAt is empty when the project has never completed an index.
type ProjectSummary struct {
	Path           string
	LastAccessedAt string
	LastIndexedAt  string
}

// openReadOnly opens an existing database without creating it or writing to
// it; a missing file yields an error wrapping os.ErrNotExist.
func openReadOnly(dbPath string) (*sql.DB, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("stat %s: %w", dbPath, err)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath), RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA query_only=ON", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("exec %q: %w", pragma, err)
		}
	}
	return db, nil
}

// ReadCollectionSnapshot reads dbPath without creating it, running schema
// setup, registering projectPath, or stamping access times. A missing file
// yields an error wrapping os.ErrNotExist; a file holding neither the shared
// nor the legacy schema yields an error.
func ReadCollectionSnapshot(dbPath, projectPath string) (CollectionSnapshot, error) {
	var snap CollectionSnapshot
	db, err := openReadOnly(dbPath)
	if err != nil {
		return snap, err
	}
	defer func() { _ = db.Close() }()

	shared, err := checkTableExists(db, "collection_meta")
	if err != nil {
		return snap, fmt.Errorf("check collection_meta table: %w", err)
	}
	if !shared {
		legacy, err := checkTableExists(db, "files")
		if err != nil {
			return snap, fmt.Errorf("check files table: %w", err)
		}
		if !legacy {
			return snap, fmt.Errorf("%s is not a lumen index", dbPath)
		}
		snap.Legacy = true
		return snap, nil
	}
	snap.Shared = true

	// One read transaction keeps every count consistent with a concurrent
	// indexer's commits.
	tx, err := db.Begin()
	if err != nil {
		return snap, fmt.Errorf("begin read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := readCollectionProfile(tx, &snap); err != nil {
		return snap, err
	}
	if err := tx.QueryRow(`SELECT
		(SELECT count(*) FROM vector_keys),
		(SELECT count(*) FROM chunk_defs cd JOIN project_files pf ON pf.file_revision_id = cd.file_revision_id)`).Scan(
		&snap.UniqueVectors, &snap.ChunkReferences); err != nil {
		return snap, fmt.Errorf("collection counts: %w", err)
	}
	snap.SharedReferences = snap.ChunkReferences - snap.UniqueVectors

	var pageCount, pageSize, freelist int64
	for pragma, dst := range map[string]*int64{"page_count": &pageCount, "page_size": &pageSize, "freelist_count": &freelist} {
		if err := tx.QueryRow(`PRAGMA ` + pragma).Scan(dst); err != nil {
			return snap, fmt.Errorf("pragma %s: %w", pragma, err)
		}
	}
	snap.DatabaseBytes = pageCount * pageSize
	snap.ReclaimableBytes = freelist * pageSize

	if snap.Projects, err = readProjectSummaries(tx); err != nil {
		return snap, err
	}
	if snap.Project, err = readProjectSnapshot(tx, normalizeProjectPath(projectPath)); err != nil {
		return snap, err
	}
	return snap, nil
}

func readCollectionProfile(tx *sql.Tx, snap *CollectionSnapshot) error {
	rows, err := tx.Query(`SELECT key, value FROM collection_meta WHERE key IN ('vec_dimensions', 'vector_storage')`)
	if err != nil {
		return fmt.Errorf("query collection_meta: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return fmt.Errorf("scan collection_meta: %w", err)
		}
		switch key {
		case "vec_dimensions":
			snap.Dimensions, _ = strconv.Atoi(value)
		case "vector_storage":
			snap.VectorStorage = value
		}
	}
	return rows.Err()
}

func readProjectSummaries(tx *sql.Tx) ([]ProjectSummary, error) {
	rows, err := tx.Query(`SELECT p.path, p.last_accessed_at,
		COALESCE((SELECT value FROM project_meta pm WHERE pm.project_id = p.id AND pm.key = 'last_indexed_at'), '')
		FROM projects p ORDER BY p.path`)
	if err != nil {
		return nil, fmt.Errorf("query projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var projects []ProjectSummary
	for rows.Next() {
		var p ProjectSummary
		if err := rows.Scan(&p.Path, &p.LastAccessedAt, &p.LastIndexedAt); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func readProjectSnapshot(tx *sql.Tx, projectPath string) (*ProjectSnapshot, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM projects WHERE path = ?`, projectPath).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select project: %w", err)
	}
	p := &ProjectSnapshot{Path: projectPath, Meta: map[string]string{}}
	if err := tx.QueryRow(`SELECT
		(SELECT count(*) FROM project_files WHERE project_id = ?),
		(SELECT count(*) FROM chunk_defs cd JOIN project_files pf ON pf.file_revision_id = cd.file_revision_id WHERE pf.project_id = ?)`,
		id, id).Scan(&p.Files, &p.Chunks); err != nil {
		return nil, fmt.Errorf("project counts: %w", err)
	}
	rows, err := tx.Query(`SELECT key, value FROM project_meta WHERE project_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("query project_meta: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("scan project_meta: %w", err)
		}
		p.Meta[key] = value
	}
	return p, rows.Err()
}
