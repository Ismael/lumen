//go:build launcher_e2e

// Real cross-OS launcher integration test.
//
// Builds the lumen binary natively, serves it from a localhost HTTP server at
// the same path layout the launcher expects on github.com, and drives
// scripts/run.cmd hook session-start through to a real exec of the freshly
// built binary. No mocks: real curl, real download, real binary.
//
// Activated only under -tags=launcher_e2e so a normal `go test ./...`
// doesn't pull in the heavy CGO build.

package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLauncherE2E_HookSessionStart(t *testing.T) {
	repoRoot := repoRoot(t)
	version := readManifestVersion(t, filepath.Join(repoRoot, ".release-please-manifest.json"))

	// Don't use t.TempDir here: Windows holds a file lock on a freshly-
	// exec'd .exe briefly after the process exits, and t.TempDir's strict
	// RemoveAll cleanup turns that transient lock into a test failure.
	pluginRoot := mustMkdirTempWithRetryCleanup(t, "lumen-plugin-root-")
	mustCopyFile(t,
		filepath.Join(repoRoot, ".release-please-manifest.json"),
		filepath.Join(pluginRoot, ".release-please-manifest.json"),
	)
	if err := os.MkdirAll(filepath.Join(pluginRoot, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir plugin bin: %v", err)
	}

	serveRoot := t.TempDir()
	assetName := launcherAssetName(version)
	assetDir := filepath.Join(serveRoot, "ory", "lumen", "releases", "download", "v"+version)
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatalf("mkdir served asset dir: %v", err)
	}
	assetPath := filepath.Join(assetDir, assetName)

	buildLumen(t, repoRoot, version, assetPath)

	var assetGets atomic.Int32
	files := http.FileServer(http.Dir(serveRoot))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, assetName) {
			assetGets.Add(1)
		}
		files.ServeHTTP(w, r)
	}))
	defer server.Close()

	emptyRepo := t.TempDir()
	stdinJSON, err := json.Marshal(map[string]string{"cwd": emptyRepo})
	if err != nil {
		t.Fatalf("marshal stdin: %v", err)
	}

	launcher := filepath.Join(repoRoot, "scripts", "run.cmd")
	hookArgs := []string{"hook", "session-start", "lumen", "--host", "claude"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// Match how MCP hosts launch run.cmd: a fresh cmd.exe with default
		// @echo on. The polyglot must not pollute stdout under these
		// conditions; enforced by the strict json.Unmarshal below.
		cmd = exec.CommandContext(ctx, "cmd.exe", append([]string{"/C", launcher}, hookArgs...)...)
	} else {
		// Invoke via /bin/sh to match how Claude Code (Node.js libuv) spawns
		// the hook on POSIX: libuv uses execvp, which falls back to /bin/sh
		// on ENOEXEC for files without a shebang. Go's exec.Cmd uses execve
		// directly (no fallback), so a bare exec.Command(launcher, ...) here
		// would fail with "exec format error" against a polyglot that doesn't
		// start with `#!` — even though production works fine.
		cmd = exec.CommandContext(ctx, "/bin/sh", append([]string{launcher}, hookArgs...)...)
	}
	cmd.Env = append(os.Environ(),
		"CLAUDE_PLUGIN_ROOT="+pluginRoot,
		"LUMEN_DOWNLOAD_BASE_URL="+server.URL,
	)
	cmd.Stdin = bytes.NewReader(stdinJSON)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("launcher exceeded 5s deadline\nstdout:\n%s\nstderr:\n%s",
			stdout.String(), stderr.String())
	}
	if runErr != nil {
		t.Fatalf("launcher exited non-zero: %v\nstdout:\n%s\nstderr:\n%s",
			runErr, stdout.String(), stderr.String())
	}

	expectedBinary := filepath.Join(pluginRoot, "bin", binaryFileName())
	info, err := os.Stat(expectedBinary)
	if err != nil {
		t.Fatalf("expected binary at %s: %v\nstderr:\n%s", expectedBinary, err, stderr.String())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("downloaded binary at %s has no executable bit: mode %v",
			expectedBinary, info.Mode())
	}

	if got := assetGets.Load(); got < 1 {
		t.Fatalf("local HTTP server saw %d GETs for %s, expected >=1\nstderr:\n%s",
			got, assetName, stderr.String())
	}

	// Strict parse: stdout must be exactly the JSON the hook prints. Any
	// shell or cmd.exe pollution from the run.cmd polyglot would break this
	// the same way it would break Claude Code's hook reader in production.
	var hook map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &hook); err != nil {
		t.Fatalf("stdout JSON parse: %v\nstdout: %q\nstderr: %s",
			err, stdout.String(), stderr.String())
	}
	spec, ok := hook["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("missing hookSpecificOutput in stdout: %v", hook)
	}
	if name, _ := spec["hookEventName"].(string); name != "SessionStart" {
		t.Fatalf("hookEventName=%q, expected SessionStart\nstdout: %s",
			name, stdout.String())
	}
}

// mustMkdirTempWithRetryCleanup creates a temp dir and registers a cleanup
// that retries removal a few times — Windows briefly holds an exclusive
// handle on a just-exec'd .exe after the process exits, which would
// otherwise turn cleanup into a spurious test failure.
func mustMkdirTempWithRetryCleanup(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() {
		var lastErr error
		for i := 0; i < 20; i++ {
			lastErr = os.RemoveAll(dir)
			if lastErr == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("cleanup of %s failed after retries: %v", dir, lastErr)
	})
	return dir
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

func readManifestVersion(t *testing.T, manifestPath string) string {
	t.Helper()
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest %q: %v", string(raw), err)
	}
	v := m["."]
	if v == "" {
		t.Fatalf("manifest has empty root version: %s", string(raw))
	}
	return v
}

func launcherAssetName(version string) string {
	name := fmt.Sprintf("lumen-%s-%s-%s", version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func binaryFileName() string {
	name := fmt.Sprintf("lumen-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func buildLumen(t *testing.T, repoRoot, version, outPath string) {
	t.Helper()
	ldflags := fmt.Sprintf("-X github.com/ory/lumen/cmd.buildVersion=%s", version)
	cmd := exec.Command("go", "build",
		"-tags=fts5",
		"-ldflags="+ldflags,
		"-o", outPath,
		".",
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out.String())
	}
}

func mustCopyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", dst, err)
	}
}
