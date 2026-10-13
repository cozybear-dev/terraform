// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The crash tier stops the fork part way through an apply, a different
// moment each time, and examines what's left on disk.
//
// The apply creates objects that each take a while, some independent and
// some in a chain where each depends on the one before, on top of a state
// that already holds either a few objects or enough of them for the fork to
// save snapshots in the background rather than after every object, which is
// the behaviour it documents as differing from upstream (difference 1).
//
// What must hold after a kill that gives Terraform no chance to react:
//
//   - The state file is a complete snapshot, or is recognizably not one:
//     either it's the state as it was at some moment, or Terraform refuses
//     to read it. An empty file is counted separately, because Terraform
//     reads that as there being no state; upstream can leave one too.
//   - A complete snapshot has every object that was there before, exactly as
//     it was, and no object that the provider didn't finish creating.
//   - Every object in it has the values that the provider returned.
//   - An object in the chain is only there if the one before it is.
//   - The backup file, if there is one, is the state from before the apply,
//     or is the beginning of it while the state file is still untouched.
//   - Planning again proposes to create exactly the objects that are missing.
//
// A snapshot may lack objects that were created before the kill. For a state
// large enough for the fork to save snapshots in the background, how many
// is reported and isn't a failure, since that is the documented difference.
// For a smaller state the fork documents that it saves after every object,
// as upstream does, and then no more can be missing than were being worked
// on at once when the kill arrived.
//
// After an interrupt (one SIGINT), which Terraform does react to, the state
// must additionally have every object that the provider finished creating.
type crashReport struct {
	Variant    string `json:"variant"`
	Existing   int    `json:"existing_objects"`
	Kills      int    `json:"kills"`
	Interrupts int    `json:"interrupts"`
	// Outcomes counts what was found after each kill.
	Outcomes map[string]int `json:"outcomes"`
	// MostMissing is the largest number of objects that had been created
	// but weren't in the state after a kill.
	MostMissing int `json:"most_objects_missing_after_a_kill"`
	// BackupsIncomplete counts the kills that arrived while the backup was
	// being written, which leaves part of it, and the state file untouched.
	BackupsIncomplete int `json:"kills_that_left_the_backup_incomplete_and_the_state_untouched"`
	// UninterruptedMS is how long the apply took when it wasn't stopped.
	UninterruptedMS int            `json:"uninterrupted_apply_ms"`
	Attempts        []crashAttempt `json:"attempts"`
	Violations      []string       `json:"violations,omitempty"`
	Result          string         `json:"result"`
}

// crashAttempt is what one interruption found.
type crashAttempt struct {
	Signal string `json:"signal"`
	// AfterMS is how long the apply had been running when it was stopped.
	AfterMS int `json:"after_ms"`
	// Created is how many objects the provider had finished creating, and
	// InState is how many of the new objects the state had afterwards.
	Created int    `json:"created"`
	InState int    `json:"in_state"`
	Outcome string `json:"outcome"`
}

const (
	crashIndependent = 48
	crashChain       = 8
	crashSleepMS     = 220

	// crashLargeFrom is the number of resource instances from which the
	// fork documents that it saves snapshots in the background (difference
	// 1 in README-FORK.md). Below it, every object is saved before the
	// graph node that created it does anything else.
	crashLargeFrom = 2000

	// crashParallelism is how many objects Terraform works on at once, the
	// default for -parallelism, which the applies here don't change. With a
	// state that's saved after every object, each of those is either still
	// being created or waiting for its turn to be saved, so this is the
	// most that a kill can leave out of the state.
	crashParallelism = 10
)

func crashConfig(existing int, slow bool) string {
	var b strings.Builder
	b.WriteString(`terraform {
  required_providers {
    fp = { source = "hashicorp/fp" }
  }
}

resource "fp_thing" "base" {
  count = ` + strconv.Itoa(existing) + `
  name  = "base-${count.index}"
  value = "value of ${count.index}"
  tags  = { index = tostring(count.index) }
}
`)
	if !slow {
		return b.String()
	}
	b.WriteString(`
resource "fp_thing" "slow" {
  count  = ` + strconv.Itoa(crashIndependent) + `
  name   = "slow-${count.index}"
  value  = "slow value ${count.index}"
  behave = "sleep:` + strconv.Itoa(crashSleepMS) + `"
}
`)
	for i := 0; i < crashChain; i++ {
		value := `"start"`
		if i > 0 {
			value = fmt.Sprintf("fp_thing.chain_%d.late", i-1)
		}
		fmt.Fprintf(&b, `
resource "fp_thing" "chain_%d" {
  name   = "chain-%d"
  value  = %s
  behave = "sleep:%d"
}
`, i, i, value, crashSleepMS)
	}
	return b.String()
}

