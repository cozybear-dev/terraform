// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package version

// ForkBuild says which build of the large-state fork this is, for the
// "terraform version" command to print on a line of its own. It is set with
// linker flags when an image of the fork is built, and is empty otherwise,
// in which case nothing is printed.
//
// It has no part in the version number, which is upstream's and is what's
// recorded in state snapshots and plan files and compared with the version
// constraints in configuration.
var ForkBuild string
