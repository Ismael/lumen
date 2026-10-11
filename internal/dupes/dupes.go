// Package dupes finds clusters of likely-duplicated code from stored chunk
// embeddings.
package dupes

import (
	"cmp"
	"math"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/ory/lumen/internal/store"
)

// Options controls which chunks are compared and how similar they must be.
type Options struct {
	Threshold float32
	MinLines  int
	Include   func(relPath string) bool // nil = every path
	Workers   int                       // <= 0 = runtime.GOMAXPROCS(0)
}

// Cluster is a connected group of chunks whose links all meet the threshold.
type Cluster struct {
	Chunks []store.ChunkRef // sorted (RelativePath, StartLine, EndLine, Kind, Symbol)
	MaxSim float32
	MinSim float32
	Lines  int // sum of member line counts
}

// Result holds the clusters found and the number of chunks compared.
type Result struct {
	Clusters []Cluster // ordered: Lines desc, MaxSim desc, first chunk's sort key
	Compared int       // eligible chunk count
}

type family int

const (
	familyNone family = iota
	familyCode
	familyDecl
)

func kindFamily(kind string) family {
	switch kind {
	case "function", "method", "const", "var":
		return familyCode
	case "interface", "type":
		return familyDecl
	default:
		return familyNone
	}
}

// rowBlock is the number of matrix rows a worker claims at a time.
const rowBlock = 32

// row is an eligible chunk with its path and symbol interned so the pair
// loop compares integers.
type row struct {
	ref    store.ChunkRef
	fam    family
	file   int
	symbol int
}

type link struct {
	i, j int
	sim  float32
}

// Find collapses nested same-symbol chunks, compares every eligible pair by
// dot product and returns the connected components of pairs whose similarity
// is at least opts.Threshold.
func Find(chunks []store.ChunkRef, vectors map[int64][]float32, opts Options) Result {
	rows, matrix, dims := eligibleRows(collapse(chunks), vectors, opts)
	res := Result{Compared: len(rows)}
	if len(rows) < 2 {
		return res
	}
	links := findLinks(rows, matrix, dims, opts.Threshold, opts.Workers)
	res.Clusters = buildClusters(rows, links)
	return res
}

// collapse drops every chunk made redundant by another chunk of the same
// file and symbol. Input order is preserved.
func collapse(chunks []store.ChunkRef) []store.ChunkRef {
	type key struct{ path, symbol string }
	groups := make(map[key][]int)
	for i, c := range chunks {
		k := key{c.RelativePath, c.Symbol}
		groups[k] = append(groups[k], i)
	}
	drop := make([]bool, len(chunks))
	for _, idx := range groups {
		for _, i := range idx {
			for _, j := range idx {
				if i != j && covers(chunks[j], chunks[i]) {
					drop[i] = true
					break
				}
			}
		}
	}
	out := make([]store.ChunkRef, 0, len(chunks))
	for i, c := range chunks {
		if !drop[i] {
			out = append(out, c)
		}
	}
	return out
}

// covers reports whether outer's range contains inner's and outer wins:
// it is larger, or the ranges are equal and outer sorts first by
// (Kind, VectorID).
func covers(outer, inner store.ChunkRef) bool {
	if outer.StartLine > inner.StartLine || outer.EndLine < inner.EndLine {
		return false
	}
	if outer.StartLine != inner.StartLine || outer.EndLine != inner.EndLine {
		return true
	}
	if c := cmp.Compare(outer.Kind, inner.Kind); c != 0 {
		return c < 0
	}
	return outer.VectorID < inner.VectorID
}

func compareRefs(a, b store.ChunkRef) int {
	return cmp.Or(
		cmp.Compare(a.RelativePath, b.RelativePath),
		cmp.Compare(a.StartLine, b.StartLine),
		cmp.Compare(a.EndLine, b.EndLine),
		cmp.Compare(a.Kind, b.Kind),
		cmp.Compare(a.Symbol, b.Symbol),
		cmp.Compare(a.VectorID, b.VectorID),
	)
}

func lineCount(c store.ChunkRef) int {
	return c.EndLine - c.StartLine + 1
}

// eligibleRows filters chunks, sorts them by their report key and copies
// their vectors into one row-major matrix. Row order is the sort order, so
// cluster members collected by ascending row are already sorted.
func eligibleRows(chunks []store.ChunkRef, vectors map[int64][]float32, opts Options) ([]row, []float32, int) {
	candidates := make([]store.ChunkRef, 0, len(chunks))
	for _, c := range chunks {
		if kindFamily(c.Kind) == familyNone || lineCount(c) < opts.MinLines {
			continue
		}
		if opts.Include != nil && !opts.Include(c.RelativePath) {
			continue
		}
		if len(vectors[c.VectorID]) == 0 {
			continue
		}
		candidates = append(candidates, c)
	}
	slices.SortStableFunc(candidates, compareRefs)
	if len(candidates) == 0 {
		return nil, nil, 0
	}

	// One index has one dimension; a vector of another length cannot be
	// compared and is treated as missing.
	dims := len(vectors[candidates[0].VectorID])
	files := make(map[string]int)
	symbols := make(map[string]int)
	intern := func(m map[string]int, s string) int {
		id, ok := m[s]
		if !ok {
			id = len(m)
			m[s] = id
		}
		return id
	}
	rows := make([]row, 0, len(candidates))
	matrix := make([]float32, 0, len(candidates)*dims)
	for _, c := range candidates {
		v := vectors[c.VectorID]
		if len(v) != dims {
			continue
		}
		rows = append(rows, row{
			ref:    c,
			fam:    kindFamily(c.Kind),
			file:   intern(files, c.RelativePath),
			symbol: intern(symbols, c.Symbol),
		})
		matrix = append(matrix, v...)
	}
	return rows, matrix, dims
}

