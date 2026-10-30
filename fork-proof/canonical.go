// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package main

// This file is the whole of what the proof disregards when it compares what
// two builds of Terraform produced. Everything else is compared exactly.
//
// A difference between upstream and the fork can only be hidden here, so
// this file is deliberately small, each rule says why it exists, and
// canonical_test.go pins down exactly what each rule does and doesn't
// touch. Adding a rule, or widening one, weakens the proof, and is a change
// for a person to review rather than for whatever is maintaining the fork
// to make.
//
// The rules are also tested from the other direction: every scenario is run
// twice with upstream, and the two runs must agree after canonicalisation
// (the "control"). A rule that's missing shows up as a control failure,
// which is reported as a problem with the proof and not with the fork.

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Rule 1 (state): the lineage.
//
// A lineage is a random UUID that Terraform generates when it first creates
// a state, so two runs never agree on it. Only the top-level property is
// replaced: it's matched by its indentation, which nothing nested has.
var stateLineage = regexp.MustCompile(`(?m)^  "lineage": "[0-9a-f-]*",$`)

// Rule 2 (state): the serial.
//
// The fork documents that it writes fewer intermediate snapshots of the
// state while applying, and the serial counts snapshots, so it's lower than
// upstream's (difference 2 in README-FORK.md). The proof separately requires
// that each build's serial never goes backwards, and increases whenever the
// state has changed. See checkSerials.
var stateSerial = regexp.MustCompile(`(?m)^  "serial": [0-9]+,$`)

// Rule 3 (state): the order of the check results.
//
// Upstream keeps the results of checks in maps, and writes them, and the
// objects within each of them, in the order that iterating over those maps
// happens to give, which differs from one run of upstream to the next. They
// are sorted, not removed, so the same results must still all be there.
//
// The results are the last property of a state, so only what follows this
// is parsed and written again. A JSON string can't contain a newline, so
// this can't occur anywhere else.
var stateCheckResults = []byte("\n  \"check_results\": ")

// Rule 4 (state, plan and command output): the keys of deposed objects.
//
// When an object is replaced by creating its successor first, Terraform
// keeps the old one in the state under a key of eight random hexadecimal
// digits until it has been destroyed, so two runs never agree on the key.
// It's replaced wherever Terraform writes it: as a property of an instance
// in a state (which is the only thing at that indentation), in the phrase
// that names a deposed object in what's printed, and in the two properties
// that carry it in the JSON description of a plan or a state. Which of
// several deposed objects of one instance is which isn't compared as a
// result; that there are as many, with the same content, still is.
var (
	stateDeposedKey  = regexp.MustCompile(`(?m)^          "deposed": "[0-9a-f]{8}",$`)
	outputDeposedKey = regexp.MustCompile(`\(deposed object [0-9a-f]{8}\)`)
	jsonDeposedKey   = regexp.MustCompile(`"(deposed|deposed_key)":"[0-9a-f]{8}"`)
	deposedKey       = regexp.MustCompile(`^[0-9a-f]{8}$`)
)

// canonicalPlanText applies rule 4 to the human-readable description of a
// plan, which is otherwise compared exactly as it was printed.
func canonicalPlanText(src []byte) []byte {
	return outputDeposedKey.ReplaceAll(src, []byte("(deposed object <deposed>)"))
}

// canonicalState applies rules 1 to 4 to the bytes of a state file. Apart
// from the check results, the result is byte for byte what was written,
// including its formatting, so a state that is equivalent but written
// differently is still a difference.
func canonicalState(src []byte) []byte {
	src = stateLineage.ReplaceAll(src, []byte(`  "lineage": "<lineage>",`))
	src = stateSerial.ReplaceAll(src, []byte(`  "serial": <serial>,`))
	src = stateDeposedKey.ReplaceAll(src, []byte(`          "deposed": "<deposed>",`))

	at := bytes.LastIndex(src, stateCheckResults)
	end := bytes.LastIndex(src, []byte("\n}"))
	if at < 0 || end < at {
		return src
	}
	start := at + len(stateCheckResults)
	dec := json.NewDecoder(bytes.NewReader(src[start:end]))
	dec.UseNumber()
	var results []any
	if err := dec.Decode(&results); err != nil || len(results) == 0 || dec.More() {
		// Nothing to sort, or not what Terraform writes, in which case it's
		// compared as it is.
		return src
	}
	for _, result := range results {
		if result, ok := result.(map[string]any); ok {
			if objects, ok := result["objects"].([]any); ok {
				sortByJSON(objects)
			}
		}
	}
	sortByJSON(results)
	sorted, err := json.MarshalIndent(results, "  ", "  ")
	if err != nil {
		return src
	}
	out := make([]byte, 0, len(src))
	out = append(out, src[:start]...)
	out = append(out, sorted...)
	return append(out, src[end:]...)
}

