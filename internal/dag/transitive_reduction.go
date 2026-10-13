// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package dag

import (
	"cmp"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
)

// transitiveReductionIndexed is an implementation of TransitiveReduction
// for large graphs, which first translates the graph into a compact
// integer-indexed form and then finds the redundant edges of many vertices
// concurrently.
//
// The transitive reduction of a DAG is unique, so this produces exactly the
// same result as transitiveReductionGeneric. It returns false without
// modifying the graph if the graph is not a DAG, in which case the caller
// must use the generic implementation instead.
func (g *AcyclicGraph) transitiveReductionIndexed() bool {
	ig, ok := g.indexed()
	if !ok {
		return false
	}
	n := len(ig.vertices)
	if n == 0 {
		return true
	}
	verts, order, out := ig.vertices, ig.order, ig.out

	// The height of a vertex is the length of the longest path starting at
	// it. Height strictly decreases along every edge, so a path from one
	// vertex to another can only pass through vertices whose height is
	// between those of the two endpoints.
	height := make([]int32, n)
	for i := n - 1; i >= 0; i-- {
		v := order[i]
		h := int32(0)
		for _, t := range out(v) {
			if height[t]+1 > h {
				h = height[t] + 1
			}
		}
		height[v] = h
	}

	// Searching the whole of what's below a vertex, for every vertex, takes
	// time in proportion to the square of the size of a deep graph. Most of
	// that is spent in parts of the graph from which none of the vertices
	// we're looking for can be reached, so we label every vertex in a way
	// that lets us recognize many of those cheaply and not search them.
	//
	// Each labelling numbers the vertices in the order a depth-first
	// traversal finishes with them, and records for each vertex the lowest
	// number among everything reachable from it. Whatever is reachable from
	// a vertex then has a number between that lowest number and the
	// vertex's own. The converse isn't true, so this can only tell us that
	// something is not reachable, but two traversals in different orders
	// between them rule out most of what isn't.
	labels := reachabilityLabels(n, order, out)

	type edge struct{ from, to int32 }

	workers := runtime.GOMAXPROCS(0)
	if workers > n {
		workers = n
	}
	const batch = 64
	var next atomic.Int64
	redundant := make([][]edge, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()

			// seen and child hold the index (plus one) of the vertex u
			// they were last marked for, so that we don't need to clear
			// them when moving on to the next u.
			seen := make([]int32, n)
			child := make([]int32, n)
			// position is where each direct descendant of the current u
			// is in targets, which along with skip is described below.
			position := make([]int32, n)
			var targets, skip []int32
			var stack []int32
			var found []edge

			for {
				end := next.Add(batch)
				start := end - batch
				if start >= int64(n) {
					break
				}
				if end > int64(n) {
					end = int64(n)
				}
				for u := int32(start); u < int32(end); u++ {
					uTargets := out(u)
					if len(uTargets) < 2 {
						continue
					}

					// The edge from u to v is redundant if v is reachable
					// from any of the other direct descendants of u. Such a
					// path can only pass through vertices that are higher
					// than the lowest direct descendant of u.
					mark := u + 1
					minHeight := height[uTargets[0]]
					stack = stack[:0]
					for _, t := range uTargets {
						child[t] = mark
						if height[t] < minHeight {
							minHeight = height[t]
						}
					}
					// The direct descendants of u in order of their number
					// in the first labelling, so that we can find the ones
					// whose numbers are in a given range.
					targets = append(targets[:0], uTargets...)
					slices.SortFunc(targets, func(a, b int32) int {
						return cmp.Compare(labels[0].rank[a], labels[0].rank[b])
					})
					// skip[i] is the position of some target at or after
					// position i such that all of those in between have
					// already been found, which lets us pass over the ones
					// we no longer need to look for without examining each
					// of them every time.
					skip = skip[:0]
					for i := range len(targets) + 1 {
						skip = append(skip, int32(i))
						if i < len(targets) {
							position[targets[i]] = int32(i)
						}
					}
					unfound := func(i int32) int32 {
						root := i
						for skip[root] != root {
							root = skip[root]
						}
						for skip[i] != root {
							skip[i], i = root, skip[i]
						}
						return root
					}
					// mightReach returns false only if none of the direct
					// descendants of u that we've not yet found can be
					// reached from v.
					mightReach := func(v int32) bool {
						low, rank := labels[0].low[v], labels[0].rank[v]
						first, _ := slices.BinarySearchFunc(targets, low, func(t, low int32) int {
							return cmp.Compare(labels[0].rank[t], low)
						})
						for i := unfound(int32(first)); int(i) < len(targets); i = unfound(i + 1) {
							t := targets[i]
							if labels[0].rank[t] >= rank {
								break
							}
							if r := labels[1].rank[t]; r >= labels[1].low[v] && r < labels[1].rank[v] {
								return true
							}
						}
						return false
					}
					for _, t := range uTargets {
						if height[t] > minHeight && mightReach(t) {
							seen[t] = mark
							stack = append(stack, t)
						}
					}
					// The lowest direct descendants of u cannot reach any
					// other direct descendant, and at least one of the
					// highest ones is not reachable from any other, so
					// there's nothing left to find once we've found all
					// of the others.
					remaining := len(uTargets) - 1

					for len(stack) > 0 && remaining > 0 {
						v := stack[len(stack)-1]
						stack = stack[:len(stack)-1]
						for _, t := range out(v) {
							if child[t] == mark {
								child[t] = 0
								skip[position[t]] = position[t] + 1
								found = append(found, edge{u, t})
								remaining--
							}
							if height[t] > minHeight && seen[t] != mark {
								seen[t] = mark
								if mightReach(t) {
									stack = append(stack, t)
								}
							}
						}
					}
				}
			}
			redundant[w] = found
		}(w)
	}
	wg.Wait()

	// Removing an edge means updating the set of outgoing edges of its
	// source and the set of incoming edges of its target. There can be a
	// very large number of redundant edges, so we do those updates
	// concurrently too, which is safe as long as each set is only updated by
	// one goroutine.
	//
	// Each worker above dealt with a distinct set of source vertices, so the
	// lists they produced can be applied to the outgoing edges concurrently.
	wg.Add(len(redundant))
	for _, found := range redundant {
		go func() {
			defer wg.Done()
			for _, e := range found {
				if s, ok := g.edgesFrom[verts[e.from]]; ok {
					s.Delete(verts[e.to])
				}
			}
		}()
	}
	wg.Wait()

	// For the incoming edges we first group the removals by target.
	counts := make([]int32, n+1)
	for _, found := range redundant {
		for _, e := range found {
			counts[e.to+1]++
		}
	}
	for i := 0; i < n; i++ {
		counts[i+1] += counts[i]
	}
	sources := make([]int32, counts[n])
	fill := make([]int32, n)
	for _, found := range redundant {
		for _, e := range found {
			sources[counts[e.to]+fill[e.to]] = e.from
			fill[e.to]++
		}
	}
	next.Store(0)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				end := next.Add(batch)
				start := end - batch
				if start >= int64(n) {
					return
				}
				for to := start; to < min(end, int64(n)); to++ {
					removed := sources[counts[to]:counts[to+1]]
					if len(removed) == 0 {
						continue
					}
					if s, ok := g.edgesTo[verts[to]]; ok {
						for _, from := range removed {
							s.Delete(verts[from])
						}
					}
				}
			}
		}()
	}
	wg.Wait()
	return true
}

