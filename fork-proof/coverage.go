// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The coverage measurement answers one question: of the statements that the
// fork added or changed, how many did the differential runs execute? A
// changed statement that nothing executed is one whose equivalence with
// upstream this proof says nothing about, so the answer, and the list of
// what wasn't reached, is part of the report.

const modulePath = "github.com/hashicorp/terraform/"

type coverageReport struct {
	// Statements counts the statements, outside tests, in blocks of code
	// that the fork added or changed, in packages that were instrumented.
	Statements int     `json:"changed_statements"`
	Covered    int     `json:"changed_statements_executed"`
	Percent    float64 `json:"percent"`
	// Files has the same for each file, least covered first.
	Files []fileCoverage `json:"files"`
	// NotInstrumented lists changed files that couldn't be measured,
	// because they're built separately from Terraform itself.
	NotInstrumented []string `json:"not_instrumented,omitempty"`
	// UncoveredList is the path of the full list of blocks not reached.
	UncoveredList string `json:"uncovered_list,omitempty"`
}

type fileCoverage struct {
	File       string  `json:"file"`
	Statements int     `json:"changed_statements"`
	Covered    int     `json:"changed_statements_executed"`
	Percent    float64 `json:"percent"`
}

type lineRange struct{ first, last int }

// changedLines returns the lines of each Go source file, other than tests,
// that differ between the two commits, as line numbers in the second.
func changedLines(repo, from, to string) (map[string][]lineRange, error) {
	out, err := mustCommand(repo, nil, "git", "diff", "-U0", "--no-color", "--no-renames", from, to, "--", "*.go")
	if err != nil {
		return nil, err
	}
	ret := make(map[string][]lineRange)
	var file string
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			if file == "/dev/null" || strings.HasSuffix(file, "_test.go") {
				file = ""
			}
		case strings.HasPrefix(line, "@@ ") && file != "":
			// @@ -a,b +c,d @@
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			start, count, found := strings.Cut(strings.TrimPrefix(fields[2], "+"), ",")
			first, _ := strconv.Atoi(start)
			n := 1
			if found {
				n, _ = strconv.Atoi(count)
			}
			if n > 0 {
				ret[file] = append(ret[file], lineRange{first, first + n - 1})
			}
		}
	}
	return ret, nil
}

func measureCoverage(t *toolchain) (*coverageReport, error) {
	base := filepath.Join(t.work, "coverage")
	var dirs []string
	for _, name := range []string{"terraform", "driver"} {
		if entries, err := os.ReadDir(filepath.Join(base, name)); err == nil && len(entries) > 0 {
			dirs = append(dirs, filepath.Join(base, name))
		}
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no coverage was recorded")
	}
	profile := filepath.Join(base, "profile.txt")
	if _, err := mustCommand(t.forkSrc, nil, "go", "tool", "covdata", "textfmt", "-i="+strings.Join(dirs, ","), "-o="+profile); err != nil {
		return nil, err
	}
	src, err := os.ReadFile(profile)
	if err != nil {
		return nil, err
	}
	changed, err := changedLines(t.repo, t.upstreamTree, t.forkTree)
	if err != nil {
		return nil, err
	}

	type block struct {
		file        string
		first, last int
	}
	statements := make(map[block]int)
	executed := make(map[block]bool)
	instrumented := make(map[string]bool)
	for _, line := range strings.Split(string(src), "\n") {
		// file:firstLine.col,lastLine.col statements count
		path, rest, ok := strings.Cut(line, ":")
		if !ok || !strings.HasPrefix(path, modulePath) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 3 {
			continue
		}
		from, to, _ := strings.Cut(fields[0], ",")
		first, _ := strconv.Atoi(strings.SplitN(from, ".", 2)[0])
		last, _ := strconv.Atoi(strings.SplitN(to, ".", 2)[0])
		n, _ := strconv.Atoi(fields[1])
		count, _ := strconv.Atoi(fields[2])
		b := block{strings.TrimPrefix(path, modulePath), first, last}
		instrumented[b.file] = true
		statements[b] = n
		if count > 0 {
			executed[b] = true
		}
	}

	report := &coverageReport{}
	perFile := make(map[string]*fileCoverage)
	var uncovered []string
	for b, n := range statements {
		touched := false
		for _, r := range changed[b.file] {
			if r.first <= b.last && b.first <= r.last {
				touched = true
				break
			}
		}
		if !touched {
			continue
		}
		fc := perFile[b.file]
		if fc == nil {
			fc = &fileCoverage{File: b.file}
			perFile[b.file] = fc
		}
		fc.Statements += n
		report.Statements += n
		if executed[b] {
			fc.Covered += n
			report.Covered += n
		} else {
			uncovered = append(uncovered, fmt.Sprintf("%s:%d-%d\t%d statements", b.file, b.first, b.last, n))
		}
	}
	for file := range changed {
		if !instrumented[file] && !strings.Contains(file, "/zzforkproof/") && !strings.HasPrefix(file, "fork-proof/") {
			report.NotInstrumented = append(report.NotInstrumented, file)
		}
	}
	sort.Strings(report.NotInstrumented)
	for _, fc := range perFile {
		if fc.Statements > 0 {
			fc.Percent = round1(100 * float64(fc.Covered) / float64(fc.Statements))
		}
		report.Files = append(report.Files, *fc)
	}
	sort.Slice(report.Files, func(i, j int) bool {
		if report.Files[i].Percent != report.Files[j].Percent {
			return report.Files[i].Percent < report.Files[j].Percent
		}
		return report.Files[i].File < report.Files[j].File
	})
	if report.Statements > 0 {
		report.Percent = round1(100 * float64(report.Covered) / float64(report.Statements))
	}
	sort.Strings(uncovered)
	report.UncoveredList = filepath.Join(base, "uncovered.txt")
	os.WriteFile(report.UncoveredList, []byte(strings.Join(uncovered, "\n")+"\n"), 0o644)
	return report, nil
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
