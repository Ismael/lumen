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
	"crypto/sha256"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ory/lumen/internal/chunker"
)

// seededVectors maps chunk symbol to the raw vector stored for it.
var seededVectors = map[string][]float32{
	"A": {3, -4, 0, 0},
	"B": {0, -1, 2, -2},
	"C": {-1, 0, 0, 0},
	"D": {-2, 2, -2, 2},
	"Q": {0, 0, -5, 12},
}

// seedVectorCollection writes projectA with main.go (A, B), util.go (C),
// dup.go (D, E sharing one vector) and zero.go (Z, an all-zero vector).
// projectB shares main.go and owns own.go (Q).
func seedVectorCollection(t *testing.T, dbPath, storage, projectA, projectB string) {
	t.Helper()
	s, err := NewCollection(dbPath, 4, storage, projectA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	store := func(path, hash string, chunks []chunker.Chunk, vectors map[int][]float32) {
		t.Helper()
		if _, err := s.StoreFileRevision(path, hash, chunks, vectors); err != nil {
			t.Fatal(err)
		}
	}
	store("main.go", "abcd", []chunker.Chunk{
		{ID: "a", FilePath: "main.go", Symbol: "A", Kind: "function", StartLine: 1, EndLine: 2, Content: "func A() {}"},
		{ID: "b", FilePath: "main.go", Symbol: "B", Kind: "method", StartLine: 3, EndLine: 4, Content: "func B() {}"},
	}, map[int][]float32{0: seededVectors["A"], 1: seededVectors["B"]})
	store("util.go", "ef01", []chunker.Chunk{
		{ID: "c", FilePath: "util.go", Symbol: "C", Kind: "function", StartLine: 1, EndLine: 1, Content: "func C() {}"},
	}, map[int][]float32{0: seededVectors["C"]})
	store("dup.go", "0102", []chunker.Chunk{
		{ID: "d", FilePath: "dup.go", Symbol: "D", Kind: "function", StartLine: 1, EndLine: 3, Content: "func same() {}"},
		{ID: "e", FilePath: "dup.go", Symbol: "E", Kind: "function", StartLine: 5, EndLine: 7, Content: "func same() {}"},
	}, map[int][]float32{0: seededVectors["D"]})
	store("zero.go", "0304", []chunker.Chunk{
		{ID: "z", FilePath: "zero.go", Symbol: "Z", Kind: "function", StartLine: 1, EndLine: 1, Content: "func Z() {}"},
	}, map[int][]float32{0: {0, 0, 0, 0}})

	if err := s.UseProject(projectB); err != nil {
		t.Fatal(err)
	}
	if attached, err := s.AttachExistingFileRevision("main.go", "abcd"); err != nil || !attached {
		t.Fatalf("attach main.go to projectB: attached=%v err=%v", attached, err)
	}
	store("own.go", "0506", []chunker.Chunk{
		{ID: "q", FilePath: "own.go", Symbol: "Q", Kind: "type", StartLine: 9, EndLine: 12, Content: "type Q int"},
	}, map[int][]float32{0: seededVectors["Q"]})
}

func normalized(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := float32(math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}

func chunksBySymbol(t *testing.T, pv *ProjectVectors) map[string]ChunkRef {
	t.Helper()
	bySymbol := make(map[string]ChunkRef, len(pv.Chunks))
	for _, c := range pv.Chunks {
		if _, dup := bySymbol[c.Symbol]; dup {
			t.Fatalf("symbol %s appears twice in %+v", c.Symbol, pv.Chunks)
		}
		bySymbol[c.Symbol] = c
	}
	return bySymbol
}

func withRawDB(t *testing.T, dbPath string, fn func(db *sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	fn(db)
}

var storages = []struct {
	name      string
	tolerance float64
}{
	// int8 quantises to 127 levels around the largest component.
	{"int8", 0.01},
	{"float32", 1e-6},
}

func TestReadProjectVectors_DecodesAndNormalises(t *testing.T) {
	for _, st := range storages {
		t.Run(st.name, func(t *testing.T) {
			base := t.TempDir()
			dbPath := filepath.Join(base, "index.db")
			projectA := filepath.Join(base, "a")
			seedVectorCollection(t, dbPath, st.name, projectA, filepath.Join(base, "b"))

			pv, err := ReadProjectVectors(dbPath, projectA)
			if err != nil {
				t.Fatal(err)
			}
			bySymbol := chunksBySymbol(t, pv)
			for _, symbol := range []string{"A", "B", "C", "D"} {
				ref, ok := bySymbol[symbol]
				if !ok {
					t.Fatalf("chunk %s missing from %+v", symbol, pv.Chunks)
				}
				got, ok := pv.Vectors[ref.VectorID]
				if !ok {
					t.Fatalf("no vector for %s", symbol)
				}
				want := normalized(seededVectors[symbol])
				var sum float64
				for i := range want {
					sum += float64(got[i]) * float64(got[i])
					if diff := math.Abs(float64(got[i] - want[i])); diff > st.tolerance {
						t.Errorf("%s[%d] = %v, want %v (diff %v)", symbol, i, got[i], want[i], diff)
					}
				}
				if math.Abs(sum-1) > 1e-5 {
					t.Errorf("%s squared norm = %v, want 1", symbol, sum)
				}
			}
		})
	}
}

func TestReadProjectVectors_ChunkMetadata(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "index.db")
	projectA := filepath.Join(base, "a")
	seedVectorCollection(t, dbPath, "int8", projectA, filepath.Join(base, "b"))

	pv, err := ReadProjectVectors(dbPath, projectA)
	if err != nil {
		t.Fatal(err)
	}
	b := chunksBySymbol(t, pv)["B"]
	if b.RelativePath != "main.go" || b.Kind != "method" || b.StartLine != 3 || b.EndLine != 4 {
		t.Errorf("B = %+v, want main.go method 3-4", b)
	}
}

func TestReadProjectVectors_ScopesToProject(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "index.db")
	projectA := filepath.Join(base, "a")
	projectB := filepath.Join(base, "b")
	seedVectorCollection(t, dbPath, "float32", projectA, projectB)

	tests := []struct {
		name    string
		project string
		want    map[string]string // symbol -> relative path
	}{
		{"project a", projectA, map[string]string{"A": "main.go", "B": "main.go", "C": "util.go", "D": "dup.go", "E": "dup.go", "Z": "zero.go"}},
		{"project b", projectB, map[string]string{"A": "main.go", "B": "main.go", "Q": "own.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pv, err := ReadProjectVectors(dbPath, tt.project)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, c := range pv.Chunks {
				got[c.Symbol] = c.RelativePath
			}
			if len(got) != len(tt.want) || len(pv.Chunks) != len(tt.want) {
				t.Fatalf("chunks = %+v, want %v", pv.Chunks, tt.want)
			}
			for symbol, path := range tt.want {
				if got[symbol] != path {
					t.Errorf("%s path = %q, want %q", symbol, got[symbol], path)
				}
			}
		})
	}
}