// canonicalPlanJSON canonicalises the output of "terraform show -json" for a
// plan file, and re-serializes it with sorted keys so that two documents are
// equal exactly when their canonical forms are the same bytes.
func canonicalPlanJSON(src []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	// Numbers are kept as they were written rather than converted to
	// floats, so that a number written differently is still a difference.
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}

	// Rule 5 (plan): the timestamp.
	//
	// This is the wall-clock time at which the plan was made.
	if _, ok := doc["timestamp"]; ok {
		doc["timestamp"] = "<timestamp>"
	}

	// Rule 6 (plan): the order of relevant_attributes.
	//
	// Upstream collects these in a map and emits them in the order that
	// iterating over it happens to give, which differs from run to run.
	// They are sorted, not removed, so the set must still be the same.
	if attrs, ok := doc["relevant_attributes"].([]any); ok {
		sortByJSON(attrs)
	}

	// Rule 7 (plan): the order of the modules in planned_values and in
	// prior_state.
	//
	// Upstream lists the child modules at each level in an order that
	// differs from run to run. They are sorted by address at every level,
	// and nothing inside a module is reordered.
	for _, path := range [][]string{{"planned_values", "root_module"}, {"prior_state", "values", "root_module"}} {
		var node any = doc
		for _, key := range path {
			m, _ := node.(map[string]any)
			node = m[key]
		}
		sortChildModules(node)
	}

	// Rule 4, in the two places a plan's description has the key of a
	// deposed object: on a change, and on a resource in a set of values.
	for _, key := range []string{"resource_changes", "resource_drift"} {
		changes, _ := doc[key].([]any)
		for _, change := range changes {
			replaceDeposedKey(change, "deposed")
		}
	}
	for _, path := range [][]string{{"planned_values", "root_module"}, {"prior_state", "values", "root_module"}} {
		var node any = doc
		for _, key := range path {
			m, _ := node.(map[string]any)
			node = m[key]
		}
		replaceDeposedKeysInModule(node)
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func replaceDeposedKey(node any, property string) {
	if m, ok := node.(map[string]any); ok {
		if key, ok := m[property].(string); ok && deposedKey.MatchString(key) {
			m[property] = "<deposed>"
		}
	}
}

func replaceDeposedKeysInModule(node any) {
	module, ok := node.(map[string]any)
	if !ok {
		return
	}
	resources, _ := module["resources"].([]any)
	for _, resource := range resources {
		replaceDeposedKey(resource, "deposed_key")
	}
	children, _ := module["child_modules"].([]any)
	for _, child := range children {
		replaceDeposedKeysInModule(child)
	}
}

func sortChildModules(node any) {
	module, ok := node.(map[string]any)
	if !ok {
		return
	}
	children, ok := module["child_modules"].([]any)
	if !ok {
		return
	}
	for _, child := range children {
		sortChildModules(child)
	}
	sort.SliceStable(children, func(i, j int) bool {
		a, _ := children[i].(map[string]any)
		b, _ := children[j].(map[string]any)
		aAddr, _ := a["address"].(string)
		bAddr, _ := b["address"].(string)
		return aAddr < bAddr
	})
}

// sortByJSON sorts the given values by their JSON serializations.
func sortByJSON(items []any) {
	sort.SliceStable(items, func(i, j int) bool {
		a, _ := json.Marshal(items[i])
		b, _ := json.Marshal(items[j])
		return string(a) < string(b)
	})
}

// Rule 8 (command output): how long something took.
//
// The output of plan and apply reports elapsed times, and repeats a
// progress line every ten seconds for anything still in progress.
//
// It also says that it's acquiring or releasing the lock on the state, but
// only if doing so has taken more than a few seconds, which with a state of
// tens of megabytes it does for upstream and doesn't for the fork.
var (
	elapsedAfter   = regexp.MustCompile(` after [0-9]+(m[0-9]+)?s`)
	elapsedBracket = regexp.MustCompile(`\[[0-9]+(m[0-9]+)?s elapsed\]`)
	stillWaiting   = regexp.MustCompile(`: Still (creating|modifying|destroying|reading|applying)\.\.\.`)
	slowLock       = regexp.MustCompile(`^(Acquiring|Releasing) state lock\. This may take a few moments\.\.\.$`)
)

// canonicalOutput canonicalises what a plan or apply command printed.
//
// Rule 9 (command output): the order of the lines.
//
// Terraform works on several resources at once, so the lines that report
// progress on each of them, and the diagnostics for each of them, appear in
// an order that differs from run to run with either build. The lines are
// sorted, so what's compared is that the same lines were printed the same
// number of times. The plan itself is compared exactly, in order, from the
// output of "terraform show", which isn't subject to this.
func canonicalOutput(src []byte) []byte {
	var lines []string
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimRight(line, " \r")
		if line == "" || stillWaiting.MatchString(line) || slowLock.MatchString(line) {
			continue
		}
		line = elapsedAfter.ReplaceAllString(line, " after <elapsed>")
		line = elapsedBracket.ReplaceAllString(line, "[<elapsed> elapsed]")
		line = outputDeposedKey.ReplaceAllString(line, "(deposed object <deposed>)")
		line = jsonDeposedKey.ReplaceAllString(line, `"$1":"<deposed>"`)
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return []byte(strings.Join(lines, "\n") + "\n")
}

// canonicalOpLog canonicalises the provider's record of the requests it
// received during one step.
//
// Rule 10 (provider requests): their order.
//
// For the same reason as rule 9, the requests arrive in an order that
// differs from run to run. The sequence numbers are dropped and the lines
// sorted, so what's compared is that the provider received the same
// requests, with the same values, the same number of times. The order that
// matters, that of the changes to resources which depend on one another, is
// checked separately against what each scenario declares. See checkOrder.
func canonicalOpLog(src []byte) []byte {
	var lines []string
	for _, line := range strings.Split(string(src), "\n") {
		if _, rest, ok := strings.Cut(line, " "); ok {
			lines = append(lines, rest)
		}
	}
	sort.Strings(lines)
	return []byte(strings.Join(lines, "\n") + "\n")
}
