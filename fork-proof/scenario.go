// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// scenario is one entry of the corpus: a series of steps, each of which
// installs a configuration and runs an operation against the state that
// the previous steps left behind.
type scenario struct {
	Name        string `json:"-"`
	Description string `json:"description"`
	// Covers names the changes in the fork that this scenario is meant to
	// exercise, using the names in COVERAGE.md.
	Covers []string `json:"covers"`
	// InitialState, if set, is a state file to begin with.
	InitialState string `json:"initial_state,omitempty"`
	// ForkEnv is added to the environment of the fork only, for the
	// settings that only the fork has.
	ForkEnv map[string]string `json:"fork_env,omitempty"`
	// IgnoreProviderOps removes requests of these kinds from the record of
	// what the provider was asked to do, for the one documented case in
	// which the fork makes fewer of them. It must be justified in the
	// scenario's description.
	IgnoreProviderOps []string `json:"ignore_provider_ops,omitempty"`
	// Parallelism is passed to plan and apply as -parallelism, if set.
	Parallelism int `json:"parallelism,omitempty"`
	// Backend is "http" to keep the state in an HTTP server that the proof
	// runs for the purpose, rather than in a local file, which is how the
	// code for remote state is reached without a cloud service.
	Backend string `json:"backend,omitempty"`
	Steps   []step `json:"steps"`

	dir string
}

type step struct {
	Name string `json:"name"`
	// Config is the subdirectory of the scenario to install as the
	// configuration before this step. Empty means no change.
	Config string `json:"config,omitempty"`
	// Op is "apply" (plan to a file, show it, then apply that file), "plan"
	// (plan to a file and show it), "apply-direct" (apply without a saved
	// plan) or "cmd" (run Args as a terraform command).
	Op        string   `json:"op"`
	PlanArgs  []string `json:"plan_args,omitempty"`
	ApplyArgs []string `json:"apply_args,omitempty"`
	Args      []string `json:"args,omitempty"`
	// What upstream is expected to do, so that a scenario that has stopped
	// exercising what it was written for is noticed. The exit codes
	// default to zero.
	PlanRC  int `json:"plan_rc,omitempty"`
	ApplyRC int `json:"apply_rc,omitempty"`
	// Expect is text that must appear in what upstream printed.
	Expect []string `json:"expect,omitempty"`
	// Order lists pairs of resource names, as given to the provider, where
	// the change to the first must be complete before the change to the
	// second begins. A name can be prefixed with "create:", "apply:" or
	// "delete:" to say which change is meant.
	Order [][2]string `json:"order,omitempty"`
}

func loadScenarios(root string, filter *regexp.Regexp) ([]*scenario, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var ret []*scenario
	for _, entry := range entries {
		if !entry.IsDir() || (filter != nil && !filter.MatchString(entry.Name())) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		src, err := os.ReadFile(filepath.Join(dir, "scenario.json"))
		if err != nil {
			return nil, err
		}
		s := &scenario{Name: entry.Name(), dir: dir}
		dec := json.NewDecoder(bytes.NewReader(src))
		dec.DisallowUnknownFields()
		if err := dec.Decode(s); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if len(s.Steps) == 0 || s.Description == "" || len(s.Covers) == 0 {
			return nil, fmt.Errorf("%s: a scenario needs a description, what it covers, and at least one step", entry.Name())
		}
		for _, st := range s.Steps {
			switch st.Op {
			case "apply", "plan", "apply-direct", "cmd":
			default:
				return nil, fmt.Errorf("%s: step %q has unknown op %q", entry.Name(), st.Name, st.Op)
			}
		}
		ret = append(ret, s)
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i].Name < ret[j].Name })
	return ret, nil
}

// actor is one of the ways each scenario is run.
type actor struct {
	name string
	// planner makes and shows plans, and applier applies them. They differ
	// for the two actors that check a plan made by one build can be applied
	// by the other.
	planner, applier string
	// reader, if set, is the other build, which must give the same
	// description of a plan file that this actor's planner wrote.
	reader string
	fork   bool
	cover  bool
}

