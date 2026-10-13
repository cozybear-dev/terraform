// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package dag

import (
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"testing"

	"github.com/hashicorp/terraform/internal/tfdiags"
)

// TestWalker_random walks random graphs in which some vertices fail, and
// checks that every vertex is visited only after everything it depends on,
// and that the ones that aren't visited are exactly those downstream of a
// failure.
func TestWalker_random(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for round := range 200 {
		n := 1 + rnd.Intn(60)
		var g AcyclicGraph
		for i := range n {
			g.Add(testV(i))
		}
		deps := make([][]int, n)
		for i := range n {
			for j := range i {
				if rnd.Intn(8) == 0 {
					g.Connect(testV(i), testV(j))
					deps[i] = append(deps[i], j)
				}
			}
		}
		fails := make([]bool, n)
		for i := range n {
			fails[i] = rnd.Intn(15) == 0
		}

		// Vertex i depends only on lower-numbered vertices, so we can
		// work out what should happen in a single pass.
		wantVisit := make([]bool, n)
		errored := make([]bool, n)
		wantErrs := 0
		for i := range n {
			wantVisit[i] = true
			for _, d := range deps[i] {
				if errored[d] {
					wantVisit[i] = false
				}
			}
			errored[i] = !wantVisit[i] || fails[i]
			if wantVisit[i] && fails[i] {
				wantErrs++
			}
		}

		var l sync.Mutex
		visited := make([]bool, n)
		var problems []string
		w := NewWalker(func(v Vertex) tfdiags.Diagnostics {
			i := int(v.(testV))
			l.Lock()
			defer l.Unlock()
			if visited[i] {
				problems = append(problems, fmt.Sprintf("%d visited twice", i))
			}
			for _, d := range deps[i] {
				if !visited[d] {
					problems = append(problems, fmt.Sprintf("%d visited before %d", i, d))
				}
			}
			visited[i] = true
			var diags tfdiags.Diagnostics
			if fails[i] {
				diags = diags.Append(fmt.Errorf("error from %d", i))
			}
			return diags
		})
		diags := w.Walk(&g)

		for _, problem := range problems {
			t.Errorf("round %d: %s", round, problem)
		}
		if !reflect.DeepEqual(visited, wantVisit) {
			t.Errorf("round %d: wrong vertices visited\ngot:  %v\nwant: %v", round, visited, wantVisit)
		}
		if len(diags) != wantErrs {
			t.Errorf("round %d: got %d diagnostics, want %d: %s", round, len(diags), wantErrs, diags.ErrWithWarnings())
		}
	}
}
