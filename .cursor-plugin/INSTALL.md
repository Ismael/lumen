# Installing Lumen for Cursor

Lumen ships a native Cursor plugin bundle in this repository.

## Bundle contents

- `.cursor-plugin/plugin.json` - Cursor plugin manifest
- `.cursor/mcp.json` - local `lumen` MCP server wiring
- `hooks/hooks-cursor.json` - SessionStart hook
- `skills/` - shared `doctor` and `reindex` skills
- `scripts/run` (POSIX) and `scripts/run.cmd` (Windows) - launcher pair used
  by the MCP server and hooks. Configs reference the extensionless
  `scripts/run`; cross-spawn / cmd.exe pick `.cmd` via PATHEXT on Windows.

## Installation

Use Cursor's plugin installation or distribution workflow with this repository's
bundle rooted at `.cursor-plugin/plugin.json`.

This repository contains the package surface and runtime assets needed by
Cursor, but public marketplace publication is separate from the repository
itself.

## Verify

Open a new Cursor agent session and ask it to use the `doctor` skill or the
Lumen `semantic_search` tool.

## Updating

Update or republish the Cursor plugin bundle after pulling the latest version
of this repository.
