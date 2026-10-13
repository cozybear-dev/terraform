// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

// Command forkproof is the integrity proof of the large-state performance
// fork of Terraform. It builds upstream Terraform at a pinned commit and
// the fork from source, has both do the same things, and compares what
// they produce. README.md says what that establishes and what it doesn't.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// report is what the proof writes: enough to say exactly what was compared
// with what, by what means, and what was found.
type report struct {
	Format string `json:"format"`
	// Verdict is "identical" if no comparison found a difference and
	// nothing prevented a comparison from being made, and "different" or
	// "incomplete" otherwise.
	Verdict string `json:"verdict"`

	Upstream struct {
		Commit string `json:"commit"`
		// Backports are commits of upstream's, later than Commit, whose
		// changes were applied to it before it was built, because the fork
		// carries them too. Tree is the result, which is what the fork was
		// compared with, and is the tree of Commit when there are none.
		Backports []backport `json:"backports,omitempty"`
		Tree      string     `json:"tree"`
		Version   string     `json:"version"`
		Binary    string     `json:"binary_sha256"`
	} `json:"upstream"`
	Fork struct {
		Commit string `json:"commit"`
		// Tree differs from Commit, and Dirty is set, only if the fork was
		// built from a working tree with changes that aren't committed.
		Tree    string `json:"tree"`
		Dirty   bool   `json:"dirty"`
		Version string `json:"version"`
		Binary  string `json:"binary_sha256"`
		// Supplied is set if the fork's binary was given rather than built,
		// such as one taken out of a published image.
		Supplied bool `json:"binary_supplied"`
	} `json:"fork"`
	Toolchain string `json:"toolchain"`
	Platform  string `json:"platform"`
	Tier      string `json:"tier"`
	Seed      uint64 `json:"seed"`
	Started   string `json:"started"`
	Seconds   int    `json:"seconds"`

	// Corpus identifies the scenarios, the provider, the component driver
	// and the rules for what's disregarded. Two reports with the same
	// corpus digest made the same comparisons.
	Corpus struct {
		Digest        string `json:"sha256"`
		Canonicaliser string `json:"canonicaliser_sha256"`
		Scenarios     int    `json:"scenarios"`
	} `json:"corpus"`

	Totals struct {
		Scenarios   map[string]int `json:"scenarios"`
		Comparisons int            `json:"comparisons"`
		Components  map[string]int `json:"component_inputs"`
	} `json:"totals"`

	Scenarios  []*scenarioReport `json:"scenarios"`
	Components []componentReport `json:"components"`
	Crash      []crashReport     `json:"crash,omitempty"`
	Coverage   *coverageReport   `json:"coverage,omitempty"`
	// Undeclared lists files that differ from upstream without being in
	// FORK_FILES. Upstream's own tests are never in that list, so this is
	// what notices one of them being changed or removed.
	Undeclared []string `json:"undeclared_files,omitempty"`
	// ChangedUpstreamTests lists the test files and test data of upstream's
	// that the fork changes or removes. There should be none: the fork's
	// own tests are in files that it adds.
	ChangedUpstreamTests []string `json:"changed_upstream_tests,omitempty"`
	Problems             []string `json:"problems,omitempty"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "compare" {
		os.Exit(compareCommand(os.Args[2:]))
	}
	var (
		work        = flag.String("work", ".work", "directory to build and run in")
		upstreamRef = flag.String("upstream-ref", "", "upstream commit to compare with (default: the content of UPSTREAM_REF)")
		backportsIn = flag.String("upstream-backports", "UPSTREAM_BACKPORTS", "file listing commits of upstream's to apply to it first, or empty for none")
		forkRef     = flag.String("fork-ref", "HEAD", "commit of the fork to prove, or WORKTREE for the tracked files as they are now")
		upstreamBin = flag.String("upstream-bin", "", "use this upstream terraform executable instead of building one")
		forkBin     = flag.String("fork-bin", "", "use this fork terraform executable instead of building one, such as one from a published image")
		tier        = flag.String("tier", "ci", "ci or nightly: how many generated inputs and how many interruptions")
		filter      = flag.String("scenarios", "", "only run the scenarios whose names match this regular expression")
		only        = flag.String("only", "", "comma-separated parts to run: scenarios, components, crash (default: all)")
		jobs        = flag.Int("jobs", max(2, runtime.NumCPU()/3), "how many terraform commands to run at once")
		seed        = flag.Uint64("seed", 20261013, "seed for the generated inputs and for when applies are interrupted")
		coverage    = flag.Bool("coverage", true, "measure which of the fork's changed statements the runs execute")
		skipBuild   = flag.Bool("skip-build", false, "use what an earlier run built")
		keep        = flag.Bool("keep", false, "keep the working directories of scenarios that pass")
		out         = flag.String("report", "", "where to write the report (default: report.json in the working directory)")
	)
	flag.Parse()
	started := time.Now()
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[%4ds] %s\n", int(time.Since(started).Seconds()), fmt.Sprintf(format, args...))
	}
	die := func(err error) {
		fmt.Fprintln(os.Stderr, "forkproof:", err)
		os.Exit(2)
	}

	if *upstreamRef == "" {
		src, err := os.ReadFile("UPSTREAM_REF")
		if err != nil {
			die(fmt.Errorf("run this from the fork-proof directory: %w", err))
		}
		*upstreamRef = strings.TrimSpace(string(src))
	}
	var backports []string
	if *backportsIn != "" {
		src, err := os.ReadFile(*backportsIn)
		if err != nil && !os.IsNotExist(err) {
			die(err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			// Anything after the commit is a note about it.
			if fields := strings.Fields(line); len(fields) != 0 && !strings.HasPrefix(fields[0], "#") {
				backports = append(backports, fields[0])
			}
		}
	}
	if *forkBin != "" {
		// Coverage needs a build of the same source that's instrumented.
		logf("a fork binary was supplied, so the scenarios run with it and coverage comes from a build of %s", *forkRef)
	}
	want := map[string]bool{"scenarios": true, "components": true, "crash": true}
	if *only != "" {
		want = make(map[string]bool)
		for _, part := range strings.Split(*only, ",") {
			want[strings.TrimSpace(part)] = true
		}
	}

	t, err := prepare(buildOptions{
		work: *work, upstreamRef: *upstreamRef, forkRef: *forkRef,
		backports:   backports,
		upstreamBin: *upstreamBin, forkBin: *forkBin,
		coverage: *coverage, skipBuild: *skipBuild,
	}, logf)
	if err != nil {
		die(err)
	}
	os.RemoveAll(filepath.Join(t.work, "coverage"))
	os.MkdirAll(filepath.Join(t.work, "coverage", "terraform"), 0o755)
	os.MkdirAll(filepath.Join(t.work, "coverage", "driver"), 0o755)

	// The toolchain is the one that built the binaries being compared,
	// which is the one their source asks for and needn't be the one that
	// built this program.
	toolchainOf := func(bin string) string {
		out, _, _ := command("", nil, "go", "version", bin)
		_, version, _ := strings.Cut(strings.TrimSpace(string(out)), ": ")
		return version
	}
	rep := &report{Format: "forkproof/1", Platform: runtime.GOOS + "/" + runtime.GOARCH, Tier: *tier, Seed: *seed}
	rep.Toolchain = toolchainOf(t.fork)
	if upstreamToolchain := toolchainOf(t.upstream); upstreamToolchain != rep.Toolchain {
		rep.Toolchain = "upstream built with " + upstreamToolchain + ", fork built with " + rep.Toolchain
	}
	rep.Started = started.UTC().Format(time.RFC3339)
	rep.Upstream.Commit, rep.Upstream.Binary = t.upstreamCommit, fileDigest(t.upstream)
	rep.Upstream.Backports, rep.Upstream.Tree = t.backports, t.upstreamTree
	if len(t.backports) == 0 {
		// The id of the tree itself, as it is when there are backports.
		if tree, err := gitOutput(t.repo, "rev-parse", t.upstreamCommit+"^{tree}"); err == nil {
			rep.Upstream.Tree = tree
		}
	}
	rep.Fork.Commit, rep.Fork.Tree, rep.Fork.Dirty = t.forkCommit, t.forkTree, t.forkDirty
	rep.Fork.Binary, rep.Fork.Supplied = fileDigest(t.fork), *forkBin != ""
	version := func(bin string) string {
		out, _, _ := command("", []string{"HOME=" + t.work, "CHECKPOINT_DISABLE=1"}, bin, "version")
		return strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	}
	rep.Upstream.Version, rep.Fork.Version = version(t.upstream), version(t.fork)
	if rep.Corpus.Digest, err = treeDigest(t.self, "scenarios", "_overlay", "canonical.go", "scenario.go", "component.go", "crash.go"); err != nil {
		die(err)
	}
	rep.Corpus.Canonicaliser = fileDigest(filepath.Join(t.self, "canonical.go"))
	rep.Totals.Scenarios = make(map[string]int)
	rep.Totals.Components = make(map[string]int)
	problem := func(format string, args ...any) {
		rep.Problems = append(rep.Problems, fmt.Sprintf(format, args...))
	}
	nightly := *tier == "nightly"

	// The fork may differ from upstream only in the files it declares.
	if undeclared, err := undeclaredFiles(t); err != nil {
		problem("the files the fork changes couldn't be checked against FORK_FILES: %s", err)
	} else if len(undeclared) != 0 {
		rep.Undeclared = undeclared
		problem("the fork differs from upstream in %d files that FORK_FILES doesn't list, the first of them %s", len(undeclared), undeclared[0])
	}

	// And never in upstream's tests, whatever it declares.
	if changed, err := changedUpstreamTests(t); err != nil {
		problem("upstream's tests couldn't be checked for changes: %s", err)
	} else if len(changed) != 0 {
		rep.ChangedUpstreamTests = changed
		problem("the fork changes or removes %d of upstream's own test files, the first of them %s", len(changed), changed[0])
	}

	if want["scenarios"] {
		var re *regexp.Regexp
		if *filter != "" {
			if re, err = regexp.Compile(*filter); err != nil {
				die(err)
			}
		}
		scenarios, err := loadScenarios(filepath.Join(t.self, "scenarios"), re)
		if err != nil {
			die(err)
		}
		rep.Corpus.Scenarios = len(scenarios)
		logf("running %d scenarios, each as %d actors", len(scenarios), len(t.actors()))
		r := &runner{t: t, sem: make(chan struct{}, max(1, *jobs)), keep: *keep}
		rep.Scenarios = make([]*scenarioReport, len(scenarios))
		// Each scenario runs its actors concurrently, so only a few
		// scenarios are in progress at once.
		slots := make(chan struct{}, max(1, *jobs/2))
		var wg sync.WaitGroup
		for i, s := range scenarios {
			wg.Add(1)
			go func() {
				defer wg.Done()
				slots <- struct{}{}
				defer func() { <-slots }()
				sr := r.runScenario(s)
				rep.Scenarios[i] = sr
				logf("scenario %-34s %-9s %d comparisons", s.Name, sr.Result, sr.Comparisons)
			}()
		}
		wg.Wait()
		for _, sr := range rep.Scenarios {
			rep.Totals.Scenarios[sr.Result]++
			rep.Totals.Comparisons += sr.Comparisons
		}
	}

	if want["components"] {
		rep.Components = runComponents(t, nightly, *seed, *jobs, logf)
		for _, c := range rep.Components {
			rep.Totals.Components[c.Mode] = c.Inputs
		}
	}

	if want["crash"] {
		kills, interrupts := 8, 4
		if nightly {
			kills, interrupts = 80, 30
		}
		rep.Crash = append(rep.Crash,
			runCrashTier(t, "small", 30, kills, interrupts, *seed, logf),
			runCrashTier(t, "large", 2200, kills, interrupts, *seed, logf),
		)
	}

	if *coverage {
		if cov, err := measureCoverage(t); err != nil {
			problem("coverage couldn't be measured: %s", err)
		} else {
			rep.Coverage = cov
			logf("the runs executed %d of the %d statements the fork changed (%.1f%%)", cov.Covered, cov.Statements, cov.Percent)
		}
	}

	// The verdict.
	different, incomplete := false, len(rep.Problems) != 0
	for _, sr := range rep.Scenarios {
		different = different || sr.Result == "different"
		incomplete = incomplete || sr.Result == "control" || sr.Result == "invalid"
	}
	for _, c := range rep.Components {
		different = different || c.Result == "different"
		incomplete = incomplete || c.Result == "invalid"
	}
	for _, c := range rep.Crash {
		different = different || c.Result == "different"
		incomplete = incomplete || c.Result == "invalid"
	}
	switch {
	case different:
		rep.Verdict = "different"
	case incomplete:
		rep.Verdict = "incomplete"
	default:
		rep.Verdict = "identical"
	}
	rep.Seconds = int(time.Since(started).Seconds())

	path := *out
	if path == "" {
		path = filepath.Join(t.work, "report.json")
	}
	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		die(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		die(err)
	}

	summarize(rep, path)
	if rep.Verdict != "identical" {
		os.Exit(1)
	}
}

// undeclaredFiles returns the files that differ between upstream and the
// fork and that FORK_FILES doesn't list.
//
// FORK_FILES is every file the fork is allowed to differ from upstream in:
// the files it adds and the files of upstream's that it changes. Anything
// else differing means that something touched a file it had no business
// touching, the important case being one of upstream's tests, which are how
// upstream's own expectations are held against the fork.
func undeclaredFiles(t *toolchain) ([]string, error) {
	src, err := os.ReadFile(filepath.Join(t.self, "FORK_FILES"))
	if err != nil {
		return nil, err
	}
	declared := make(map[string]bool)
	for _, line := range strings.Split(string(src), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			declared[line] = true
		}
	}
	out, err := gitOutput(t.repo, "diff", "--name-only", "--no-renames", t.upstreamTree, t.forkTree)
	if err != nil {
		return nil, err
	}
	var undeclared []string
	for _, file := range strings.Split(out, "\n") {
		if file == "" || declared[file] {
			continue
		}
		// A directory can be declared as a whole, with a trailing slash.
		covered := false
		for dir := filepath.Dir(file); dir != "."; dir = filepath.Dir(dir) {
			if declared[dir+"/"] {
				covered = true
				break
			}
		}
		if !covered {
			undeclared = append(undeclared, file)
		}
	}
	return undeclared, nil
}

// changedUpstreamTests returns the test files and test data that exist in
// upstream and that the fork changes or removes.
//
// Being listed in FORK_FILES doesn't excuse one. Upstream's tests are what
// hold upstream's expectations against the fork when its own suite is run on
// the fork's tree, so the fork keeps the tests it adds in files of its own
// and leaves upstream's exactly as they are.
func changedUpstreamTests(t *toolchain) ([]string, error) {
	out, err := gitOutput(t.repo, "diff", "--name-status", "--no-renames", t.upstreamTree, t.forkTree)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, line := range strings.Split(out, "\n") {
		status, file, ok := strings.Cut(line, "\t")
		if !ok || status == "A" {
			// A file that the fork adds isn't one of upstream's.
			continue
		}
		if strings.HasSuffix(file, "_test.go") || strings.Contains("/"+file, "/testdata/") {
			changed = append(changed, file)
		}
	}
	return changed, nil
}

// summarize prints what a person needs in order to act on the report.
func summarize(rep *report, path string) {
	fmt.Printf("\nUpstream %s", short(rep.Upstream.Commit))
	for _, b := range rep.Upstream.Backports {
		fmt.Printf(" with %s", short(b.Commit))
	}
	fmt.Printf(", fork %s", short(rep.Fork.Commit))
	if rep.Fork.Dirty {
		fmt.Printf(" with uncommitted changes")
	}
	fmt.Printf(", %s, tier %s\n", rep.Platform, rep.Tier)

	if len(rep.Scenarios) != 0 {
		keys := make([]string, 0, len(rep.Totals.Scenarios))
		for k := range rep.Totals.Scenarios {
			keys = append(keys, fmt.Sprintf("%d %s", rep.Totals.Scenarios[k], k))
		}
		sort.Strings(keys)
		fmt.Printf("Scenarios: %s (%d comparisons)\n", strings.Join(keys, ", "), rep.Totals.Comparisons)
	}
	for _, sr := range rep.Scenarios {
		for _, text := range sr.Invalid {
			fmt.Printf("  INVALID   %s: %s\n", sr.Name, text)
		}
		for _, d := range sr.Control {
			fmt.Printf("  CONTROL   %s, step %s: upstream disagrees with itself about %s: %s\n", sr.Name, d.Step, d.Artefact, d.Detail)
		}
		for _, d := range sr.Differences {
			fmt.Printf("  DIFFERENT %s, step %s, %s: %s: %s\n", sr.Name, d.Step, d.Actor, d.Artefact, d.Detail)
		}
	}
	for _, c := range rep.Components {
		status := "identical"
		if c.Result != "identical" {
			status = strings.ToUpper(c.Result)
		}
		fmt.Printf("Component %-9s %7d inputs: %s", c.Mode, c.Inputs, status)
		if c.Different != 0 {
			fmt.Printf(" (%d differ)", c.Different)
		}
		if c.Unstable != 0 {
			fmt.Printf(" (%d excluded, upstream unstable)", c.Unstable)
		}
		fmt.Println()
		for _, example := range c.Examples {
			fmt.Printf("    %s\n", example)
		}
	}
	for _, c := range rep.Crash {
		fmt.Printf("Crash %-5s (%d objects already in the state): %d kills, %d interrupts: %s; at most %d created objects missing after a kill\n",
			c.Variant, c.Existing, c.Kills, c.Interrupts, strings.ToUpper(c.Result[:1])+c.Result[1:], c.MostMissing)
		var outcomes []string
		for outcome, n := range c.Outcomes {
			outcomes = append(outcomes, fmt.Sprintf("%d %s", n, outcome))
		}
		sort.Strings(outcomes)
		fmt.Printf("    %s\n", strings.Join(outcomes, "; "))
		for _, v := range c.Violations {
			fmt.Printf("    VIOLATION %s\n", v)
		}
	}
	if rep.Coverage != nil {
		fmt.Printf("Coverage: %d of %d changed statements executed (%.1f%%); what wasn't is listed in %s\n",
			rep.Coverage.Covered, rep.Coverage.Statements, rep.Coverage.Percent, rep.Coverage.UncoveredList)
	}
	for _, p := range rep.Problems {
		fmt.Printf("PROBLEM %s\n", p)
	}
	fmt.Printf("Verdict: %s (%d seconds). Report: %s\n", strings.ToUpper(rep.Verdict), rep.Seconds, path)
}
