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
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ory/lumen/internal/config"
	"github.com/ory/lumen/internal/indexlock"
)

// enterWorktreeWaitBudget bounds how long the EnterWorktree hook blocks the
// agent. It must stay below the hook timeout declared in hooks/hooks.json.
const enterWorktreeWaitBudget = 30 * time.Second

const enterWorktreeLockPollInterval = 100 * time.Millisecond

var hookEnterWorktreeCmd = &cobra.Command{
	Use:   "enter-worktree",
	Short: "Index a worktree entered via Claude Code's EnterWorktree tool and wait briefly for it",
	Args:  cobra.NoArgs,
	RunE:  runHookEnterWorktree,
}

// enterWorktreeInput is the subset of the PostToolUse payload for EnterWorktree
// that the hook reads.
type enterWorktreeInput struct {
	ToolResponse struct {
		WorktreePath string `json:"worktreePath"`
	} `json:"tool_response"`
}

type indexerProcess interface {
	Wait() error
}

type enterWorktreeDeps struct {
	resolve      func(path string) (root, dbPath string, err error)
	spawn        func(root string) (indexerProcess, error)
	locksBusy    func(dbPath, root string) (bool, error)
	budget       time.Duration
	pollInterval time.Duration
}

func defaultEnterWorktreeDeps() enterWorktreeDeps {
	return enterWorktreeDeps{
		resolve: resolveEnterWorktreeTarget,
		spawn: func(root string) (indexerProcess, error) {
			// Returned separately so a failed start yields a nil interface, not a
			// non-nil one wrapping a nil *exec.Cmd.
			cmd, err := startBackgroundIndexer(root)
			if err != nil {
				return nil, err
			}
			return cmd, nil
		},
		locksBusy:    worktreeIndexLocksBusy,
		budget:       enterWorktreeWaitBudget,
		pollInterval: enterWorktreeLockPollInterval,
	}
}

// runHookEnterWorktree never returns an error: a failing hook would surface
// as a failed EnterWorktree call to the agent.
func runHookEnterWorktree(_ *cobra.Command, _ []string) error {
	logger, logFile := silentDebugLogger()
	if logFile != nil {
		defer func() { _ = logFile.Close() }()
	}
	enterWorktree(os.Stdin, defaultEnterWorktreeDeps(), logger)
	return nil
}

// silentDebugLogger is newDebugLogger without its stderr fallback, so the hook
// never produces output even when the debug log cannot be opened.
func silentDebugLogger() (*slog.Logger, io.Closer) {
	logger, logFile := newDebugLogger()
	if logFile == nil {
		return slog.New(slog.DiscardHandler), nil
	}
	return logger, logFile
}

// enterWorktree runs the whole hook within deps.budget, which starts here.
func enterWorktree(stdin io.Reader, deps enterWorktreeDeps, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), deps.budget)
	defer cancel()

	path, err := parseEnterWorktreePath(stdin)
	if err != nil {
		logger.Warn("enter-worktree hook: invalid input", "err", err)
		return
	}
	if path == "" {
		logger.Debug("enter-worktree hook: no worktreePath in tool_response")
		return
	}
	root, dbPath, err := resolveWithinBudget(ctx, path, deps.resolve)
	if err != nil {
		logger.Warn("enter-worktree hook: resolve project", "path", path, "err", err)
		return
	}
	waitForWorktreeIndex(ctx, root, dbPath, deps, logger)
}

// resolveWithinBudget abandons resolve when ctx ends: embedding server probes
// and git calls behind it take no context and can outlast the hook timeout.
func resolveWithinBudget(ctx context.Context, path string, resolve func(string) (string, string, error)) (root, dbPath string, err error) {
	type result struct {
		root, dbPath string
		err          error
	}
	done := make(chan result, 1)
	go func() {
		r, d, err := resolve(path)
		done <- result{r, d, err}
	}()
	select {
	case res := <-done:
		return res.root, res.dbPath, res.err
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

func parseEnterWorktreePath(r io.Reader) (string, error) {
	dec := json.NewDecoder(r)
	var input enterWorktreeInput
	if err := dec.Decode(&input); err != nil {
		return "", fmt.Errorf("decode hook input: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("decode hook input: unexpected data after the JSON payload")
	}
	return input.ToolResponse.WorktreePath, nil
}

// resolveEnterWorktreeTarget returns the project root and collection DB path
// that `lumen index` will use for path.
func resolveEnterWorktreeTarget(path string) (root, dbPath string, err error) {
	cfg, err := config.NewConfigService(config.DefaultConfigPath())
	if err != nil {
		return "", "", fmt.Errorf("load config: %w", err)
	}
	modelName := newEmbedder(cfg).ModelName()
	root = resolveProjectRoot(path, configuredDBPathFor(cfg, modelName))
	return root, configuredDBPath(cfg, root, modelName), nil
}

// waitForWorktreeIndex spawns the indexer for root and waits, until ctx ends,
// for it to exit and then for any other indexer of root (or cleanup of the
// collection) to finish. On timeout the indexer is left running.
func waitForWorktreeIndex(ctx context.Context, root, dbPath string, deps enterWorktreeDeps, logger *slog.Logger) {
	proc, err := deps.spawn(root)
	if err != nil {
		logger.Warn("enter-worktree hook: spawn indexer", "project", root, "err", err)
		return
	}
	exited := make(chan error, 1)
	go func() { exited <- proc.Wait() }()

	// Locks are only probed after the child exits: before it takes its own
	// locks, free locks would be indistinguishable from a finished index.
	select {
	case err := <-exited:
		if err != nil {
			logger.Warn("enter-worktree hook: indexer failed", "project", root, "err", err)
		}
	case <-ctx.Done():
		logger.Info("enter-worktree hook: indexer still running at budget", "project", root, "budget", deps.budget.String())
		return
	}

	for {
		busy, err := deps.locksBusy(dbPath, root)
		if err != nil {
			logger.Warn("enter-worktree hook: probe index locks", "project", root, "err", err)
			return
		}
		if !busy {
			return
		}
		select {
		case <-ctx.Done():
			logger.Info("enter-worktree hook: index locks still held at budget", "project", root, "budget", deps.budget.String())
			return
		case <-time.After(deps.pollInterval):
		}
	}
}

// worktreeIndexLocksBusy reports whether `lumen index` would currently skip
// root: its project lock is held, or the collection lock is held exclusively
// by cleanup. Shared collection holders are indexers of other worktrees.
func worktreeIndexLocksBusy(dbPath, root string) (bool, error) {
	for _, lockPath := range []string{
		indexlock.LockPathForProject(dbPath, root),
		indexlock.LockPathForDB(dbPath),
	} {
		held, err := indexlock.ExclusivelyHeld(lockPath)
		if err != nil || held {
			return held, err
		}
	}
	return false, nil
}
