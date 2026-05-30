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
	"strings"

	"github.com/ory/lumen/internal/config"
	"github.com/ory/lumen/internal/embedder"
	"github.com/ory/lumen/internal/git"
	"github.com/ory/lumen/internal/index"
	"github.com/ory/lumen/internal/merkle"
	"github.com/spf13/cobra"
)

// statusResult is the combined outcome of the two status checks: embedding
// service reachability and index presence/freshness.
type statusResult struct {
	server           config.ServerConfig
	serviceReachable bool
	serviceMessage   string

	projectPath   string
	indexed       bool
	totalFiles    int
	indexedFiles  int
	totalChunks   int
	model         string
	lastIndexedAt string
	stale         bool
}

// statusExitCode returns 0 only when the service is reachable, the index
// exists, and the index is fresh; otherwise 1.
func statusExitCode(r statusResult) int {
	if r.serviceReachable && r.indexed && !r.stale {
		return 0
	}
	return 1
}

// formatStatus renders a human-readable status report combining the embedding
// service line and the index block (or a "not indexed" line).
func formatStatus(r statusResult) string {
	var b strings.Builder

	if r.serviceReachable {
		fmt.Fprintf(&b, "Embedding service: OK (%s, %s, %s)\n", r.server.Backend, r.server.Host, r.server.Model)
	} else {
		fmt.Fprintf(&b, "Embedding service: ERROR (%s, %s) — %s\n", r.server.Backend, r.server.Host, r.serviceMessage)
	}

	if !r.indexed {
		fmt.Fprintf(&b, "Index: %s — not indexed (run `lumen index .`, or it will seed on first search)", r.projectPath)
		return b.String()
	}

	stale := "no"
	if r.stale {
		stale = "yes"
	}
	lastIndexed := r.lastIndexedAt
	if lastIndexed == "" {
		lastIndexed = "never"
	}
	fmt.Fprintf(&b, "Index: %s\n", r.projectPath)
	fmt.Fprintf(&b, "  Files: %d | Indexed: %d | Chunks: %d | Model: %s\n", r.totalFiles, r.indexedFiles, r.totalChunks, r.model)
	fmt.Fprintf(&b, "  Last indexed: %s | Stale: %s", lastIndexed, stale)
	return b.String()
}

// errStatusUnhealthy signals a non-zero exit without printing a cobra error
// banner; the human-readable report has already been written to stdout.
var errStatusUnhealthy = errors.New("status: unhealthy")

func init() {
	statusCmd.Flags().StringP("model", "m", "", "embedding model (default: $LUMEN_EMBED_MODEL or "+embedder.DefaultModel+")")
	statusCmd.Flags().StringP("backend", "b", "", "embedding backend to select (\"ollama\" or \"lmstudio\"); disambiguates when --model is configured on multiple backends")
	// runStatus prints the report itself; suppress cobra's error/usage output so
	// a non-zero exit on stale/unreachable does not double-print.
	statusCmd.SilenceErrors = true
	statusCmd.SilenceUsage = true
	rootCmd.AddCommand(statusCmd)
}

var statusCmd = &cobra.Command{
	Use:   "status [path]",
	Short: "Report embedding-service health and index freshness",
	Long: `Reports whether the embedding service is reachable and whether the
project's index exists and is fresh.

With no argument, inspects the current directory. The path is normalized to the
git repository root (or an existing ancestor index) so it reports on the same
index that searches use.

status performs no indexing and never creates an index database. Exit code is 0
only when the service is reachable and the index exists and is fresh; otherwise
1.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runStatus,
}

func runStatus(cmd *cobra.Command, args []string) error {
	target := "."
	if len(args) == 1 {
		target = args[0]
	}
	projectPath, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	cfg, err := loadConfigWithFlags(cmd)
	if err != nil {
		return err
	}
	emb := newEmbedder(cfg)

	r := collectStatus(cmd.Context(), cfg, emb, projectPath)
	fmt.Fprintln(cmd.OutOrStdout(), formatStatus(r))

	if statusExitCode(r) != 0 {
		return errStatusUnhealthy
	}
	return nil
}

// collectStatus runs both checks without side effects: it pings the configured
// embedding server and reads the index DB read-only (never creating it). The
// project path is normalized the same way `lumen index` does.
func collectStatus(ctx context.Context, cfg *config.ConfigService, emb *embedder.FailoverEmbedder, projectPath string) statusResult {
	r := statusResult{projectPath: projectPath}

	// Probe the primary configured server. Unlike the MCP handleHealthCheck
	// handler, which targets the active server after failover, a one-shot CLI
	// command has no live failover state, so servers[0] is the right choice.
	servers := cfg.Servers()
	if len(servers) > 0 {
		r.server = servers[0]
		r.serviceReachable, r.serviceMessage = probeEmbeddingService(ctx, servers[0])
	}

	modelName := emb.ModelName()

	// Normalize the path the same way the indexer does so we read the right DB.
	if root, gitErr := git.RepoRoot(projectPath); gitErr == nil {
		projectPath = root
	} else if ancestor := findAncestorIndex(projectPath, modelName); ancestor != "" {
		projectPath = ancestor
	}
	r.projectPath = projectPath

	if unindexable, _ := merkle.IsRootUnindexable(projectPath); unindexable {
		r.indexed = false
		return r
	}

	// Detect a missing index without creating one: stat the DB file first.
	dbPath := config.DBPathForProject(projectPath, modelName)
	if _, statErr := os.Stat(dbPath); statErr != nil {
		r.indexed = false
		return r
	}

	idx, openErr := index.NewIndexer(dbPath, emb, cfg.MaxChunkTokens())
	if openErr != nil {
		r.indexed = false
		return r
	}
	defer func() { _ = idx.Close() }()

	info, infoErr := idx.Status(projectPath)
	if infoErr != nil {
		r.indexed = false
		return r
	}
	r.indexed = true
	r.totalFiles = info.TotalFiles
	r.indexedFiles = info.IndexedFiles
	r.totalChunks = info.TotalChunks
	r.model = info.EmbeddingModel
	r.lastIndexedAt = info.LastIndexedAt

	fresh, freshErr := idx.IsFresh(projectPath)
	if freshErr != nil {
		// Treat an un-checkable index as stale rather than silently fresh.
		r.stale = true
	} else {
		r.stale = !fresh
	}
	return r
}