// skipPair reports whether a and b must not be linked regardless of their
// similarity.
func skipPair(a, b *row) bool {
	if a.fam != b.fam {
		return true
	}
	if a.file != b.file {
		return false
	}
	if a.ref.StartLine <= b.ref.EndLine && b.ref.StartLine <= a.ref.EndLine {
		return true
	}
	if a.symbol != b.symbol {
		return false
	}
	first, second := a, b
	if second.ref.StartLine < first.ref.StartLine {
		first, second = second, first
	}
	return first.ref.EndLine+1 >= second.ref.StartLine
}

func dot(a, b []float32) float32 {
	var s float32
	for k := range a {
		s += a[k] * b[k]
	}
	return s
}

// findLinks compares row i with every row j > i across workers. Links are
// gathered per worker; callers must not depend on their order.
func findLinks(rows []row, matrix []float32, dims int, threshold float32, workers int) []link {
	n := len(rows)
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	workers = min(workers, (n+rowBlock-1)/rowBlock)

	var next atomic.Int64
	perWorker := make([][]link, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			var local []link
			for {
				start := int(next.Add(rowBlock)) - rowBlock
				if start >= n {
					break
				}
				for i := start; i < min(start+rowBlock, n); i++ {
					vi := matrix[i*dims : (i+1)*dims]
					for j := i + 1; j < n; j++ {
						if skipPair(&rows[i], &rows[j]) {
							continue
						}
						if sim := dot(vi, matrix[j*dims:(j+1)*dims]); sim >= threshold {
							local = append(local, link{i: i, j: j, sim: sim})
						}
					}
				}
			}
			perWorker[w] = local
		})
	}
	wg.Wait()

	var links []link
	for _, l := range perWorker {
		links = append(links, l...)
	}
	return links
}

// components is a union-find that also tracks the similarity range of the
// links inside each component.
type components struct {
	parent []int
	size   []int
	minSim []float32
	maxSim []float32
}

func newComponents(n int) *components {
	c := &components{
		parent: make([]int, n),
		size:   make([]int, n),
		minSim: make([]float32, n),
		maxSim: make([]float32, n),
	}
	for i := range n {
		c.parent[i] = i
		c.size[i] = 1
		c.minSim[i] = float32(math.Inf(1))
		c.maxSim[i] = float32(math.Inf(-1))
	}
	return c
}

func (c *components) find(i int) int {
	for c.parent[i] != i {
		c.parent[i] = c.parent[c.parent[i]]
		i = c.parent[i]
	}
	return i
}

func (c *components) add(l link) {
	ri, rj := c.find(l.i), c.find(l.j)
	if ri != rj {
		if c.size[ri] < c.size[rj] {
			ri, rj = rj, ri
		}
		c.parent[rj] = ri
		c.size[ri] += c.size[rj]
		c.minSim[ri] = min(c.minSim[ri], c.minSim[rj])
		c.maxSim[ri] = max(c.maxSim[ri], c.maxSim[rj])
	}
	c.minSim[ri] = min(c.minSim[ri], l.sim)
	c.maxSim[ri] = max(c.maxSim[ri], l.sim)
}

func buildClusters(rows []row, links []link) []Cluster {
	comps := newComponents(len(rows))
	for _, l := range links {
		comps.add(l)
	}

	index := make(map[int]int)
	var clusters []Cluster
	for i, r := range rows {
		root := comps.find(i)
		if comps.size[root] < 2 {
			continue
		}
		ci, ok := index[root]
		if !ok {
			ci = len(clusters)
			index[root] = ci
			clusters = append(clusters, Cluster{
				MaxSim: comps.maxSim[root],
				MinSim: comps.minSim[root],
			})
		}
		clusters[ci].Chunks = append(clusters[ci].Chunks, r.ref)
		clusters[ci].Lines += lineCount(r.ref)
	}

	slices.SortStableFunc(clusters, func(a, b Cluster) int {
		return cmp.Or(
			cmp.Compare(b.Lines, a.Lines),
			cmp.Compare(b.MaxSim, a.MaxSim),
			compareRefs(a.Chunks[0], b.Chunks[0]),
		)
	})
	return clusters
}