func TestReadProjectVectors_ExcludesIncompleteRevisions(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "index.db")
	projectA := filepath.Join(base, "a")
	seedVectorCollection(t, dbPath, "int8", projectA, filepath.Join(base, "b"))

	pv, err := ReadProjectVectors(dbPath, projectA)
	if err != nil {
		t.Fatal(err)
	}
	if pv.IncompleteFiles != 0 {
		t.Fatalf("IncompleteFiles = %d on a complete index, want 0", pv.IncompleteFiles)
	}

	withRawDB(t, dbPath, func(db *sql.DB) {
		if _, err := db.Exec(`UPDATE file_revisions SET complete = 0 WHERE relative_path IN ('util.go', 'dup.go')`); err != nil {
			t.Fatal(err)
		}
	})
	pv, err = ReadProjectVectors(dbPath, projectA)
	if err != nil {
		t.Fatal(err)
	}
	if pv.IncompleteFiles != 2 {
		t.Errorf("IncompleteFiles = %d, want 2", pv.IncompleteFiles)
	}
	bySymbol := chunksBySymbol(t, pv)
	for _, symbol := range []string{"C", "D", "E"} {
		if _, ok := bySymbol[symbol]; ok {
			t.Errorf("chunk %s from an incomplete revision was returned", symbol)
		}
	}
	if len(pv.Chunks) != 3 {
		t.Errorf("chunks = %+v, want only A, B and Z", pv.Chunks)
	}
}

