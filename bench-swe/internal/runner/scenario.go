package runner

import (
	"encoding/json"
	"fmt"
	"os"
)

type Scenario string

const (
	Baseline  Scenario = "baseline"
	WithLumen Scenario = "with-lumen"
)

// LumenMCPServerName is the --mcp-config server key for the lumen MCP server.
// It is deliberately the plugin-namespaced name (plugin_lumen_lumen), not a
// bare "lumen", so the resolved tool id (mcp__<key>__semantic_search) matches
// what a real marketplace plugin install registers. The SessionStart hook —
// loaded here via --plugin-dir from hooks/hooks.json — advertises that exact
// plugin-namespaced tool id; a bare key would make the hook point the agent at
// a tool name that does not exist in this --strict-mcp-config setup.
const LumenMCPServerName = "plugin_lumen_lumen"

func AllScenarios() []Scenario {
	return []Scenario{Baseline, WithLumen}
}

func ParseScenarios(filter string) ([]Scenario, error) {
	switch filter {
	case "", "all":
		return AllScenarios(), nil
	case "baseline":
		return []Scenario{Baseline}, nil
	case "with-lumen":
		return []Scenario{WithLumen}, nil
	default:
		return nil, fmt.Errorf("unknown scenario %q (valid: baseline, with-lumen, all)", filter)
	}
}

type mcpConfig struct {
	MCPServers map[string]mcpServer `json:"mcpServers"`
}

type mcpServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

// WriteMCPConfig writes a temp MCP config JSON file.
// Returns path to the temp file and a cleanup function.
func WriteMCPConfig(s Scenario, lumenBinary, backend, model string) (string, func(), error) {
	var cfg mcpConfig

	switch s {
	case Baseline:
		cfg = mcpConfig{MCPServers: map[string]mcpServer{}}
	case WithLumen:
		cfg = mcpConfig{
			MCPServers: map[string]mcpServer{
				LumenMCPServerName: {
					Command: lumenBinary,
					Args:    []string{"stdio"},
					Env: map[string]string{
						"LUMEN_BACKEND":     backend,
						"LUMEN_EMBED_MODEL": model,
					},
				},
			},
		}
	default:
		return "", nil, fmt.Errorf("unknown scenario: %s", s)
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return "", nil, err
	}

	f, err := os.CreateTemp("", fmt.Sprintf("bench-swe-mcp-%s-*.json", s))
	if err != nil {
		return "", nil, err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, err
	}
	_ = f.Close()

	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

// ClaudeArgs returns the extra CLI arguments for a given scenario.
func ClaudeArgs(s Scenario, repoRoot string) []string {
	switch s {
	case WithLumen:
		return []string{
			"--plugin-dir", repoRoot,
		}
	default:
		return nil
	}
}