func (t *toolchain) actors() []actor {
	ret := []actor{
		{name: "upstream", planner: t.upstream, applier: t.upstream, reader: t.fork},
		{name: "upstream-again", planner: t.upstream, applier: t.upstream},
		{name: "fork", planner: t.fork, applier: t.fork, reader: t.upstream, fork: true},
		{name: "upstream-plans-fork-applies", planner: t.upstream, applier: t.fork, fork: true},
		{name: "fork-plans-upstream-applies", planner: t.fork, applier: t.upstream, fork: true},
	}
	if t.cover != "" {
		ret = append(ret, actor{name: "fork-coverage", planner: t.cover, applier: t.cover, fork: true, cover: true})
	}
	return ret
}

// stepResult is what one actor produced in one step: a set of named
// artefacts, already canonicalised, which are compared with upstream's.
type stepResult struct {
	artefacts map[string][]byte
	// serial is the serial of the state after the step, or -1.
	serial int
	// rawOpLog is the provider's record for the step, in order.
	rawOpLog []byte
	// errors are problems with running the step at all.
	errors []string
}

type runner struct {
	t    *toolchain
	sem  chan struct{}
	keep bool
}

func (r *runner) terraform(a actor, s *scenario, extraEnv []string, home, wd, oplog, bin string, args ...string) ([]byte, int) {
	env := []string{
		"HOME=" + home,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"TF_CLI_CONFIG_FILE=" + r.t.cliConfig,
		"TF_IN_AUTOMATION=1",
		"TF_INPUT=0",
		"CHECKPOINT_DISABLE=1",
		"TZ=UTC",
		"LC_ALL=C",
		"FP_OPLOG=" + oplog,
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	}
	env = append(env, extraEnv...)
	if bin == r.t.fork || bin == r.t.cover {
		keys := make([]string, 0, len(s.ForkEnv))
		for key := range s.ForkEnv {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			env = append(env, key+"="+s.ForkEnv[key])
		}
	}
	if bin == r.t.cover {
		env = append(env, "GOCOVERDIR="+filepath.Join(r.t.work, "coverage", "terraform"))
	}
	r.sem <- struct{}{}
	defer func() { <-r.sem }()
	out, rc, err := command(wd, env, bin, args...)
	if err != nil {
		return []byte(err.Error()), -1
	}
	return out, rc
}

// kept are the entries of a working directory that survive a change of
// configuration.
var kept = map[string]bool{
	"terraform.tfstate":        true,
	"terraform.tfstate.backup": true,
	".terraform":               true,
	".terraform.lock.hcl":      true,
}

func installConfig(wd, config string) error {
	entries, err := os.ReadDir(wd)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !kept[entry.Name()] {
			if err := os.RemoveAll(filepath.Join(wd, entry.Name())); err != nil {
				return err
			}
		}
	}
	return copyTree(config, wd)
}

func readOrAbsent(path string) []byte {
	src, err := os.ReadFile(path)
	if err != nil {
		return []byte("<absent>\n")
	}
	return src
}

var serialLine = regexp.MustCompile(`(?m)^  "serial": ([0-9]+),$`)

