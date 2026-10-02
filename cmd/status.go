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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ory/lumen/internal/config"
	"github.com/ory/lumen/internal/embedder"
	"github.com/ory/lumen/internal/index"
	"github.com/ory/lumen/internal/indexlock"
	"github.com/ory/lumen/internal/merkle"
	"github.com/ory/lumen/internal/store"
	"github.com/spf13/cobra"
)

const statusProbeTimeout = 5 * time.Second

// serverStatus is the probe outcome for one configured embedding server; a nil
// err means the server is reachable and has the configured model loaded.
type serverStatus struct {
	server config.ServerConfig
	err    error
}

type indexState int

const (
	indexMissing indexState = iota
	indexLegacy
	indexPresent
	indexUnreadable
)

type worktreeStatus struct {
	path     string
	lastUsed time.Time // zero when no timestamp is recorded
}

type collectionStatus struct {
	dbPath           string
	uniqueVectors    int
	sharedReferences int
	databaseBytes    int64
	reclaimableBytes int64
	worktrees        []worktreeStatus
}

type statusResult struct {
	servers []serverStatus

	root    string
	dbPath  string
	state   indexState
	readErr error

	totalFiles     int
	indexedFiles   int
	chunks         int
	model          string
	dims           int
	vectorStorage  string
	maxChunkTokens int
	lastIndexedAt  string
	stale          string
	indexingNow    bool
	lastError      string

	collection *collectionStatus // nil when the collection database does not exist
}

// errStatusUnhealthy produces a non-zero exit after the report, which already
// shows each server's error, has been printed.
var errStatusUnhealthy = errors.New("status: no usable embedding server")

func init() {
	statusCmd.Flags().StringP("model", "m", "", "embedding model (default: $LUMEN_EMBED_MODEL or "+embedder.DefaultModel+")")
	statusCmd.Flags().StringP("backend", "b", "", "embedding backend to select (\"ollama\" or \"lmstudio\"); disambiguates when --model is configured on multiple backends")
	rootCmd.AddCommand(statusCmd)
}