// reachabilityLabelCount is the number of labellings that
// reachabilityLabels produces.
const reachabilityLabelCount = 2

// reachabilityLabel is one labelling of the vertices of a graph. If w is
// reachable from v, and isn't v itself, then low[v] <= rank[w] < rank[v].
type reachabilityLabel struct {
	rank, low []int32
}

// reachabilityLabels labels the n vertices of a graph, given a topological
// ordering of them and a function returning the direct descendants of each.
func reachabilityLabels(n int, order []int32, out func(int32) []int32) [reachabilityLabelCount]reachabilityLabel {
	var labels [reachabilityLabelCount]reachabilityLabel
	var wg sync.WaitGroup
	for i := range labels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The two traversals visit the roots, and the descendants of
			// each vertex, in opposite orders.
			reverse := i%2 == 1
			rank := make([]int32, n)
			low := make([]int32, n)
			visited := make([]bool, n)
			type frame struct {
				v    int32
				next int32
			}
			var stack []frame
			counter := int32(0)
			for k := range order {
				root := order[k]
				if reverse {
					root = order[n-1-k]
				}
				if visited[root] {
					continue
				}
				visited[root] = true
				stack = append(stack, frame{root, 0})
				for len(stack) > 0 {
					f := &stack[len(stack)-1]
					targets := out(f.v)
					if int(f.next) < len(targets) {
						t := targets[f.next]
						if reverse {
							t = targets[len(targets)-1-int(f.next)]
						}
						f.next++
						if !visited[t] {
							visited[t] = true
							stack = append(stack, frame{t, 0})
						}
						continue
					}
					v := f.v
					stack = stack[:len(stack)-1]
					l := counter
					for _, t := range targets {
						l = min(l, low[t])
					}
					rank[v], low[v] = counter, l
					counter++
				}
			}
			labels[i] = reachabilityLabel{rank, low}
		}()
	}
	wg.Wait()
	return labels
}