// runActor runs every step of a scenario as one actor.
func (r *runner) runActor(s *scenario, a actor) []*stepResult {
	base := filepath.Join(r.t.work, "run", s.Name, a.name)
	os.RemoveAll(base)
	wd, home, logs := filepath.Join(base, "wd"), filepath.Join(base, "home"), filepath.Join(base, "logs")
	for _, dir := range []string{wd, home, logs} {
		os.MkdirAll(dir, 0o755)
	}
	if s.InitialState != "" {
		src, err := os.ReadFile(filepath.Join(s.dir, s.InitialState))
		if err == nil {
			err = os.WriteFile(filepath.Join(wd, "terraform.tfstate"), src, 0o644)
		}
		if err != nil {
			return []*stepResult{{errors: []string{err.Error()}}}
		}
	}

	// Where the state is kept.
	var remote *stateServer
	var extraEnv []string
	if s.Backend == "http" {
		var err error
		if remote, err = newStateServer(); err != nil {
			return []*stepResult{{errors: []string{err.Error()}}}
		}
		defer remote.close()
		// The address is given through the environment and not in the
		// configuration, because each run has a server of its own and the
		// configuration is part of what's compared.
		extraEnv = remote.env()
	}

	parallelism := func(args []string) []string {
		if s.Parallelism > 0 {
			return append(args, "-parallelism="+strconv.Itoa(s.Parallelism))
		}
		return args
	}

	var results []*stepResult
	for i, st := range s.Steps {
		res := &stepResult{artefacts: make(map[string][]byte), serial: -1}
		results = append(results, res)
		oplog := filepath.Join(logs, fmt.Sprintf("%02d-oplog.txt", i))
		save := func(name string, raw []byte) {
			os.WriteFile(filepath.Join(logs, fmt.Sprintf("%02d-%s", i, name)), raw, 0o644)
		}
		run := func(bin string, args ...string) ([]byte, int) {
			return r.terraform(a, s, extraEnv, home, wd, oplog, bin, args...)
		}

		if st.Config != "" {
			if err := installConfig(wd, filepath.Join(s.dir, st.Config)); err != nil {
				res.errors = append(res.errors, err.Error())
				break
			}
			if remote != nil {
				if err := os.WriteFile(filepath.Join(wd, "zz_backend.tf"), []byte(stateServerConfig), 0o644); err != nil {
					res.errors = append(res.errors, err.Error())
					break
				}
			}
			out, rc := run(a.planner, "init", "-input=false", "-no-color")
			save("init.txt", out)
			if rc != 0 {
				res.errors = append(res.errors, fmt.Sprintf("init failed with exit status %d:\n%s", rc, tail(out, 15)))
				break
			}
		}

		planFile := filepath.Join(wd, "tfplan")
		os.Remove(planFile)
		switch st.Op {
		case "apply", "plan":
			args := append([]string{"plan", "-input=false", "-no-color", "-out=tfplan"}, parallelism(st.PlanArgs)...)
			out, rc := run(a.planner, args...)
			save("plan.txt", out)
			res.artefacts["plan.exit"] = []byte(strconv.Itoa(rc) + "\n")
			res.artefacts["plan.output"] = canonicalOutput(out)
			if _, err := os.Stat(planFile); err != nil {
				// There's no plan to show or apply.
				break
			}

			text, textRC := run(a.planner, "show", "-no-color", "tfplan")
			save("show.txt", text)
			res.artefacts["plan.text"] = canonicalPlanText(text)
			described, jsonRC := run(a.planner, "show", "-json", "tfplan")
			save("show.json", described)
			if textRC != 0 || jsonRC != 0 {
				res.errors = append(res.errors, fmt.Sprintf("showing the plan failed with exit status %d and %d", textRC, jsonRC))
			}
			if canonical, err := canonicalPlanJSON(described); err == nil {
				res.artefacts["plan.json"] = canonical
			} else {
				res.artefacts["plan.json"] = described
				res.errors = append(res.errors, "the plan's JSON description couldn't be parsed: "+err.Error())
			}
			if a.reader != "" {
				// The other build describes this plan file.
				described, rc := run(a.reader, "show", "-json", "tfplan")
				save("show-by-other.json", described)
				if canonical, err := canonicalPlanJSON(described); err == nil && rc == 0 {
					res.artefacts["plan.json as read by the other build"] = canonical
				} else {
					res.artefacts["plan.json as read by the other build"] = append([]byte(fmt.Sprintf("exit status %d\n", rc)), described...)
				}
			}
			for name, content := range planFileMembers(planFile) {
				res.artefacts["plan file member "+name] = content
			}

			if st.Op == "apply" {
				args := append([]string{"apply", "-input=false", "-no-color"}, parallelism(st.ApplyArgs)...)
				out, rc := run(a.applier, append(args, "tfplan")...)
				save("apply.txt", out)
				res.artefacts["apply.exit"] = []byte(strconv.Itoa(rc) + "\n")
				res.artefacts["apply.output"] = canonicalOutput(out)
			}
		case "apply-direct":
			args := append([]string{"apply", "-input=false", "-no-color", "-auto-approve"}, parallelism(st.PlanArgs)...)
			out, rc := run(a.applier, args...)
			save("apply.txt", out)
			res.artefacts["apply.exit"] = []byte(strconv.Itoa(rc) + "\n")
			res.artefacts["apply.output"] = canonicalOutput(out)
		case "cmd":
			out, rc := run(a.applier, st.Args...)
			save("cmd.txt", out)
			res.artefacts["command.exit"] = []byte(strconv.Itoa(rc) + "\n")
			res.artefacts["command.output"] = canonicalOutput(out)
		}

		state := readOrAbsent(filepath.Join(wd, "terraform.tfstate"))
		if remote != nil {
			var requests []byte
			state, requests = remote.snapshot()
			res.artefacts["requests to the state server"] = requests
		}
		save("state.tfstate", state)
		res.artefacts["state"] = canonicalState(state)
		res.artefacts["state backup"] = canonicalState(readOrAbsent(filepath.Join(wd, "terraform.tfstate.backup")))
		if m := serialLine.FindSubmatch(state); m != nil {
			res.serial, _ = strconv.Atoi(string(m[1]))
		}
		res.rawOpLog, _ = os.ReadFile(oplog)
		res.artefacts["provider requests"] = canonicalOpLog(filterOpLog(res.rawOpLog, s.IgnoreProviderOps))
	}
	if !r.keep {
		os.RemoveAll(wd)
		os.RemoveAll(home)
	}
	return results
}

