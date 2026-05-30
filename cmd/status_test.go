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
	"strings"
	"testing"

	"github.com/ory/lumen/internal/config"
)

func TestStatusExitCode(t *testing.T) {
	tests := []struct {
		name string
		r    statusResult
		want int
	}{
		{"healthy and fresh", statusResult{serviceReachable: true, indexed: true, stale: false}, 0},
		{"service unreachable", statusResult{serviceReachable: false, indexed: true, stale: false}, 1},
		{"index missing", statusResult{serviceReachable: true, indexed: false}, 1},
		{"index stale", statusResult{serviceReachable: true, indexed: true, stale: true}, 1},
		{"all bad", statusResult{serviceReachable: false, indexed: false, stale: true}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusExitCode(tt.r); got != tt.want {
				t.Errorf("statusExitCode(%+v) = %d, want %d", tt.r, got, tt.want)
			}
		})
	}
}

func TestFormatStatus(t *testing.T) {
	t.Run("healthy and fresh", func(t *testing.T) {
		r := statusResult{
			server:           config.ServerConfig{Backend: "ollama", Host: "http://localhost:11434", Model: "jina"},
			serviceReachable: true,
			serviceMessage:   "service is healthy",
			projectPath:      "/repo",
			indexed:          true,
			totalFiles:       158,
			indexedFiles:     158,
			totalChunks:      1646,
			model:            "jina",
			lastIndexedAt:    "2026-05-30T12:00:00Z",
			stale:            false,
		}
		out := formatStatus(r)
		if !strings.Contains(out, "Embedding service: OK (ollama, http://localhost:11434, jina)") {
			t.Errorf("missing healthy service line:\n%s", out)
		}
		if !strings.Contains(out, "Files: 158 | Indexed: 158 | Chunks: 1646 | Model: jina") {
			t.Errorf("missing index stats line:\n%s", out)
		}
		if !strings.Contains(out, "Stale: no") {
			t.Errorf("expected Stale: no:\n%s", out)
		}
	})

	t.Run("service unreachable", func(t *testing.T) {
		r := statusResult{
			server:           config.ServerConfig{Backend: "ollama", Host: "http://localhost:11434"},
			serviceReachable: false,
			serviceMessage:   "service unreachable: connection refused",
		}
		out := formatStatus(r)
		if !strings.Contains(out, "Embedding service: ERROR (ollama, http://localhost:11434) — service unreachable: connection refused") {
			t.Errorf("missing error service line:\n%s", out)
		}
	})

	t.Run("not indexed", func(t *testing.T) {
		r := statusResult{
			server:           config.ServerConfig{Backend: "ollama", Host: "http://localhost:11434", Model: "jina"},
			serviceReachable: true,
			serviceMessage:   "service is healthy",
			projectPath:      "/repo",
			indexed:          false,
		}
		out := formatStatus(r)
		if !strings.Contains(out, "/repo — not indexed") {
			t.Errorf("missing not-indexed line:\n%s", out)
		}
	})

	t.Run("indexed but never indexed shows never", func(t *testing.T) {
		r := statusResult{
			server:           config.ServerConfig{Backend: "ollama", Host: "h", Model: "m"},
			serviceReachable: true,
			projectPath:      "/repo",
			indexed:          true,
			lastIndexedAt:    "",
			stale:            true,
		}
		out := formatStatus(r)
		if !strings.Contains(out, "Last indexed: never") {
			t.Errorf("expected 'Last indexed: never' when lastIndexedAt empty:\n%s", out)
		}
	})

	t.Run("stale shows yes", func(t *testing.T) {
		r := statusResult{
			server:           config.ServerConfig{Backend: "ollama", Host: "h", Model: "m"},
			serviceReachable: true,
			projectPath:      "/repo",
			indexed:          true,
			lastIndexedAt:    "2026-05-30T12:00:00Z",
			stale:            true,
		}
		out := formatStatus(r)
		if !strings.Contains(out, "Stale: yes") {
			t.Errorf("expected Stale: yes:\n%s", out)
		}
	})
}
