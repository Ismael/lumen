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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ory/lumen/internal/config"
	"github.com/ory/lumen/internal/store"
)

// backgroundIndexStaleness is how old last_indexed_at must be before
// SessionStart spawns a background indexer. This prevents every new terminal
// from triggering a full merkle walk when the index was just updated.
const backgroundIndexStaleness = 5 * time.Minute

// NOTE: Hooks are now declared in hooks/hooks.json (plugin system).
// The hook subcommands remain as the execution targets for those declarations.

func init() {
	rootCmd.AddCommand(hookCmd)
	hookCmd.AddCommand(hookSessionStartCmd)
	hookCmd.AddCommand(hookPreToolUseCmd)
	hookCmd.AddCommand(hookEnterWorktreeCmd)

	hookPreToolUseCmd.Flags().StringVar(&hookPreToolUsePluginName, "plugin-name", "", "Claude Code plugin name")
}

var hookCmd = &cobra.Command{
	Use:   "hook",
	Short: "Hook handlers for AI coding agent integration",
}

var hookPreToolUsePluginName string

var hookSessionStartCmd = &cobra.Command{
	Use:   "session-start",
	Short: "Pre-warm the project index in the background at session start",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return sessionStart(os.Stdin, os.Stdout, spawnBackgroundIndexer)
	},
}

// hookOutput is the JSON structure Claude Code expects from a synchronous hook.
type hookOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
}

// sessionStartInput is the subset of SessionStart hook input used across hosts.
type sessionStartInput struct {
	CWD string `json:"cwd"`
}

func sessionStart(stdin io.Reader, stdout io.Writer, bgIndexer func(string)) error {
	var input sessionStartInput
	_ = json.NewDecoder(stdin).Decode(&input)

	cwd := input.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	prewarmIndex(cwd, bgIndexer)

	// Cursor parses hook stdout as JSON; an empty object adds no context in
	// either Cursor or Claude Code.
	_, err := io.WriteString(stdout, "{}\n")
	return err
}

func mcpToolReference(pluginName, mcpName, toolName string) string {
	if pluginName != "" {
		mcpName = "plugin_" + pluginName + "_" + mcpName
	}
	return "mcp__" + mcpName + "__" + toolName
}

// prewarmIndex starts bgIndexer for cwd's project when its index is missing,
// incomplete, empty, failed or stale, so the first search in the session
// doesn't pay the full seed + embed cost synchronously.
func prewarmIndex(cwd string, bgIndexer func(string)) {
	cfg, err := config.NewConfigService(config.DefaultConfigPath())
	if err != nil {
		return
	}
	emb := newEmbedder(cfg)
	modelName := emb.ModelName()
	dims := cfg.ServerDims(0)

	// Normalize cwd so the DB path matches what `lumen index` and the MCP
	// handler use.
	cwd = resolveProjectRoot(cwd, configuredDBPathFor(cfg, modelName))

	dbPath := configuredDBPath(cfg, cwd, modelName)
	if _, err := os.Stat(dbPath); err != nil {
		bgIndexer(cwd)
		return
	}

	s, err := store.NewCollection(dbPath, dims, cfg.VectorStorage(), cwd)
	if err != nil {
		return
	}
	defer func() { _ = s.Close() }()

	stats, err := s.Stats()
	if err != nil {
		return
	}

	// An index is only ready after a successful indexing pass produced chunks.
	// A database can exist (and even contain file rows) after embedding failed.
	lastIndexedAt, metaErr := s.GetMeta("last_indexed_at")
	lastIndexError, _ := s.GetMeta(store.MetaLastIndexError)
	completed := metaErr == nil && lastIndexedAt != ""
	stale := true
	if completed {
		if t, parseErr := time.Parse(time.RFC3339, lastIndexedAt); parseErr == nil {
			stale = time.Since(t) > backgroundIndexStaleness
		}
	}
	if !completed || stale || stats.TotalChunks == 0 || lastIndexError != "" {
		bgIndexer(cwd)
	}
}

// --- PreToolUse hook ---

var hookPreToolUseCmd = &cobra.Command{
	Use:   "pre-tool-use [mcp-name]",
	Short: "Intercept Grep calls and suggest semantic search when appropriate",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runHookPreToolUse,
}

// preToolUseInput is the JSON structure Claude Code sends to PreToolUse hooks.
type preToolUseInput struct {
	ToolName string         `json:"tool_name"`
	Input    map[string]any `json:"tool_input"`
}

func runHookPreToolUse(_ *cobra.Command, args []string) error {
	mcpName := filepath.Base(os.Args[0])
	if len(args) > 0 {
		mcpName = args[0]
	}

	var input preToolUseInput
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		// Can't parse input — allow silently (exit 0, no stdout).
		return nil
	}

	result := evaluateToolCall(input, mcpName, hookPreToolUsePluginName)
	if result == nil {
		// Silent allow — exit 0 with no stdout.
		return nil
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(result)
}

// evaluateToolCall determines whether a tool call should be intercepted
// with a suggestion to use semantic search instead.
// Returns nil for silent allow (no output), or a hookOutput with a suggestion.
func evaluateToolCall(input preToolUseInput, mcpName, pluginName string) *hookOutput {
	switch input.ToolName {
	case "Grep", "Glob":
		// Always suggest semantic search for any file/code search.
	case "Bash":
		cmd, _ := input.Input["command"].(string)
		if !strings.Contains(cmd, "grep") && !strings.Contains(cmd, "rg ") && !strings.Contains(cmd, "find ") {
			return nil
		}
	default:
		return nil
	}

	toolRef := mcpToolReference(pluginName, mcpName, "semantic_search")
	return &hookOutput{
		HookSpecificOutput: hookSpecificOutput{
			HookEventName: "PreToolUse",
			AdditionalContext: fmt.Sprintf(
				"Load and call %s instead of Grep/Glob/find/rg for significantly faster and better search results to reduce context window use and give better quality results.",
				toolRef,
			),
		},
	}
}
