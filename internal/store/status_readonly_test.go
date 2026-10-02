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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ory/lumen/internal/chunker"
)

// seedTwoProjectCollection writes a collection where projectA owns main.go
// (2 chunks) and util.go (1 chunk) and projectB shares main.go. The returned
// store is still open so its WAL may hold data not yet checkpointed.
func seedTwoProjectCollection(t *testing.T, dbPath, projectA, projectB string) *Store {
	t.Helper()
	s, err := NewCollection(dbPath, 4, "int8", projectA)
	if err != nil {
		t.Fatal(err)
	}
	mainChunks := []chunker.Chunk{
		{ID: "a", FilePath: "main.go", Symbol: "A", Kind: "function", StartLine: 1, EndLine: 2, Content: "func A() {}"},
		{ID: "b", FilePath: "main.go", Symbol: "B", Kind: "function", StartLine: 3, EndLine: 4, Content: "func B() {}"},
	}
	if _, err := s.StoreFileRevision("main.go", "abcd", mainChunks, map[int][]float32{0: {1, 0, 0, 0}, 1: {0, 1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	utilChunks := []chunker.Chunk{{ID: "c", FilePath: "util.go", Symbol: "C", Kind: "function", StartLine: 1, EndLine: 1, Content: "func C() {}"}}
	if _, err := s.StoreFileRevision("util.go", "ef01", utilChunks, map[int][]float32{0: {0, 0, 1, 0}}); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"root_hash":        "deadbeef",
		"total_files":      "5",
		"last_indexed_at":  "2026-10-01T10:12:03Z",
		MetaLastIndexError: "embed failed",
	} {
		if err := s.SetMeta(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UseProject(projectB); err != nil {
		t.Fatal(err)
	}
	if attached, err := s.AttachExistingFileRevision("main.go", "abcd"); err != nil || !attached {
		t.Fatalf("attach main.go to projectB: attached=%v err=%v", attached, err)
	}
	if err := s.UseProject(projectA); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReadCollectionSnapshot_CountsMatchCollectionStats(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "data", "index.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	projectA := filepath.Join(base, "project")
	projectB := filepath.Join(base, "project.worktree1")
	s := seedTwoProjectCollection(t, dbPath, projectA, projectB)
	defer func() { _ = s.Close() }()

	want, err := s.CollectionStats()
	if err != nil {
		t.Fatal(err)
	}

	snap, err := ReadCollectionSnapshot(dbPath, projectA)
	if err != nil {
		t.Fatalf("ReadCollectionSnapshot: %v", err)
	}
	if !snap.Shared {
		t.Fatal("Shared = false, want true")
	}
	if snap.Project == nil {
		t.Fatal("Project = nil, want projectA")
	}
	if snap.Project.Files != want.ProjectFiles || snap.Project.Chunks != want.ProjectChunks {
		t.Errorf("project files/chunks = %d/%d, want %d/%d", snap.Project.Files, snap.Project.Chunks, want.ProjectFiles, want.ProjectChunks)
	}
	if snap.Project.Files != 2 || snap.Project.Chunks != 3 {
		t.Errorf("project files/chunks = %d/%d, want 2/3", snap.Project.Files, snap.Project.Chunks)
	}
	if snap.UniqueVectors != want.UniqueVectors || snap.ChunkReferences != want.ChunkReferences || snap.SharedReferences != want.SharedReferences {
		t.Errorf("collection = %d/%d/%d, want %d/%d/%d", snap.UniqueVectors, snap.ChunkReferences, snap.SharedReferences,
			want.UniqueVectors, want.ChunkReferences, want.SharedReferences)
	}
	if snap.UniqueVectors != 3 || snap.SharedReferences != 2 {
		t.Errorf("unique/shared = %d/%d, want 3/2", snap.UniqueVectors, snap.SharedReferences)
	}
	if snap.DatabaseBytes != want.DatabaseBytes || snap.ReclaimableBytes != want.ReclaimableBytes {
		t.Errorf("bytes = %d/%d, want %d/%d", snap.DatabaseBytes, snap.ReclaimableBytes, want.DatabaseBytes, want.ReclaimableBytes)
	}
	if snap.VectorStorage != "int8" || snap.Dimensions != 4 {
		t.Errorf("profile = %s/%d, want int8/4", snap.VectorStorage, snap.Dimensions)
	}

	meta := snap.Project.Meta
	if meta["root_hash"] != "deadbeef" || meta["total_files"] != "5" ||
		meta["last_indexed_at"] != "2026-10-01T10:12:03Z" || meta[MetaLastIndexError] != "embed failed" {
		t.Errorf("meta = %v", meta)
	}

	if len(snap.Projects) != 2 {
		t.Fatalf("Projects = %+v, want 2 entries", snap.Projects)
	}
	byPath := map[string]ProjectSummary{}
	for _, p := range snap.Projects {
		byPath[p.Path] = p
	}
	a, okA := byPath[projectA]
	b, okB := byPath[projectB]
	if !okA || !okB {
		t.Fatalf("Projects = %+v, want %s and %s", snap.Projects, projectA, projectB)
	}
	if a.LastIndexedAt != "2026-10-01T10:12:03Z" || b.LastIndexedAt != "" {
		t.Errorf("last indexed = %q/%q", a.LastIndexedAt, b.LastIndexedAt)
	}
	if _, err := time.Parse(time.RFC3339, a.LastAccessedAt); err != nil {
		t.Errorf("projectA LastAccessedAt %q: %v", a.LastAccessedAt, err)
	}

	other, err := ReadCollectionSnapshot(dbPath, projectB)
	if err != nil {
		t.Fatal(err)
	}
	if other.Project == nil || other.Project.Files != 1 || other.Project.Chunks != 2 || len(other.Project.Meta) != 0 {
		t.Errorf("projectB = %+v, want 1 file, 2 chunks, no meta", other.Project)
	}
}

func TestReadCollectionSnapshot_UsesStoreProjectPathNormalisation(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "index.db")
	project := filepath.Join(base, "project")
	s := seedTwoProjectCollection(t, dbPath, project+string(filepath.Separator)+"sub"+string(filepath.Separator)+"..", filepath.Join(base, "other"))
	_ = s.Close()

	snap, err := ReadCollectionSnapshot(dbPath, project+string(filepath.Separator))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Project == nil || snap.Project.Path != project {
		t.Fatalf("Project = %+v, want row for %s", snap.Project, project)
	}
}

func TestReadCollectionSnapshot_UnknownProject(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "index.db")
	s := seedTwoProjectCollection(t, dbPath, filepath.Join(base, "a"), filepath.Join(base, "b"))
	_ = s.Close()

	snap, err := ReadCollectionSnapshot(dbPath, filepath.Join(base, "never-indexed"))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Project != nil {
		t.Errorf("Project = %+v, want nil", snap.Project)
	}
	if len(snap.Projects) != 2 || snap.UniqueVectors != 3 {
		t.Errorf("collection = %+v, want 2 projects and 3 vectors", snap)
	}
}

func TestReadCollectionSnapshot_DoesNotModifyDatabase(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "index.db")
	s := seedTwoProjectCollection(t, dbPath, filepath.Join(base, "a"), filepath.Join(base, "b"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

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
			// A read-only SQLite connection cannot remove the WAL sidecars it
			// opens; they are checked separately for being empty.
			if strings.HasSuffix(e.Name(), "-wal") || strings.HasSuffix(e.Name(), "-shm") {
				continue
			}
			entries = append(entries, e.Name())
		}
		return sha256.Sum256(data), info.ModTime(), entries
	}
	beforeSum, beforeMtime, beforeEntries := fingerprint()

	// An unknown project would be inserted by any code path that registers
	// memberships, so it exercises the no-write guarantee.
	for _, project := range []string{filepath.Join(base, "a"), filepath.Join(base, "unknown")} {
		if _, err := ReadCollectionSnapshot(dbPath, project); err != nil {
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
	snap, err := ReadCollectionSnapshot(dbPath, filepath.Join(base, "unknown"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Projects) != 2 || snap.Project != nil {
		t.Errorf("projects = %+v, want the 2 seeded projects only", snap.Projects)
	}
}

func TestReadCollectionSnapshot_MissingDatabaseCreatesNothing(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "lumen", "abc", "index.db")
	_, err := ReadCollectionSnapshot(dbPath, base)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
	if _, statErr := os.Stat(filepath.Join(base, "lumen")); !os.IsNotExist(statErr) {
		t.Errorf("data directory was created (stat err = %v)", statErr)
	}
}

func TestReadCollectionSnapshot_LegacySchemaIsNotShared(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	s, err := openStore(dbPath, 4)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	snap, err := ReadCollectionSnapshot(dbPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Shared || !snap.Legacy || snap.Project != nil {
		t.Errorf("snapshot = %+v, want legacy, non-shared, no project", snap)
	}
}

func TestReadCollectionSnapshot_RejectsNonIndexFiles(t *testing.T) {
	tests := []struct {
		name  string
		write func(t *testing.T, dbPath string)
	}{
		{"zero-byte file", func(t *testing.T, dbPath string) {
			if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"unrelated sqlite database", func(t *testing.T, dbPath string) {
			db, err := sql.Open("sqlite3", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Exec(`CREATE TABLE notes (body TEXT)`); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "index.db")
			tt.write(t, dbPath)
			snap, err := ReadCollectionSnapshot(dbPath, t.TempDir())
			if err == nil || errors.Is(err, os.ErrNotExist) {
				t.Errorf("err = %v, snapshot = %+v; want a not-an-index error", err, snap)
			}
		})
	}
}

func TestReadCollectionSnapshot_ReportsIncompleteFiles(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "index.db")
	projectA := filepath.Join(base, "a")
	s := seedTwoProjectCollection(t, dbPath, projectA, filepath.Join(base, "b"))
	snap, err := ReadCollectionSnapshot(dbPath, projectA)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Project == nil || snap.Project.HasIncompleteFiles {
		t.Fatalf("complete project: %+v, want HasIncompleteFiles false", snap.Project)
	}
	if err := s.UpsertFile("pending.go", ""); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	snap, err = ReadCollectionSnapshot(dbPath, projectA)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Project == nil || !snap.Project.HasIncompleteFiles {
		t.Errorf("project with sentinel revision: %+v, want HasIncompleteFiles true", snap.Project)
	}
}