func TestReadProjectVectors_SharedVectorDecodedOnce(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "index.db")
	projectA := filepath.Join(base, "a")
	seedVectorCollection(t, dbPath, "float32", projectA, filepath.Join(base, "b"))

	pv, err := ReadProjectVectors(dbPath, projectA)
	if err != nil {
		t.Fatal(err)
	}
	bySymbol := chunksBySymbol(t, pv)
	d, e := bySymbol["D"], bySymbol["E"]
	if d.VectorID != e.VectorID {
		t.Fatalf("D and E vector ids = %d, %d; fixture should share one", d.VectorID, e.VectorID)
	}
	// A, B, C, D/E share one entry; Z is absent.
	if len(pv.Vectors) != 4 {
		t.Errorf("len(Vectors) = %d, want 4", len(pv.Vectors))
	}
	if _, ok := pv.Vectors[d.VectorID]; !ok {
		t.Error("shared vector missing from map")
	}
}

func TestReadProjectVectors_ZeroVectorLeftOut(t *testing.T) {
	for _, st := range storages {
		t.Run(st.name, func(t *testing.T) {
			base := t.TempDir()
			dbPath := filepath.Join(base, "index.db")
			projectA := filepath.Join(base, "a")
			seedVectorCollection(t, dbPath, st.name, projectA, filepath.Join(base, "b"))

			pv, err := ReadProjectVectors(dbPath, projectA)
			if err != nil {
				t.Fatal(err)
			}
			z, ok := chunksBySymbol(t, pv)["Z"]
			if !ok {
				t.Fatal("zero-vector chunk missing from Chunks")
			}
			if _, ok := pv.Vectors[z.VectorID]; ok {
				t.Error("zero-norm vector present in Vectors")
			}
		})
	}
}

func TestReadProjectVectors_DoesNotModifyDatabase(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "index.db")
	projectA := filepath.Join(base, "a")
	seedVectorCollection(t, dbPath, "int8", projectA, filepath.Join(base, "b"))

	fingerprint := func() (sum [sha256.Size]byte, mtime time.Time, entries []string) {
		t.Helper()
		data, err := os.ReadFile(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		dirEntries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range dirEntries {
			if strings.HasSuffix(e.Name(), "-wal") || strings.HasSuffix(e.Name(), "-shm") {
				continue
			}
			entries = append(entries, e.Name())
		}
		return sha256.Sum256(data), info.ModTime(), entries
	}
	beforeSum, beforeMtime, beforeEntries := fingerprint()

	for _, project := range []string{projectA, filepath.Join(base, "unknown")} {
		if _, err := ReadProjectVectors(dbPath, project); err != nil && !errors.Is(err, ErrProjectNotFound) {
			t.Fatal(err)
		}
	}

	afterSum, afterMtime, afterEntries := fingerprint()
	if beforeSum != afterSum {
		t.Error("index.db contents changed")
	}
	if !beforeMtime.Equal(afterMtime) {
		t.Errorf("index.db mtime changed: %v -> %v", beforeMtime, afterMtime)
	}
	if len(beforeEntries) != len(afterEntries) {
		t.Errorf("directory entries changed: %v -> %v", beforeEntries, afterEntries)
	}
	if info, err := os.Stat(dbPath + "-wal"); err == nil && info.Size() != 0 {
		t.Errorf("WAL has %d bytes after read-only access, want 0", info.Size())
	}
	withRawDB(t, dbPath, func(db *sql.DB) {
		var projects int
		if err := db.QueryRow(`SELECT count(*) FROM projects`).Scan(&projects); err != nil {
			t.Fatal(err)
		}
		if projects != 2 {
			t.Errorf("projects = %d, want the 2 seeded", projects)
		}
	})
}

