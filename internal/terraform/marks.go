// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"github.com/hashicorp/terraform/internal/lang/marks"
	"github.com/zclconf/go-cty/cty"
)

// valueMarksEqual compares the marks of 2 cty.Values for equality.
func valueMarksEqual(a, b cty.Value) bool {
	_, aMarks := marks.UnmarkDeepWithPaths(a)
	_, bMarks := marks.UnmarkDeepWithPaths(b)
	return marks.MarksEqual(aMarks, bMarks)
}
