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
	"math"

	"github.com/ory/lumen/internal/sqlitevec"
)

var (
	// ErrNotCollection means the database holds the legacy schema or is not a
	// lumen index.
	ErrNotCollection = errors.New("not a shared collection index")
	// ErrProjectNotFound means the project has no row in the collection.
	ErrProjectNotFound = errors.New("project not found in collection")
	// ErrCorruptIndex means a chunk has no stored vector or a vector's length
	// disagrees with the collection's dimensions.
	ErrCorruptIndex = errors.New("corrupt index")
)

// ChunkRef locates one indexed chunk and the vector that embeds it.
type ChunkRef struct {
	RelativePath string
	Symbol       string
	Kind         string
	StartLine    int
	EndLine      int
	VectorID     int64
}

// ProjectVectors holds the chunks of a project's complete file revisions and
// their embeddings.
type ProjectVectors struct {
	Chunks []ChunkRef
	// Vectors holds one L2-normalised entry per distinct VectorID. Zero-norm
	// vectors are absent.
	Vectors map[int64][]float32
	// IncompleteFiles counts project files whose current revision was left
	// mid-index; their chunks are not in Chunks.
	IncompleteFiles int
}

// ReadProjectVectors loads every chunk and embedding of projectPath from dbPath
// without creating the file, registering the project, or stamping access
// times. A missing file yields an error wrapping os.ErrNotExist.
func ReadProjectVectors(dbPath, projectPath string) (*ProjectVectors, error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	shared, err := checkTableExists(db, "collection_meta")
	if err != nil {
		return nil, fmt.Errorf("check collection_meta table: %w", err)
	}
	if !shared {
		return nil, fmt.Errorf("%w: %s has no shared collection schema", ErrNotCollection, dbPath)
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var profile CollectionSnapshot
	if err := readCollectionProfile(tx, &profile); err != nil {
		return nil, err
	}
	if profile.Dimensions <= 0 || (profile.VectorStorage != "int8" && profile.VectorStorage != "float32") {
		return nil, fmt.Errorf("%w: %s has vector profile %q/%d", ErrCorruptIndex, dbPath, profile.VectorStorage, profile.Dimensions)
	}

	normalized := normalizeProjectPath(projectPath)
	var projectID int64
	err = tx.QueryRow(`SELECT id FROM projects WHERE path = ?`, normalized).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrProjectNotFound, normalized)
	}
	if err != nil {
		return nil, fmt.Errorf("select project: %w", err)
	}

	pv := &ProjectVectors{Vectors: map[int64][]float32{}}
	if err := tx.QueryRow(`SELECT count(*) FROM project_files pf
		JOIN file_revisions fr ON fr.id = pf.file_revision_id
		WHERE pf.project_id = ? AND fr.complete = 0`, projectID).Scan(&pv.IncompleteFiles); err != nil {
		return nil, fmt.Errorf("count incomplete files: %w", err)
	}

	rows, err := tx.Query(`SELECT pf.relative_path, cd.symbol, cd.kind, cd.start_line, cd.end_line, cd.vector_id, v.embedding
		FROM project_files pf
		JOIN file_revisions fr ON fr.id = pf.file_revision_id AND fr.complete = 1
		JOIN chunk_defs cd ON cd.file_revision_id = fr.id
		LEFT JOIN vec_vectors v ON v.vector_id = cd.vector_id
		WHERE pf.project_id = ?
		ORDER BY pf.relative_path, cd.ordinal`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[int64]bool{}
	for rows.Next() {
		var ref ChunkRef
		var blob []byte
		if err := rows.Scan(&ref.RelativePath, &ref.Symbol, &ref.Kind, &ref.StartLine, &ref.EndLine, &ref.VectorID, &blob); err != nil {
			return nil, fmt.Errorf("scan chunk: %w", err)
		}
		pv.Chunks = append(pv.Chunks, ref)
		if seen[ref.VectorID] {
			continue
		}
		seen[ref.VectorID] = true
		if blob == nil {
			return nil, fmt.Errorf("%w: %s: vector %d of %s has no stored embedding", ErrCorruptIndex, dbPath, ref.VectorID, ref.RelativePath)
		}
		vec, ok := decodeStoredVector(blob, profile.VectorStorage, profile.Dimensions)
		if !ok {
			return nil, fmt.Errorf("%w: %s: vector %d is %d bytes, want %d-dimension %s", ErrCorruptIndex, dbPath, ref.VectorID, len(blob), profile.Dimensions, profile.VectorStorage)
		}
		if l2Normalize(vec) {
			pv.Vectors[ref.VectorID] = vec
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read chunks: %w", err)
	}
	return pv, nil
}

// decodeStoredVector decodes a vec_vectors blob. int8 blobs hold signed bytes;
// their per-vector scale is not stored, which cosine similarity does not need.
func decodeStoredVector(blob []byte, storage string, dims int) ([]float32, bool) {
	switch storage {
	case "float32":
		return sqlitevec.DeserializeFloat32(blob, dims)
	case "int8":
		if len(blob) != dims {
			return nil, false
		}
		vec := make([]float32, dims)
		for i, b := range blob {
			vec[i] = float32(int8(b))
		}
		return vec, true
	}
	return nil, false
}

// l2Normalize scales vec to unit length in place and reports false, leaving
// vec untouched, when its norm is zero.
func l2Normalize(vec []float32) bool {
	var sum float64
	for _, x := range vec {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return false
	}
	norm := float32(math.Sqrt(sum))
	for i := range vec {
		vec[i] /= norm
	}
	return true
}
