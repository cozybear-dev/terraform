// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"log"

	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/dag"
)

// ModuleExpansionTransformer is a GraphTransformer that adds graph nodes
// representing the possible expansion of each module call in the configuration,
// and ensures that any nodes representing objects declared within a module
// are dependent on the expansion node so that they will be visited only
// after the module expansion has been decided.
//
// This transform must be applied only after all nodes representing objects
// that can be contained within modules have already been added.
type ModuleExpansionTransformer struct {
	Config *configs.Config

	// Concrete allows injection of a wrapped module node by the graph builder
	// to alter the evaluation behavior.
	Concrete ConcreteModuleNodeFunc

	closers map[string]*nodeCloseModule

	// byPath has the vertices that were in the graph before we added any,
	// other than those we never connect, grouped by the string
	// representation of the module they belong to. Finding the vertices of
	// each module by examining every vertex in the graph takes time in
	// proportion to the number of modules multiplied by the size of the
	// graph.
	byPath map[string][]dag.Vertex
}

func (t *ModuleExpansionTransformer) Transform(g *Graph) error {
	t.closers = make(map[string]*nodeCloseModule)
	t.byPath = make(map[string][]dag.Vertex)
	for v := range g.VerticesSeq() {
		switch v := v.(type) {
		case GraphNodeDestroyer:
			// skip destroyers, as they can only depend on other resources.
			continue
		case GraphNodeModulePath:
			key := v.ModulePath().String()
			t.byPath[key] = append(t.byPath[key], v)
		}
	}
	// The root module is always a singleton and so does not need expansion
	// processing, but any descendant modules do. We'll process them
	// recursively using t.transform.
	for _, cfg := range t.Config.Children {
		err := t.transform(g, cfg, nil)
		if err != nil {
			return err
		}
	}

	// Now go through and connect all nodes to their respective module closers.
	// This is done all at once here, because orphaned modules were already
	// handled by the RemovedModuleTransformer, and those module closers are in
	// the graph already, and need to be connected to their parent closers.
	for v := range g.VerticesSeq() {
		switch v.(type) {
		case GraphNodeDestroyer:
			// Destroy nodes can only be ordered relative to other resource
			// instances.
			continue
		case *nodeCloseModule:
			// a module closer cannot connect to itself
			continue
		}

		// any node that executes within the scope of a module should be a
		// GraphNodeModulePath
		pather, ok := v.(GraphNodeModulePath)
		if !ok {
			continue
		}
		if closer, ok := t.closers[pather.ModulePath().String()]; ok {
			// The module closer depends on each child resource instance, since
			// during apply the module expansion will complete before the
			// individual instances are applied.
			g.Connect(closer, v)
		}
	}

	// Modules implicitly depend on their child modules, so connect closers to
	// other which contain their path.
	for _, d := range t.closers {
		for prefix := 1; prefix < len(d.Addr); prefix++ {
			if c, ok := t.closers[d.Addr[:prefix].String()]; ok {
				g.Connect(c, d)
			}
		}
	}

	return nil
}

func (t *ModuleExpansionTransformer) transform(g *Graph, c *configs.Config, parentNode dag.Vertex) error {
	_, call := c.Path.Call()
	modCall := c.Parent.Module.ModuleCalls[call.Name]

	n := &nodeExpandModule{
		Addr:       c.Path,
		Config:     c.Module,
		ModuleCall: modCall,
		ModuleTree: c,
	}
	var expander dag.Vertex = n
	if t.Concrete != nil {
		expander = t.Concrete(n)
	}

	g.Add(expander)
	log.Printf("[TRACE] ModuleExpansionTransformer: Added %s as %T", c.Path, expander)

	if parentNode != nil {
		log.Printf("[TRACE] ModuleExpansionTransformer: %s must wait for expansion of %s", expander.Name(), parentNode.Name())
		g.Connect(expander, parentNode)
	}

	// Add the closer (which acts as the root module node) to provide a
	// single exit point for the expanded module.
	closer := &nodeCloseModule{
		Addr: c.Path,
	}
	g.Add(closer)
	g.Connect(closer, expander)
	t.closers[c.Path.String()] = closer

	// Everything that we add to the graph ourselves belongs to a different
	// module, apart from this module's own expander and closer, which are
	// already connected, so the vertices that were there to start with are
	// all of the ones to connect.
	for _, childV := range t.byPath[c.Path.String()] {
		// don't connect a node to itself
		if childV == expander {
			continue
		}
		log.Printf("[TRACE] ModuleExpansionTransformer: %s must wait for expansion of %s", childV.Name(), c.Path)
		g.Connect(childV, expander)
	}

	// Also visit child modules, recursively.
	for _, cc := range c.Children {
		if err := t.transform(g, cc, expander); err != nil {
			return err
		}
	}

	return nil
}
