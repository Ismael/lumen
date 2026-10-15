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

package chunker

import (
	"strings"
	"testing"
)

func TestTreeSitterChunker_NoDuplicateChunks(t *testing.T) {
	c1 := Chunk{FilePath: "f.go", StartLine: 1, EndLine: 5, Kind: "function", Symbol: "A"}
	c2 := Chunk{FilePath: "f.go", StartLine: 1, EndLine: 5, Kind: "method", Symbol: "A"}
	result := deduplicateByExactRange([]Chunk{c1, c2})
	if len(result) != 1 {
		t.Fatalf("expected 1 chunk after dedup, got %d", len(result))
	}
	if result[0].Kind != "method" {
		t.Errorf("expected kind=method (later wins), got %s", result[0].Kind)
	}
}

func TestTreeSitterChunker_WrappedDeclarationsChunkedOnce(t *testing.T) {
	langs := DefaultLanguages(512)

	type wantChunk struct {
		symbol        string
		kind          string
		startLine     int
		endLine       int
		contentPrefix string
		contentHas    string
	}
	cases := []struct {
		name string
		ext  string
		src  string
		want []wantChunk
	}{
		{
			name: "python decorated def",
			ext:  ".py",
			src:  "@decorator\ndef foo():\n    pass\n",
			want: []wantChunk{{symbol: "foo", kind: "function", startLine: 1, endLine: 3, contentPrefix: "@decorator"}},
		},
		{
			name: "python decorated class with comment above decorator",
			ext:  ".py",
			src:  "# doc\n@dataclass\nclass Foo:\n    pass\n",
			want: []wantChunk{{symbol: "Foo", kind: "type", startLine: 1, endLine: 4, contentPrefix: "# doc\n@dataclass"}},
		},
		{
			name: "python module-level assignment with leading comment",
			ext:  ".py",
			src:  "# lead\nCONST = 5\n",
			want: []wantChunk{{symbol: "CONST", kind: "var", startLine: 1, endLine: 2, contentPrefix: "# lead"}},
		},
		{
			name: "python method that ends the class body stays separate",
			ext:  ".py",
			src:  "class A:\n    def m(self):\n        pass\n",
			want: []wantChunk{
				{symbol: "A", kind: "type", startLine: 1, endLine: 3, contentPrefix: "class A:"},
				{symbol: "m", kind: "function", startLine: 2, endLine: 3, contentPrefix: "def m(self):"},
			},
		},
		{
			name: "python decorated method that ends the class body stays separate",
			ext:  ".py",
			src:  "class A:\n    @property\n    def p(self):\n        return 1\n",
			want: []wantChunk{
				{symbol: "A", kind: "type", startLine: 1, endLine: 4, contentPrefix: "class A:"},
				{symbol: "p", kind: "function", startLine: 2, endLine: 4, contentPrefix: "@property\n    def p(self):"},
			},
		},
		{
			name: "ts commented exported arrow function",
			ext:  ".ts",
			src:  "// doc\nexport const f = () => {\n  return 1;\n};\n",
			want: []wantChunk{{symbol: "f", kind: "function", startLine: 1, endLine: 4, contentPrefix: "// doc\nexport const f"}},
		},
		{
			name: "js commented exported arrow function",
			ext:  ".js",
			src:  "// doc\nexport const f = () => {\n  return 1;\n};\n",
			want: []wantChunk{{symbol: "f", kind: "function", startLine: 1, endLine: 4, contentPrefix: "// doc\nexport const f"}},
		},
		{
			name: "tsx commented exported arrow function",
			ext:  ".tsx",
			src:  "// doc\nexport const C = () => <div/>;",
			want: []wantChunk{{symbol: "C", kind: "function", startLine: 1, endLine: 2, contentPrefix: "// doc\nexport const C"}},
		},
		{
			name: "svelte commented exported arrow function",
			ext:  ".svelte",
			src:  "<script lang=\"ts\">\n// doc\nexport const f = () => 1;\n</script>\n",
			want: []wantChunk{{symbol: "f", kind: "function", startLine: 2, endLine: 3, contentPrefix: "// doc\nexport const f"}},
		},
		{
			name: "ts commented exported non-function const",
			ext:  ".ts",
			src:  "// doc\nexport const BAR = 5;\n",
			want: []wantChunk{{symbol: "BAR", kind: "const", startLine: 1, endLine: 2, contentPrefix: "// doc\nexport const BAR"}},
		},
		{
			name: "ts uncommented exported arrow function",
			ext:  ".ts",
			src:  "export const N = () => 2;\n",
			want: []wantChunk{{symbol: "N", kind: "function", startLine: 1, endLine: 1, contentPrefix: "export const N"}},
		},
		{
			name: "ts commented exported function declaration",
			ext:  ".ts",
			src:  "// doc\nexport function baz() {}\n",
			want: []wantChunk{{symbol: "baz", kind: "function", startLine: 1, endLine: 2, contentPrefix: "// doc", contentHas: "export function baz"}},
		},
		{
			name: "cpp template function",
			ext:  ".cpp",
			src:  "template <typename T>\nT biggest(T a, T b) {\n  return a > b ? a : b;\n}\n",
			want: []wantChunk{{symbol: "biggest", kind: "function", startLine: 1, endLine: 4, contentPrefix: "template <typename T>"}},
		},
		{
			name: "cpp template function returning pointer",
			ext:  ".cpp",
			src:  "template <typename T>\nT* make() {\n  return new T();\n}\n",
			want: []wantChunk{{symbol: "make", kind: "function", startLine: 1, endLine: 4, contentPrefix: "template <typename T>"}},
		},
		{
			name: "cpp template class",
			ext:  ".cpp",
			src:  "template <typename T>\nclass Box {\n  T v;\n};\n",
			want: []wantChunk{{symbol: "Box", kind: "type", startLine: 1, endLine: 4, contentPrefix: "template <typename T>"}},
		},
		{
			name: "cpp template struct",
			ext:  ".cpp",
			src:  "template <typename T>\nstruct Pair {\n  T a, b;\n};\n",
			want: []wantChunk{{symbol: "Pair", kind: "type", startLine: 1, endLine: 4, contentPrefix: "template <typename T>"}},
		},
		{
			name: "cpp template alias",
			ext:  ".cpp",
			src:  "template <typename T>\nusing Vec = std::vector<T>;\n",
			want: []wantChunk{{symbol: "Vec", kind: "type", startLine: 1, endLine: 2, contentPrefix: "template <typename T>"}},
		},
		{
			name: "cpp template qualified method",
			ext:  ".cpp",
			src:  "template <typename T>\nT Box<T>::get() {\n  return v;\n}\n",
			want: []wantChunk{{symbol: "get", kind: "method", startLine: 1, endLine: 4, contentPrefix: "template <typename T>"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks, err := langs[tc.ext].Chunk("f"+tc.ext, []byte(tc.src))
			if err != nil {
				t.Fatalf("Chunk: %v", err)
			}
			if len(chunks) != len(tc.want) {
				t.Fatalf("got %d chunks, want %d: %+v", len(chunks), len(tc.want), chunks)
			}
			for _, w := range tc.want {
				var matched []Chunk
				for _, c := range chunks {
					if c.Symbol == w.symbol {
						matched = append(matched, c)
					}
				}
				if len(matched) != 1 {
					t.Fatalf("symbol %q: got %d chunks, want 1: %+v", w.symbol, len(matched), matched)
				}
				got := matched[0]
				if got.Kind != w.kind || got.StartLine != w.startLine || got.EndLine != w.endLine {
					t.Errorf("symbol %q: got kind=%s lines %d-%d, want kind=%s lines %d-%d",
						w.symbol, got.Kind, got.StartLine, got.EndLine, w.kind, w.startLine, w.endLine)
				}
				if !strings.HasPrefix(got.Content, w.contentPrefix) {
					t.Errorf("symbol %q: content does not start with %q:\n%s", w.symbol, w.contentPrefix, got.Content)
				}
				if w.contentHas != "" && !strings.Contains(got.Content, w.contentHas) {
					t.Errorf("symbol %q: content missing %q:\n%s", w.symbol, w.contentHas, got.Content)
				}
			}
		})
	}
}