func filterOpLog(src []byte, ignore []string) []byte {
	if len(ignore) == 0 {
		return src
	}
	var out bytes.Buffer
	for _, line := range strings.SplitAfter(string(src), "\n") {
		fields := strings.Fields(line)
		skip := false
		for _, op := range ignore {
			if len(fields) > 2 && fields[2] == op {
				skip = true
			}
		}
		if !skip {
			out.WriteString(line)
		}
	}
	return out.Bytes()
}

// planFileMembers returns the content of each member of a plan file that
// can be compared between builds, keyed by its name.
//
// The state snapshots are canonicalised like any other state. The
// configuration snapshot and the dependency lock file are compared as they
// are. The plan itself (the "tfplan" member) is a protocol buffers message
// whose fields the fork writes in a different order, which is documented
// (difference 4) and which the format permits, so its bytes aren't
// compared. What it means is compared instead, through each build's
// description of it and through each build's description of the other's.
func planFileMembers(path string) map[string][]byte {
	ret := make(map[string][]byte)
	zr, err := zip.OpenReader(path)
	if err != nil {
		ret["(archive)"] = []byte("unreadable: " + err.Error() + "\n")
		return ret
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		rc, err := f.Open()
		if err != nil {
			ret[f.Name] = []byte("unreadable: " + err.Error() + "\n")
			continue
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		switch {
		case err != nil:
			ret[f.Name] = []byte("unreadable: " + err.Error() + "\n")
		case f.Name == "tfplan":
			// See above.
		case f.Name == "tfstate" || f.Name == "tfstate-prev":
			ret[f.Name] = canonicalState(content)
		default:
			ret[f.Name] = content
		}
	}
	sort.Strings(names)
	ret["(names)"] = []byte(strings.Join(names, "\n") + "\n")
	return ret
}

// difference is one artefact that isn't what it should be.
type difference struct {
	Step     string `json:"step"`
	Actor    string `json:"actor"`
	Artefact string `json:"artefact"`
	Detail   string `json:"detail"`
}

type scenarioReport struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Covers      []string `json:"covers"`
	Steps       int      `json:"steps"`
	Comparisons int      `json:"comparisons"`
	// Result is "identical", "different" (the fork differs from upstream),
	// "control" (upstream differs from itself, so the proof needs
	// attention rather than the fork) or "invalid" (the scenario didn't do
	// what it declares).
	Result string `json:"result"`
	// Digest identifies everything upstream produced for the scenario,
	// after canonicalisation.
	Digest      string       `json:"digest"`
	Differences []difference `json:"differences,omitempty"`
	Control     []difference `json:"control_differences,omitempty"`
	Invalid     []string     `json:"invalid,omitempty"`
}

func firstDifference(want, got []byte) string {
	wantLines, gotLines := strings.Split(string(want), "\n"), strings.Split(string(got), "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			clip := func(s string) string {
				if len(s) > 300 {
					return s[:300] + "..."
				}
				return s
			}
			return fmt.Sprintf("line %d: upstream has %q, this has %q", i+1, clip(w), clip(g))
		}
	}
	return "identical"
}