// crashState is as much of a state file as the crash tier examines.
type crashState struct {
	Resources []struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Instances []struct {
			IndexKey   json.RawMessage `json:"index_key"`
			Status     string          `json:"status"`
			Attributes struct {
				ID    string `json:"id"`
				Name  string `json:"name"`
				Value string `json:"value"`
				Late  string `json:"late"`
			} `json:"attributes"`
		} `json:"instances"`
	} `json:"resources"`
}

var planToAdd = regexp.MustCompile(`Plan: ([0-9]+) to add, ([0-9]+) to change, ([0-9]+) to destroy`)

func runCrashTier(t *toolchain, variant string, existing, kills, interrupts int, seed uint64, logf func(string, ...any)) crashReport {
	report := crashReport{Variant: variant, Existing: existing, Kills: kills, Interrupts: interrupts, Outcomes: make(map[string]int), Result: "identical"}
	fail := func(format string, args ...any) {
		report.Violations = append(report.Violations, fmt.Sprintf(format, args...))
		report.Result = "different"
	}
	invalid := func(format string, args ...any) crashReport {
		report.Violations = append(report.Violations, fmt.Sprintf(format, args...))
		report.Result = "invalid"
		return report
	}

	root := filepath.Join(t.work, "crash", variant)
	os.RemoveAll(root)
	home := filepath.Join(root, "home")
	template := filepath.Join(root, "template")
	os.MkdirAll(home, 0o755)
	os.MkdirAll(template, 0o755)
	env := func(oplog string) []string {
		return []string{
			"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "TF_CLI_CONFIG_FILE=" + t.cliConfig,
			"TF_IN_AUTOMATION=1", "TF_INPUT=0", "CHECKPOINT_DISABLE=1", "TZ=UTC", "LC_ALL=C", "FP_OPLOG=" + oplog,
		}
	}

	// The state to begin each attempt with, and the plan to apply.
	os.WriteFile(filepath.Join(template, "main.tf"), []byte(crashConfig(existing, false)), 0o644)
	for _, args := range [][]string{{"init", "-input=false", "-no-color"}, {"apply", "-input=false", "-no-color", "-auto-approve"}} {
		if out, rc, err := command(template, env(filepath.Join(root, "setup-oplog.txt")), t.fork, args...); err != nil || rc != 0 {
			return invalid("preparing the state to begin with failed: terraform %s: exit status %d\n%s", args[0], rc, tail(out, 10))
		}
	}
	os.Remove(filepath.Join(template, "terraform.tfstate.backup"))
	before, err := os.ReadFile(filepath.Join(template, "terraform.tfstate"))
	if err != nil {
		return invalid("%s", err)
	}
	var beforeState crashState
	if err := json.Unmarshal(before, &beforeState); err != nil || len(beforeState.Resources) != 1 || len(beforeState.Resources[0].Instances) != existing {
		return invalid("the state to begin with isn't what was expected")
	}
	os.WriteFile(filepath.Join(template, "main.tf"), []byte(crashConfig(existing, true)), 0o644)
	if out, rc, err := command(template, env(filepath.Join(root, "setup-oplog.txt")), t.fork, "plan", "-input=false", "-no-color", "-out=tfplan"); err != nil || rc != 0 {
		return invalid("planning failed: exit status %d\n%s", rc, tail(out, 10))
	}
	total := crashIndependent + crashChain

	// How long an uninterrupted apply takes decides when to stop the others.
	measure := filepath.Join(root, "measure")
	copyTree(template, measure)
	started := time.Now()
	if out, rc, err := command(measure, env(filepath.Join(root, "measure-oplog.txt")), t.fork, "apply", "-input=false", "-no-color", "tfplan"); err != nil || rc != 0 {
		return invalid("an uninterrupted apply failed: exit status %d\n%s", rc, tail(out, 10))
	}
	duration := time.Since(started)
	report.UninterruptedMS = int(duration.Milliseconds())
	os.RemoveAll(measure)

	rng := rand.New(rand.NewPCG(seed, uint64(existing)))
	for attempt := 0; attempt < kills+interrupts; attempt++ {
		interrupt := attempt >= kills
		label := fmt.Sprintf("%s attempt %d", variant, attempt)
		dir := filepath.Join(root, fmt.Sprintf("attempt-%03d", attempt))
		if err := copyTree(template, dir); err != nil {
			return invalid("%s", err)
		}
		oplog := filepath.Join(dir, "oplog.txt")

		// Anywhere from just after it starts to just before it would finish.
		delay := time.Duration((0.08 + 0.9*rng.Float64()) * float64(duration))
		// An interrupted apply exits in its own time, so it can be the
		// instrumented build, and what it does on being told to stop is
		// then counted in the coverage. A killed one records nothing.
		bin, cmdEnv := t.fork, env(oplog)
		if interrupt && t.cover != "" {
			bin = t.cover
			cmdEnv = append(cmdEnv, "GOCOVERDIR="+filepath.Join(t.work, "coverage", "terraform"))
		}
		cmd := exec.Command(bin, "apply", "-input=false", "-no-color", "tfplan")
		cmd.Dir = dir
		cmd.Env = cmdEnv
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			return invalid("%s", err)
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		stopped := false
		select {
		case <-exited:
			// It finished first, which is still a valid attempt.
		case <-time.After(delay):
			stopped = true
			if interrupt {
				cmd.Process.Signal(syscall.SIGINT)
			} else {
				// Only Terraform itself, as when it runs out of memory.
				cmd.Process.Kill()
			}
			select {
			case <-exited:
			case <-time.After(90 * time.Second):
				fail("%s: Terraform was still running 90 seconds after being interrupted", label)
				cmd.Process.Kill()
				<-exited
			}
		}
		// Whatever is left of the provider goes too, before we look.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		time.Sleep(50 * time.Millisecond)
		report.Attempts = append(report.Attempts, crashAttempt{Signal: "SIGKILL", AfterMS: int(delay.Milliseconds()), Outcome: "not examined"})
		// Nothing is appended again before the next attempt.
		this := &report.Attempts[len(report.Attempts)-1]
		if interrupt {
			this.Signal = "SIGINT"
		}
		if !stopped {
			this.Signal = "none: it finished first"
		}
		record := func(outcome string) {
			this.Outcome = outcome
			report.Outcomes[outcome]++
		}

		// What the provider finished creating.
		created := make(map[string]bool)
		log, _ := os.ReadFile(oplog)
		for _, line := range strings.Split(string(log), "\n") {
			if fields := strings.Fields(line); len(fields) >= 6 && fields[1] == "end" && fields[2] == "create" {
				if name, err := strconv.Unquote(fields[4]); err == nil {
					created[name] = true
				}
			}
		}

		this.Created = len(created)
		if backup, err := os.ReadFile(filepath.Join(dir, "terraform.tfstate.backup")); err == nil && !bytes.Equal(backup, before) {
			// The backup is written, without being renamed into place,
			// before the state file is first touched, by upstream and by
			// the fork alike. So it can be incomplete, but only if that's
			// as far as the apply got, in which case the state file must
			// still be exactly what it was.
			untouched, _ := os.ReadFile(filepath.Join(dir, "terraform.tfstate"))
			if bytes.HasPrefix(before, backup) && bytes.Equal(untouched, before) {
				report.BackupsIncomplete++
			} else {
				fail("%s: the backup file isn't the state from before the apply", label)
			}
		}

		after, err := os.ReadFile(filepath.Join(dir, "terraform.tfstate"))
		if err != nil {
			fail("%s: the state file is gone: %s", label, err)
			continue
		}
		var state crashState
		switch {
		case len(after) == 0:
			record("empty file")
			if interrupt {
				fail("%s: the state file was empty after an interrupt", label)
			}
			continue
		case json.Unmarshal(after, &state) != nil:
			// It must be refused, not mistaken for a state.
			_, rc, _ := command(dir, env(oplog), t.fork, "show", "-json")
			if rc == 0 {
				fail("%s: the state file is incomplete, and Terraform read it without complaint", label)
			}
			record("incomplete file, which Terraform refuses")
			if interrupt {
				fail("%s: the state file was incomplete after an interrupt", label)
			}
			continue
		}

		present := make(map[string]bool)
		for _, rs := range state.Resources {
			if rs.Name == "base" {
				var want, got bytes.Buffer
				json.NewEncoder(&want).Encode(beforeState.Resources[0])
				json.NewEncoder(&got).Encode(rs)
				if !bytes.Equal(want.Bytes(), got.Bytes()) {
					fail("%s: the objects that were already in the state aren't as they were", label)
				}
				continue
			}
			for _, inst := range rs.Instances {
				attrs := inst.Attributes
				present[attrs.Name] = true
				if !created[attrs.Name] {
					fail("%s: the state has %s, which the provider didn't finish creating", label, attrs.Name)
				}
				if attrs.ID != "id-"+attrs.Name || attrs.Late != "late:"+attrs.Name+":"+attrs.Value || inst.Status != "" {
					fail("%s: the state has %s with values the provider didn't return", label, attrs.Name)
				}
			}
		}
		if !present["base-0"] && len(state.Resources) == 0 {
			fail("%s: the state has no resources at all", label)
		}
		foundBase := false
		for _, rs := range state.Resources {
			foundBase = foundBase || rs.Name == "base"
		}
		if !foundBase {
			fail("%s: the objects that were already in the state are gone", label)
		}
		for i := 1; i < crashChain; i++ {
			if present[fmt.Sprintf("chain-%d", i)] && !present[fmt.Sprintf("chain-%d", i-1)] {
				fail("%s: the state has chain-%d without chain-%d, which it depends on", label, i, i-1)
			}
		}
		this.Created, this.InState = len(created), len(present)
		missing := 0
		var missingNames []string
		for name := range created {
			if !present[name] {
				missing++
				missingNames = append(missingNames, name)
			}
		}
		sort.Strings(missingNames)
		switch {
		case interrupt && missing != 0:
			this.Outcome = "after an interrupt, missing what had been created"
			fail("%s: after an interrupt the state lacks %d objects that had been created: %s", label, missing, strings.Join(missingNames, ", "))
		case !stopped || missing == 0:
			record("complete, nothing missing")
		case existing < crashLargeFrom && missing > crashParallelism:
			this.Outcome = "a state saved after every object, missing more than were in progress"
			report.MostMissing = max(report.MostMissing, missing)
			fail("%s: a state of %d objects is saved after every object, and after a kill it lacks %d that had been created, more than the %d that can be in progress at once: %s",
				label, existing, missing, crashParallelism, strings.Join(missingNames, ", "))
		default:
			record("complete, behind what had been created")
			report.MostMissing = max(report.MostMissing, missing)
		}

		// The state must be usable, and what's missing must be exactly
		// what Terraform now proposes to create.
		out, rc, _ := command(dir, env(oplog), t.fork, "plan", "-input=false", "-no-color", "-lock=false")
		m := planToAdd.FindSubmatch(out)
		switch {
		case rc != 0:
			fail("%s: planning with the state that was left failed:\n%s", label, tail(out, 8))
		case len(present) == total:
			if m != nil {
				fail("%s: every object is in the state, and Terraform still proposes changes", label)
			}
		case m == nil || string(m[1]) != strconv.Itoa(total-len(present)) || string(m[2]) != "0" || string(m[3]) != "0":
			fail("%s: %d objects are missing from the state, and Terraform proposes: %s", label, total-len(present), strings.TrimSpace(string(planToAdd.Find(out))))
		}
		if report.Result == "identical" {
			os.RemoveAll(dir)
		}
	}
	os.RemoveAll(template)
	logf("crash %-6s %d kills and %d interrupts: %v, at most %d objects missing after a kill, %d violations",
		variant, kills, interrupts, report.Outcomes, report.MostMissing, len(report.Violations))
	return report
}
