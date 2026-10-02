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
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ory/lumen/internal/config"
	"github.com/ory/lumen/internal/indexlock"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func budgetContext(t *testing.T, budget time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	t.Cleanup(cancel)
	return ctx
}

// fakeIndexerProcess is a spawned indexer whose exit the test controls.
type fakeIndexerProcess struct {
	exit chan struct{}
	err  error
}

func newFakeIndexerProcess() *fakeIndexerProcess {
	return &fakeIndexerProcess{exit: make(chan struct{})}
}

func exitedIndexerProcess() *fakeIndexerProcess {
	p := newFakeIndexerProcess()
	close(p.exit)
	return p
}

func (p *fakeIndexerProcess) Wait() error {
	<-p.exit
	return p.err
}

func TestParseEnterWorktreePath(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name: "valid payload",
			input: `{"hook_event_name":"PostToolUse","tool_name":"EnterWorktree","cwd":"/cwd",` +
				`"tool_input":{"path":"/input"},` +
				`"tool_response":{"worktreePath":"/repo.wt","worktreeBranch":"test","message":"Entered worktree"}}`,
			want: "/repo.wt",
		},
		{
			name:  "missing tool_response ignores cwd and tool_input",
			input: `{"cwd":"/cwd","tool_input":{"path":"/input"}}`,
		},
		{
			name:  "empty worktreePath",
			input: `{"cwd":"/cwd","tool_response":{"worktreePath":""}}`,
		},
		{
			name:    "invalid JSON",
			input:   `{not json`,
			wantErr: true,
		},
		{
			name:    "empty input",
			input:   ``,
			wantErr: true,
		},
		{
			name:  "trailing newline",
			input: "{\"tool_response\":{\"worktreePath\":\"/repo.wt\"}}\n",
			want:  "/repo.wt",
		},
		{
			name:    "trailing garbage",
			input:   `{"tool_response":{"worktreePath":"/repo.wt"}} garbage`,
			wantErr: true,
		},
		{
			name:    "second JSON value",
			input:   `{"tool_response":{"worktreePath":"/repo.wt"}}{"tool_response":{"worktreePath":"/other"}}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseEnterWorktreePath(strings.NewReader(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnterWorktree_SpawnsOnlyForValidPayload(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantSpawn []string
	}{
		{
			name:      "valid payload spawns for resolved root",
			input:     `{"cwd":"/cwd","tool_response":{"worktreePath":"/repo.wt/sub"}}`,
			wantSpawn: []string{"/repo.wt"},
		},
		{name: "missing tool_response", input: `{"cwd":"/cwd","tool_input":{"path":"/input"}}`},
		{name: "empty worktreePath", input: `{"tool_response":{"worktreePath":""}}`},
		{name: "invalid JSON", input: `nope`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var spawned []string
			deps := enterWorktreeDeps{
				resolve: func(path string) (string, string, error) {
					return filepath.Dir(path), "/data/index.db", nil
				},
				spawn: func(root string) (indexerProcess, error) {
					spawned = append(spawned, root)
					return exitedIndexerProcess(), nil
				},
				locksBusy:    func(string, string) (bool, error) { return false, nil },
				budget:       time.Second,
				pollInterval: time.Millisecond,
			}
			enterWorktree(strings.NewReader(tt.input), deps, discardLogger())
			if strings.Join(spawned, ",") != strings.Join(tt.wantSpawn, ",") {
				t.Errorf("spawned = %v, want %v", spawned, tt.wantSpawn)
			}
		})
	}
}

func TestEnterWorktree_ResolveErrorDoesNotSpawn(t *testing.T) {
	spawned := false
	deps := enterWorktreeDeps{
		resolve: func(string) (string, string, error) { return "", "", errors.New("config broken") },
		spawn: func(string) (indexerProcess, error) {
			spawned = true
			return exitedIndexerProcess(), nil
		},
		locksBusy:    func(string, string) (bool, error) { return false, nil },
		budget:       time.Second,
		pollInterval: time.Millisecond,
	}
	enterWorktree(strings.NewReader(`{"tool_response":{"worktreePath":"/repo.wt"}}`), deps, discardLogger())
	if spawned {
		t.Error("spawn must not run when the target cannot be resolved")
	}
}

func TestEnterWorktree_BudgetCoversResolve(t *testing.T) {
	const budget = 200 * time.Millisecond
	const resolveTime = 5 * time.Second
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	spawned := false
	deps := enterWorktreeDeps{
		resolve: func(string) (string, string, error) {
			select {
			case <-unblock:
			case <-time.After(resolveTime):
			}
			return "/repo.wt", "/data/index.db", nil
		},
		spawn: func(string) (indexerProcess, error) {
			spawned = true
			return exitedIndexerProcess(), nil
		},
		locksBusy:    func(string, string) (bool, error) { return false, nil },
		budget:       budget,
		pollInterval: time.Millisecond,
	}

	start := time.Now()
	enterWorktree(strings.NewReader(`{"tool_response":{"worktreePath":"/repo.wt"}}`), deps, discardLogger())
	elapsed := time.Since(start)

	if elapsed < budget || elapsed >= resolveTime {
		t.Errorf("returned after %v, want about %v", elapsed, budget)
	}
	if spawned {
		t.Error("spawn must not run once the budget is spent resolving the target")
	}
}

func TestEnterWorktree_BudgetSharedWithWaitPhase(t *testing.T) {
	const budget = time.Second
	const resolveTime = 600 * time.Millisecond
	var probes atomic.Int64
	deps := enterWorktreeDeps{
		resolve: func(string) (string, string, error) {
			time.Sleep(resolveTime)
			return "/repo.wt", "/data/index.db", nil
		},
		spawn: func(string) (indexerProcess, error) { return exitedIndexerProcess(), nil },
		locksBusy: func(string, string) (bool, error) {
			probes.Add(1)
			return true, nil
		},
		budget:       budget,
		pollInterval: 5 * time.Millisecond,
	}

	start := time.Now()
	enterWorktree(strings.NewReader(`{"tool_response":{"worktreePath":"/repo.wt"}}`), deps, discardLogger())
	elapsed := time.Since(start)

	if probes.Load() == 0 {
		t.Fatal("budget ran out while resolving; the wait phase was never reached")
	}
	if elapsed < budget || elapsed >= budget+resolveTime {
		t.Errorf("returned after %v, want one shared budget of %v", elapsed, budget)
	}
}

func TestSilentDebugLogger(t *testing.T) {
	tests := []struct {
		name     string
		dataHome func(t *testing.T) string
		wantFile bool
	}{
		{
			name:     "log file available",
			dataHome: func(t *testing.T) string { return t.TempDir() },
			wantFile: true,
		},
		{
			name: "log directory cannot be created",
			dataHome: func(t *testing.T) string {
				blocker := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(blocker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return blocker
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataHome := tt.dataHome(t)
			t.Setenv("XDG_DATA_HOME", dataHome)

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			origStderr := os.Stderr
			os.Stderr = w
			logger, closer := silentDebugLogger()
			logger.Warn("probe message")
			if closer != nil {
				_ = closer.Close()
			}
			os.Stderr = origStderr
			_ = w.Close()
			stderr, _ := io.ReadAll(r)
			_ = r.Close()

			if len(stderr) != 0 {
				t.Errorf("logger wrote to stderr: %q", stderr)
			}
			logged, _ := os.ReadFile(filepath.Join(dataHome, "lumen", "debug.log"))
			if got := strings.Contains(string(logged), "probe message"); got != tt.wantFile {
				t.Errorf("message in debug.log = %v, want %v", got, tt.wantFile)
			}
		})
	}
}

func TestEnterWorktreeBudgetFitsHookTimeout(t *testing.T) {
	// Margin for process start-up, stdin, and shutdown outside the budget.
	const margin = 10 * time.Second
	timeout := time.Duration(enterWorktreeHookEntry(t).Timeout) * time.Second
	if enterWorktreeWaitBudget+margin >= timeout {
		t.Errorf("budget %v + margin %v must be below the hooks.json timeout %v", enterWorktreeWaitBudget, margin, timeout)
	}
}

func TestWaitForWorktreeIndex(t *testing.T) {
	const budget = 300 * time.Millisecond
	tests := []struct {
		name string
		// busyCalls is how many probes report busy before the locks are free;
		// -1 keeps them busy forever.
		busyCalls  int
		neverExits bool
		spawnErr   error
		probeErr   error
		wantProbes func(n int64) bool
		wantAtLeast,
		wantUnder time.Duration
	}{
		{
			name:       "child exits and locks free returns promptly",
			wantProbes: func(n int64) bool { return n == 1 },
			wantUnder:  budget / 2,
		},
		{
			name:        "locks held for a while then released",
			busyCalls:   5,
			wantProbes:  func(n int64) bool { return n == 6 },
			wantAtLeast: 5 * 10 * time.Millisecond,
			wantUnder:   budget,
		},
		{
			name:        "locks stay held returns at budget",
			busyCalls:   -1,
			wantProbes:  func(n int64) bool { return n > 1 },
			wantAtLeast: budget,
			wantUnder:   budget + 200*time.Millisecond,
		},
		{
			name:        "child never exits returns at budget without probing",
			neverExits:  true,
			wantProbes:  func(n int64) bool { return n == 0 },
			wantAtLeast: budget,
			wantUnder:   budget + 200*time.Millisecond,
		},
		{
			name:       "spawn error returns immediately",
			spawnErr:   errors.New("exec failed"),
			wantProbes: func(n int64) bool { return n == 0 },
			wantUnder:  budget / 2,
		},
		{
			name:       "probe error returns immediately",
			busyCalls:  -1,
			probeErr:   errors.New("permission denied"),
			wantProbes: func(n int64) bool { return n == 1 },
			wantUnder:  budget / 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := exitedIndexerProcess()
			if tt.neverExits {
				proc = newFakeIndexerProcess()
				t.Cleanup(func() { close(proc.exit) })
			}
			var probes atomic.Int64
			deps := enterWorktreeDeps{
				spawn: func(root string) (indexerProcess, error) {
					if root != "/repo.wt" {
						t.Errorf("spawn root = %q, want /repo.wt", root)
					}
					if tt.spawnErr != nil {
						return nil, tt.spawnErr
					}
					return proc, nil
				},
				locksBusy: func(dbPath, root string) (bool, error) {
					if dbPath != "/data/index.db" || root != "/repo.wt" {
						t.Errorf("probe args = (%q, %q)", dbPath, root)
					}
					n := probes.Add(1)
					if tt.probeErr != nil {
						return false, tt.probeErr
					}
					return tt.busyCalls < 0 || n <= int64(tt.busyCalls), nil
				},
				budget:       budget,
				pollInterval: 10 * time.Millisecond,
			}

			start := time.Now()
			waitForWorktreeIndex(budgetContext(t, deps.budget), "/repo.wt", "/data/index.db", deps, discardLogger())
			elapsed := time.Since(start)

			if n := probes.Load(); !tt.wantProbes(n) {
				t.Errorf("unexpected probe count %d", n)
			}
			if elapsed < tt.wantAtLeast {
				t.Errorf("returned after %v, want at least %v", elapsed, tt.wantAtLeast)
			}
			if elapsed >= tt.wantUnder {
				t.Errorf("returned after %v, want under %v", elapsed, tt.wantUnder)
			}
		})
	}
}

func TestWaitForWorktreeIndex_ProbesOnlyAfterChildExits(t *testing.T) {
	proc := newFakeIndexerProcess()
	var mu sync.Mutex
	exited := false
	probedBeforeExit := false
	deps := enterWorktreeDeps{
		spawn: func(string) (indexerProcess, error) { return proc, nil },
		locksBusy: func(string, string) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			if !exited {
				probedBeforeExit = true
			}
			return false, nil
		},
		budget:       2 * time.Second,
		pollInterval: time.Millisecond,
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		exited = true
		mu.Unlock()
		close(proc.exit)
	}()

	waitForWorktreeIndex(budgetContext(t, deps.budget), "/repo.wt", "/data/index.db", deps, discardLogger())

	mu.Lock()
	defer mu.Unlock()
	if !exited {
		t.Error("returned before the spawned indexer exited")
	}
	if probedBeforeExit {
		t.Error("locks were probed before the spawned indexer exited")
	}
}

func TestWorktreeIndexLocksBusy_RealLocks(t *testing.T) {
	const root = "/repo.wt"
	tests := []struct {
		name string
		hold func(t *testing.T, dbPath string) *indexlock.Lock
		want bool
	}{
		{
			name: "no lock files",
			hold: func(*testing.T, string) *indexlock.Lock { return nil },
		},
		{
			name: "project lock held",
			hold: func(t *testing.T, dbPath string) *indexlock.Lock {
				return mustAcquire(t, indexlock.TryAcquire, indexlock.LockPathForProject(dbPath, root))
			},
			want: true,
		},
		{
			name: "exclusive collection lock held",
			hold: func(t *testing.T, dbPath string) *indexlock.Lock {
				return mustAcquire(t, indexlock.TryAcquire, indexlock.LockPathForDB(dbPath))
			},
			want: true,
		},
		{
			name: "shared collection lock held by another worktree",
			hold: func(t *testing.T, dbPath string) *indexlock.Lock {
				return mustAcquire(t, indexlock.TryAcquireShared, indexlock.LockPathForDB(dbPath))
			},
		},
		{
			name: "another worktree's project lock held",
			hold: func(t *testing.T, dbPath string) *indexlock.Lock {
				return mustAcquire(t, indexlock.TryAcquire, indexlock.LockPathForProject(dbPath, "/repo.other"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "index.db")
			lock := tt.hold(t, dbPath)
			defer lock.Release()

			busy, err := worktreeIndexLocksBusy(dbPath, root)
			if err != nil {
				t.Fatalf("worktreeIndexLocksBusy: %v", err)
			}
			if busy != tt.want {
				t.Errorf("busy = %v, want %v", busy, tt.want)
			}
		})
	}
}

func TestWorktreeIndexLocksBusy_DoesNotCreateLockFiles(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.db")
	if _, err := worktreeIndexLocksBusy(dbPath, "/repo.wt"); err != nil {
		t.Fatalf("worktreeIndexLocksBusy: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("probe created files: %v", entries)
	}
}

func TestWaitForWorktreeIndex_RealExclusiveCollectionLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	cleanup := mustAcquire(t, indexlock.TryAcquire, indexlock.LockPathForDB(dbPath))
	const holdFor = 100 * time.Millisecond
	time.AfterFunc(holdFor, cleanup.Release)

	deps := enterWorktreeDeps{
		spawn:        func(string) (indexerProcess, error) { return exitedIndexerProcess(), nil },
		locksBusy:    worktreeIndexLocksBusy,
		budget:       2 * time.Second,
		pollInterval: 5 * time.Millisecond,
	}
	start := time.Now()
	waitForWorktreeIndex(budgetContext(t, deps.budget), "/repo.wt", dbPath, deps, discardLogger())
	elapsed := time.Since(start)

	if elapsed < holdFor {
		t.Errorf("returned after %v while cleanup held the collection lock for %v", elapsed, holdFor)
	}
	if elapsed >= deps.budget {
		t.Errorf("returned at budget (%v) instead of after the lock was released", elapsed)
	}
}

func mustAcquire(t *testing.T, acquire func(string) (*indexlock.Lock, error), path string) *indexlock.Lock {
	t.Helper()
	lock, err := acquire(path)
	if err != nil {
		t.Fatalf("acquire %s: %v", path, err)
	}
	if lock == nil {
		t.Fatalf("acquire %s: already held", path)
	}
	return lock
}

func TestResolveEnterWorktreeTarget_MatchesIndexCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mainRepo := filepath.Join(parent, "project")
	linked := filepath.Join(parent, "project.wt1")
	if err := os.MkdirAll(filepath.Join(mainRepo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainRepo, "sub", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, mainRepo, "init", "-q")
	runGit(t, mainRepo, "add", ".")
	runGit(t, mainRepo, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "-q", "-m", "init")
	runGit(t, mainRepo, "worktree", "add", "-q", "-b", "wt1", linked)

	cfg, err := config.NewConfigService(config.DefaultConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	model := newEmbedder(cfg).ModelName()
	mainDBPath := configuredDBPath(cfg, mainRepo, model)

	tests := []struct {
		name     string
		path     string
		wantRoot string
	}{
		{name: "main checkout subdirectory", path: filepath.Join(mainRepo, "sub"), wantRoot: mainRepo},
		{name: "linked sibling worktree", path: linked, wantRoot: linked},
		{name: "linked worktree subdirectory", path: filepath.Join(linked, "sub"), wantRoot: linked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, dbPath, err := resolveEnterWorktreeTarget(tt.path)
			if err != nil {
				t.Fatalf("resolveEnterWorktreeTarget: %v", err)
			}
			if root != tt.wantRoot {
				t.Errorf("root = %q, want %q", root, tt.wantRoot)
			}
			if dbPath != mainDBPath {
				t.Errorf("dbPath = %q, want the shared collection %q", dbPath, mainDBPath)
			}
		})
	}
}

func TestStartBackgroundIndexer_ReturnsWaitableHandle(t *testing.T) {
	// TestMain makes the re-executed test binary exit immediately on "index".
	proc, err := startBackgroundIndexer(t.TempDir())
	if err != nil {
		t.Fatalf("startBackgroundIndexer: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Wait: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("spawned indexer did not exit")
	}
}

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

func readHooksManifest(t *testing.T) map[string][]struct {
	Matcher string        `json:"matcher"`
	Hooks   []hookCommand `json:"hooks"`
} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Hooks map[string][]struct {
			Matcher string        `json:"matcher"`
			Hooks   []hookCommand `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("hooks.json is not valid JSON: %v", err)
	}
	return manifest.Hooks
}

func enterWorktreeHookEntry(t *testing.T) hookCommand {
	t.Helper()
	entries := readHooksManifest(t)["PostToolUse"]
	if len(entries) != 1 || entries[0].Matcher != "EnterWorktree" || len(entries[0].Hooks) != 1 {
		t.Fatalf("PostToolUse entries = %+v, want one EnterWorktree entry with one hook", entries)
	}
	return entries[0].Hooks[0]
}

func TestHooksJSON_EnterWorktreeEntry(t *testing.T) {
	if len(readHooksManifest(t)["SessionStart"]) == 0 {
		t.Error("SessionStart entry missing")
	}
	h := enterWorktreeHookEntry(t)
	const wantCmd = `"${CLAUDE_PLUGIN_ROOT}/scripts/run" hook enter-worktree`
	if h.Type != "command" || h.Command != wantCmd || h.Timeout != 45 {
		t.Errorf("hook = %+v, want command %q with timeout 45", h, wantCmd)
	}
}