// runScenario runs a scenario as every actor and compares what they did.
func (r *runner) runScenario(s *scenario) *scenarioReport {
	actors := r.t.actors()
	results := make([][]*stepResult, len(actors))
	var wg sync.WaitGroup
	for i, a := range actors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = r.runActor(s, a)
		}()
	}
	wg.Wait()

	report := &scenarioReport{Name: s.Name, Description: s.Description, Covers: s.Covers, Steps: len(s.Steps)}
	upstream := results[0]
	digest := sha256.New()
	diffDir := filepath.Join(r.t.work, "differences", s.Name)
	os.RemoveAll(diffDir)

	for i, st := range s.Steps {
		if i >= len(upstream) {
			report.Invalid = append(report.Invalid, fmt.Sprintf("step %q was never reached", st.Name))
			continue
		}
		want := upstream[i]
		for _, problem := range want.errors {
			report.Invalid = append(report.Invalid, fmt.Sprintf("step %q: %s", st.Name, problem))
		}

		// Did upstream do what the scenario says this step is for?
		check := func(artefact string, expected int) {
			if got, ok := want.artefacts[artefact]; ok && strings.TrimSpace(string(got)) != strconv.Itoa(expected) {
				report.Invalid = append(report.Invalid, fmt.Sprintf("step %q: upstream's %s was %s, and the scenario expects %d", st.Name, artefact, strings.TrimSpace(string(got)), expected))
			}
		}
		check("plan.exit", st.PlanRC)
		check("apply.exit", st.ApplyRC)
		printed := string(want.artefacts["plan.output"]) + string(want.artefacts["plan.text"]) + string(want.artefacts["apply.output"]) + string(want.artefacts["command.output"])
		for _, text := range st.Expect {
			if !strings.Contains(printed, text) {
				report.Invalid = append(report.Invalid, fmt.Sprintf("step %q: upstream didn't print %q", st.Name, text))
			}
		}

		names := make([]string, 0, len(want.artefacts))
		for name := range want.artefacts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(digest, "%s\x00%s\x00%d\x00", st.Name, name, len(want.artefacts[name]))
			digest.Write(want.artefacts[name])
		}

		// The description of upstream's plan that everything is compared
		// with, including each build's reading of the other's plan file.
		for j, a := range actors {
			if i >= len(results[j]) {
				d := difference{Step: st.Name, Actor: a.name, Artefact: "(step)", Detail: "this actor never reached the step"}
				if j == 1 {
					report.Control = append(report.Control, d)
				} else {
					report.Differences = append(report.Differences, d)
				}
				continue
			}
			got := results[j][i]
			var found []difference
			for _, problem := range got.errors {
				if j != 0 {
					found = append(found, difference{Step: st.Name, Actor: a.name, Artefact: "(step)", Detail: problem})
				}
			}
			compare := func(artefact string, wantContent, gotContent []byte) {
				report.Comparisons++
				if bytes.Equal(wantContent, gotContent) {
					return
				}
				dir := filepath.Join(diffDir, fmt.Sprintf("%02d-%s", i, st.Name))
				os.MkdirAll(dir, 0o755)
				file := strings.NewReplacer(" ", "_", "/", "_", "(", "", ")", "").Replace(artefact)
				os.WriteFile(filepath.Join(dir, file+".upstream"), wantContent, 0o644)
				os.WriteFile(filepath.Join(dir, file+"."+a.name), gotContent, 0o644)
				found = append(found, difference{Step: st.Name, Actor: a.name, Artefact: artefact, Detail: firstDifference(wantContent, gotContent)})
			}
			all := make(map[string]bool)
			for name := range want.artefacts {
				all[name] = true
			}
			for name := range got.artefacts {
				all[name] = true
			}
			artefacts := make([]string, 0, len(all))
			for name := range all {
				artefacts = append(artefacts, name)
			}
			sort.Strings(artefacts)
			for _, name := range artefacts {
				const byOther = "plan.json as read by the other build"
				if name == byOther {
					// Whoever wrote the plan file and whoever read it,
					// the description must be upstream's own.
					if content, ok := got.artefacts[name]; ok {
						compare(name, want.artefacts["plan.json"], content)
					}
					continue
				}
				if j == 0 {
					continue
				}
				wantContent, ok := want.artefacts[name]
				if !ok {
					wantContent = []byte("<not produced>\n")
				}
				gotContent, ok := got.artefacts[name]
				if !ok {
					gotContent = []byte("<not produced>\n")
				}
				compare(name, wantContent, gotContent)
			}

			// Checks that aren't comparisons with upstream: each actor's
			// own serials, and the order of its changes.
			if problem := checkOrder(got.rawOpLog, st.Order); problem != "" {
				found = append(found, difference{Step: st.Name, Actor: a.name, Artefact: "order of changes", Detail: problem})
			}
			report.Comparisons++
			if i > 0 && i-1 < len(results[j]) {
				if problem := checkSerials(results[j][i-1], got); problem != "" {
					found = append(found, difference{Step: st.Name, Actor: a.name, Artefact: "serial", Detail: problem})
				}
				report.Comparisons++
			}

			switch j {
			case 0:
				for _, d := range found {
					if d.Artefact == "plan.json as read by the other build" {
						d.Actor = "fork reading upstream's plan file"
						report.Differences = append(report.Differences, d)
					} else {
						report.Invalid = append(report.Invalid, fmt.Sprintf("step %q: upstream: %s: %s", st.Name, d.Artefact, d.Detail))
					}
				}
			case 1:
				report.Control = append(report.Control, found...)
			default:
				report.Differences = append(report.Differences, found...)
			}
		}
	}

	report.Digest = hex.EncodeToString(digest.Sum(nil))
	switch {
	case len(report.Invalid) != 0:
		report.Result = "invalid"
	case len(report.Control) != 0:
		report.Result = "control"
	case len(report.Differences) != 0:
		report.Result = "different"
	default:
		report.Result = "identical"
		if !r.keep {
			os.RemoveAll(filepath.Join(r.t.work, "run", s.Name))
		}
	}
	return report
}

