#!/usr/bin/env bash
# Tests for run URL construction and OS/arch detection logic.
# Runs entirely offline — no real HTTP calls are made.
set -euo pipefail

PASS=0
FAIL=0

ok() {
  echo "  PASS: $1"
  PASS=$((PASS + 1))
}

fail() {
  echo "  FAIL: $1"
  echo "        expected: $2"
  echo "        got:      $3"
  FAIL=$((FAIL + 1))
}

assert_eq() {
  local desc="$1" expected="$2" got="$3"
  if [ "$expected" = "$got" ]; then
    ok "$desc"
  else
    fail "$desc" "$expected" "$got"
  fi
}

# ---------------------------------------------------------------------------
# asset_name <version_tag> <os> <arch>
# Mirrors the logic in run: strip leading 'v', build asset filename.
# ---------------------------------------------------------------------------
asset_name() {
  local version="$1" os="$2" arch="$3"
  local ver_no_v="${version#v}"
  case "$os" in
    windows) echo "lumen-${ver_no_v}-${os}-${arch}.exe" ;;
    *)       echo "lumen-${ver_no_v}-${os}-${arch}" ;;
  esac
}

# download_url <repo> <version_tag> <os> <arch>
download_url() {
  local repo="$1" version="$2" os="$3" arch="$4"
  local asset
  asset="$(asset_name "$version" "$os" "$arch")"
  echo "https://github.com/${repo}/releases/download/${version}/${asset}"
}

# ---------------------------------------------------------------------------
# arch normalisation (mirrors run case statement)
# ---------------------------------------------------------------------------
normalise_arch() {
  case "$1" in
    x86_64)  echo "amd64" ;;
    aarch64) echo "arm64" ;;
    *)       echo "$1" ;;
  esac
}

echo "=== asset name tests ==="
assert_eq "macOS arm64 asset" \
  "lumen-0.0.1-alpha.4-darwin-arm64" \
  "$(asset_name "v0.0.1-alpha.4" "darwin" "arm64")"

assert_eq "macOS amd64 asset" \
  "lumen-0.0.1-alpha.4-darwin-amd64" \
  "$(asset_name "v0.0.1-alpha.4" "darwin" "amd64")"

assert_eq "Linux amd64 asset" \
  "lumen-0.0.1-alpha.4-linux-amd64" \
  "$(asset_name "v0.0.1-alpha.4" "linux" "amd64")"

assert_eq "Linux arm64 asset" \
  "lumen-0.0.1-alpha.4-linux-arm64" \
  "$(asset_name "v0.0.1-alpha.4" "linux" "arm64")"

assert_eq "Windows amd64 asset (.exe)" \
  "lumen-0.0.1-alpha.4-windows-amd64.exe" \
  "$(asset_name "v0.0.1-alpha.4" "windows" "amd64")"

echo ""
echo "=== download URL tests ==="
REPO="ory/lumen"
VERSION="v0.0.1-alpha.4"

assert_eq "macOS arm64 URL" \
  "https://github.com/ory/lumen/releases/download/v0.0.1-alpha.4/lumen-0.0.1-alpha.4-darwin-arm64" \
  "$(download_url "$REPO" "$VERSION" "darwin" "arm64")"

assert_eq "Linux amd64 URL" \
  "https://github.com/ory/lumen/releases/download/v0.0.1-alpha.4/lumen-0.0.1-alpha.4-linux-amd64" \
  "$(download_url "$REPO" "$VERSION" "linux" "amd64")"

assert_eq "Windows amd64 URL" \
  "https://github.com/ory/lumen/releases/download/v0.0.1-alpha.4/lumen-0.0.1-alpha.4-windows-amd64.exe" \
  "$(download_url "$REPO" "$VERSION" "windows" "amd64")"

echo ""
echo "=== arch normalisation tests ==="
assert_eq "x86_64 → amd64"  "amd64" "$(normalise_arch "x86_64")"
assert_eq "aarch64 → arm64" "arm64" "$(normalise_arch "aarch64")"
assert_eq "arm64 passthrough" "arm64" "$(normalise_arch "arm64")"
assert_eq "amd64 passthrough" "amd64" "$(normalise_arch "amd64")"

