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

package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ory/lumen/internal/config"
	"github.com/ory/lumen/internal/embedder"
	"github.com/ory/lumen/internal/index"
	"github.com/ory/lumen/internal/indexlock"
	"github.com/ory/lumen/internal/store"
	"github.com/spf13/cobra"
)

func TestStatusExitCode(t *testing.T) {
	ok := serverStatus{}
	bad := serverStatus{err: errors.New("down")}
	tests := []struct {
		name    string
		servers []serverStatus
		want    int
	}{
		{"one usable", []serverStatus{ok}, 0},
		{"one of two usable", []serverStatus{bad, ok}, 0},
		{"all failing", []serverStatus{bad, bad}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := statusResult{servers: tt.servers, state: indexMissing}
			if got := statusExitCode(r); got != tt.want {
				t.Errorf("statusExitCode = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFormatStatus(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	home := "/home/u"
	okServer := serverStatus{server: config.ServerConfig{Backend: "ollama", Host: "http://localhost:11434", Model: "ordis/jina-embeddings-v2-base-code"}}
	badServer := serverStatus{
		server: config.ServerConfig{Backend: "lmstudio", Host: "http://localhost:1234", Model: "x"},
		err:    errors.New(`configured model "x" is not loaded`),
	}
	collection := &collectionStatus{
		dbPath:           "/home/u/.local/share/lumen/3fa1/index.db",
		uniqueVectors:    10204,
		sharedReferences: 15880,
		databaseBytes:    412 * 1024 * 1024,
		reclaimableBytes: 18 * 1024 * 1024,
		worktrees: []worktreeStatus{
			{path: "/src/project", lastUsed: now.Add(-2 * time.Hour)},
			{path: "/src/project.worktree1", lastUsed: now.Add(-10 * time.Second)},
			{path: "/src/project.worktree2", lastUsed: now.Add(-12 * 24 * time.Hour)},
			{path: "/src/project.worktree3", lastUsed: now.Add(-5 * time.Minute)},
			{path: "/src/project.worktree4"},
		},
	}
	indexed := statusResult{
		servers:        []serverStatus{okServer},
		root:           "/src/project.worktree1",
		state:          indexPresent,
		totalFiles:     812,
		indexedFiles:   812,
		chunks:         9431,
		model:          "ordis/jina-embeddings-v2-base-code",
		dims:           768,
		vectorStorage:  "int8",
		maxChunkTokens: 512,
		lastIndexedAt:  "2026-10-01T10:12:03Z",
		stale:          "no",
		collection:     collection,
	}

	tests := []struct {
		name string
		r    statusResult
		want string
	}{
		{
			name: "indexed with collection block",
			r:    indexed,
			want: `Embedding service: OK (ollama, http://localhost:11434, ordis/jina-embeddings-v2-base-code)
Index: /src/project.worktree1
  Files: 812 | Indexed: 812 | Chunks: 9,431 | Model: ordis/jina-embeddings-v2-base-code (768d, int8, 512 tok)
  Last indexed: 2026-10-01T10:12:03Z | Stale: no | Indexing now: no
  Last error: —
Collection: ~/.local/share/lumen/3fa1/index.db (shared by 5 worktrees)
  Unique vectors: 10,204 | Shared refs: 15,880 | Dedup: 60.9%
  DB size: 412 MB | Reclaimable: 18 MB
  Worktrees: project (2h ago), project.worktree1 (now), project.worktree2 (12d ago), project.worktree3 (5m ago), project.worktree4 (unknown)
`,
		},
		{
			name: "indexed, never completed, stale, indexing, with error",
			r: func() statusResult {
				r := indexed
				r.lastIndexedAt = ""
				r.stale = "yes"
				r.indexingNow = true
				r.lastError = "embed: connection refused"
				r.collection = &collectionStatus{dbPath: "/var/lumen/index.db", worktrees: []worktreeStatus{{path: "/src/project.worktree1", lastUsed: now}}}
				return r
			}(),
			want: `Embedding service: OK (ollama, http://localhost:11434, ordis/jina-embeddings-v2-base-code)
Index: /src/project.worktree1
  Files: 812 | Indexed: 812 | Chunks: 9,431 | Model: ordis/jina-embeddings-v2-base-code (768d, int8, 512 tok)
  Last indexed: never | Stale: yes | Indexing now: yes
  Last error: embed: connection refused
Collection: /var/lumen/index.db (shared by 1 worktree)
  Unique vectors: 0 | Shared refs: 0 | Dedup: 0.0%
  DB size: 0 B | Reclaimable: 0 B
  Worktrees: project.worktree1 (now)
`,
		},
		{
			name: "error server and not indexed without collection",
			r:    statusResult{servers: []serverStatus{okServer, badServer}, root: "/src/p", state: indexMissing},
			want: `Embedding service: OK (ollama, http://localhost:11434, ordis/jina-embeddings-v2-base-code)
Embedding service: ERROR (lmstudio, http://localhost:1234) — configured model "x" is not loaded
Index: /src/p — not indexed (run ` + "`lumen index .`" + `)
`,
		},
		{
			name: "legacy index",
			r:    statusResult{servers: []serverStatus{badServer}, root: "/src/p", state: indexLegacy},
			want: `Embedding service: ERROR (lmstudio, http://localhost:1234) — configured model "x" is not loaded
Index: /src/p — legacy index (will migrate on next index/search)
`,
		},
		{
			name: "not indexed but collection exists",
			r: statusResult{servers: []serverStatus{okServer}, root: "/src/p.wt", state: indexMissing, collection: &collectionStatus{
				dbPath: "/data/index.db", uniqueVectors: 3, sharedReferences: 1, databaseBytes: 1536, reclaimableBytes: 512,
				worktrees: []worktreeStatus{{path: "/src/p", lastUsed: now.Add(-3 * time.Hour)}},
			}},
			want: `Embedding service: OK (ollama, http://localhost:11434, ordis/jina-embeddings-v2-base-code)
Index: /src/p.wt — not indexed (run ` + "`lumen index .`" + `)
Collection: /data/index.db (shared by 1 worktree)
  Unique vectors: 3 | Shared refs: 1 | Dedup: 25.0%
  DB size: 1.5 KB | Reclaimable: 512 B
  Worktrees: p (3h ago)
`,
		},
		{
			name: "index read error",
			r:    statusResult{servers: []serverStatus{okServer}, root: "/src/p", state: indexUnreadable, readErr: errors.New("file is not a database")},
			want: `Embedding service: OK (ollama, http://localhost:11434, ordis/jina-embeddings-v2-base-code)
Index: /src/p — unreadable: file is not a database
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatStatus(tt.r, now, home); got != tt.want {
				t.Errorf("formatStatus mismatch\n got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestIndexingNow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	root := "/src/project"

	if indexingNow(dbPath, root) {
		t.Error("no lock files: indexingNow = true, want false")
	}
	if _, err := os.Stat(indexlock.LockPathForDB(dbPath)); !os.IsNotExist(err) {
		t.Errorf("collection lock file created by probe (stat err = %v)", err)
	}

	tests := []struct {
		name    string
		acquire func() (*indexlock.Lock, error)
		want    bool
	}{
		{"project lock held", func() (*indexlock.Lock, error) {
			return indexlock.TryAcquire(indexlock.LockPathForProject(dbPath, root))
		}, true},
		{"other worktree's project lock held", func() (*indexlock.Lock, error) {
			return indexlock.TryAcquire(indexlock.LockPathForProject(dbPath, "/src/other"))
		}, false},
		{"shared collection lock held", func() (*indexlock.Lock, error) {
			return indexlock.TryAcquireShared(indexlock.LockPathForDB(dbPath))
		}, false},
		{"exclusive collection lock held", func() (*indexlock.Lock, error) {
			return indexlock.TryAcquire(indexlock.LockPathForDB(dbPath))
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lock, err := tt.acquire()
			if err != nil || lock == nil {
				t.Fatalf("acquire: lock=%v err=%v", lock, err)
			}
			got := indexingNow(dbPath, root)
			lock.Release()
			if got != tt.want {
				t.Errorf("indexingNow = %v, want %v", got, tt.want)
			}
			if indexingNow(dbPath, root) {
				t.Error("indexingNow = true after release; probe must not keep a lock")
			}
		})
	}
}

// statusTestEnv isolates config and data directories and points a single
// Ollama server at an httptest server that reports the default model loaded
// when healthy is true, or fails with HTTP 500 otherwise.
func statusTestEnv(t *testing.T, healthy bool) (dataDir string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"` + embedder.DefaultModel + `"}]}`))
	}))
	t.Cleanup(srv.Close)
	dataDir = t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", dataDir)
	for _, k := range []string{"LUMEN_EMBED_MODEL", "LM_STUDIO_HOST", "LUMEN_EMBED_DIMS", "LUMEN_EMBED_CTX", "LUMEN_MAX_CHUNK_TOKENS", "LUMEN_VECTOR_STORAGE"} {
		t.Setenv(k, "")
	}
	t.Setenv("LUMEN_BACKEND", config.BackendOllama)
	t.Setenv("OLLAMA_HOST", srv.URL)
	return dataDir
}

func newStatusTestCommand(out *bytes.Buffer) *cobra.Command {
	c := &cobra.Command{Use: "status"}
	c.Flags().StringP("model", "m", "", "")
	c.Flags().StringP("backend", "b", "", "")
	c.SetOut(out)
	c.SetErr(out)
	c.SetContext(context.Background())
	return c
}

func TestRunStatus_ExitCode(t *testing.T) {
	tests := []struct {
		name    string
		healthy bool
		wantErr bool
		wantOut string
	}{
		{"usable server, not indexed", true, false, "not indexed"},
		{"all probes failing", false, true, "Embedding service: ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := statusTestEnv(t, tt.healthy)
			var out bytes.Buffer
			err := runStatus(newStatusTestCommand(&out), []string{t.TempDir()})
			if tt.wantErr != (err != nil) {
				t.Fatalf("runStatus err = %v, wantErr %v\n%s", err, tt.wantErr, out.String())
			}
			if err != nil && !errors.Is(err, errStatusUnhealthy) {
				t.Errorf("err = %v, want errStatusUnhealthy", err)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("output missing %q:\n%s", tt.wantOut, out.String())
			}
			if entries, _ := os.ReadDir(dataDir); len(entries) != 0 {
				t.Errorf("status created data under XDG_DATA_HOME: %v", entries)
			}
		})
	}
}

