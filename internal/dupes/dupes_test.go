package dupes

import (
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ory/lumen/internal/store"
)

// unit normalises in float64 so that e.g. unit(3, 4, 0, 0) yields exactly
// float32(0.6) and float32(0.8), keeping the dot products in the tests exact.
func unit(xs ...float64) []float32 {
	var n float64
	for _, x := range xs {
		n += x * x
	}
	n = math.Sqrt(n)
	out := make([]float32, len(xs))
	for i, x := range xs {
		out[i] = float32(x / n)
	}
	return out
}

var testVectors = map[int64][]float32{
	1:  unit(1, 0, 0, 0),
	2:  unit(1, 0, 0, 0),
	3:  unit(3, 4, 0, 0),
	4:  unit(0, 1, 0, 0),
	5:  unit(0, 0, 1, 0),
	7:  unit(0, 0, 3, 4),
	8:  unit(1, 0, 0, 0),
	9:  unit(1, 0, 0, 0),
	10: unit(1, 0, 0, 0),
	11: unit(-1, 0, 0, 0),
	12: unit(0, 1, 0, 0),
}

func ref(path, sym, kind string, start, end int, vid int64) store.ChunkRef {
	return store.ChunkRef{RelativePath: path, Symbol: sym, Kind: kind, StartLine: start, EndLine: end, VectorID: vid}
}