echo ""
echo "=== binary candidate priority tests ==="
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

BIN_DIR="${TMP_DIR}/bin"
mkdir -p "$BIN_DIR"

# Simulate: only downloaded binary present → should pick lumen-os-arch
touch "${BIN_DIR}/lumen-linux-amd64"
chmod +x "${BIN_DIR}/lumen-linux-amd64"

FOUND=""
for candidate in "${BIN_DIR}/lumen" "${BIN_DIR}/lumen-linux-amd64"; do
  if [ -x "$candidate" ]; then FOUND="$candidate"; break; fi
done
assert_eq "picks lumen-linux-amd64 when lumen absent" \
  "${BIN_DIR}/lumen-linux-amd64" "$FOUND"

# Simulate: both present → local dev build wins
touch "${BIN_DIR}/lumen"
chmod +x "${BIN_DIR}/lumen"

FOUND=""
for candidate in "${BIN_DIR}/lumen" "${BIN_DIR}/lumen-linux-amd64"; do
  if [ -x "$candidate" ]; then FOUND="$candidate"; break; fi
done
assert_eq "prefers bin/lumen (dev build) over downloaded binary" \
  "${BIN_DIR}/lumen" "$FOUND"

echo ""
echo "=== version resolution tests ==="
TMP_MANIFEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_MANIFEST_DIR" "$TMP_DIR"' EXIT

MANIFEST="${TMP_MANIFEST_DIR}/.release-please-manifest.json"
printf '{\n  ".": "1.2.3"\n}\n' > "$MANIFEST"

resolved_version_from_manifest() {
  local manifest="$1"
  local ver="v$(grep '"[.]"' "$manifest" | sed 's/.*"[^"]*"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')"
  echo "$ver"
}

resolved_version_from_plugin_manifest() {
  local manifest="$1"
  local ver
  ver="v$(grep '"version"' "$manifest" | sed 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')"
  echo "$ver"
}

assert_eq "manifest version resolution" \
  "v1.2.3" \
  "$(resolved_version_from_manifest "$MANIFEST")"

assert_eq "pre-release version preserved" \
  "v0.0.1-alpha.4" \
  "$(printf '{\n  ".": "0.0.1-alpha.4"\n}\n' | grep '"[.]"' | sed 's/.*"[^"]*"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/v\1/')"

PLUGIN_MANIFEST="${TMP_MANIFEST_DIR}/plugin.json"
printf '{\n  "name": "lumen",\n  "version": "4.5.6"\n}\n' > "$PLUGIN_MANIFEST"
assert_eq "native plugin manifest version resolution" \
  "v4.5.6" \
  "$(resolved_version_from_plugin_manifest "$PLUGIN_MANIFEST")"

echo ""
echo "=== stdio download-on-first-run integration test ==="

# Verifies that run stdio downloads the binary and execs it when no binary
# is present (first install). curl is stubbed in PATH — no real network calls.
(
  _SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
  _PLUGIN_ROOT="$(mktemp -d)"
  _PLUGIN_DATA="$(mktemp -d)"
  _FAKE_CURL_DIR="$(mktemp -d)"
  trap 'rm -rf "$_PLUGIN_ROOT" "$_PLUGIN_DATA" "$_FAKE_CURL_DIR"' EXIT

  OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
  ARCH_RAW="$(uname -m)"
  case "$ARCH_RAW" in x86_64) ARCH="amd64" ;; aarch64) ARCH="arm64" ;; *) ARCH="$ARCH_RAW" ;; esac
  _EXPECTED_BINARY="${_PLUGIN_DATA}/bin/lumen-${OS}-${ARCH}"

  mkdir -p "${_PLUGIN_ROOT}/.codex-plugin"
  printf '{\n  "name": "lumen",\n  "version": "0.0.1"\n}\n' > "${_PLUGIN_ROOT}/.codex-plugin/plugin.json"

  # Stub curl: write a minimal executable to the -o destination and succeed
  cat > "${_FAKE_CURL_DIR}/curl" <<'FAKECURL'
