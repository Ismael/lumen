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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ory/lumen/internal/config"
	"github.com/ory/lumen/internal/embedder"
	"github.com/ory/lumen/internal/store"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedIndex creates a real SQLite index DB at the hash-named directory for
// (projectPath, model) with project_path recorded in project_meta, so that
// tests exercise the same metadata-scan code path as production.
func seedIndex(t *testing.T, projectPath, model string) string {
	t.Helper()
	dbPath := config.DBPathForProject(projectPath, model)
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	s, err := store.New(dbPath, 4)
	require.NoError(t, err)
	require.NoError(t, s.SetMeta("project_path", projectPath))
	require.NoError(t, s.Close())
	return filepath.Dir(dbPath)
}

// runPurgeCmd invokes runPurge with the provided args and optional flag names
// (each set to "true") and returns captured stdout, stderr, and the error.
func runPurgeCmd(t *testing.T, args []string, flags ...string) (stdout, stderr string, err error) {
	t.Helper()
	outBuf := new(bytes.Buffer)
	errBuf := new(bytes.Buffer)
	cmd := &cobra.Command{}
	registerPurgeFlags(cmd)
	for _, f := range flags {
		require.NoError(t, cmd.Flags().Set(f, "true"))
	}
	cmd.SetOut(outBuf)
	cmd.SetErr(errBuf)
	err = runPurge(cmd, args)
	return outBuf.String(), errBuf.String(), err
}

func TestPurge_NoArgs_PurgesCwdOnly(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	projectA := filepath.Join(tmp, "projectA")
	projectB := filepath.Join(tmp, "projectB")
	require.NoError(t, os.MkdirAll(projectA, 0o755))
	require.NoError(t, os.MkdirAll(projectB, 0o755))
	runGit(t, projectA, "init")
	runGit(t, projectB, "init")

	hashDirA := seedIndex(t, projectA, embedder.DefaultModel)
	hashDirB := seedIndex(t, projectB, embedder.DefaultModel)

	// No-args default operates on the current working directory.
	t.Chdir(projectA)

	_, _, err := runPurgeCmd(t, nil)
	require.NoError(t, err)

	_, err = os.Stat(hashDirA)
	assert.True(t, os.IsNotExist(err), "cwd project A hash dir should be gone")
	_, err = os.Stat(hashDirB)
	assert.NoError(t, err, "project B hash dir should be untouched")
}

func TestPurge_NoArgs_CwdWithoutIndex_ReportsNone(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	empty := filepath.Join(tmp, "empty")
	require.NoError(t, os.MkdirAll(empty, 0o755))
	t.Chdir(empty)

	_, stderrOut, err := runPurgeCmd(t, nil)
	require.NoError(t, err)
	assert.Contains(t, strings.ToLower(stderrOut), "no index found")
}

func TestPurge_All_RemovesEverything(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	seedIndex(t, "/project/a", embedder.DefaultModel)
	hashDirB := seedIndex(t, "/project/b", embedder.DefaultModel)

	// A legacy index dir with no project_path metadata must also be wiped.
	lumenRoot := filepath.Join(tmp, "lumen")
	legacyDir := filepath.Join(lumenRoot, "legacyhash")
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))

	_, stderrOut, err := runPurgeCmd(t, nil, flagAll)
	require.NoError(t, err)
	assert.Contains(t, stderrOut, "Removed all index data")
	assert.Contains(t, stderrOut, hashDirB, "should log each removed index dir")

	_, err = os.Stat(lumenRoot)
	assert.True(t, os.IsNotExist(err), "lumen data dir should be gone, got err=%v", err)
}

func TestPurge_All_WithPaths_Errors(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	_, _, err := runPurgeCmd(t, []string{"/some/path"}, flagAll)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--all")
}

func TestPurge_SinglePath_RemovesOnlyThatProject(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	// Two independent projects with seeded indexes.
	projectA := filepath.Join(tmp, "projectA")
	projectB := filepath.Join(tmp, "projectB")
	require.NoError(t, os.MkdirAll(projectA, 0o755))
	require.NoError(t, os.MkdirAll(projectB, 0o755))
	runGit(t, projectA, "init")
	runGit(t, projectB, "init")

	hashDirA := seedIndex(t, projectA, embedder.DefaultModel)
	hashDirB := seedIndex(t, projectB, embedder.DefaultModel)

	_, stderrOut, err := runPurgeCmd(t, []string{projectA})
	require.NoError(t, err)
	assert.Contains(t, stderrOut, projectA, "should log the purged project path")

	_, err = os.Stat(hashDirA)
	assert.True(t, os.IsNotExist(err), "project A hash dir should be gone")
	_, err = os.Stat(hashDirB)
	assert.NoError(t, err, "project B hash dir should be untouched")
}

func TestPurge_PathInsideGitRepo_ResolvesToGitRoot(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	repoDir := filepath.Join(tmp, "repo")
	subDir := filepath.Join(repoDir, "sub")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	runGit(t, repoDir, "init")

	hashDir := seedIndex(t, repoDir, embedder.DefaultModel)

	_, _, err := runPurgeCmd(t, []string{subDir})
	require.NoError(t, err)

	_, err = os.Stat(hashDir)
	assert.True(t, os.IsNotExist(err), "git-root hash dir should be removed when passing a subdirectory")
}

