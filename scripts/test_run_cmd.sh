#!/usr/bin/env bash
# Tests run.cmd polyglot delegation on POSIX across multiple shells.
#
# The polyglot must work when invoked via:
#   - the user's shell (./scripts/run.cmd args) — relies on the shell's
#     ENOEXEC fallback to /bin/sh.
#   - libc execvp (Node.js / Cursor / Claude Code spawn) — falls back to
#     /bin/sh on ENOEXEC.
#   - explicit shell wrappers (bash run.cmd, dash run.cmd, ...) — runs
#     the file as a sh script directly.
#
# We can't reproduce the libc execvp path from inside this bash script
# (without a tiny C helper), but we can cover every shell that runs the
# script directly. If the polyglot breaks under any of these shells, the
# matching production spawn path would also break.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

cp "${SCRIPT_DIR}/run.cmd" "${TMP_DIR}/run.cmd"
chmod +x "${TMP_DIR}/run.cmd"

cat > "${TMP_DIR}/run.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'delegated:%s\n' "$*"
EOF
chmod +x "${TMP_DIR}/run.sh"

EXPECTED="delegated:stdio --flag"
PASS=0
FAIL=0

run_under() {
  local desc="$1"; shift
  local got rc=0
  got="$("$@" "${TMP_DIR}/run.cmd" stdio --flag 2>/dev/null)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "  FAIL ($desc): exec returned $rc"
    FAIL=$((FAIL + 1))
    return
  fi
  if [ "$got" != "$EXPECTED" ]; then
    echo "  FAIL ($desc): expected '$EXPECTED', got '$got'"
    FAIL=$((FAIL + 1))
    return
  fi
  echo "  PASS ($desc)"
  PASS=$((PASS + 1))
}

echo "=== direct invocation (parent shell handles ENOEXEC) ==="
DIRECT_RC=0
DIRECT_OUTPUT="$("${TMP_DIR}/run.cmd" stdio --flag 2>/dev/null)" || DIRECT_RC=$?
if [ "$DIRECT_RC" -ne 0 ]; then
  echo "  FAIL (direct): exec returned $DIRECT_RC"
  FAIL=$((FAIL + 1))
elif [ "$DIRECT_OUTPUT" != "$EXPECTED" ]; then
  echo "  FAIL (direct): expected '$EXPECTED', got '$DIRECT_OUTPUT'"
  FAIL=$((FAIL + 1))
else
  echo "  PASS (direct)"
  PASS=$((PASS + 1))
fi

echo ""
echo "=== explicit shell invocation ==="
for shell in /bin/sh bash dash zsh ksh; do
  case "$shell" in
    /*)
      [ -x "$shell" ] || { echo "  SKIP ($shell): not present"; continue; } ;;
    *)
      command -v "$shell" >/dev/null 2>&1 || { echo "  SKIP ($shell): not on PATH"; continue; } ;;
  esac
  run_under "$shell" "$shell"
done

echo ""
echo "=== summary ==="
echo "  passed: $PASS"
echo "  failed: $FAIL"
[ "$FAIL" -eq 0 ] || exit 1
