#!/usr/bin/env bash
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

OUTPUT="$("${TMP_DIR}/run.cmd" stdio --flag)"
EXPECTED="delegated:stdio --flag"

if [ "$OUTPUT" != "$EXPECTED" ]; then
  echo "FAIL: run.cmd should delegate to run.sh when exec'd directly on Unix"
  echo "expected: $EXPECTED"
  echo "got:      $OUTPUT"
  exit 1
fi

echo "PASS: run.cmd delegates to run.sh when exec'd directly on Unix"
