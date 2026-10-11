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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ory/lumen/internal/chunker"
	"github.com/ory/lumen/internal/config"
	"github.com/ory/lumen/internal/embedder"
	"github.com/ory/lumen/internal/store"
)

type dupesFixtureChunk struct {
	path, symbol, kind string
	start, end         int
	vec                []float32
}

// dupesFixture is a git-initialised project whose index lives at the path the
// command computes for it.
type dupesFixture struct {
	root   string
	dbPath string
	dims   int
}

func newDupesFixture(t *testing.T) *dupesFixture {
	t.Helper()
	statusTestEnv(t, true)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	cfg, err := config.NewConfigService(config.DefaultConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	return &dupesFixture{
		root:   root,
		dbPath: configuredDBPath(cfg, root, embedder.DefaultModel),
		dims:   cfg.ServerDims(0),
	}
}

// vec pads components with zeros to the model dimensions.
func (f *dupesFixture) vec(components ...float32) []float32 {
	v := make([]float32, f.dims)
	copy(v, components)
	return v
}

// seed indexes chunks and creates a directory for each file's parent.
func (f *dupesFixture) seed(t *testing.T, chunks []dupesFixtureChunk) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewConfigService(config.DefaultConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.NewCollection(f.dbPath, f.dims, cfg.VectorStorage(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	var order []string
	byFile := map[string][]dupesFixtureChunk{}
	for _, c := range chunks {
		if _, seen := byFile[c.path]; !seen {
			order = append(order, c.path)
		}
		byFile[c.path] = append(byFile[c.path], c)
	}
	for _, path := range order {
		if err := os.MkdirAll(filepath.Join(f.root, filepath.Dir(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		var cs []chunker.Chunk
		vectors := map[int][]float32{}
		for i, c := range byFile[path] {
			cs = append(cs, chunker.Chunk{
				ID: path + "#" + c.symbol, FilePath: path, Symbol: c.symbol, Kind: c.kind,
				StartLine: c.start, EndLine: c.end, Content: path + "#" + c.symbol,
			})
			vectors[i] = f.vec(c.vec...)
		}
		if _, err := s.StoreFileRevision(path, "hash-"+path, cs, vectors); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *dupesFixture) markIncomplete(t *testing.T, paths ...string) {
	t.Helper()
	db, err := sql.Open("sqlite3", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, p := range paths {
		if _, err := db.Exec(`UPDATE file_revisions SET complete = 0 WHERE relative_path = ?`, p); err != nil {
			t.Fatal(err)
		}
	}
}

// pair returns two function chunks in different files with identical
// direction and the given line count.
func pair(symbol string, lines int, pathA, pathB string, vec ...float32) []dupesFixtureChunk {
	return []dupesFixtureChunk{
		{pathA, symbol, "function", 10, 10 + lines - 1, vec},
		{pathB, symbol, "method", 5, 5 + lines - 1, vec},
	}
}

func joinChunks(groups ...[]dupesFixtureChunk) []dupesFixtureChunk {
	var all []dupesFixtureChunk
	for _, g := range groups {
		all = append(all, g...)
	}
	return all
}

// execDupes executes a fresh dupes command with args and returns its output.
func execDupes(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	c := newDupesCommand()
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs(args)
	err = c.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func TestDupesCommand_PrintsClustersInOrderWithFooter(t *testing.T) {
	f := newDupesFixture(t)
	f.seed(t, joinChunks(
		pair("Small", 6, "a/small1.go", "a/small2.go", 0, 1),
		pair("Big", 20, "b/big1.go", "b/big2.go", 1),
		// Distinct directions, so the unrelated chunk is never linked.
		[]dupesFixtureChunk{{"c/other.go", "Other", "function", 1, 30, []float32{0, 0, 1}}},
	))

	stdout, _, err := execDupes(t, f.root)
	if err != nil {
		t.Fatal(err)
	}
	want := "#1  2 chunks · 40 lines · similarity 1.00\n" +
		"  b/big1.go:10-29   Big (function)\n" +
		"  b/big2.go:5-24    Big (method)\n" +
		"\n" +
		"#2  2 chunks · 12 lines · similarity 1.00\n" +
		"  a/small1.go:10-15   Small (function)\n" +
		"  a/small2.go:5-10    Small (method)\n" +
		"\n" +
		"Showing 2 of 2 clusters · 5 chunks compared · threshold 0.93 · model " + embedder.DefaultModel + "\n"
	if stdout != want {
		t.Errorf("output mismatch\n got:\n%s\nwant:\n%s", stdout, want)
	}
}

func TestDupesCommand_Limit(t *testing.T) {
	f := newDupesFixture(t)
	f.seed(t, joinChunks(
		pair("Big", 20, "b/big1.go", "b/big2.go", 1),
		pair("Small", 6, "a/small1.go", "a/small2.go", 0, 1),
	))

	stdout, _, err := execDupes(t, "--limit", "1", f.root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "#1  ") || strings.Contains(stdout, "#2  ") {
		t.Errorf("want exactly one cluster:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Showing 1 of 2 clusters") {
		t.Errorf("footer missing Showing 1 of 2:\n%s", stdout)
	}

	stdout, _, err = execDupes(t, "--limit", "0", f.root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "#2  ") || !strings.Contains(stdout, "Showing 2 of 2 clusters") {
		t.Errorf("--limit 0 must show every cluster:\n%s", stdout)
	}
}

func TestDupesCommand_NoTests(t *testing.T) {
	f := newDupesFixture(t)
	f.seed(t, joinChunks(
		pair("Real", 10, "lib/a.go", "lib/b.go", 1),
		pair("Fixture", 30, "lib/a_test.go", "lib/b_test.go", 0, 1),
	))

	stdout, _, err := execDupes(t, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "_test.go") {
		t.Fatalf("test-file cluster expected without --no-tests:\n%s", stdout)
	}

	stdout, _, err = execDupes(t, "--no-tests", f.root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "_test.go") {
		t.Errorf("--no-tests kept a test file:\n%s", stdout)
	}
	if !strings.Contains(stdout, "lib/a.go") || !strings.Contains(stdout, "Showing 1 of 1 clusters") {
		t.Errorf("non-test cluster missing:\n%s", stdout)
	}
}

func TestDupesCommand_SubdirectoryScope(t *testing.T) {
	f := newDupesFixture(t)
	f.seed(t, joinChunks(
		// Same direction everywhere, so an unscoped run links all of them.
		pair("InScope", 10, "src/foo/a.go", "src/foo/b.go", 1),
		[]dupesFixtureChunk{{"src/foobar/c.go", "InScope", "function", 1, 10, []float32{1}}},
		pair("TestOnly", 10, "src/foo/x_test.go", "src/foo/y_test.go", 0, 1),
	))

	stdout, _, err := execDupes(t, filepath.Join(f.root, "src", "foo"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "foobar") {
		t.Errorf("src/foobar leaked into src/foo scope:\n%s", stdout)
	}
	for _, want := range []string{"src/foo/a.go", "src/foo/b.go", "src/foo/x_test.go", "· scope src/foo/\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}

	stdout, _, err = execDupes(t, "--no-tests", filepath.Join(f.root, "src", "foo"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "foobar") || strings.Contains(stdout, "_test.go") {
		t.Errorf("scope with --no-tests leaked paths:\n%s", stdout)
	}
	if !strings.Contains(stdout, "src/foo/a.go") || !strings.Contains(stdout, "· scope src/foo/\n") {
		t.Errorf("scoped cluster or scope footer missing:\n%s", stdout)
	}

	stdout, _, err = execDupes(t, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "scope") {
		t.Errorf("root run must not print a scope:\n%s", stdout)
	}
}

func TestDupesCommand_SimilarityRangeAcrossLinks(t *testing.T) {
	f := newDupesFixture(t)
	// a-c falls below the threshold, so c joins the cluster only through b.
	f.seed(t, []dupesFixtureChunk{
		{"r/a.go", "F", "function", 1, 10, []float32{1, 0}},
		{"r/b.go", "F", "function", 1, 10, []float32{1, 0.25}},
		{"r/c.go", "F", "function", 1, 10, []float32{1, 0.5}},
	})

	stdout, _, err := execDupes(t, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "3 chunks · 30 lines · similarity 0.97–0.98\n") {
		t.Errorf("want a similarity range:\n%s", stdout)
	}
}

func TestDupesCommand_RejectsFileArgument(t *testing.T) {
	f := newDupesFixture(t)
	f.seed(t, pair("Big", 10, "b/big1.go", "b/big2.go", 1))
	file := filepath.Join(f.root, "b", "big1.go")
	if err := os.WriteFile(file, []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := execDupes(t, file)
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("err = %v, want an error saying dupes takes a directory", err)
	}
}

func TestDupesCommand_RejectsInvalidFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"threshold below range", []string{"--threshold", "0.5"}, "--threshold"},
		{"threshold above range", []string{"--threshold", "1.1"}, "--threshold"},
		{"min-lines zero", []string{"--min-lines", "0"}, "--min-lines"},
		{"limit negative", []string{"--limit", "-1"}, "--limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDupesFixture(t)
			f.seed(t, pair("Big", 10, "b/big1.go", "b/big2.go", 1))
			stdout, _, err := execDupes(t, append(tt.args, f.root)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to name %s", err, tt.want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}

func TestDupesCommand_AcceptsBoundaryFlags(t *testing.T) {
	f := newDupesFixture(t)
	f.seed(t, pair("Big", 10, "b/big1.go", "b/big2.go", 1))
	for _, th := range []string{"0.80", "1.0"} {
		if _, _, err := execDupes(t, "--threshold", th, f.root); err != nil {
			t.Errorf("--threshold %s: %v", th, err)
		}
	}
}

func TestDupesCommand_NotIndexed(t *testing.T) {
	t.Run("no database", func(t *testing.T) {
		f := newDupesFixture(t)
		_, _, err := execDupes(t, f.root)
		if err == nil || !strings.Contains(err.Error(), "not indexed") || !strings.Contains(err.Error(), "lumen index") {
			t.Errorf("err = %v, want a not-indexed error pointing at lumen index", err)
		}
	})
	t.Run("project absent from existing database", func(t *testing.T) {
		f := newDupesFixture(t)
		f.seed(t, pair("Big", 10, "b/big1.go", "b/big2.go", 1))
		other, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "-C", other, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
		// Point the other project at the seeded collection so the file exists
		// but holds no row for it.
		cfg, err := config.NewConfigService(config.DefaultConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		otherDB := configuredDBPath(cfg, other, embedder.DefaultModel)
		if err := os.MkdirAll(filepath.Dir(otherDB), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := copyFile(f.dbPath, otherDB); err != nil {
			t.Fatal(err)
		}
		_, _, err = execDupes(t, other)
		if err == nil || !strings.Contains(err.Error(), "not indexed") || !strings.Contains(err.Error(), "lumen index") {
			t.Errorf("err = %v, want a not-indexed error pointing at lumen index", err)
		}
	})
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func TestDupesCommand_UnreadableIndexNamesDatabase(t *testing.T) {
	f := newDupesFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.dbPath, []byte("this is not a sqlite database, but it is long enough to look like one"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := execDupes(t, f.root)
	if err == nil || !strings.Contains(err.Error(), f.dbPath) {
		t.Errorf("err = %v, want it to name %s", err, f.dbPath)
	}
}

func TestDupesCommand_NothingCompared(t *testing.T) {
	f := newDupesFixture(t)
	f.seed(t, pair("Big", 10, "b/big1.go", "b/big2.go", 1))

	stdout, _, err := execDupes(t, "--min-lines", "50", f.root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "No indexed code chunks matched") {
		t.Errorf("missing no-chunks message:\n%s", stdout)
	}
	for _, hint := range []string{"scope", "--min-lines", "--no-tests"} {
		if !strings.Contains(stdout, hint) {
			t.Errorf("message should mention %s:\n%s", hint, stdout)
		}
	}
	if strings.Contains(stdout, "No duplicates") {
		t.Errorf("must not claim there are no duplicates:\n%s", stdout)
	}
	if !strings.Contains(stdout, "0 chunks compared · threshold 0.93") {
		t.Errorf("footer fields missing:\n%s", stdout)
	}
}

func TestDupesCommand_NoClusters(t *testing.T) {
	f := newDupesFixture(t)
	f.seed(t, []dupesFixtureChunk{
		{"a/one.go", "One", "function", 1, 10, []float32{1}},
		{"a/two.go", "Two", "function", 1, 10, []float32{0, 1}},
	})

	stdout, _, err := execDupes(t, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout, "No duplicates found at threshold 0.93 (2 chunks compared)\n") {
		t.Errorf("missing no-duplicates line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "threshold 0.93 · model "+embedder.DefaultModel) {
		t.Errorf("footer fields missing:\n%s", stdout)
	}
	if strings.Contains(stdout, "Showing") {
		t.Errorf("no clusters to show:\n%s", stdout)
	}
}

func TestDupesCommand_MidIndexFilesAreReported(t *testing.T) {
	f := newDupesFixture(t)
	f.seed(t, joinChunks(
		pair("Big", 10, "b/big1.go", "b/big2.go", 1),
		pair("Half", 10, "c/half1.go", "c/half2.go", 0, 1),
	))
	f.markIncomplete(t, "c/half1.go", "c/half2.go")

	stdout, _, err := execDupes(t, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(stdout, "\n2 files mid-index were skipped\n") {
		t.Errorf("missing mid-index line:\n%s", stdout)
	}
	if strings.Contains(stdout, "half") {
		t.Errorf("mid-index files must not be compared:\n%s", stdout)
	}

	f2 := newDupesFixture(t)
	f2.seed(t, pair("Big", 10, "b/big1.go", "b/big2.go", 1))
	stdout, _, err = execDupes(t, f2.root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "mid-index") {
		t.Errorf("mid-index line printed with no incomplete files:\n%s", stdout)
	}
}

func TestFormatDupesSimilarity(t *testing.T) {
	tests := []struct {
		min, max float32
		want     string
	}{
		{1, 1, "1.00"},
		{0.96, 0.96, "0.96"},
		{0.951, 0.954, "0.95"},
		{0.93, 0.96, "0.93–0.96"},
		{0.9, 1.0000001, "0.90–1.00"},
	}
	for _, tt := range tests {
		if got := formatDupesSimilarity(tt.min, tt.max); got != tt.want {
			t.Errorf("formatDupesSimilarity(%v, %v) = %q, want %q", tt.min, tt.max, got, tt.want)
		}
	}
}
