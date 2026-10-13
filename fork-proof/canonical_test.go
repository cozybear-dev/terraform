// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package main

import (
	"strings"
	"testing"
)

// These tests pin down what each rule in canonical.go disregards and, more
// importantly, what it doesn't. A change that makes any rule disregard more
// must fail here.

func TestCanonicalState(t *testing.T) {
	state := `{
  "version": 4,
  "terraform_version": "1.18.0",
  "serial": 7,
  "lineage": "5f6c2f6e-0f0e-4d6b-9d1c-2a1e3b4c5d6e",
  "outputs": {
    "serial": {
      "value": {
        "serial": 99,
        "lineage": "not-the-lineage"
      },
      "type": "string"
    }
  },
  "resources": [],
  "check_results": null
}
`
	got := string(canonicalState([]byte(state)))
	for _, want := range []string{`"serial": <serial>,`, `"lineage": "<lineage>",`, `"serial": 99,`, `"lineage": "not-the-lineage"`, `"terraform_version": "1.18.0"`} {
		if !strings.Contains(got, want) {
			t.Errorf("canonical state doesn't have %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"serial": 7`) || strings.Contains(got, "5f6c2f6e") {
		t.Errorf("the serial or the lineage survived:\n%s", got)
	}

	// Everything else is significant, down to the formatting.
	for name, other := range map[string]string{
		"a value":            strings.Replace(state, `"value": {`, `"value":  {`, 1),
		"the version":        strings.Replace(state, `"1.18.0"`, `"1.18.1"`, 1),
		"a nested serial":    strings.Replace(state, `"serial": 99`, `"serial": 98`, 1),
		"a nested lineage":   strings.Replace(state, "not-the-lineage", "another", 1),
		"the resources":      strings.Replace(state, `"resources": []`, `"resources": [{}]`, 1),
		"a trailing newline": state + "\n",
		"the check results":  strings.Replace(state, `"check_results": null`, `"check_results": []`, 1),
	} {
		if string(canonicalState([]byte(other))) == got {
			t.Errorf("a difference in %s was disregarded", name)
		}
	}
	// A lineage that isn't where Terraform writes it isn't one.
	indented := strings.Replace(state, `  "lineage": "5f6c`, `    "lineage": "5f6c`, 1)
	if !strings.Contains(string(canonicalState([]byte(indented))), "5f6c2f6e") {
		t.Errorf("a lineage at another level of nesting was disregarded")
	}
}

func TestCanonicalStateCheckResults(t *testing.T) {
	state := func(results string) string {
		return "{\n  \"version\": 4,\n  \"resources\": [],\n  \"check_results\": " + results + "\n}\n"
	}
	a := state(`[
    {
      "object_kind": "resource",
      "config_addr": "fp_thing.a",
      "status": "pass",
      "objects": [
        {
          "object_addr": "fp_thing.a[1]",
          "status": "pass"
        },
        {
          "object_addr": "fp_thing.a[0]",
          "status": "fail",
          "failure_messages": [
            "second",
            "first"
          ]
        }
      ]
    },
    {
      "object_kind": "check",
      "config_addr": "check.x",
      "status": "pass",
      "objects": null
    }
  ]`)
	b := state(`[
    {
      "object_kind": "check",
      "config_addr": "check.x",
      "status": "pass",
      "objects": null
    },
    {
      "object_kind": "resource",
      "config_addr": "fp_thing.a",
      "status": "pass",
      "objects": [
        {
          "object_addr": "fp_thing.a[0]",
          "status": "fail",
          "failure_messages": [
            "second",
            "first"
          ]
        },
        {
          "object_addr": "fp_thing.a[1]",
          "status": "pass"
        }
      ]
    }
  ]`)
	want := string(canonicalState([]byte(a)))
	if got := string(canonicalState([]byte(b))); got != want {
		t.Errorf("the order of the check results wasn't disregarded:\n%s\n%s", want, got)
	}
	for name, other := range map[string]string{
		"a status":                    strings.Replace(a, `"status": "fail"`, `"status": "pass"`, 1),
		"an address":                  strings.Replace(a, `fp_thing.a[1]`, `fp_thing.a[2]`, 1),
		"the order of the messages":   strings.Replace(strings.Replace(a, `"second"`, `"FIRST"`, 1), `"first"`, `"second"`, 1),
		"a missing result":            state(`[]`),
		"no results at all":           state(`null`),
		"something after the results": strings.Replace(a, "\n}\n", ",\n  \"extra\": 1\n}\n", 1),
	} {
		if string(canonicalState([]byte(other))) == want {
			t.Errorf("a difference in %s was disregarded", name)
		}
	}
	// Nothing before the check results is touched.
	spaced := strings.Replace(a, `"resources": []`, `"resources":  []`, 1)
	if string(canonicalState([]byte(spaced))) == want {
		t.Errorf("a difference in the formatting of the resources was disregarded")
	}
}

func TestCanonicalDeposedKeys(t *testing.T) {
	state := func(key, attr string) string {
		return "{\n  \"resources\": [\n    {\n      \"instances\": [\n        {\n          \"deposed\": \"" + key + "\",\n          \"attributes\": {\n            \"deposed\": \"" + attr + "\",\n            \"id\": \"x\"\n          }\n        }\n      ]\n    }\n  ],\n  \"check_results\": null\n}\n"
	}
	want := string(canonicalState([]byte(state("0a1b2c3d", "0a1b2c3d"))))
	if got := string(canonicalState([]byte(state("ffffffff", "0a1b2c3d")))); got != want {
		t.Errorf("the key of a deposed object wasn't disregarded in a state")
	}
	if got := string(canonicalState([]byte(state("0a1b2c3d", "ffffffff")))); got == want {
		t.Errorf("an attribute that happens to be called deposed was disregarded")
	}
	if got := string(canonicalState([]byte(state("not-a-key", "0a1b2c3d")))); got == want {
		t.Errorf("something that isn't a deposed key was disregarded")
	}

	text := "  # fp_thing.a (deposed object 0a1b2c3d) will be destroyed\n  # (left over from a partially-failed replacement of this instance)\n"
	if string(canonicalPlanText([]byte(text))) != string(canonicalPlanText([]byte(strings.Replace(text, "0a1b2c3d", "99999999", 1)))) {
		t.Errorf("the key of a deposed object wasn't disregarded in the text of a plan")
	}
	if string(canonicalPlanText([]byte(text))) == string(canonicalPlanText([]byte(strings.Replace(text, "fp_thing.a", "fp_thing.b", 1)))) {
		t.Errorf("the address of a deposed object was disregarded")
	}
	if string(canonicalPlanText([]byte("id = 0a1b2c3d"))) != "id = 0a1b2c3d" {
		t.Errorf("eight hexadecimal digits that aren't a deposed key were disregarded")
	}

	plan := func(key, attr string) string {
		return `{"resource_changes":[{"address":"fp_thing.a","deposed":"` + key + `","change":{"before":{"deposed":"` + attr + `"}}}],
		"prior_state":{"values":{"root_module":{"resources":[{"address":"fp_thing.a","deposed_key":"` + key + `","values":{"deposed_key":"` + attr + `"}}],
		"child_modules":[{"address":"module.m","resources":[{"address":"module.m.fp_thing.b","deposed_key":"` + key + `"}]}]}}}}`
	}
	wantJSON, err := canonicalPlanJSON([]byte(plan("0a1b2c3d", "0a1b2c3d")))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := canonicalPlanJSON([]byte(plan("ffffffff", "0a1b2c3d"))); string(got) != string(wantJSON) {
		t.Errorf("the key of a deposed object wasn't disregarded in the description of a plan:\n%s\n%s", wantJSON, got)
	}
	if got, _ := canonicalPlanJSON([]byte(plan("0a1b2c3d", "ffffffff"))); string(got) == string(wantJSON) {
		t.Errorf("a value that happens to be under a property called deposed was disregarded")
	}
}

func TestCanonicalPlanJSON(t *testing.T) {
	plan := `{"format_version":"1.2","timestamp":"2026-10-13T12:00:00Z","applyable":true,
	"relevant_attributes":[{"resource":"b","attribute":["x"]},{"resource":"a","attribute":["y"]}],
	"planned_values":{"root_module":{"resources":[{"address":"r2"},{"address":"r1"}],"child_modules":[
		{"address":"module.b","child_modules":[{"address":"module.b.module.z"},{"address":"module.b.module.a"}]},
		{"address":"module.a","resources":[{"address":"module.a.x","values":{"n":1.50}}]}]}},
	"resource_changes":[{"address":"r2"},{"address":"r1"}]}`
	reordered := `{"timestamp":"1999-01-01T00:00:00Z","format_version":"1.2","applyable":true,
	"relevant_attributes":[{"attribute":["y"],"resource":"a"},{"resource":"b","attribute":["x"]}],
	"planned_values":{"root_module":{"resources":[{"address":"r2"},{"address":"r1"}],"child_modules":[
		{"address":"module.a","resources":[{"address":"module.a.x","values":{"n":1.50}}]},
		{"address":"module.b","child_modules":[{"address":"module.b.module.a"},{"address":"module.b.module.z"}]}]}},
	"resource_changes":[{"address":"r2"},{"address":"r1"}]}`
	want, err := canonicalPlanJSON([]byte(plan))
	if err != nil {
		t.Fatal(err)
	}
	got, err := canonicalPlanJSON([]byte(reordered))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("the timestamp, the order of relevant attributes or the order of modules wasn't disregarded:\n%s\n%s", want, got)
	}

	for name, other := range map[string]string{
		"the order of resources in a module": strings.Replace(plan, `[{"address":"r2"},{"address":"r1"}],"child_modules"`, `[{"address":"r1"},{"address":"r2"}],"child_modules"`, 1),
		"the order of resource changes":      strings.Replace(plan, `"resource_changes":[{"address":"r2"},{"address":"r1"}]`, `"resource_changes":[{"address":"r1"},{"address":"r2"}]`, 1),
		"how a number is written":            strings.Replace(plan, `1.50`, `1.5`, 1),
		"a relevant attribute":               strings.Replace(plan, `"attribute":["x"]`, `"attribute":["z"]`, 1),
		"a missing relevant attribute":       strings.Replace(plan, `{"resource":"b","attribute":["x"]},`, ``, 1),
		"a module address":                   strings.Replace(plan, `module.b.module.z`, `module.b.module.y`, 1),
		"whether it can be applied":          strings.Replace(plan, `"applyable":true`, `"applyable":false`, 1),
		"a missing timestamp":                strings.Replace(plan, `"timestamp":"2026-10-13T12:00:00Z",`, ``, 1),
	} {
		got, err := canonicalPlanJSON([]byte(other))
		if err != nil {
			t.Fatalf("%s: %s", name, err)
		}
		if string(got) == string(want) {
			t.Errorf("a difference in %s was disregarded", name)
		}
	}
}

func TestCanonicalOutput(t *testing.T) {
	a := "fp_thing.b: Creating...\nfp_thing.a: Creating...\nfp_thing.a: Still creating... [10s elapsed]\nfp_thing.a: Creation complete after 12s [id=id-a]\nfp_thing.b: Creation complete after 1m3s [id=id-b]\n\nApply complete! Resources: 2 added, 0 changed, 0 destroyed.\n"
	b := "fp_thing.a: Creating...\nfp_thing.b: Creating...\nfp_thing.b: Creation complete after 0s [id=id-b]\nfp_thing.a: Creation complete after 0s [id=id-a]\n\nApply complete! Resources: 2 added, 0 changed, 0 destroyed.\n"
	if string(canonicalOutput([]byte(a))) != string(canonicalOutput([]byte(b))) {
		t.Errorf("the order of the lines or the elapsed times weren't disregarded:\n%s\n%s", canonicalOutput([]byte(a)), canonicalOutput([]byte(b)))
	}
	slow := "Acquiring state lock. This may take a few moments...\n" + b + "Releasing state lock. This may take a few moments...\n"
	if string(canonicalOutput([]byte(slow))) != string(canonicalOutput([]byte(b))) {
		t.Errorf("the messages about a slow lock weren't disregarded")
	}
	for name, other := range map[string]string{
		"a line printed twice":    b + "fp_thing.a: Creating...\n",
		"a missing line":          strings.Replace(b, "fp_thing.b: Creating...\n", "", 1),
		"an id":                   strings.Replace(b, "[id=id-a]", "[id=id-x]", 1),
		"the count of resources":  strings.Replace(b, "2 added", "3 added", 1),
		"an error":                b + "Error: something failed\n",
		"an error about the lock": b + "Error: Error acquiring the state lock\n",
		"a word after the timing": strings.Replace(b, "complete after 0s", "failed after 0s", 1),
	} {
		if string(canonicalOutput([]byte(other))) == string(canonicalOutput([]byte(b))) {
			t.Errorf("a difference in %s was disregarded", name)
		}
	}
}

func TestCanonicalOpLog(t *testing.T) {
	a := "000001 call plan fp_thing \"a\" 1111\n000002 begin create fp_thing \"a\" 2222\n000003 end create fp_thing \"a\" 2222\n000004 call plan fp_thing \"b\" 3333\n"
	b := "000001 call plan fp_thing \"b\" 3333\n000001 call plan fp_thing \"a\" 1111\n000002 begin create fp_thing \"a\" 2222\n000009 end create fp_thing \"a\" 2222\n"
	if string(canonicalOpLog([]byte(a))) != string(canonicalOpLog([]byte(b))) {
		t.Errorf("the order of the requests or their sequence numbers weren't disregarded")
	}
	for name, other := range map[string]string{
		"a request made twice":    a + "000005 call plan fp_thing \"b\" 3333\n",
		"a missing request":       strings.Replace(a, "000004 call plan fp_thing \"b\" 3333\n", "", 1),
		"the values in a request": strings.Replace(a, "3333", "4444", 1),
		"the kind of request":     strings.Replace(a, "begin create", "begin delete", 1),
		"the resource":            strings.Replace(a, "\"b\"", "\"c\"", 1),
	} {
		if string(canonicalOpLog([]byte(other))) == string(canonicalOpLog([]byte(a))) {
			t.Errorf("a difference in %s was disregarded", name)
		}
	}
}

func TestCheckOrder(t *testing.T) {
	log := []byte(`000001 begin create fp_thing "a" x
000002 end create fp_thing "a" x
000003 begin create fp_thing "b" y
000004 begin delete fp_thing "a" z
000005 end create fp_thing "b" y
000006 end delete fp_thing "a" z
`)
	for _, test := range []struct {
		pair [2]string
		ok   bool
	}{
		{[2]string{"create:a", "b"}, true},
		{[2]string{"b", "create:a"}, false},
		{[2]string{"create:b", "delete:a"}, false}, // they overlap
		{[2]string{"create:a", "delete:a"}, true},
		{[2]string{"a", "b"}, false}, // the last change to a ends after b begins
		{[2]string{"a", "missing"}, false},
		{[2]string{"missing", "a"}, false},
	} {
		problem := checkOrder(log, [][2]string{test.pair})
		if (problem == "") != test.ok {
			t.Errorf("%v: got %q, want ok=%v", test.pair, problem, test.ok)
		}
	}
}