#!/usr/bin/env bash
while [ $# -gt 0 ]; do
  case "$1" in
    -o) mkdir -p "$(dirname "$2")"; printf '#!/usr/bin/env bash\nexit 0\n' > "$2"; chmod +x "$2"; shift 2 ;;
    *)  shift ;;
  esac
done
exit 0
FAKECURL
  chmod +x "${_FAKE_CURL_DIR}/curl"

  EXIT_CODE=0
  PLUGIN_ROOT="${_PLUGIN_ROOT}" PLUGIN_DATA="${_PLUGIN_DATA}" \
    PATH="${_FAKE_CURL_DIR}:${PATH}" \
    bash "${_SCRIPT_DIR}/run" stdio >/dev/null 2>&1 || EXIT_CODE=$?

  if [ "$EXIT_CODE" -ne 0 ]; then
    echo "  FAIL: run stdio with missing binary should download and exec (exit $EXIT_CODE)"
    exit 1
  fi
  if [ ! -x "$_EXPECTED_BINARY" ]; then
    echo "  FAIL: run stdio should have downloaded binary to ${_EXPECTED_BINARY}"
    exit 1
  fi
  echo "  PASS: Codex plugin manifest resolves and download uses PLUGIN_DATA"
) && PASS=$((PASS + 1)) || FAIL=$((FAIL + 1))

echo ""
echo "=== GitHub API tag parsing tests ==="

# Simulates the sed command used in run to extract tag_name from JSON
parse_tag_from_json() {
  echo "$1" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}

assert_eq "parse tag from typical API response" \
  "v1.2.3" \
  "$(parse_tag_from_json '  "tag_name": "v1.2.3",')"

assert_eq "parse tag with pre-release suffix" \
  "v0.0.1-alpha.4" \
  "$(parse_tag_from_json '  "tag_name": "v0.0.1-alpha.4",')"

assert_eq "parse tag with surrounding fields" \
  "v2.0.0" \
  "$(parse_tag_from_json '{"url":"https://...","tag_name": "v2.0.0","name":"v2.0.0"}')"

assert_eq "empty on missing tag_name" \
  "" \
  "$(parse_tag_from_json '{"error": "Not Found"}')"

assert_eq "empty on HTML error page" \
  "" \
  "$(parse_tag_from_json '<html><body>404</body></html>')"

echo ""
echo "=== fallback URL construction tests ==="

# Given a resolved latest tag, verify the fallback URL is correct
fallback_url() {
  local repo="$1" tag="$2" os="$3" arch="$4"
  local ver_no_v="${tag#v}"
  local asset
  case "$os" in
    windows) asset="lumen-${ver_no_v}-${os}-${arch}.exe" ;;
    *)       asset="lumen-${ver_no_v}-${os}-${arch}" ;;
  esac
  echo "https://github.com/${repo}/releases/download/${tag}/${asset}"
}

assert_eq "fallback URL for darwin arm64" \
  "https://github.com/ory/lumen/releases/download/v0.0.31/lumen-0.0.31-darwin-arm64" \
  "$(fallback_url "ory/lumen" "v0.0.31" "darwin" "arm64")"

assert_eq "fallback URL for linux amd64" \
  "https://github.com/ory/lumen/releases/download/v0.0.31/lumen-0.0.31-linux-amd64" \
  "$(fallback_url "ory/lumen" "v0.0.31" "linux" "amd64")"

assert_eq "fallback URL for windows amd64" \
  "https://github.com/ory/lumen/releases/download/v0.0.31/lumen-0.0.31-windows-amd64.exe" \
  "$(fallback_url "ory/lumen" "v0.0.31" "windows" "amd64")"

echo ""
echo "=== tag validation tests ==="

# Mirrors the validation in run: tag must be non-empty and match ^v[0-9]
validate_tag() {
  local tag="$1"
  if [ -z "$tag" ] || ! echo "$tag" | grep -qE '^v[0-9]'; then
    echo "invalid"
  else
    echo "valid"
  fi
}

