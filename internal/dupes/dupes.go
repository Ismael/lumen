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
	Chunks []store.ChunkRef // sorted (RelativePath, StartLine, EndLine, Kind, Symbol, VectorID)
	MaxSim float32
	MinSim float32
	Lines  int // distinct lines covered by Chunks
}

// Result holds the clusters found and the number of chunks compared.
type Result struct {
	Clusters []Cluster // ordered: Lines desc, MaxSim desc, then the first chunk's sort key
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

// row is an eligible chunk with its path interned so the pair loop compares
// integers.
type row struct {
	ref  store.ChunkRef
	fam  family
	file int
	run  int
}

type link struct {
	i, j int
	sim  float32
}

// Find groups each file's split pieces into runs, drops runs nested inside a
// same-symbol run of the same family and members nested inside another member
// of their run, compares every eligible pair outside a shared run and returns
// the connected components of pairs whose similarity is at least
// opts.Threshold.
func Find(chunks []store.ChunkRef, vectors map[int64][]float32, opts Options) Result {
	rows, matrix, dims := eligibleRows(collapse(chunks, vectors), vectors, opts)
	res := Result{Compared: len(rows)}
	if len(rows) < 2 {
		return res
	}
	links := findLinks(rows, matrix, dims, opts.Threshold, opts.Workers)
	res.Clusters = buildClusters(rows, links)
	return res
}

// candidate is a chunk that survived collapse, tagged with the index of its
// run.
type candidate struct {
	ref store.ChunkRef
	run int
}

// run is a sequence of chunks in one file with the same (Symbol, Kind), each
// touching or overlapping the ones before it: the pieces of one split
// declaration, or a single unsplit chunk.
type run struct {
	path, symbol, kind string
	fam                family
	start, end         int
	hasVector          bool
	members            []int
}

// buildRuns groups every code and decl chunk into runs. Other kinds belong to
// no run.
func buildRuns(chunks []store.ChunkRef, vectors map[int64][]float32) []run {
	type key struct{ path, symbol, kind string }
	groups := make(map[key][]int)
	for i, c := range chunks {
		if kindFamily(c.Kind) == familyNone {
			continue
		}
		k := key{c.RelativePath, c.Symbol, c.Kind}
		groups[k] = append(groups[k], i)
	}

	var runs []run
	for _, idx := range groups {
		slices.SortFunc(idx, func(a, b int) int {
			return cmp.Compare(chunks[a].StartLine, chunks[b].StartLine)
		})
		cur := -1
		for _, i := range idx {
			c := chunks[i]
			hasVector := len(vectors[c.VectorID]) > 0
			if cur >= 0 && c.StartLine <= runs[cur].end+1 {
				r := &runs[cur]
				r.end = max(r.end, c.EndLine)
				r.hasVector = r.hasVector || hasVector
				r.members = append(r.members, i)
				continue
			}
			runs = append(runs, run{
				path:      c.RelativePath,
				symbol:    c.Symbol,
				kind:      c.Kind,
				fam:       kindFamily(c.Kind),
				start:     c.StartLine,
				end:       c.EndLine,
				hasVector: hasVector,
				members:   []int{i},
			})
			cur = len(runs) - 1
		}
	}
	return runs
}

// collapse drops every run whose span lies inside the span of another run
// with the same file, symbol and family that has a member with a vector, and
// within each surviving run every member nested inside another member with a
// vector. The surviving chunks come back in no particular order.
func collapse(chunks []store.ChunkRef, vectors map[int64][]float32) []candidate {
	runs := buildRuns(chunks, vectors)
	type key struct {
		path, symbol string
		fam          family
	}
	groups := make(map[key][]int)
	for r, ru := range runs {
		k := key{ru.path, ru.symbol, ru.fam}
		groups[k] = append(groups[k], r)
	}

	out := make([]candidate, 0, len(chunks))
	for _, idx := range groups {
	nextRun:
		for _, r := range idx {
			for _, o := range idx {
				if o != r && runs[o].hasVector && covers(&runs[o], &runs[r]) {
					continue nextRun
				}
			}
			for _, m := range runs[r].members {
				if !nestedMember(chunks, vectors, runs[r].members, m) {
					out = append(out, candidate{ref: chunks[m], run: r})
				}
			}
		}
	}
	return out
}

// covers reports whether outer's span contains inner's and outer wins: it is
// larger, or the spans are equal and outer's Kind sorts first. Runs of one
// kind never share a span, so Kind always breaks the tie.
func covers(outer, inner *run) bool {
	if outer.start > inner.start || outer.end < inner.end {
		return false
	}
	if outer.start != inner.start || outer.end != inner.end {
		return true
	}
	return outer.kind < inner.kind
}

// nestedMember reports whether member m lies inside another member with a
// vector: strictly, with an equal span and a smaller VectorID, or as an
// identical copy that appears earlier in the input.
func nestedMember(chunks []store.ChunkRef, vectors map[int64][]float32, members []int, m int) bool {
	inner := chunks[m]
	for _, o := range members {
		outer := chunks[o]
		if o == m || len(vectors[outer.VectorID]) == 0 {
			continue
		}
		if outer.StartLine > inner.StartLine || outer.EndLine < inner.EndLine {
			continue
		}
		if outer.StartLine != inner.StartLine || outer.EndLine != inner.EndLine {
			return true
		}
		if outer.VectorID < inner.VectorID || (outer.VectorID == inner.VectorID && o < m) {
			return true
		}
	}
	return false
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

// eligibleRows filters candidates, sorts them by their report key and copies
// their vectors into one row-major matrix. Row order is the sort order, so
// cluster members collected by ascending row are already sorted.
func eligibleRows(cands []candidate, vectors map[int64][]float32, opts Options) ([]row, []float32, int) {
	kept := make([]candidate, 0, len(cands))
	for _, c := range cands {
		if kindFamily(c.ref.Kind) == familyNone || lineCount(c.ref) < opts.MinLines {
			continue
		}
		if opts.Include != nil && !opts.Include(c.ref.RelativePath) {
			continue
		}
		if len(vectors[c.ref.VectorID]) == 0 {
			continue
		}
		kept = append(kept, c)
	}
	slices.SortStableFunc(kept, func(a, b candidate) int { return compareRefs(a.ref, b.ref) })
	if len(kept) == 0 {
		return nil, nil, 0
	}

	// One index has one dimension; a vector of another length cannot be
	// compared and is treated as missing.
	dims := len(vectors[kept[0].ref.VectorID])
	files := make(map[string]int)
	rows := make([]row, 0, len(kept))
	matrix := make([]float32, 0, len(kept)*dims)
	for _, c := range kept {
		v := vectors[c.ref.VectorID]
		if len(v) != dims {
			continue
		}
		file, ok := files[c.ref.RelativePath]
		if !ok {
			file = len(files)
			files[c.ref.RelativePath] = file
		}
		rows = append(rows, row{ref: c.ref, fam: kindFamily(c.ref.Kind), file: file, run: c.run})
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
	return a.run == b.run
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
						// A normalised vector's self-dot can round to just below 1.
						sim := float32(1)
						if rows[i].ref.VectorID != rows[j].ref.VectorID {
							sim = dot(vi, matrix[j*dims:(j+1)*dims])
						}
						if sim >= threshold {
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

// coveredLines counts the distinct lines of chunks sorted by (RelativePath,
// StartLine), so overlapping split pieces count their shared lines once.
func coveredLines(chunks []store.ChunkRef) int {
	total, last := 0, 0
	for i, c := range chunks {
		if i == 0 || c.RelativePath != chunks[i-1].RelativePath {
			last = c.StartLine - 1
		}
		if c.EndLine > last {
			total += c.EndLine - max(c.StartLine, last+1) + 1
			last = c.EndLine
		}
	}
	return total
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
	}
	for i := range clusters {
		clusters[i].Lines = coveredLines(clusters[i].Chunks)
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
