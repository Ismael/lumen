# Installing Lumen for OpenCode

Install Lumen as an OpenCode plugin from npm. The plugin registers a local MCP
server automatically.

## Prerequisites

- [OpenCode.ai](https://opencode.ai) installed

## Installation

Add Lumen to the `plugin` array in your `opencode.json`:

```json
{
  "plugin": ["@ory/lumen-opencode"]
}
```

Restart OpenCode. The plugin registers a local `mcp.lumen` server that runs
`scripts/run stdio` on all platforms (cross-spawn resolves `scripts/run.cmd`
via PATHEXT on Windows).

## Verify

```bash
opencode mcp list
```

Then ask OpenCode to call the Lumen `semantic_search`, `health_check`, or
`index_status` MCP tools directly.

## Updating

Restart OpenCode after updating the version pin in `opencode.json`, or pin a
specific version:

```json
{
  "plugin": ["@ory/lumen-opencode@0.0.29"]
}
```

## Uninstalling

Remove the `@ory/lumen-opencode` entry from `opencode.json` and restart
OpenCode.
