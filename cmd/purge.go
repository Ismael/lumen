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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ory/lumen/internal/config"
	"github.com/ory/lumen/internal/git"
	"github.com/ory/lumen/internal/store"
	"github.com/spf13/cobra"
)

const (
	flagAll     = "all"
	flagMissing = "missing"
	flagDryRun  = "dry-run"
)

func init() {
	registerPurgeFlags(purgeCmd)
	rootCmd.AddCommand(purgeCmd)
}

// registerPurgeFlags declares the purge command's flags. Shared by init and
// the test helper so tests exercise the real flag set.
func registerPurgeFlags(cmd *cobra.Command) {
	cmd.Flags().Bool(flagAll, false, "Remove every index under the data directory")
	cmd.Flags().Bool(flagMissing, false, "Remove indexes whose project folder no longer exists")
	cmd.Flags().Bool(flagDryRun, false, "With --missing, list what would be removed without deleting")
}

// lumenDataDir returns the directory holding all lumen index databases.
func lumenDataDir() string {
	return filepath.Join(config.XDGDataDir(), "lumen")
}

var purgeCmd = &cobra.Command{
	Use:   "purge [path...]",
	Short: "Remove lumen index data",
	Long: `Deletes lumen index databases under ~/.local/share/lumen/.

With no arguments, removes only the index for the current working directory's
project (the path is normalized to its git root first).

With one or more paths, removes the index directories associated with those
projects. Each path is normalized to its git root, then matched against the
project_path recorded inside each index database, so switching embedding models
or using custom models never leaves orphan indexes.

  --all       Remove every index (irreversible — all indexes will be rebuilt on
              the next search). Also clears legacy indexes created by older
              binaries that did not record project_path.
  --missing   Remove every index whose recorded project folder no longer exists
              on disk. Only deletes when the folder is confirmed missing.
  --dry-run   With --missing, list what would be removed without deleting.

Note: a concurrently running indexer for a purged project may log a write
error and exit; re-run "lumen index" afterwards to rebuild.`,
	Args: cobra.ArbitraryArgs,
	RunE: runPurge,
}

func runPurge(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool(flagAll)
	missing, _ := cmd.Flags().GetBool(flagMissing)
	dryRun, _ := cmd.Flags().GetBool(flagDryRun)

	if err := validatePurgeFlags(all, missing, dryRun, len(args)); err != nil {
		return err
	}

	stderr := cmd.ErrOrStderr()
	stdout := cmd.OutOrStdout()

	switch {
	case all:
		return purgeAll(stderr)
	case missing:
		return purgeMissing(stderr, stdout, dryRun)
	default:
		if len(args) == 0 {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("determine working directory: %w", err)
			}
			args = []string{cwd}
		}
		return purgeProjects(stderr, stdout, args)
	}
}

// validatePurgeFlags enforces mutual exclusivity of the purge modes.
func validatePurgeFlags(all, missing, dryRun bool, nArgs int) error {
	if all && missing {
		return fmt.Errorf("--all and --missing cannot be combined")
	}
	if all && nArgs > 0 {
		return fmt.Errorf("--all cannot be combined with explicit paths")
	}
	if missing && nArgs > 0 {
		return fmt.Errorf("--missing cannot be combined with explicit paths")
	}
	if dryRun && !missing {
		return fmt.Errorf("--dry-run is only valid with --missing")
	}
	return nil
}

func purgeAll(stderr io.Writer) error {
	dataDir := lumenDataDir()

	info, err := os.Stat(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			_, _ = fmt.Fprintln(stderr, "No index data found — nothing to purge.")
			return nil
		}
		return fmt.Errorf("stat data directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dataDir)
	}

	// Log each index directory before wiping, matching the per-index logging
	// used by the other purge modes. Legacy dirs without project_path metadata
	// are logged by path alone.
	indexMap, legacy, _ := scanIndexes(dataDir)
	for projectPath, hashDirs := range indexMap {
		for _, hashDir := range hashDirs {
			_, _ = fmt.Fprintf(stderr, "Removed %s (%s)\n", hashDir, projectPath)
		}
	}
	for _, hashDir := range legacy {
		_, _ = fmt.Fprintf(stderr, "Removed %s\n", hashDir)
	}

	if err := os.RemoveAll(dataDir); err != nil {
		return fmt.Errorf("remove index data: %w", err)
	}
	_, _ = fmt.Fprintf(stderr, "Removed all index data (%s)\n", dataDir)
	return nil
}

