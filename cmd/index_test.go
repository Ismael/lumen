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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunIndex_RefusesUnindexableRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".lumenignore"), []byte("**\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runIndex(indexCmd, []string{dir})
	if err == nil {
		t.Fatal("expected runIndex to refuse an un-indexable root, got nil error")
	}
	if !strings.Contains(err.Error(), ".lumenignore catch-all") {
		t.Fatalf("expected error to mention the .lumenignore catch-all reason, got %q", err.Error())
	}
}
