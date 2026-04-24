---
name: reindex
description: Refresh or rebuild the bundled Lumen index for the current project, preferring MCP-driven refreshes and using the CLI only for an explicit clean rebuild.
---

# Lumen Reindex

Refresh or rebuild the bundled Lumen index for the current project.

## Steps

1. Call the Lumen `index_status` tool for the current working directory so you
   can report the current state before making changes.
2. If the user wants the index refreshed or seeded, call the Lumen
   `semantic_search` tool with a broad natural-language query and set `path` or
   `cwd` to the current working directory. The search tool refreshes stale or
   missing indexes automatically.
3. If the user explicitly asks for a clean rebuild, explain the options and
   run one via the shell:
   - `lumen purge . && lumen index .` — deletes only the current project's
     cached index before rebuilding. Prefer this.
   - `lumen purge && lumen index .` — deletes every cached index on the host
     before rebuilding. Use only when the user asks for a full wipe.
4. After the refresh or rebuild, report the new index status.