assert_eq "valid semver tag"    "valid"   "$(validate_tag "v1.2.3")"
assert_eq "valid pre-release"   "valid"   "$(validate_tag "v0.0.1-alpha.4")"
assert_eq "empty tag"           "invalid" "$(validate_tag "")"
assert_eq "garbage tag"         "invalid" "$(validate_tag "not-a-version")"
assert_eq "html fragment"       "invalid" "$(validate_tag "<html>")"

echo ""
echo "=== stdio first-install MCP handshake test ==="

# End-to-end guard against the bug in #125: when the binary is missing and
# run is invoked as `run stdio` (how Claude Code starts the MCP server
# on first install), the launcher must download the binary AND the resulting
# process must speak MCP over stdio. If the stdio path fast-exits before
# reaching the download, or the downloaded artefact is never actually exec'd
# with stdin/stdout intact, the MCP server is dead for the entire session —
# Claude Code does not retry failed MCP servers.
#
# A cross-compiled mock MCP server stands in for the real binary. A stubbed
# `curl` (shadowing real curl via PATH) copies the mock into place. A real
# JSON-RPC initialize request is sent on stdin; the stdout response must be
# a well-formed MCP initialize result from the mock. Passing transitively
# proves the launcher did not fast-exit, reached the download code path,
# wrote the artefact where it would exec it, made it executable, and
# exec'd it with stdin/stdout inherited correctly.
(
  _SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
  _REPO_ROOT="$(cd "${_SCRIPT_DIR}/.." && pwd)"
  _TMPROOT="$(mktemp -d)"
  _FAKE_CURL_DIR="$(mktemp -d)"
  _MOCK_BIN_DIR="$(mktemp -d)"
  trap 'rm -rf "$_TMPROOT" "$_FAKE_CURL_DIR" "$_MOCK_BIN_DIR"' EXIT

  _OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
  _ARCH_RAW="$(uname -m)"
  case "$_ARCH_RAW" in
    x86_64)  _ARCH="amd64" ;;
    aarch64) _ARCH="arm64" ;;
    *)       _ARCH="$_ARCH_RAW" ;;
  esac
  _EXPECTED_BINARY="${_TMPROOT}/bin/lumen-${_OS}-${_ARCH}"

  _MOCK_BIN="${_MOCK_BIN_DIR}/mock_lumen"
  if ! (cd "${_REPO_ROOT}" && CGO_ENABLED=0 go build -o "${_MOCK_BIN}" ./scripts/testdata/mock_mcp_server) >"${_TMPROOT}/mock_build.log" 2>&1; then
    echo "  FAIL: could not build mock MCP server"
    sed 's/^/          /' "${_TMPROOT}/mock_build.log"
    exit 1
  fi

  printf '{\n  ".": "0.0.1"\n}\n' > "${_TMPROOT}/.release-please-manifest.json"
  mkdir -p "${_TMPROOT}/bin"

  # Stub curl: parse -o <target> and copy the prebuilt mock into place.
  cat > "${_FAKE_CURL_DIR}/curl" <<'FAKECURL'
#!/usr/bin/env bash
while [ $# -gt 0 ]; do
  case "$1" in
    -o) mkdir -p "$(dirname "$2")"; cp "$LUMEN_MOCK_BINARY" "$2"; chmod +x "$2"; shift 2 ;;
    *)  shift ;;
  esac