func TestFind(t *testing.T) {
	tests := []struct {
		name   string
		chunks []store.ChunkRef
		opts   Options
		want   Result
	}{
		{
			name: "similarity equal to threshold links",
			chunks: []store.ChunkRef{
				ref("a.go", "A", "function", 1, 10, 1),
				ref("b.go", "B", "function", 1, 10, 3),
			},
			opts: Options{Threshold: 0.6, MinLines: 1},
			want: Result{Compared: 2, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.go", "A", "function", 1, 10, 1),
					ref("b.go", "B", "function", 1, 10, 3),
				},
				MaxSim: 0.6, MinSim: 0.6, Lines: 20,
			}}},
		},
		{
			name: "similarity just below threshold does not link",
			chunks: []store.ChunkRef{
				ref("a.go", "A", "function", 1, 10, 1),
				ref("b.go", "B", "function", 1, 10, 3),
			},
			opts: Options{Threshold: math.Nextafter32(0.6, 1), MinLines: 1},
			want: Result{Compared: 2},
		},
		{
			name: "chunks shorter than MinLines are ineligible",
			chunks: []store.ChunkRef{
				ref("a.go", "A", "function", 1, 5, 1),
				ref("b.go", "B", "function", 1, 5, 2),
				ref("c.go", "C", "function", 1, 4, 8),
			},
			opts: Options{Threshold: 0.9, MinLines: 5},
			want: Result{Compared: 2, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.go", "A", "function", 1, 5, 1),
					ref("b.go", "B", "function", 1, 5, 2),
				},
				MaxSim: 1, MinSim: 1, Lines: 10,
			}}},
		},
		{
			name: "chunks without a vector are ineligible",
			chunks: []store.ChunkRef{
				ref("a.go", "A", "function", 1, 10, 1),
				ref("b.go", "B", "function", 1, 10, 999),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 1},
		},
		{
			name: "different families and doc kinds never link",
			chunks: []store.ChunkRef{
				ref("a.go", "F", "function", 1, 10, 1),
				ref("b.go", "I", "interface", 1, 10, 2),
				ref("c.md", "S", "section", 1, 10, 8),
				ref("d.md", "D", "document", 1, 10, 9),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 2},
		},
		{
			name: "code links with code and decl with decl",
			chunks: []store.ChunkRef{
				ref("a.go", "C", "const", 1, 10, 1),
				ref("b.go", "V", "var", 1, 20, 2),
				ref("c.go", "T", "type", 1, 5, 4),
				ref("d.go", "I", "interface", 1, 5, 12),
				ref("e.go", "M", "method", 1, 5, 5),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 5, Clusters: []Cluster{
				{
					Chunks: []store.ChunkRef{
						ref("a.go", "C", "const", 1, 10, 1),
						ref("b.go", "V", "var", 1, 20, 2),
					},
					MaxSim: 1, MinSim: 1, Lines: 30,
				},
				{
					Chunks: []store.ChunkRef{
						ref("c.go", "T", "type", 1, 5, 4),
						ref("d.go", "I", "interface", 1, 5, 12),
					},
					MaxSim: 1, MinSim: 1, Lines: 10,
				},
			}},
		},
		{
			name: "nested chunks with different symbols in one file are not linked",
			chunks: []store.ChunkRef{
				ref("b.go", "Outer", "function", 1, 20, 8),
				ref("b.go", "Inner", "function", 5, 10, 9),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 2},
		},
		{
			name: "partially overlapping chunks in one file are not linked",
			chunks: []store.ChunkRef{
				ref("a.go", "F", "function", 1, 10, 1),
				ref("a.go", "G", "method", 5, 15, 2),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 2},
		},
		{
			name: "pieces of one run never link to each other",
			chunks: []store.ChunkRef{
				ref("a.go", "F", "function", 21, 30, 8),
				ref("a.go", "F", "function", 11, 20, 2),
				ref("a.go", "F", "function", 1, 10, 1),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 3},
		},
		{
			name: "same-file pair sharing a vector id is skipped when overlapping",
			chunks: []store.ChunkRef{
				ref("a.go", "F", "function", 1, 10, 1),
				ref("a.go", "G", "function", 5, 15, 1),
			},
			opts: Options{Threshold: 1, MinLines: 1},
			want: Result{Compared: 2},
		},
		{
			name: "same-file pair sharing a vector id is skipped within one run",
			chunks: []store.ChunkRef{
				ref("a.go", "F", "function", 1, 10, 1),
				ref("a.go", "F", "function", 21, 30, 1),
				ref("a.go", "F", "function", 11, 20, 2),
			},
			opts: Options{Threshold: 1, MinLines: 1},
			want: Result{Compared: 3},
		},
		{
			name: "same symbol separated by other lines is linked",
			chunks: []store.ChunkRef{
				ref("a.ts", "render", "method", 12, 21, 2),
				ref("a.ts", "render", "method", 1, 10, 1),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 2, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.ts", "render", "method", 1, 10, 1),
					ref("a.ts", "render", "method", 12, 21, 2),
				},
				MaxSim: 1, MinSim: 1, Lines: 20,
			}}},
		},
		{
			name: "contained same-symbol chunk collapses into the larger one",
			chunks: []store.ChunkRef{
				ref("a.ts", "X", "function", 308, 359, 2),
				ref("a.ts", "X", "const", 306, 359, 1),
				ref("b.ts", "Y", "function", 1, 54, 8),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 2, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.ts", "X", "const", 306, 359, 1),
					ref("b.ts", "Y", "function", 1, 54, 8),
				},
				MaxSim: 1, MinSim: 1, Lines: 108,
			}}},
		},
		{
			name: "cross-file double emission gives one two-chunk cluster",
			chunks: []store.ChunkRef{
				ref("a.ts", "X", "const", 306, 359, 1),
				ref("a.ts", "X", "function", 308, 359, 2),
				ref("b.ts", "X", "function", 308, 359, 9),
				ref("b.ts", "X", "const", 306, 359, 8),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 2, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.ts", "X", "const", 306, 359, 1),
					ref("b.ts", "X", "const", 306, 359, 8),
				},
				MaxSim: 1, MinSim: 1, Lines: 108,
			}}},
		},
		{
			name: "equal spans keep the run first by kind",
			chunks: []store.ChunkRef{
				ref("a.ts", "X", "function", 1, 10, 9),
				ref("a.ts", "X", "const", 5, 10, 8),
				ref("a.ts", "X", "const", 1, 6, 10),
				ref("b.ts", "Y", "function", 1, 10, 1),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 3, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.ts", "X", "const", 1, 6, 10),
					ref("a.ts", "X", "const", 5, 10, 8),
					ref("b.ts", "Y", "function", 1, 10, 1),
				},
				MaxSim: 1, MinSim: 1, Lines: 22,
			}}},
		},
		{
			name: "split double emission collapses to the const run",
			chunks: []store.ChunkRef{
				ref("a.ts", "X", "const", 361, 416, 1),
				ref("a.ts", "X", "const", 407, 439, 2),
				ref("a.ts", "X", "function", 363, 422, 8),
				ref("a.ts", "X", "function", 413, 439, 9),
				ref("b.ts", "X", "function", 413, 439, 10),
				ref("b.ts", "X", "function", 363, 422, 1),
				ref("b.ts", "X", "const", 407, 439, 2),
				ref("b.ts", "X", "const", 361, 416, 8),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 4, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.ts", "X", "const", 361, 416, 1),
					ref("a.ts", "X", "const", 407, 439, 2),
					ref("b.ts", "X", "const", 361, 416, 8),
					ref("b.ts", "X", "const", 407, 439, 2),
				},
				MaxSim: 1, MinSim: 1, Lines: 56 + 33 + 56 + 33,
			}}},
		},
		{
			name: "chunks sharing a vector id are linked",
			chunks: []store.ChunkRef{
				ref("a.go", "F", "function", 1, 10, 1),
				ref("a.go", "G", "function", 20, 29, 1),
			},
			opts: Options{Threshold: 0.99, MinLines: 1},
			want: Result{Compared: 2, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.go", "F", "function", 1, 10, 1),
					ref("a.go", "G", "function", 20, 29, 1),
				},
				MaxSim: 1, MinSim: 1, Lines: 20,
			}}},
		},
		{
			name: "links chain transitively and singletons are dropped",
			chunks: []store.ChunkRef{
				ref("c.go", "C", "function", 1, 10, 4),
				ref("a.go", "A", "function", 1, 10, 1),
				ref("d.go", "D", "function", 1, 10, 11),
				ref("b.go", "B", "function", 1, 10, 3),
			},
			opts: Options{Threshold: 0.5, MinLines: 1},
			want: Result{Compared: 4, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.go", "A", "function", 1, 10, 1),
					ref("b.go", "B", "function", 1, 10, 3),
					ref("c.go", "C", "function", 1, 10, 4),
				},
				MaxSim: 0.8, MinSim: 0.6, Lines: 30,
			}}},
		},
		{
			name: "clusters order by lines then max similarity",
			chunks: []store.ChunkRef{
				ref("c.go", "C", "function", 1, 10, 5),
				ref("d.go", "D", "function", 1, 10, 7),
				ref("a.go", "A", "function", 1, 10, 4),
				ref("b.go", "B", "function", 1, 10, 12),
				ref("x.go", "X", "function", 1, 20, 1),
				ref("y.go", "Y", "function", 1, 20, 2),
			},
			opts: Options{Threshold: 0.6, MinLines: 1},
			want: Result{Compared: 6, Clusters: []Cluster{
				{
					Chunks: []store.ChunkRef{
						ref("x.go", "X", "function", 1, 20, 1),
						ref("y.go", "Y", "function", 1, 20, 2),
					},
					MaxSim: 1, MinSim: 1, Lines: 40,
				},
				{
					Chunks: []store.ChunkRef{
						ref("a.go", "A", "function", 1, 10, 4),
						ref("b.go", "B", "function", 1, 10, 12),
					},
					MaxSim: 1, MinSim: 1, Lines: 20,
				},
				{
					Chunks: []store.ChunkRef{
						ref("c.go", "C", "function", 1, 10, 5),
						ref("d.go", "D", "function", 1, 10, 7),
					},
					MaxSim: 0.6, MinSim: 0.6, Lines: 20,
				},
			}},
		},
		{
			name: "equal lines and similarity order by first chunk",
			chunks: []store.ChunkRef{
				ref("m.go", "M", "function", 1, 10, 1),
				ref("n.go", "N", "function", 1, 10, 2),
				ref("z.go", "Z", "function", 1, 10, 12),
				ref("k.go", "K", "function", 1, 10, 4),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 4, Clusters: []Cluster{
				{
					Chunks: []store.ChunkRef{
						ref("k.go", "K", "function", 1, 10, 4),
						ref("z.go", "Z", "function", 1, 10, 12),
					},
					MaxSim: 1, MinSim: 1, Lines: 20,
				},
				{
					Chunks: []store.ChunkRef{
						ref("m.go", "M", "function", 1, 10, 1),
						ref("n.go", "N", "function", 1, 10, 2),
					},
					MaxSim: 1, MinSim: 1, Lines: 20,
				},
			}},
		},
		{
			name: "chunks at one path and start line order by end, kind, symbol",
			chunks: []store.ChunkRef{
				ref("b.go", "Q", "function", 1, 10, 2),
				ref("a.go", "W", "function", 1, 12, 1),
				ref("a.go", "Y", "method", 1, 10, 1),
				ref("a.go", "X", "method", 1, 10, 1),
				ref("a.go", "Z", "function", 1, 10, 1),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 5, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.go", "Z", "function", 1, 10, 1),
					ref("a.go", "X", "method", 1, 10, 1),
					ref("a.go", "Y", "method", 1, 10, 1),
					ref("a.go", "W", "function", 1, 12, 1),
					ref("b.go", "Q", "function", 1, 10, 2),
				},
				MaxSim: 1, MinSim: 1, Lines: 52,
			}}},
		},
		{
			name: "constructor inside its class of the same symbol is not collapsed",
			chunks: []store.ChunkRef{
				ref("A.java", "Foo", "type", 3, 16, 1),
				ref("A.java", "Foo", "function", 6, 11, 2),
				ref("B.java", "Foo", "type", 3, 16, 8),
				ref("B.java", "Foo", "function", 6, 11, 9),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 4, Clusters: []Cluster{
				{
					Chunks: []store.ChunkRef{
						ref("A.java", "Foo", "type", 3, 16, 1),
						ref("B.java", "Foo", "type", 3, 16, 8),
					},
					MaxSim: 1, MinSim: 1, Lines: 28,
				},
				{
					Chunks: []store.ChunkRef{
						ref("A.java", "Foo", "function", 6, 11, 2),
						ref("B.java", "Foo", "function", 6, 11, 9),
					},
					MaxSim: 1, MinSim: 1, Lines: 12,
				},
			}},
		},
		{
			name: "container without a vector does not collapse the chunk inside it",
			chunks: []store.ChunkRef{
				ref("a.ts", "X", "const", 306, 359, 999),
				ref("a.ts", "X", "function", 308, 359, 1),
				ref("b.ts", "X", "function", 308, 359, 2),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 2, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.ts", "X", "function", 308, 359, 1),
					ref("b.ts", "X", "function", 308, 359, 2),
				},
				MaxSim: 1, MinSim: 1, Lines: 104,
			}}},
		},
		{
			name: "doc-kind container does not collapse a code chunk",
			chunks: []store.ChunkRef{
				ref("a.md", "X", "section", 1, 50, 8),
				ref("a.md", "X", "function", 10, 20, 1),
				ref("b.go", "Y", "function", 1, 11, 2),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 2, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.md", "X", "function", 10, 20, 1),
					ref("b.go", "Y", "function", 1, 11, 2),
				},
				MaxSim: 1, MinSim: 1, Lines: 22,
			}}},
		},
		{
			name: "same-kind copy nested in a run member collapses",
			chunks: []store.ChunkRef{
				ref("a.py", "decorated", "function", 3, 7, 1),
				ref("a.py", "decorated", "function", 4, 7, 2),
				ref("a.py", "Thing", "type", 9, 12, 4),
				ref("a.py", "Thing", "type", 10, 12, 12),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 2},
		},
		{
			name: "same-kind double emission across files gives one cluster of outer chunks",
			chunks: []store.ChunkRef{
				ref("a.py", "decorated", "function", 4, 7, 2),
				ref("a.py", "decorated", "function", 3, 7, 1),
				ref("b.py", "decorated", "function", 3, 7, 8),
				ref("b.py", "decorated", "function", 4, 7, 9),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 2, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.py", "decorated", "function", 3, 7, 1),
					ref("b.py", "decorated", "function", 3, 7, 8),
				},
				MaxSim: 1, MinSim: 1, Lines: 10,
			}}},
		},
		{
			name: "equal-span run members keep the smaller vector id",
			chunks: []store.ChunkRef{
				ref("a.py", "decorated", "function", 3, 7, 9),
				ref("a.py", "decorated", "function", 3, 7, 8),
				ref("b.py", "H", "function", 1, 5, 1),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 2, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.py", "decorated", "function", 3, 7, 8),
					ref("b.py", "H", "function", 1, 5, 1),
				},
				MaxSim: 1, MinSim: 1, Lines: 10,
			}}},
		},
		{
			name: "split pieces of a run are all kept",
			chunks: []store.ChunkRef{
				ref("a.go", "F", "function", 11, 20, 2),
				ref("a.go", "F", "function", 1, 10, 1),
				ref("b.go", "F", "function", 1, 10, 8),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 3, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.go", "F", "function", 1, 10, 1),
					ref("a.go", "F", "function", 11, 20, 2),
					ref("b.go", "F", "function", 1, 10, 8),
				},
				MaxSim: 1, MinSim: 1, Lines: 30,
			}}},
		},
		{
			name: "vectorless run member does not drop the member nested in it",
			chunks: []store.ChunkRef{
				ref("a.py", "decorated", "function", 3, 7, 999),
				ref("a.py", "decorated", "function", 4, 7, 1),
				ref("b.py", "G", "function", 1, 4, 2),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 2, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.py", "decorated", "function", 4, 7, 1),
					ref("b.py", "G", "function", 1, 4, 2),
				},
				MaxSim: 1, MinSim: 1, Lines: 8,
			}}},
		},
		{
			name: "ineligible middle piece still joins its neighbours into one run",
			chunks: []store.ChunkRef{
				ref("a.go", "F", "function", 1, 10, 1),
				ref("a.go", "F", "function", 11, 12, 2),
				ref("a.go", "F", "function", 13, 22, 8),
			},
			opts: Options{Threshold: 0.9, MinLines: 5},
			want: Result{Compared: 2},
		},
		{
			name: "vectorless equal-span run first by kind does not drop the other run",
			chunks: []store.ChunkRef{
				ref("a.ts", "X", "const", 1, 10, 999),
				ref("a.ts", "X", "function", 1, 10, 1),
				ref("b.ts", "Y", "function", 1, 10, 2),
			},
			opts: Options{Threshold: 0.9, MinLines: 1},
			want: Result{Compared: 2, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.ts", "X", "function", 1, 10, 1),
					ref("b.ts", "Y", "function", 1, 10, 2),
				},
				MaxSim: 1, MinSim: 1, Lines: 20,
			}}},
		},
		{
			name: "Include rejects paths",
			chunks: []store.ChunkRef{
				ref("a.go", "A", "function", 1, 10, 1),
				ref("vendor/c.go", "C", "function", 1, 10, 8),
				ref("b.go", "B", "function", 1, 10, 2),
			},
			opts: Options{Threshold: 0.9, MinLines: 1, Include: func(p string) bool {
				return !strings.HasPrefix(p, "vendor/")
			}},
			want: Result{Compared: 2, Clusters: []Cluster{{
				Chunks: []store.ChunkRef{
					ref("a.go", "A", "function", 1, 10, 1),
					ref("b.go", "B", "function", 1, 10, 2),
				},
				MaxSim: 1, MinSim: 1, Lines: 20,
			}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Find(tt.chunks, testVectors, tt.opts)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Find() =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestFind_DoesNotModifyInput(t *testing.T) {
	chunks := []store.ChunkRef{
		ref("b.ts", "X", "function", 363, 422, 1),
		ref("a.ts", "X", "function", 413, 439, 9),
		ref("a.ts", "X", "const", 407, 439, 2),
		ref("a.ts", "X", "function", 363, 422, 8),
		ref("a.ts", "X", "const", 361, 416, 1),
		ref("a.py", "decorated", "function", 4, 7, 2),
		ref("a.py", "decorated", "function", 3, 7, 1),
	}
	before := slices.Clone(chunks)
	_ = Find(chunks, testVectors, Options{Threshold: 0.5, MinLines: 1})
	if !reflect.DeepEqual(chunks, before) {
		t.Errorf("Find modified its input:\n%+v\nwant\n%+v", chunks, before)
	}
}

func TestFind_SharedVectorIDLinksAtThresholdOne(t *testing.T) {
	// Normalised as store does: float32 norm, then divide. Its self-dot
	// rounds to just below 1.
	x := float32(1) / float32(math.Sqrt(2))
	vec := []float32{x, x}
	if dot(vec, vec) >= 1 {
		t.Fatalf("fixture self-dot = %v; want < 1", dot(vec, vec))
	}
	chunks := []store.ChunkRef{
		ref("a.go", "F", "function", 1, 10, 1),
		ref("a.go", "G", "function", 20, 29, 1),
	}
	want := Result{Compared: 2, Clusters: []Cluster{{
		Chunks: chunks,
		MaxSim: 1, MinSim: 1, Lines: 20,
	}}}
	got := Find(chunks, map[int64][]float32{1: vec}, Options{Threshold: 1, MinLines: 1})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Find() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestFind_ResultIndependentOfWorkers(t *testing.T) {
	const (
		dims      = 16
		centroids = 30
		nChunks   = 400
		nVectors  = 350
	)
	rng := rand.New(rand.NewPCG(1, 2))

	centres := make([][]float64, centroids)
	for i := range centres {
		centres[i] = make([]float64, dims)
		for d := range centres[i] {
			centres[i][d] = rng.NormFloat64()
		}
	}
	vectors := make(map[int64][]float32, nVectors)
	// The highest ids get no vector so some chunks are ineligible.
	for id := range int64(nVectors - 20) {
		c := centres[rng.IntN(centroids)]
		v := make([]float64, dims)
		for d := range v {
			v[d] = c[d] + 0.15*rng.NormFloat64()
		}
		vectors[id] = unit(v...)
	}

	kinds := []string{"function", "method", "const", "var", "interface", "type", "section"}
	chunks := make([]store.ChunkRef, nChunks)
	for i := range chunks {
		start := 1 + rng.IntN(500)
		chunks[i] = store.ChunkRef{
			RelativePath: fmt.Sprintf("pkg/file%02d.go", rng.IntN(25)),
			Symbol:       fmt.Sprintf("Sym%02d", rng.IntN(40)),
			Kind:         kinds[rng.IntN(len(kinds))],
			StartLine:    start,
			EndLine:      start + rng.IntN(60),
			VectorID:     int64(rng.IntN(nVectors)),
		}
	}

	opts := Options{Threshold: 0.85, MinLines: 3}
	var results []Result
	for _, w := range []int{1, 2, 7, 0} {
		opts.Workers = w
		results = append(results, Find(chunks, vectors, opts))
	}
	if len(results[0].Clusters) < 5 {
		t.Fatalf("fixture produced %d clusters; want enough to exercise merging", len(results[0].Clusters))
	}
	for i, w := range []int{2, 7, 0} {
		if !reflect.DeepEqual(results[0], results[i+1]) {
			t.Errorf("Workers=%d result differs from Workers=1", w)
		}
	}
}
