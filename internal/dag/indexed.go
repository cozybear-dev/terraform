// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package dag

// indexedGraph is a compact, read-only snapshot of a directed acyclic graph
// where each vertex is identified by a small integer, which allows the
// algorithms that need to visit a large part of the graph many times to do so
// without the overhead of hashing vertices.
type indexedGraph struct {
	vertices []Vertex
	index    map[Vertex]int32

	// The targets of the edges from vertex i are
	// targets[offsets[i]:offsets[i+1]].
	offsets []int32
	targets []int32

	// order is a topological ordering of the vertices, where each vertex
	// appears before all of the vertices it has edges to.
	order []int32
}

// indexed returns an indexed snapshot of the graph, or returns false if
// the graph cannot be represented that way because it contains a cycle or
// has an edge to a vertex that isn't in the graph.
func (g *Graph) indexed() (*indexedGraph, bool) {
	n := g.vertices.Len()
	ig := &indexedGraph{
		vertices: make([]Vertex, 0, n),
		index:    make(map[Vertex]int32, n),
		offsets:  make([]int32, n+1),
	}
	edgeCount := 0
	for v := range g.vertices.All() {
		ig.index[v] = int32(len(ig.vertices))
		ig.vertices = append(ig.vertices, v)
		edgeCount += g.edgesFrom[v].Len()
	}

	ig.targets = make([]int32, 0, edgeCount)
	indegree := make([]int32, n)
	for i, v := range ig.vertices {
		for t := range g.edgesFrom[v].All() {
			ti, ok := ig.index[t]
			if !ok || ti == int32(i) {
				return nil, false
			}
			ig.targets = append(ig.targets, ti)
			indegree[ti]++
		}
		ig.offsets[i+1] = int32(len(ig.targets))
	}

	ig.order = make([]int32, 0, n)
	for i := range indegree {
		if indegree[i] == 0 {
			ig.order = append(ig.order, int32(i))
		}
	}
	for next := 0; next < len(ig.order); next++ {
		for _, t := range ig.out(ig.order[next]) {
			indegree[t]--
			if indegree[t] == 0 {
				ig.order = append(ig.order, t)
			}
		}
	}
	if len(ig.order) != n {
		return nil, false // there's at least one cycle
	}

	return ig, true
}

// out returns the targets of all of the edges from the given vertex.
func (ig *indexedGraph) out(i int32) []int32 {
	return ig.targets[ig.offsets[i]:ig.offsets[i+1]]
}

// AncestorLabelSets calculates, for each vertex in the graph, which labels
// are present among all of the vertices that the method Ancestors would
// return for that vertex, without the cost of walking the graph separately
// for each one.
//
// The label function must return the label of the given vertex as an integer
// greater than or equal to zero and less than labelCount, or a negative
// number if the vertex has no label. Any number of vertices can share the
// same label.
//
// The visit function is called once for each vertex for which want returns
// true, with a bit set whose bit i is set if label i is present. The set
// is valid only until visit returns, and visit must not modify it.
//
// AncestorLabelSets returns false without calling visit at all if the graph
// contains cycles, in which case the caller must use Ancestors instead.
func (g *AcyclicGraph) AncestorLabelSets(labelCount int, label func(Vertex) int, want func(Vertex) bool, visit func(v Vertex, labels LabelSet)) bool {
	ig, ok := g.indexed()
	if !ok {
		return false
	}
	n := len(ig.vertices)
	words := (labelCount + 63) / 64

	labels := make([]int, n)
	// pending is the number of edges to each vertex from vertices we've
	// not dealt with yet, so that we can recycle the set for a vertex once
	// nothing else needs it.
	pending := make([]int32, n)
	for i, v := range ig.vertices {
		labels[i] = label(v)
		for _, t := range ig.out(int32(i)) {
			pending[t]++
		}
	}

	sets := make([]LabelSet, n)
	var free []LabelSet
	for i := n - 1; i >= 0; i-- {
		v := ig.order[i]
		var set LabelSet
		if len(free) > 0 {
			set = free[len(free)-1]
			free = free[:len(free)-1]
			clear(set)
		} else {
			set = make(LabelSet, words)
		}

		for _, t := range ig.out(v) {
			if l := labels[t]; l >= 0 {
				set[l/64] |= 1 << (l % 64)
			}
			for w, bits := range sets[t] {
				set[w] |= bits
			}
			pending[t]--
			if pending[t] == 0 {
				free = append(free, sets[t])
				sets[t] = nil
			}
		}

		if want(ig.vertices[v]) {
			visit(ig.vertices[v], set)
		}
		if pending[v] == 0 {
			free = append(free, set)
		} else {
			sets[v] = set
		}
	}
	return true
}

// LabelSet is a set of the integer labels passed to AncestorLabelSets.
type LabelSet []uint64

// Has returns true if the given label is in the set.
func (s LabelSet) Has(label int) bool {
	return s[label/64]&(1<<(label%64)) != 0
}

// Remove removes the given label from the set.
func (s LabelSet) Remove(label int) {
	s[label/64] &^= 1 << (label % 64)
}