done
exit 0
FAKECURL
  chmod +x "${_FAKE_CURL_DIR}/curl"

  _INIT_REQ='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"launcher-e2e","version":"1.0"}}}'

  _STDOUT="${_TMPROOT}/stdout.txt"
  _STDERR="${_TMPROOT}/stderr.txt"
  EXIT_CODE=0
  printf '%s\n' "$_INIT_REQ" | \
    CLAUDE_PLUGIN_ROOT="${_TMPROOT}" \
    PATH="${_FAKE_CURL_DIR}:${PATH}" \
    LUMEN_MOCK_BINARY="${_MOCK_BIN}" \
    bash "${_SCRIPT_DIR}/run" stdio >"${_STDOUT}" 2>"${_STDERR}" \
    || EXIT_CODE=$?

  if [ "$EXIT_CODE" -ne 0 ]; then
    echo "  FAIL: run stdio exited $EXIT_CODE — MCP server would be dead for the session"
    echo "        stderr:"
    sed 's/^/          /' "${_STDERR}"
    exit 1
  fi
  if [ ! -x "$_EXPECTED_BINARY" ]; then
    echo "  FAIL: run stdio did not place artefact at ${_EXPECTED_BINARY}"
    exit 1
  fi
  if ! grep -q '"jsonrpc":"2.0"' "${_STDOUT}"; then
    echo "  FAIL: MCP initialize produced no JSON-RPC 2.0 response on stdout"
    echo "        stdout:"
    sed 's/^/          /' "${_STDOUT}"
    exit 1
  fi
  if ! grep -q '"name":"mock-lumen"' "${_STDOUT}"; then
    echo "  FAIL: MCP response did not come from the exec'd mock — launcher may be swallowing stdout"
    echo "        stdout:"
    sed 's/^/          /' "${_STDOUT}"
    exit 1
  fi
  echo "  PASS: run stdio downloads, execs, and brokers MCP initialize on first install"
) && PASS=$((PASS + 1)) || FAIL=$((FAIL + 1))

echo ""
echo "=== build-from-source fallback tests ==="

# Each case runs the launcher against a throwaway package root with stubbed
# `go` and `curl` on a minimal PATH; no real build or network access happens.
# Version in the root's manifest is 0.0.1; the source-build stamp is
# bin/lumen.source-version.
_BF_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
_BF_ARCH_RAW="$(uname -m)"
case "$_BF_ARCH_RAW" in x86_64) _BF_ARCH="amd64" ;; aarch64) _BF_ARCH="arm64" ;; *) _BF_ARCH="$_BF_ARCH_RAW" ;; esac

# bf_stage <root> <stubs> <with_go_mod> <go_mode:ok|fail|slow|none>
bf_stage() {
  local root="$1" stubs="$2" with_mod="$3" go_mode="$4"
  printf '{\n  ".": "0.0.1"\n}\n' > "${root}/.release-please-manifest.json"
  if [ "$with_mod" = "1" ]; then printf 'module example.com/lumen\n' > "${root}/go.mod"; fi

  case "$go_mode" in
    ok|slow)
      cat > "${stubs}/go" <<'FAKEGO'
#!/usr/bin/env bash
{ echo "pwd=$(pwd)"; echo "cgo=${CGO_ENABLED:-}"; echo "args=$*"; echo "started"; } >> "$STUB_LOG"
out=""
while [ $# -gt 0 ]; do
  case "$1" in -o) out="$2"; shift 2 ;; *) shift ;; esac
done
if [ "${STUB_GO_SLEEP:-0}" != "0" ]; then sleep "$STUB_GO_SLEEP"; fi
echo "build output on stdout"
printf '#!/usr/bin/env bash\necho built-binary-ran\n' > "$out"
chmod +x "$out"
FAKEGO
      ;;
    fail)
      cat > "${stubs}/go" <<'FAKEGO'
#!/usr/bin/env bash
echo "go $*" >> "$STUB_LOG"
out=""
while [ $# -gt 0 ]; do
  case "$1" in -o) out="$2"; shift 2 ;; *) shift ;; esac
done
printf 'partial' > "$out"
echo "compile error"
exit 1
FAKEGO
      ;;
  esac
  if [ -f "${stubs}/go" ]; then chmod +x "${stubs}/go"; fi

  cat > "${stubs}/curl" <<'FAKECURL'
#!/usr/bin/env bash
echo "curl $*" >> "$STUB_LOG"
while [ $# -gt 0 ]; do
  case "$1" in
    -o) mkdir -p "$(dirname "$2")"; printf '#!/usr/bin/env bash\necho downloaded-binary-ran\n' > "$2"; chmod +x "$2"; shift 2 ;;
    *)  shift ;;
  esac
