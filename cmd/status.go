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
	"fmt"
	"strings"

	"github.com/ory/lumen/internal/config"
)

// statusResult is the combined outcome of the two status checks: embedding
// service reachability and index presence/freshness.
type statusResult struct {
	server           config.ServerConfig
	serviceReachable bool
	serviceMessage   string

	projectPath   string
	indexed       bool
	totalFiles    int
	indexedFiles  int
	totalChunks   int
	model         string
	lastIndexedAt string
	stale         bool
}

// statusExitCode returns 0 only when the service is reachable, the index
// exists, and the index is fresh; otherwise 1.
func statusExitCode(r statusResult) int {
	if r.serviceReachable && r.indexed && !r.stale {
		return 0
	}
	return 1
}

// formatStatus renders a human-readable status report combining the embedding
// service line and the index block (or a "not indexed" line).
func formatStatus(r statusResult) string {
	var b strings.Builder

	if r.serviceReachable {
		fmt.Fprintf(&b, "Embedding service: OK (%s, %s, %s)\n", r.server.Backend, r.server.Host, r.server.Model)
	} else {
		fmt.Fprintf(&b, "Embedding service: ERROR (%s, %s) — %s\n", r.server.Backend, r.server.Host, r.serviceMessage)
	}

	if !r.indexed {
		fmt.Fprintf(&b, "Index: %s — not indexed (run `lumen index .`, or it will seed on first search)", r.projectPath)
		return b.String()
	}

	stale := "no"
	if r.stale {
		stale = "yes"
	}
	lastIndexed := r.lastIndexedAt
	if lastIndexed == "" {
		lastIndexed = "never"
	}
	fmt.Fprintf(&b, "Index: %s\n", r.projectPath)
	fmt.Fprintf(&b, "  Files: %d | Indexed: %d | Chunks: %d | Model: %s\n", r.totalFiles, r.indexedFiles, r.totalChunks, r.model)
	fmt.Fprintf(&b, "  Last indexed: %s | Stale: %s", lastIndexed, stale)
	return b.String()
}