func purgeProjects(stderr, stdout io.Writer, args []string) error {
	indexMap, _, err := scanIndexes(lumenDataDir())
	if err != nil {
		return err
	}

	seen := make(map[string]bool)
	totalRemoved := 0
	for _, arg := range args {
		removed, err := purgeOneTarget(stderr, indexMap, seen, arg)
		if err != nil {
			return err
		}
		totalRemoved += removed
	}
	_, _ = fmt.Fprintf(stdout, "Removed %d index director%s.\n", totalRemoved, pluralY(totalRemoved))
	return nil
}

func purgeMissing(stderr, stdout io.Writer, dryRun bool) error {
	indexMap, _, err := scanIndexes(lumenDataDir())
	if err != nil {
		return err
	}

	verb := "Removed"
	if dryRun {
		verb = "Would remove"
	}

	removed := 0
	for projectPath, hashDirs := range indexMap {
		if _, statErr := os.Stat(projectPath); statErr == nil {
			continue // folder still exists — keep the index
		} else if !os.IsNotExist(statErr) {
			// Conservative: any error other than "not exist" must never delete.
			_, _ = fmt.Fprintf(stderr, "Skipping %s: cannot stat (%v)\n", projectPath, statErr)
			continue
		}
		for _, hashDir := range hashDirs {
			if !dryRun {
				if err := os.RemoveAll(hashDir); err != nil {
					return fmt.Errorf("remove %s: %w", hashDir, err)
				}
			}
			_, _ = fmt.Fprintf(stderr, "%s %s (%s)\n", verb, hashDir, projectPath)
			removed++
		}
	}

	_, _ = fmt.Fprintf(stdout, "%s %d index director%s whose folder no longer exists.\n", verb, removed, pluralY(removed))
	return nil
}

// scanIndexes walks dataDir (one level deep) and returns a map of stored
// project_path → list of hash directories for that project, plus the hash
// directories that can't be read or lack project_path metadata (legacy
// indexes). Path-based purge modes ignore the legacy slice so a single broken
// index never blocks purging of others; --all uses it to log those dirs.
func scanIndexes(dataDir string) (indexMap map[string][]string, legacy []string, err error) {
	indexMap = make(map[string][]string)
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return indexMap, nil, nil
		}
		return nil, nil, fmt.Errorf("read data dir: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		hashDir := filepath.Join(dataDir, entry.Name())
		dbPath := filepath.Join(hashDir, "index.db")
		stored, err := store.ReadMetaAt(dbPath, "project_path")
		if err != nil || stored == "" {
			legacy = append(legacy, hashDir)
			continue
		}
		indexMap[stored] = append(indexMap[stored], hashDir)
	}
	return indexMap, legacy, nil
}

// purgeOneTarget resolves arg to a project root and removes every hash
// directory whose stored project_path matches. seen tracks hash directories
// already deleted during this invocation so two args resolving to the same
// project are not double-counted.
func purgeOneTarget(stderr io.Writer, indexMap map[string][]string, seen map[string]bool, arg string) (int, error) {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return 0, fmt.Errorf("resolve %q: %w", arg, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	target := abs
	inGitRepo := false
	if root, err := git.RepoRoot(abs); err == nil {
		target = root
		inGitRepo = true
	}

	match := ""
	if _, ok := indexMap[target]; ok {
		match = target
	} else if !inGitRepo {
		// Non-git fallback: match the deepest stored path that contains the
		// target. Mirrors `findAncestorIndex` semantics used by index/search.
		match = longestAncestor(indexMap, target)
	}

	if match == "" {
		_, _ = fmt.Fprintf(stderr, "No index found for %s.\n", abs)
		return 0, nil
	}

	removed := 0
	for _, hashDir := range indexMap[match] {
		if seen[hashDir] {
			continue
		}
		seen[hashDir] = true
		if err := os.RemoveAll(hashDir); err != nil {
			return removed, fmt.Errorf("remove %s: %w", hashDir, err)
		}
		removed++
	}
	_, _ = fmt.Fprintf(stderr, "Removed %d index director%s for %s.\n",
		removed, pluralY(removed), match)
	return removed, nil
}

// longestAncestor returns the longest key in indexMap that is either equal to
// target or an ancestor directory of target, or "" if no such key exists.
func longestAncestor(indexMap map[string][]string, target string) string {
	best := ""
	for stored := range indexMap {
		if stored == target || strings.HasPrefix(target, stored+string(filepath.Separator)) {
			if len(stored) > len(best) {
				best = stored
			}
		}
	}
	return best
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