done
exit 0
FAKECURL
  chmod +x "${stubs}/curl"

  local tool
  for tool in bash env uname tr dirname grep sed cat mkdir rm mv mktemp chmod sleep; do
    ln -sf "$(command -v "$tool")" "${stubs}/$tool"
  done
}

# bf_old_binary <path>: an executable that identifies itself as a previous build.
bf_old_binary() {
  mkdir -p "$(dirname "$1")"
  printf '#!/usr/bin/env bash\necho old-binary-ran\n' > "$1"
  chmod +x "$1"
}

# bf_listing <dir>: sorted, space-separated names (portable; no wc padding).
bf_listing() {
  if [ -d "$1" ]; then (cd "$1" && ls -A | sort | tr '\n' ' '); fi
}

# build_case <name> <with_go_mod:0|1> <go_mode> <setup>
# setup: none | stale-stamp | no-stamp | matching-stamp | downloaded | old-lumen
build_case() {
  local name="$1" with_mod="$2" go_mode="$3" setup="$4"
  (
    _SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
    _ROOT="$(mktemp -d)"
    _STUBS="$(mktemp -d)"
    _LOG="${_ROOT}.log"
    trap 'rm -rf "$_ROOT" "$_STUBS" "$_LOG" "${_ROOT}.err"' EXIT

    bf_stage "$_ROOT" "$_STUBS" "$with_mod" "$go_mode"
    case "$setup" in
      stale-stamp)    bf_old_binary "${_ROOT}/bin/lumen"; echo "0.0.0" > "${_ROOT}/bin/lumen.source-version" ;;
      no-stamp|old-lumen) bf_old_binary "${_ROOT}/bin/lumen" ;;
      matching-stamp) bf_old_binary "${_ROOT}/bin/lumen"; echo "0.0.1" > "${_ROOT}/bin/lumen.source-version" ;;
      downloaded)     bf_old_binary "${_ROOT}/bin/lumen-${_BF_OS}-${_BF_ARCH}" ;;
    esac

    : > "$_LOG"
    _OUT="$(env -u PLUGIN_DATA -u PLUGIN_ROOT -u CURSOR_PLUGIN_ROOT \
      CLAUDE_PLUGIN_ROOT="$_ROOT" STUB_LOG="$_LOG" PATH="$_STUBS" \
      bash "${_SCRIPT_DIR}/run" stdio 2>"${_ROOT}.err")" || { echo "  FAIL: ${name}: launcher exited non-zero"; sed 's/^/          /' "${_ROOT}.err"; exit 1; }
    _ERR="$(cat "${_ROOT}.err")"
    _LOGTXT="$(cat "$_LOG")"
    _LIST="$(bf_listing "${_ROOT}/bin")"

    expect_out() { [ "$_OUT" = "$1" ] || { echo "  FAIL: ${name}: stdout was '${_OUT}', want '$1'"; exit 1; }; }
    expect_log() { echo "$_LOGTXT" | grep -q -- "$1" || { echo "  FAIL: ${name}: log missing '$1': $_LOGTXT"; exit 1; }; }
    expect_no_log() { if echo "$_LOGTXT" | grep -q -- "$1"; then echo "  FAIL: ${name}: log unexpectedly has '$1'"; exit 1; fi; }
    expect_list() { [ "$_LIST" = "$1" ] || { echo "  FAIL: ${name}: bin/ is '${_LIST}', want '$1'"; exit 1; }; }
    expect_stamp() { [ "$(cat "${_ROOT}/bin/lumen.source-version" 2>/dev/null)" = "$1" ] || { echo "  FAIL: ${name}: stamp is '$(cat "${_ROOT}/bin/lumen.source-version" 2>/dev/null)', want '$1'"; exit 1; }; }

    case "$name" in
      build-succeeds)
        expect_out "built-binary-ran"
        expect_no_log "^curl"
        expect_log "^pwd=${_ROOT}$"
        expect_log "^cgo=1$"
        expect_log "^args=build -tags=fts5 -buildvcs=false -ldflags=-X github.com/ory/lumen/cmd.buildVersion=0.0.1 -o ${_ROOT}/bin/\.[^ ]* \.$"
        expect_list "lumen lumen.source-version "
        expect_stamp "0.0.1"
        echo "$_ERR" | grep -q "build output on stdout" || { echo "  FAIL: ${name}: go stdout not redirected to stderr"; exit 1; }
        ;;
      build-fails)
        expect_out "downloaded-binary-ran"
        expect_log "^go build"
        expect_log "^curl"
        expect_list "lumen-${_BF_OS}-${_BF_ARCH} "
        ;;
      no-go-mod|no-go)
        expect_out "downloaded-binary-ran"
        expect_no_log "^go "
        expect_no_log "^started"
        ;;
      stale-stamp|no-stamp)
        expect_out "built-binary-ran"
        expect_log "^started"
        expect_no_log "^curl"
        expect_list "lumen lumen.source-version "
        expect_stamp "0.0.1"
        ;;
      matching-stamp)
        expect_out "old-binary-ran"
        expect_no_log "^started"
        expect_no_log "^curl"
        ;;
      downloaded-candidate)
        expect_out "built-binary-ran"
        expect_no_log "^curl"
        expect_list "lumen lumen-${_BF_OS}-${_BF_ARCH} lumen.source-version "
        ;;
      no-go-old-lumen)
        expect_out "old-binary-ran"
        expect_no_log "^curl"
        expect_list "lumen "
        ;;
      build-fails-stale)
        expect_out "old-binary-ran"
        expect_log "^go build"
        expect_no_log "^curl"
        expect_list "lumen lumen.source-version "
        expect_stamp "0.0.0"
        ;;
    esac
    echo "  PASS: ${name}"
  ) && PASS=$((PASS + 1)) || FAIL=$((FAIL + 1))
}

