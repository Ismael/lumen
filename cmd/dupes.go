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
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ory/lumen/internal/dupes"
	"github.com/ory/lumen/internal/embedder"
	"github.com/ory/lumen/internal/store"
	"github.com/spf13/cobra"
)

const (
	dupesDefaultThreshold = 0.93
	dupesMinThreshold     = 0.80
	dupesMaxThreshold     = 1.0
)

func init() {
	rootCmd.AddCommand(newDupesCommand())
}

func newDupesCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "dupes [dir]",
		Short: "Find clusters of likely-duplicated code in the index",
		Long: `Compares the embeddings already stored in the index of the project at dir
(default: the current directory) and prints clusters of chunks whose cosine
similarity reaches --threshold. When dir is a subdirectory, only chunks under it
are compared.

dupes is read-only: it never reindexes, never calls the embedding server for
embeddings and never modifies the index. Run 'lumen index' first.

Clusters are built by single linkage: two chunks are in the same cluster when a
chain of pairs above the threshold connects them. The similarity shown for a
cluster is the range over those links, not over every pair of members, so at
lower thresholds related but different code can chain into one cluster.

Copies in different files score just below 1.0 because the file path is part of
the embedding input, so --threshold 1.0 finds almost nothing; use 0.97-0.99 for
near-exact copies. The 0.93 default was calibrated on float32 storage, and int8
(the default storage) shifts scores slightly.

Only functions, methods, constants, variables, interfaces and types are
compared; documentation chunks are not. A function longer than the chunk limit
is split, so a copy of it can appear as several clusters. Outside a git
repository, a subdirectory finds its ancestor's index only for the default
int8/512 profile.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runDupes,
	}
	c.Flags().StringP("model", "m", "", "embedding model (default: $LUMEN_EMBED_MODEL or "+embedder.DefaultModel+")")
	c.Flags().StringP("backend", "b", "", "embedding backend to select (\"ollama\" or \"lmstudio\"); disambiguates when --model is configured on multiple backends")
	c.Flags().Float32("threshold", dupesDefaultThreshold, "minimum cosine similarity, between 0.80 and 1.0; the default was calibrated on qwen3-embedding-0.6b with float32 storage and other models may need tuning")
	c.Flags().Int("min-lines", 5, "ignore chunks shorter than this many lines")
	c.Flags().Int("limit", 20, "maximum number of clusters to show (0 shows all)")
	c.Flags().Bool("no-tests", false, "exclude test files")
	return c
}

type dupesFlags struct {
	threshold float32
	minLines  int
	limit     int
	noTests   bool
}

func readDupesFlags(cmd *cobra.Command) (dupesFlags, error) {
	var f dupesFlags
	var err error
	if f.threshold, err = cmd.Flags().GetFloat32("threshold"); err != nil {
		return f, err
	}
	if f.minLines, err = cmd.Flags().GetInt("min-lines"); err != nil {
		return f, err
	}
	if f.limit, err = cmd.Flags().GetInt("limit"); err != nil {
		return f, err
	}
	if f.noTests, err = cmd.Flags().GetBool("no-tests"); err != nil {
		return f, err
	}
	switch {
	case math.IsNaN(float64(f.threshold)) || f.threshold < dupesMinThreshold || f.threshold > dupesMaxThreshold:
		return f, fmt.Errorf("invalid --threshold %v: must be between %.2f and %.1f", f.threshold, dupesMinThreshold, dupesMaxThreshold)
	case f.minLines < 1:
		return f, fmt.Errorf("invalid --min-lines %d: must be at least 1", f.minLines)
	case f.limit < 0:
		return f, fmt.Errorf("invalid --limit %d: must be 0 (all) or more", f.limit)
	}
	return f, nil
}

func runDupes(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	flags, err := readDupesFlags(cmd)
	if err != nil {
		return err
	}
	target := ""
	if len(args) == 1 {
		target = args[0]
	}

	cfg, err := loadConfigWithFlags(cmd)
	if err != nil {
		return err
	}
	model := newEmbedder(cfg).ModelName()
	root, searchPath, err := resolveIndexRoot(target, "", configuredDBPathFor(cfg, model))
	if err != nil {
		return err
	}
	info, err := os.Stat(searchPath)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory: dupes takes a directory", searchPath)
	}
	rel, err := filepath.Rel(root, searchPath)
	if err != nil {
		return fmt.Errorf("resolve %s relative to %s: %w", searchPath, root, err)
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s is outside the index root %s", searchPath, root)
	}

	dbPath := configuredDBPath(cfg, root, model)
	pv, err := store.ReadProjectVectors(dbPath, root)
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, store.ErrProjectNotFound):
		return fmt.Errorf("%s is not indexed for model %s: run `%s` first", root, model, dupesIndexHint(cmd, root))
	case err != nil:
		return fmt.Errorf("read index %s: %w", dbPath, err)
	}

	res := dupes.Find(pv.Chunks, pv.Vectors, dupes.Options{
		Threshold: flags.threshold,
		MinLines:  flags.minLines,
		Include:   dupesInclude(rel, flags.noTests),
	})
	return printDupes(cmd.OutOrStdout(), res, dupesReport{
		flags:           flags,
		model:           model,
		scope:           rel,
		incompleteFiles: pv.IncompleteFiles,
	})
}

// dupesIndexHint is the `lumen index` command that builds the collection dupes
// reads, repeating the user's --model and --backend.
func dupesIndexHint(cmd *cobra.Command, root string) string {
	parts := []string{"lumen index"}
	for _, name := range []string{"model", "backend"} {
		if v, _ := cmd.Flags().GetString(name); v != "" {
			parts = append(parts, "--"+name, v)
		}
	}
	return strings.Join(append(parts, root), " ")
}

// dupesInclude scopes chunks to the directory rel (slash-separated, "." for
// the whole index) and optionally drops test files. It returns nil when
// nothing is filtered.
func dupesInclude(rel string, noTests bool) func(string) bool {
	if rel == "." && !noTests {
		return nil
	}
	return func(path string) bool {
		path = filepath.ToSlash(path)
		if rel != "." && path != rel && !strings.HasPrefix(path, rel+"/") {
			return false
		}
		return !noTests || !isTestFile(path)
	}
}

type dupesReport struct {
	flags           dupesFlags
	model           string
	scope           string
	incompleteFiles int
}

func printDupes(w io.Writer, res dupes.Result, r dupesReport) error {
	var b strings.Builder
	total := len(res.Clusters)
	shown := total
	if r.flags.limit > 0 && r.flags.limit < total {
		shown = r.flags.limit
	}

	switch {
	case res.Compared == 0:
		b.WriteString("No indexed code chunks matched (check the directory scope, --min-lines and --no-tests)\n")
	case total == 0:
		fmt.Fprintf(&b, "No duplicates found at threshold %s (%d chunks compared)\n", formatDupesThreshold(r.flags.threshold), res.Compared)
	}

	for i, c := range res.Clusters[:shown] {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "#%d  %d chunks · %d lines · similarity %s\n", i+1, len(c.Chunks), c.Lines, formatDupesSimilarity(c.MinSim, c.MaxSim))
		writeDupesChunks(&b, c)
	}
	if shown > 0 {
		b.WriteByte('\n')
	}

	fields := make([]string, 0, 5)
	if total > 0 {
		fields = append(fields, fmt.Sprintf("Showing %d of %d clusters", shown, total))
	}
	fields = append(fields,
		fmt.Sprintf("%d chunks compared", res.Compared),
		"threshold "+formatDupesThreshold(r.flags.threshold),
		"model "+r.model,
	)
	if r.scope != "." {
		fields = append(fields, "scope "+r.scope+"/")
	}
	b.WriteString(strings.Join(fields, " · "))
	b.WriteByte('\n')
	if r.incompleteFiles > 0 {
		fmt.Fprintf(&b, "%d files mid-index were skipped\n", r.incompleteFiles)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func writeDupesChunks(b *strings.Builder, c dupes.Cluster) {
	locations := make([]string, len(c.Chunks))
	width := 0
	for i, ch := range c.Chunks {
		locations[i] = fmt.Sprintf("%s:%d-%d", ch.RelativePath, ch.StartLine, ch.EndLine)
		width = max(width, len(locations[i]))
	}
	for i, ch := range c.Chunks {
		fmt.Fprintf(b, "  %-*s   %s (%s)\n", width, locations[i], ch.Symbol, ch.Kind)
	}
}

// formatDupesSimilarity shows the strongest link, or the min–max range of the
// cluster's links when the two differ at two decimals.
func formatDupesSimilarity(minSim, maxSim float32) string {
	lo, hi := fmt.Sprintf("%.2f", minSim), fmt.Sprintf("%.2f", maxSim)
	if lo == hi {
		return hi
	}
	return lo + "–" + hi
}

func formatDupesThreshold(t float32) string {
	return strconv.FormatFloat(float64(t), 'g', -1, 32)
}
