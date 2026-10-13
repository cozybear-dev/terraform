// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package dag

import (
	"math/rand"
	"testing"

	_ "github.com/hashicorp/terraform/internal/logging"
)

func TestAcyclicGraphTransitiveReduction_matchesGeneric(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		n := 1 + rnd.Intn(200)
		density := rnd.Float64()

		var indexed, generic AcyclicGraph
		for v := 0; v < n; v++ {
			indexed.Add(testV(v))
			generic.Add(testV(v))
		}
		for from := 0; from < n; from++ {
			for to := from + 1; to < n; to++ {
				if rnd.Float64() < density {
					indexed.Connect(testV(from), testV(to))
					generic.Connect(testV(from), testV(to))
				}
			}
		}

		if !indexed.transitiveReductionIndexed() {
			t.Fatalf("graph %d: indexed implementation rejected a valid DAG", i)
		}
		generic.transitiveReductionGeneric()

		if got, want := indexed.String(), generic.String(); got != want {
			t.Fatalf("graph %d: wrong result\ngot:\n%s\nwant:\n%s", i, got, want)
		}
		for v := 0; v < n; v++ {
			got, want := indexed.EdgesTo(testV(v)), generic.EdgesTo(testV(v))
			if got.Len() != want.Len() {
				t.Fatalf("graph %d: vertex %d has %d incoming edges; want %d", i, v, got.Len(), want.Len())
			}
			for from := range want.All() {
				if !got.Contains(from) {
					t.Fatalf("graph %d: vertex %d is missing its incoming edge from %v", i, v, from)
				}
			}
		}
	}
}

func TestAcyclicGraphTransitiveReduction_cycle(t *testing.T) {
	var g AcyclicGraph
	g.Add(testV(1))
	g.Add(testV(2))
	g.Add(testV(3))
	g.Connect(testV(1), testV(2))
	g.Connect(testV(2), testV(3))
	g.Connect(testV(3), testV(1))

	if g.transitiveReductionIndexed() {
		t.Fatal("indexed implementation accepted a graph with a cycle")
	}
	// The generic implementation must still terminate.
	g.TransitiveReduction()
}

func TestAcyclicGraphAncestorLabelSets(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		n := 1 + rnd.Intn(60)
		density := rnd.Float64()
		labelCount := 1 + rnd.Intn(130)

		var g AcyclicGraph
		for v := 0; v < n; v++ {
			g.Add(testV(v))
		}
		for from := 0; from < n; from++ {
			for to := from + 1; to < n; to++ {
				if rnd.Float64() < density {
					g.Connect(testV(from), testV(to))
				}
			}
		}
		label := func(v Vertex) int {
			if v.(testV)%7 == 0 {
				return -1
			}
			return int(v.(testV)) % labelCount
		}
		want := func(v Vertex) bool {
			return v.(testV)%3 != 0
		}

		visited := 0
		ok := g.AncestorLabelSets(labelCount, label, want, func(v Vertex, got LabelSet) {
			visited++
			if !want(v) {
				t.Fatalf("graph %d: visited unwanted vertex %v", i, v)
			}
			wantLabels := make(map[int]bool)
			for a := range g.Ancestors(v).All() {
				if l := label(a); l >= 0 {
					wantLabels[l] = true
				}
			}
			for l := 0; l < labelCount; l++ {
				if got.Has(l) != wantLabels[l] {
					t.Fatalf("graph %d: vertex %v: label %d presence is %t; want %t", i, v, l, got.Has(l), wantLabels[l])
				}
			}
		})
		if !ok {
			t.Fatalf("graph %d: rejected a valid DAG", i)
		}
		wantVisited := 0
		for v := range g.VerticesSeq() {
			if want(v) {
				wantVisited++
			}
		}
		if visited != wantVisited {
			t.Fatalf("graph %d: visited %d vertices; want %d", i, visited, wantVisited)
		}
	}
}

func TestGraphConnectAll(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		n := 1 + rnd.Intn(300)
		var batched, simple Graph
		for v := 0; v < n; v++ {
			batched.Add(testV(v))
			simple.Add(testV(v))
		}
		// Some edges that exist beforehand.
		for j := 0; j < n; j++ {
			from, to := testV(rnd.Intn(n)), testV(rnd.Intn(n))
			batched.Connect(from, to)
			simple.Connect(from, to)
		}

		var sources []Vertex
		var targets [][]Vertex
		for from := 0; from < n; from++ {
			if rnd.Intn(4) == 0 {
				continue
			}
			var ts []Vertex
			for j, count := 0, rnd.Intn(6); j < count; j++ {
				to := testV(rnd.Intn(n))
				ts = append(ts, to)
				simple.Connect(testV(from), to)
			}
			sources = append(sources, testV(from))
			targets = append(targets, ts)
		}
		batched.ConnectAll(sources, targets)

		if got, want := batched.String(), simple.String(); got != want {
			t.Fatalf("graph %d: wrong result\ngot:\n%s\nwant:\n%s", i, got, want)
		}
		for v := 0; v < n; v++ {
			got, want := batched.EdgesTo(testV(v)), simple.EdgesTo(testV(v))
			if got.Len() != want.Len() {
				t.Fatalf("graph %d: vertex %d has %d incoming edges; want %d", i, v, got.Len(), want.Len())
			}
			for from := range want.All() {
				if !got.Contains(from) {
					t.Fatalf("graph %d: vertex %d is missing its incoming edge from %v", i, v, from)
				}
			}
		}
	}
}