// checkSerials checks that an actor's state serial hasn't gone backwards,
// and that it increased if the state changed. This is what's required of
// the serial in place of its being equal to upstream's. See rule 2.
func checkSerials(before, after *stepResult) string {
	if before.serial < 0 || after.serial < 0 {
		return ""
	}
	if after.serial < before.serial {
		return fmt.Sprintf("the serial went backwards, from %d to %d", before.serial, after.serial)
	}
	if !bytes.Equal(before.artefacts["state"], after.artefacts["state"]) && after.serial == before.serial {
		return fmt.Sprintf("the state changed but the serial stayed at %d", after.serial)
	}
	return ""
}

// checkOrder checks that, in the provider's record of a step, the change to
// the first resource of each given pair was complete before the change to
// the second began.
func checkOrder(oplog []byte, pairs [][2]string) string {
	if len(pairs) == 0 {
		return ""
	}
	type event struct {
		phase, op, name string
	}
	var events []event
	for _, line := range strings.Split(string(oplog), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || (fields[1] != "begin" && fields[1] != "end") {
			continue
		}
		name, err := strconv.Unquote(fields[4])
		if err != nil {
			name = fields[4]
		}
		events = append(events, event{fields[1], fields[2], name})
	}
	find := func(spec, phase string, last bool) int {
		op, name, ok := strings.Cut(spec, ":")
		if !ok {
			op, name = "", spec
		}
		found := -1
		for i, e := range events {
			if e.phase == phase && e.name == name && (op == "" || e.op == op) {
				found = i
				if !last {
					break
				}
			}
		}
		return found
	}
	for _, pair := range pairs {
		end, begin := find(pair[0], "end", true), find(pair[1], "begin", false)
		switch {
		case end < 0:
			return fmt.Sprintf("the provider was never asked to change %q", pair[0])
		case begin < 0:
			return fmt.Sprintf("the provider was never asked to change %q", pair[1])
		case end > begin:
			return fmt.Sprintf("the change to %q began before the change to %q was complete", pair[1], pair[0])
		}
	}
	return ""
}