var statusCmd = &cobra.Command{
	Use:   "status [path]",
	Short: "Report embedding-service health and index state",
	Long: `Reports whether each configured embedding server is usable, whether the
project at path (default: the current directory) is indexed and fresh, whether
indexing is running, and what the shared collection holding it contains.

The path is normalized to the git repository root (or an existing ancestor
index) so the report covers the same index that searches use. Checking
freshness hashes every file in the project.

status is read-only: it never indexes and never creates or modifies an index
database. Exit code is 0 when at least one embedding server is usable,
regardless of index state; otherwise 1.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		err := runStatus(cmd, args)
		if errors.Is(err, errStatusUnhealthy) {
			cmd.SilenceErrors = true
		}
		return err
	},
}

func runStatus(cmd *cobra.Command, args []string) error {
	target := ""
	if len(args) == 1 {
		target = args[0]
	}
	cfg, err := loadConfigWithFlags(cmd)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	r, err := collectStatus(ctx, cfg, target)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	if _, err := fmt.Fprint(cmd.OutOrStdout(), formatStatus(r, time.Now(), home)); err != nil {
		return err
	}
	if statusExitCode(r) != 0 {
		return errStatusUnhealthy
	}
	return nil
}

// statusExitCode is 0 when at least one server is usable; lumen fails over
// between servers, so one is enough.
func statusExitCode(r statusResult) int {
	for _, s := range r.servers {
		if s.err == nil {
			return 0
		}
	}
	return 1
}

func collectStatus(ctx context.Context, cfg *config.ConfigService, target string) (statusResult, error) {
	var r statusResult
	r.servers = probeServers(ctx, cfg.Servers())
	r.model = activeModel(r.servers)

	root, _, err := resolveIndexRoot(target, "", r.model)
	if err != nil {
		return r, err
	}
	r.root = root
	r.dims = cfg.ServerDims(0)
	r.vectorStorage = cfg.VectorStorage()
	r.maxChunkTokens = cfg.MaxChunkTokens()
	r.dbPath = configuredDBPath(cfg, root, r.model)

	snap, err := store.ReadCollectionSnapshot(r.dbPath, root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.state = legacyOrMissing(root, r.model)
		return r, nil
	case err != nil:
		r.state = indexUnreadable
		r.readErr = err
		return r, nil
	case snap.Legacy:
		r.state = indexLegacy
		return r, nil
	}

	r.collection = newCollectionStatus(r.dbPath, snap)
	if snap.Project == nil {
		r.state = legacyOrMissing(root, r.model)
		return r, nil
	}
	r.state = indexPresent
	r.dims = snap.Dimensions
	r.vectorStorage = snap.VectorStorage
	meta := snap.Project.Meta
	r.totalFiles, _ = strconv.Atoi(meta["total_files"])
	r.indexedFiles = snap.Project.Files
	r.chunks = snap.Project.Chunks
	r.lastIndexedAt = meta["last_indexed_at"]
	r.lastError = meta[store.MetaLastIndexError]
	r.indexingNow = indexingNow(r.dbPath, root)
	r.stale = staleness(root, meta["root_hash"], snap.Project.HasIncompleteFiles)
	return r, nil
}

// probeServers probes every server concurrently, keeping config order.
func probeServers(ctx context.Context, servers []config.ServerConfig) []serverStatus {
	results := make([]serverStatus, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Go(func() {
			probeCtx, cancel := context.WithTimeout(ctx, statusProbeTimeout)
			defer cancel()
			results[i] = serverStatus{server: srv, err: embedder.ProbeServer(probeCtx, srv)}
		})
	}
	wg.Wait()
	return results
}

// activeModel picks the model of the first usable server (falling back to the
// first configured one), which is the server FailoverEmbedder would index with.
func activeModel(servers []serverStatus) string {
	for _, s := range servers {
		if s.err == nil {
			return s.server.Model
		}
	}
	if len(servers) > 0 {
		return servers[0].server.Model
	}
	return ""
}

func legacyOrMissing(root, model string) indexState {
	if _, err := os.Stat(config.LegacyDBPathForProject(root, model)); err == nil {
		return indexLegacy
	}
	return indexMissing
}

func newCollectionStatus(dbPath string, snap store.CollectionSnapshot) *collectionStatus {
	c := &collectionStatus{
		dbPath:           dbPath,
		uniqueVectors:    snap.UniqueVectors,
		sharedReferences: snap.SharedReferences,
		databaseBytes:    snap.DatabaseBytes,
		reclaimableBytes: snap.ReclaimableBytes,
	}
	for _, p := range snap.Projects {
		w := worktreeStatus{path: p.Path}
		for _, ts := range []string{p.LastAccessedAt, p.LastIndexedAt} {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				w.lastUsed = t
				break
			}
		}
		c.worktrees = append(c.worktrees, w)
	}
	return c
}

// indexingNow reports whether `lumen index` would skip root right now: its
// project lock is held, or cleanup holds the collection lock exclusively.
// Other worktrees' indexers only hold the collection lock shared.
func indexingNow(dbPath, root string) bool {
	return indexlock.IsHeld(indexlock.LockPathForProject(dbPath, root)) ||
		indexlock.IsHeld(indexlock.LockPathForDB(dbPath))
}

func staleness(root, storedRootHash string, hasIncompleteFiles bool) string {
	if storedRootHash == "" || hasIncompleteFiles {
		return "yes"
	}
	tree, err := merkle.BuildTree(root, index.MakeSkip(root))
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	if tree.RootHash == storedRootHash {
		return "no"
	}
	return "yes"
}

func formatStatus(r statusResult, now time.Time, home string) string {
	var b strings.Builder
	for _, s := range r.servers {
		if s.err == nil {
			fmt.Fprintf(&b, "Embedding service: OK (%s, %s, %s)\n", s.server.Backend, s.server.Host, s.server.Model)
		} else {
			fmt.Fprintf(&b, "Embedding service: ERROR (%s, %s) — %v\n", s.server.Backend, s.server.Host, s.err)
		}
	}

	switch r.state {
	case indexMissing:
		fmt.Fprintf(&b, "Index: %s — not indexed (run `lumen index .`)\n", r.root)
	case indexLegacy:
		fmt.Fprintf(&b, "Index: %s — legacy index (will migrate on next index/search)\n", r.root)
	case indexUnreadable:
		fmt.Fprintf(&b, "Index: %s — unreadable: %v\n", r.root, r.readErr)
	case indexPresent:
		lastIndexed := orDefault(r.lastIndexedAt, "never")
		fmt.Fprintf(&b, "Index: %s\n", r.root)
		fmt.Fprintf(&b, "  Files: %s | Indexed: %s | Chunks: %s | Model: %s (%dd, %s, %d tok)\n",
			formatCount(r.totalFiles), formatCount(r.indexedFiles), formatCount(r.chunks),
			r.model, r.dims, r.vectorStorage, r.maxChunkTokens)
		fmt.Fprintf(&b, "  Last indexed: %s | Stale: %s | Indexing now: %s\n", lastIndexed, r.stale, yesNo(r.indexingNow))
		fmt.Fprintf(&b, "  Last error: %s\n", orDefault(r.lastError, "—"))
	}

	if c := r.collection; c != nil {
		noun := "worktrees"
		if len(c.worktrees) == 1 {
			noun = "worktree"
		}
		fmt.Fprintf(&b, "Collection: %s (shared by %d %s)\n", shortenHome(c.dbPath, home), len(c.worktrees), noun)
		dedup := 0.0
		if refs := c.uniqueVectors + c.sharedReferences; refs > 0 {
			dedup = float64(c.sharedReferences) / float64(refs) * 100
		}
		fmt.Fprintf(&b, "  Unique vectors: %s | Shared refs: %s | Dedup: %.1f%%\n",
			formatCount(c.uniqueVectors), formatCount(c.sharedReferences), dedup)
		fmt.Fprintf(&b, "  DB size: %s | Reclaimable: %s\n", formatSize(c.databaseBytes), formatSize(c.reclaimableBytes))
		names := make([]string, len(c.worktrees))
		for i, w := range c.worktrees {
			names[i] = fmt.Sprintf("%s (%s)", filepath.Base(w.path), formatAge(w.lastUsed, now))
		}
		fmt.Fprintf(&b, "  Worktrees: %s\n", strings.Join(names, ", "))
	}
	return b.String()
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func shortenHome(path, home string) string {
	if home != "" && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

// formatCount renders n with thousands separators.
func formatCount(n int) string {
	if n < 0 {
		return "-" + formatCount(-n)
	}
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	suffixes := []string{"KB", "MB", "GB", "TB"}
	i := -1
	for value >= unit && i < len(suffixes)-1 {
		value /= unit
		i++
	}
	if value < 10 {
		return fmt.Sprintf("%.1f %s", value, suffixes[i])
	}
	return fmt.Sprintf("%.0f %s", value, suffixes[i])
}

func formatAge(t, now time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}
