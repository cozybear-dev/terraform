// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"log"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/states"
)

// GraphNodeAttachResourceState is an interface that can be implemented
// to request that a ResourceState is attached to the node.
//
// Due to a historical naming inconsistency, the type ResourceState actually
// represents the state for a particular _instance_, while InstanceState
// represents the values for that instance during a particular phase
// (e.g. primary vs. deposed). Consequently, GraphNodeAttachResourceState
// is supported only for nodes that represent resource instances, even though
// the name might suggest it is for containing resources.
type GraphNodeAttachResourceState interface {
	GraphNodeResourceInstance

	// Sets the state
	AttachResourceState(*states.Resource)
}

// AttachStateTransformer goes through the graph and attaches
// state to nodes that implement the interfaces above.
type AttachStateTransformer struct {
	State *states.State // State is the root state
}

func (t *AttachStateTransformer) Transform(g *Graph) error {
	// If no state, then nothing to do
	if t.State == nil {
		log.Printf("[DEBUG] Not attaching any node states: overall state is nil")
		return nil
	}

	for v := range g.VerticesSeq() {
		// Nodes implement this interface to request state attachment.
		an, ok := v.(GraphNodeAttachResourceState)
		if !ok {
			continue
		}
		addr := an.ResourceInstanceAddr()

		rs := t.State.Resource(addr.ContainingResource())
		if rs == nil {
			log.Printf("[DEBUG] Resource state not found for node %q, instance %s", v.Name(), addr)
			continue
		}

		is := rs.Instance(addr.Resource.Key)
		if is == nil {
			// We don't actually need this here, since we'll attach the whole
			// resource state, but we still check because it'd be weird
			// for the specific instance we're attaching to not to exist.
			log.Printf("[DEBUG] Resource instance state not found for node %q, instance %s", v.Name(), addr)
			continue
		}

		// make sure to attach a copy of the state, so instances can modify the
		// same ResourceState.
		//
		// A node only needs the state of its own instance along with the
		// resource-level metadata. Copying all of the other instances of
		// the resource for every node would make the total cost grow with
		// the square of the number of instances.
		an.AttachResourceState(&states.Resource{
			Addr:           rs.Addr,
			ProviderConfig: rs.ProviderConfig,
			Instances: map[addrs.InstanceKey]*states.ResourceInstance{
				addr.Resource.Key: is.DeepCopy(),
			},
		})
	}

	return nil
}