build_case build-succeeds       1 ok   none
build_case build-fails          1 fail none
build_case no-go-mod            0 ok   none
build_case no-go                1 none none
build_case stale-stamp          1 ok   stale-stamp
build_case no-stamp             1 ok   no-stamp
build_case matching-stamp       1 ok   matching-stamp
build_case downloaded-candidate 1 ok   downloaded
build_case no-go-old-lumen      1 none old-lumen
build_case build-fails-stale    1 fail stale-stamp

# An interrupted launcher must not leave its temp build file behind.
(
  _SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
  _ROOT="$(mktemp -d)"
  _STUBS="$(mktemp -d)"
  _LOG="${_ROOT}.log"
  trap 'rm -rf "$_ROOT" "$_STUBS" "$_LOG"' EXIT
  bf_stage "$_ROOT" "$_STUBS" 1 slow
  : > "$_LOG"

  env -u PLUGIN_DATA -u PLUGIN_ROOT -u CURSOR_PLUGIN_ROOT \
    CLAUDE_PLUGIN_ROOT="$_ROOT" STUB_LOG="$_LOG" STUB_GO_SLEEP=1 PATH="$_STUBS" \
    bash "${_SCRIPT_DIR}/run" stdio >/dev/null 2>&1 &
  _PID=$!
  for _ in $(seq 1 50); do
    grep -q '^started' "$_LOG" && break
    sleep 0.1
  done
  kill -TERM "$_PID" 2>/dev/null || true
  wait "$_PID" 2>/dev/null || true

  _LIST="$(bf_listing "${_ROOT}/bin")"
  if [ -n "$_LIST" ]; then
    echo "  FAIL: interrupted-build: bin/ not cleaned up: '${_LIST}'"
    exit 1
  fi
  echo "  PASS: interrupted-build"
) && PASS=$((PASS + 1)) || FAIL=$((FAIL + 1))

echo ""
echo "=== summary ==="
echo "  passed: $PASS"
echo "  failed: $FAIL"
[ "$FAIL" -eq 0 ] || exit 1
