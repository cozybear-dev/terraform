// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package local

import (
	"github.com/hashicorp/terraform/internal/states"
)

// stateLargeInstanceCount is the number of resource instances from which
// this package treats a state as large, and does what would be a waste of
// effort, or would change what Terraform does for no benefit, with a
// smaller one.
const stateLargeInstanceCount = 2000

// countResourceInstances returns the number of resource instances in the
// given state, or some number that is at least limit if there are that many.
func countResourceInstances(state *states.State, limit int) int {
	count := 0
	for _, ms := range state.Modules {
		for _, rs := range ms.Resources {
			count += len(rs.Instances)
			if count >= limit {
				return count
			}
		}
	}
	return count
}
