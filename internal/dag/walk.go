// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package dag

import (
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/terraform/internal/logging"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

// Walker is used to walk every vertex of a graph in parallel.
//
// A vertex will only be walked when the dependencies of that vertex have
// been walked. If two vertices can be walked at the same time, they will be.
//
// Non-parallelism can be enforced by introducing a lock in your callback
// function.
//
// The walker starts a goroutine for a vertex only once all of its
// dependencies are complete, so the number of goroutines at any time is the
// number of vertices that are ready to be visited or are being visited,
// rather than a multiple of the number of vertices in the graph. That
// matters for a graph with hundreds of thousands of vertices, because each
// waiting goroutine has a stack that the garbage collector must scan each
// time it runs.
//
// A single walker is only valid for one graph walk. After the walk is complete
// you must construct a new walker to walk again.
type Walker struct {
	// Callback is what is called for each vertex
	Callback walkFunc

	started atomic.Bool

	// wait is done when all vertices have executed.
	wait sync.WaitGroup

	// vertices has the state of each vertex in the graph being walked.
	//
	// The dependencies of vertex i, which are the vertices that must
	// complete before it starts, are deps[depOffsets[i]:depOffsets[i+1]],
	// and the vertices that have it as a dependency are
	// waiters[waiterOffsets[i]:waiterOffsets[i+1]].
	vertices      []walkerVertex
	deps          []int32
	depOffsets    []int32
	waiters       []int32
	waiterOffsets []int32
}

// NewWalker creates a new walker with the given callback function.
func NewWalker(cb walkFunc, opts ...func(*Walker)) *Walker {
	w := &Walker{
		Callback: cb,
	}
	for _, opt := range opts {
		opt(w)
	}

	return w
}

type walkerVertex struct {
	vertex Vertex

	// pending is the number of dependencies that haven't completed yet.
	pending atomic.Int32

	// depFailed is set if any dependency completed with errors, which
	// includes having been skipped because one of its own dependencies
	// failed.
	depFailed atomic.Bool

	// done is set once the vertex has completed, regardless of success.
	done atomic.Bool

	// diags and upstreamFailed are the result of visiting the vertex. They
	// are written by the goroutine that visits it, and read only once the
	// whole walk is complete. Diagnostics from a vertex that was skipped
	// because of an upstream failure are excluded from the final set.
	diags          tfdiags.Diagnostics
	upstreamFailed bool
}

// Walk loads the graph and dispatches the concurrent walker. Walk can only be
// called once for a single Walker.
func (w *Walker) Walk(g *AcyclicGraph) tfdiags.Diagnostics {
	if g == nil {
		return nil
	}

	if w.started.Load() {
		panic("graph walker already started")
	}
	w.started.Store(true)

	n := g.vertices.Len()
	if n == 0 {
		return nil
	}
	w.vertices = make([]walkerVertex, n)
	index := make(map[Vertex]int32, n)
	for v := range g.vertices.All() {
		i := int32(len(index))
		index[v] = i
		w.vertices[i].vertex = v
	}

	// Connect each vertex to its dependencies, and each dependency to the
	// vertices that are waiting for it.
	w.depOffsets = make([]int32, n+1)
	waiterCounts := make([]int32, n+1)
	edgeCount := 0
	for i := range w.vertices {
		edgeCount += g.edgesFrom[w.vertices[i].vertex].Len()
	}
	w.deps = make([]int32, 0, edgeCount)
	for i := range w.vertices {
		for dep := range g.edgesFrom[w.vertices[i].vertex].All() {
			di, ok := index[dep]
			if !ok {
				// Vertex doesn't exist... shouldn't be possible but ignore.
				continue
			}
			w.deps = append(w.deps, di)
			waiterCounts[di+1]++
		}
		w.depOffsets[i+1] = int32(len(w.deps))
		w.vertices[i].pending.Store(w.depOffsets[i+1] - w.depOffsets[i])
	}
	w.waiterOffsets = waiterCounts
	for i := 0; i < n; i++ {
		w.waiterOffsets[i+1] += w.waiterOffsets[i]
	}
	w.waiters = make([]int32, len(w.deps))
	next := make([]int32, n)
	copy(next, w.waiterOffsets[:n])
	for i := range w.vertices {
		for _, di := range w.deps[w.depOffsets[i]:w.depOffsets[i+1]] {
			w.waiters[next[di]] = int32(i)
			next[di]++
		}
	}

	// Our walk is not done until every vertex finishes.
	w.wait.Add(n)

	// The vertices that have no dependencies are where we start. We find
	// all of them before starting any, because as soon as one is running
	// it can complete and change which of the others are ready.
	var ready []int32
	for i := range w.vertices {
		if w.vertices[i].pending.Load() == 0 {
			ready = append(ready, int32(i))
		}
	}

	stopReporting := w.reportWaiting()
	for _, i := range ready {
		go w.walkVertex(i)
	}

	// Wait for completion
	w.wait.Wait()
	stopReporting()

	var diags tfdiags.Diagnostics
	for i := range w.vertices {
		if w.vertices[i].upstreamFailed {
			// Ignore diagnostics for nodes that had failed upstreams, since
			// the downstream diagnostics are likely to be redundant.
			continue
		}
		diags = diags.Append(w.vertices[i].diags)
	}

	return diags
}

// walkVertex walks a single vertex, all of whose dependencies must already
// be complete, and then starts any vertices that were waiting only for it.
//
// It walks one of those itself rather than starting a goroutine for each of
// them, so a chain of vertices that each wait for the one before is walked
// by a single goroutine. Besides the cost of starting it, a new goroutine
// begins with a small stack that has to grow before it can evaluate
// anything, and growing many stacks at once is something the goroutines
// contend with each other for.
func (w *Walker) walkVertex(i int32) {
	for i >= 0 {
		i = w.walkOne(i)
	}
}

// walkOne walks a single vertex and starts all but one of the vertices that
// were waiting only for it. It returns the remaining one for the caller to
// walk, or -1 if there is none.
func (w *Walker) walkOne(i int32) (next int32) {
	// When we're done executing, lower the waitgroup count
	defer w.wait.Done()

	info := &w.vertices[i]
	v := info.vertex

	// A vertex that implements [AlwaysRunVertex] runs even if its
	// dependencies had errors.
	depsSuccess := !info.depFailed.Load()
	if !depsSuccess {
		if _, ok := v.(AlwaysRunVertex); ok {
			depsSuccess = true
		}
	}

	// Run our callback or note that our upstream failed
	var diags tfdiags.Diagnostics
	if depsSuccess {
		diags = w.Callback(v)
	} else {
		log.Printf("[TRACE] dag/walk: upstream of %q errored, so skipping", v.Name())
		// This won't be displayed to the user because we'll set upstreamFailed,
		// but we need to ensure there's at least one error in here so that
		// the failures will cascade downstream.
		diags = diags.Append(errors.New("upstream dependencies failed"))
		info.upstreamFailed = true
	}
	info.diags = diags
	failed := diags.HasErrors()
	info.done.Store(true)

	next = -1
	for _, wi := range w.waiters[w.waiterOffsets[i]:w.waiterOffsets[i+1]] {
		waiter := &w.vertices[wi]
		if failed {
			// This must happen before we count ourselves as complete,
			// because the waiter can start as soon as we do that.
			waiter.depFailed.Store(true)
		}
		if waiter.pending.Add(-1) == 0 {
			if next < 0 {
				next = wi
			} else {
				go w.walkVertex(wi)
			}
		}
	}
	return next
}

// reportWaiting periodically logs what each vertex that hasn't started yet
// is waiting for, if logging is enabled, until the returned function is
// called.
func (w *Walker) reportWaiting() (stop func()) {
	if !logging.IsEnabled() {
		return func() {}
	}

	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(time.Second * 5)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
			}
			for i := range w.vertices {
				if w.vertices[i].pending.Load() == 0 {
					continue
				}
				for _, di := range w.deps[w.depOffsets[i]:w.depOffsets[i+1]] {
					if !w.vertices[di].done.Load() {
						log.Printf("[TRACE] dag/walk: vertex %q is waiting for %q",
							w.vertices[i].vertex.Name(), w.vertices[di].vertex.Name())
						break
					}
				}
			}
		}
	}()
	return func() {
		close(stopCh)
		<-doneCh
	}
}