func TestReadProjectVectors_Errors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, base, dbPath, project string)
		want  error
	}{
		{"missing database", func(t *testing.T, base, dbPath, project string) {}, os.ErrNotExist},
		{"legacy schema", func(t *testing.T, base, dbPath, project string) {
			s, err := openStore(dbPath, 4)
			if err != nil {
				t.Fatal(err)
			}
			_ = s.Close()
		}, ErrNotCollection},
		{"unrelated sqlite database", func(t *testing.T, base, dbPath, project string) {
			withRawDB(t, dbPath, func(db *sql.DB) {
				if _, err := db.Exec(`CREATE TABLE notes (body TEXT)`); err != nil {
					t.Fatal(err)
				}
			})
		}, ErrNotCollection},
		{"zero-byte file", func(t *testing.T, base, dbPath, project string) {
			if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}, ErrNotCollection},
		{"unknown project", func(t *testing.T, base, dbPath, project string) {
			seedVectorCollection(t, dbPath, "int8", filepath.Join(base, "other"), filepath.Join(base, "b"))
		}, ErrProjectNotFound},
		{"missing vector row", func(t *testing.T, base, dbPath, project string) {
			seedVectorCollection(t, dbPath, "int8", project, filepath.Join(base, "b"))
			withRawDB(t, dbPath, func(db *sql.DB) {
				if _, err := db.Exec(`DELETE FROM vec_vectors WHERE vector_id = (SELECT vector_id FROM chunk_defs WHERE chunk_key = 'c')`); err != nil {
					t.Fatal(err)
				}
			})
		}, ErrCorruptIndex},
		{"blob length differs from dimensions", func(t *testing.T, base, dbPath, project string) {
			seedVectorCollection(t, dbPath, "float32", project, filepath.Join(base, "b"))
			withRawDB(t, dbPath, func(db *sql.DB) {
				if _, err := db.Exec(`UPDATE collection_meta SET value = '8' WHERE key = 'vec_dimensions'`); err != nil {
					t.Fatal(err)
				}
			})
		}, ErrCorruptIndex},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			dbPath := filepath.Join(base, "data", "index.db")
			project := filepath.Join(base, "a")
			if tt.name != "missing database" {
				if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			tt.setup(t, base, dbPath, project)

			pv, err := ReadProjectVectors(dbPath, project)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want errors.Is %v", err, tt.want)
			}
			if pv != nil {
				t.Errorf("result = %+v, want nil on error", pv)
			}
			for _, other := range []error{ErrNotCollection, ErrProjectNotFound, ErrCorruptIndex, os.ErrNotExist} {
				if other != tt.want && errors.Is(err, other) {
					t.Errorf("err also matches %v", other)
				}
			}
		})
	}
	t.Run("missing database creates nothing", func(t *testing.T) {
		base := t.TempDir()
		_, _ = ReadProjectVectors(filepath.Join(base, "lumen", "index.db"), base)
		if _, err := os.Stat(filepath.Join(base, "lumen")); !os.IsNotExist(err) {
			t.Errorf("data directory was created (stat err = %v)", err)
		}
	})
}

func TestDecodeStoredVector(t *testing.T) {
	tests := []struct {
		name    string
		blob    []byte
		storage string
		dims    int
		want    []float32
		ok      bool
	}{
		{"int8 is signed", []byte{0xff, 3, 0xfd, 0}, "int8", 4, []float32{-1, 3, -3, 0}, true},
		{"int8 too short", []byte{1, 2, 3}, "int8", 4, nil, false},
		{"int8 too long", []byte{1, 2, 3, 4, 5}, "int8", 4, nil, false},
		{"float32 too short", make([]byte, 15), "float32", 4, nil, false},
		{"float32 int8-sized", make([]byte, 4), "float32", 4, nil, false},
		{"unknown storage", make([]byte, 4), "bf16", 4, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := decodeStoredVector(tt.blob, tt.storage, tt.dims)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}