func TestCollectStatus_NonDefaultProfileAndStaleness(t *testing.T) {
	statusTestEnv(t, true)
	t.Setenv("LUMEN_VECTOR_STORAGE", "float32")
	t.Setenv("LUMEN_MAX_CHUNK_TOKENS", "256")
	cfg, err := config.NewConfigService(config.DefaultConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(project, "main.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	model := embedder.DefaultModel
	dbPath := configuredDBPath(cfg, project, model)
	if dbPath == config.DBPathForProject(project, model) {
		t.Fatal("non-default profile must not resolve to the default collection path")
	}

	before, err := collectStatus(context.Background(), cfg, project)
	if err != nil {
		t.Fatal(err)
	}
	if before.state != indexMissing || before.collection != nil || before.dbPath != dbPath {
		t.Fatalf("before indexing: state=%v collection=%v dbPath=%s want missing at %s", before.state, before.collection, before.dbPath, dbPath)
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	idx, err := index.NewIndexerForProject(dbPath, &stubEmbedder{model: model}, cfg.MaxChunkTokens(), cfg.VectorStorage(), project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Index(context.Background(), project, false, nil); err != nil {
		t.Fatal(err)
	}
	_ = idx.Close()

	fresh, err := collectStatus(context.Background(), cfg, project)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.state != indexPresent {
		t.Fatalf("state = %v, want indexPresent", fresh.state)
	}
	if fresh.stale != "no" {
		t.Errorf("stale = %q, want no", fresh.stale)
	}
	if fresh.vectorStorage != "float32" || fresh.maxChunkTokens != 256 || fresh.dims != 4 {
		t.Errorf("profile = %s/%d tok/%dd, want float32/256/4", fresh.vectorStorage, fresh.maxChunkTokens, fresh.dims)
	}
	if fresh.totalFiles != 1 || fresh.indexedFiles != 1 || fresh.chunks == 0 || fresh.lastIndexedAt == "" {
		t.Errorf("counts = files %d indexed %d chunks %d last %q", fresh.totalFiles, fresh.indexedFiles, fresh.chunks, fresh.lastIndexedAt)
	}
	if fresh.collection == nil || len(fresh.collection.worktrees) != 1 || fresh.collection.worktrees[0].path != project {
		t.Errorf("collection = %+v, want one worktree %s", fresh.collection, project)
	}

	original, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() { println() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edited, err := collectStatus(context.Background(), cfg, project)
	if err != nil {
		t.Fatal(err)
	}
	if edited.stale != "yes" {
		t.Errorf("after edit stale = %q, want yes", edited.stale)
	}
	if err := os.WriteFile(src, original, 0o644); err != nil {
		t.Fatal(err)
	}
	reverted, err := collectStatus(context.Background(), cfg, project)
	if err != nil {
		t.Fatal(err)
	}
	if reverted.stale != "no" {
		t.Errorf("after reverting the edit stale = %q, want no", reverted.stale)
	}

	// An interrupted index leaves incomplete revisions while root_hash may
	// already match the tree; the indexer treats that as needing a reindex.
	s, err := store.NewCollection(dbPath, 4, cfg.VectorStorage(), project)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertFile("pending.go", ""); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	incomplete, err := collectStatus(context.Background(), cfg, project)
	if err != nil {
		t.Fatal(err)
	}
	if incomplete.stale != "yes" {
		t.Errorf("with incomplete revisions stale = %q, want yes", incomplete.stale)
	}
}

func TestCollectStatus_FindsNonGitAncestorUnderConfiguredProfile(t *testing.T) {
	statusTestEnv(t, true)
	t.Setenv("LUMEN_VECTOR_STORAGE", "float32")
	cfg, err := config.NewConfigService(config.DefaultConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	ancestor, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(ancestor, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := configuredDBPath(cfg, ancestor, embedder.DefaultModel)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := collectStatus(context.Background(), cfg, sub)
	if err != nil {
		t.Fatal(err)
	}
	if r.root != ancestor {
		t.Errorf("root = %q, want the ancestor %q indexed under the configured profile", r.root, ancestor)
	}
}

func TestCollectStatus_OtherWorktreeOnlyAndLegacy(t *testing.T) {
	dataDir := statusTestEnv(t, true)
	cfg, err := config.NewConfigService(config.DefaultConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model := embedder.DefaultModel

	legacy := config.LegacyDBPathForProject(project, model)
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := collectStatus(context.Background(), cfg, project)
	if err != nil {
		t.Fatal(err)
	}
	if r.state != indexLegacy || r.collection != nil {
		t.Errorf("legacy only: state=%v collection=%v, want legacy without collection", r.state, r.collection)
	}
	if err := os.RemoveAll(filepath.Join(dataDir, "lumen")); err != nil {
		t.Fatal(err)
	}

	dbPath := configuredDBPath(cfg, project, model)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.NewCollection(dbPath, 4, "int8", filepath.Join(t.TempDir(), "sibling"))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	r, err = collectStatus(context.Background(), cfg, project)
	if err != nil {
		t.Fatal(err)
	}
	if r.state != indexMissing || r.collection == nil || len(r.collection.worktrees) != 1 {
		t.Errorf("sibling only: state=%v collection=%+v, want not indexed with 1-worktree collection", r.state, r.collection)
	}
	snap, err := store.ReadCollectionSnapshot(dbPath, project)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Project != nil || len(snap.Projects) != 1 {
		t.Errorf("status registered a project: %+v", snap.Projects)
	}
}

func TestCollectStatus_NonIndexFileAtCollectionPath(t *testing.T) {
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
			statusTestEnv(t, true)
			cfg, err := config.NewConfigService(config.DefaultConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			project, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dbPath := configuredDBPath(cfg, project, embedder.DefaultModel)
			if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
				t.Fatal(err)
			}
			tt.write(t, dbPath)

			r, err := collectStatus(context.Background(), cfg, project)
			if err != nil {
				t.Fatal(err)
			}
			if r.state != indexUnreadable || r.readErr == nil || r.collection != nil {
				t.Errorf("state=%v readErr=%v collection=%v, want unreadable with error and no collection", r.state, r.readErr, r.collection)
			}
		})
	}
}

// executeStatusCommand runs `lumen <args>` through the real root command so
// cobra's own argument, flag and error handling is exercised.
func executeStatusCommand(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
		statusCmd.SilenceErrors = false
		statusCmd.SilenceUsage = false
	})
	err = rootCmd.Execute()
	return out.String(), errOut.String(), err
}

func TestStatusCommand_ErrorReporting(t *testing.T) {
	t.Run("too many arguments", func(t *testing.T) {
		statusTestEnv(t, true)
		_, stderr, err := executeStatusCommand(t, "status", "a", "b")
		if err == nil || !strings.Contains(stderr, "accepts at most 1 arg") {
			t.Errorf("err=%v stderr=%q, want arg error printed", err, stderr)
		}
	})
	t.Run("unknown flag", func(t *testing.T) {
		statusTestEnv(t, true)
		_, stderr, err := executeStatusCommand(t, "status", "--bogus")
		if err == nil || !strings.Contains(stderr, "unknown flag: --bogus") {
			t.Errorf("err=%v stderr=%q, want flag error printed", err, stderr)
		}
	})
	t.Run("empty server list in config", func(t *testing.T) {
		statusTestEnv(t, true)
		for _, k := range []string{"LUMEN_BACKEND", "OLLAMA_HOST"} {
			t.Setenv(k, "")
		}
		dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "lumen")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("servers: []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, stderr, err := executeStatusCommand(t, "status", t.TempDir())
		if err == nil || !strings.Contains(stderr, "servers list is empty") {
			t.Errorf("err=%v stderr=%q, want config error printed", err, stderr)
		}
	})
	t.Run("no usable server is reported only on stdout", func(t *testing.T) {
		statusTestEnv(t, false)
		stdout, stderr, err := executeStatusCommand(t, "status", t.TempDir())
		if !errors.Is(err, errStatusUnhealthy) {
			t.Fatalf("err = %v, want errStatusUnhealthy", err)
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want empty", stderr)
		}
		if !strings.Contains(stdout, "Embedding service: ERROR") {
			t.Errorf("stdout missing ERROR line:\n%s", stdout)
		}
	})
}