func TestPurge_PathWithAncestorIndex_ResolvesToAncestor(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	grandparent := filepath.Join(tmp, "workspace")
	child := filepath.Join(grandparent, "a", "b")
	require.NoError(t, os.MkdirAll(child, 0o755))

	hashDir := seedIndex(t, grandparent, embedder.DefaultModel)

	_, _, err := runPurgeCmd(t, []string{child})
	require.NoError(t, err)

	_, err = os.Stat(hashDir)
	assert.True(t, os.IsNotExist(err), "ancestor hash dir should be removed")
}

func TestPurge_PathWithoutIndex_ReportsNoneAndExitsZero(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	// Path exists but has no index anywhere up the tree.
	dir := filepath.Join(tmp, "empty")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	_, stderrOut, err := runPurgeCmd(t, []string{dir})
	require.NoError(t, err)
	assert.Contains(t, strings.ToLower(stderrOut), "no index found")
}

func TestPurge_MultiplePaths_RemovesEach(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	projectA := filepath.Join(tmp, "projectA")
	projectB := filepath.Join(tmp, "projectB")
	require.NoError(t, os.MkdirAll(projectA, 0o755))
	require.NoError(t, os.MkdirAll(projectB, 0o755))
	runGit(t, projectA, "init")
	runGit(t, projectB, "init")

	hashDirA := seedIndex(t, projectA, embedder.DefaultModel)
	hashDirB := seedIndex(t, projectB, embedder.DefaultModel)

	_, _, err := runPurgeCmd(t, []string{projectA, projectB})
	require.NoError(t, err)

	_, err = os.Stat(hashDirA)
	assert.True(t, os.IsNotExist(err), "project A hash dir should be gone")
	_, err = os.Stat(hashDirB)
	assert.True(t, os.IsNotExist(err), "project B hash dir should be gone")
}

func TestPurge_UnknownModelName_StillPurgedByStoredMetadata(t *testing.T) {
	// Indexes created with custom or aliased model names (not in KnownModels)
	// must still be purged — the match is by stored project_path, not by
	// enumerating known models and recomputing the hash.
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	project := filepath.Join(tmp, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	runGit(t, project, "init")

	hashDir := seedIndex(t, project, "some-custom-alias-model-not-in-registry")

	_, _, err := runPurgeCmd(t, []string{project})
	require.NoError(t, err)

	_, err = os.Stat(hashDir)
	assert.True(t, os.IsNotExist(err), "custom-model hash dir should be removed via stored project_path")
}

func TestPurge_MultiplePaths_MixedHitsAndMisses(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	projectA := filepath.Join(tmp, "projectA")
	empty := filepath.Join(tmp, "empty")
	require.NoError(t, os.MkdirAll(projectA, 0o755))
	require.NoError(t, os.MkdirAll(empty, 0o755))
	runGit(t, projectA, "init")

	hashDirA := seedIndex(t, projectA, embedder.DefaultModel)

	_, stderrOut, err := runPurgeCmd(t, []string{projectA, empty})
	require.NoError(t, err)

	_, err = os.Stat(hashDirA)
	assert.True(t, os.IsNotExist(err), "project A hash dir should be gone")
	assert.Contains(t, strings.ToLower(stderrOut), "no index found", "miss should be reported")
}

func TestPurge_Missing_RemovesOnlyDeletedFolders(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	gone := filepath.Join(tmp, "gone")
	alive := filepath.Join(tmp, "alive")
	require.NoError(t, os.MkdirAll(gone, 0o755))
	require.NoError(t, os.MkdirAll(alive, 0o755))

	hashGone := seedIndex(t, gone, embedder.DefaultModel)
	hashAlive := seedIndex(t, alive, embedder.DefaultModel)

	// Delete one project's folder.
	require.NoError(t, os.RemoveAll(gone))

	_, stderrOut, err := runPurgeCmd(t, nil, flagMissing)
	require.NoError(t, err)
	assert.Contains(t, stderrOut, hashGone, "should log the removed index dir")

	_, err = os.Stat(hashGone)
	assert.True(t, os.IsNotExist(err), "index for deleted folder should be removed")
	_, err = os.Stat(hashAlive)
	assert.NoError(t, err, "index for existing folder should be kept")
}

func TestPurge_Missing_AllFoldersExist_RemovesNothing(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	alive := filepath.Join(tmp, "alive")
	require.NoError(t, os.MkdirAll(alive, 0o755))
	hashAlive := seedIndex(t, alive, embedder.DefaultModel)

	_, _, err := runPurgeCmd(t, nil, flagMissing)
	require.NoError(t, err)

	_, err = os.Stat(hashAlive)
	assert.NoError(t, err, "index for existing folder should be kept")
}

func TestPurge_Missing_WithPaths_Errors(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	_, _, err := runPurgeCmd(t, []string{"/some/path"}, flagMissing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--missing")
}

func TestPurge_DryRun_Missing_DeletesNothing(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	gone := filepath.Join(tmp, "gone")
	require.NoError(t, os.MkdirAll(gone, 0o755))
	hashGone := seedIndex(t, gone, embedder.DefaultModel)
	require.NoError(t, os.RemoveAll(gone))

	_, stderrOut, err := runPurgeCmd(t, nil, flagMissing, flagDryRun)
	require.NoError(t, err)
	assert.Contains(t, stderrOut, "Would remove")
	assert.Contains(t, stderrOut, hashGone)

	_, err = os.Stat(hashGone)
	assert.NoError(t, err, "dry-run must not delete the index dir")
}

func TestPurge_DryRun_WithoutMissing_Errors(t *testing.T) {
	tmp := resolvedTempDir(t)
	t.Setenv("XDG_DATA_HOME", tmp)

	_, _, err := runPurgeCmd(t, nil, flagDryRun)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--dry-run is only valid with --missing")
}
