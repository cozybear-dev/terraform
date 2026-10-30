// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// toolchain is everything the proof builds before it compares anything.
type toolchain struct {
	work string // absolute path of the working directory
	repo string // absolute path of the repository that contains the fork
	self string // absolute path of the directory that contains the proof

	upstreamRef, upstreamCommit string
	// upstreamTree is what the fork is compared with: the tree of
	// upstreamCommit, or that tree with the changes of the backports
	// applied to it.
	upstreamTree        string
	backports           []backport
	forkRef, forkCommit string
	// forkTree is the commit whose tree the fork was built from, which
	// differs from forkCommit only when the working tree was used.
	forkTree  string
	forkDirty bool
	// plannerCommit is a commit of the full fork, when the fork being
	// proven is a build only for applying the plans that the full fork
	// saves. Two more actors then apply plans saved by a build of it.
	plannerCommit string

	goVersion string

	upstreamSrc, forkSrc, plannerSrc string

	// The executables, by what they are.
	upstream, fork, cover string
	driverUpstream        string
	driverFork            string
	driverCover           string
	// planner is empty unless there is a plannerCommit.
	planner string

	cliConfig string
}

// command runs a command and returns what it printed and its exit code. An
// error is returned only if the command couldn't be run at all.
func command(dir string, env []string, name string, args ...string) ([]byte, int, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return out.Bytes(), exit.ExitCode(), nil
	}
	if err != nil {
		return out.Bytes(), -1, err
	}
	return out.Bytes(), 0, nil
}

func mustCommand(dir string, env []string, name string, args ...string) ([]byte, error) {
	out, rc, err := command(dir, env, name, args...)
	if err != nil {
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	if rc != 0 {
		return out, fmt.Errorf("%s %s: exit status %d\n%s", name, strings.Join(args, " "), rc, tail(out, 40))
	}
	return out, nil
}

func tail(out []byte, lines int) string {
	all := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

func gitOutput(dir string, args ...string) (string, error) {
	out, err := mustCommand(dir, nil, "git", args...)
	return strings.TrimSpace(string(out)), err
}

// exportTree extracts the tree of the given commit into the given
// directory, unless that has already been done.
func exportTree(repo, commit, dir string) error {
	marker := filepath.Join(dir, ".forkproof-commit")
	if got, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(got)) == commit {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	archive := exec.Command("git", "archive", "--format=tar", commit)
	archive.Dir = repo
	extract := exec.Command("tar", "-x", "-C", dir)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return err
	}
	extract.Stdin = pipe
	var stderr bytes.Buffer
	archive.Stderr = &stderr
	extract.Stderr = &stderr
	if err := extract.Start(); err != nil {
		return err
	}
	if err := archive.Run(); err != nil {
		return fmt.Errorf("git archive %s: %w\n%s", commit, err, stderr.String())
	}
	if err := extract.Wait(); err != nil {
		return fmt.Errorf("extracting %s: %w\n%s", commit, err, stderr.String())
	}
	return os.WriteFile(marker, []byte(commit+"\n"), 0o644)
}

// copyTree copies a directory and everything in it.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			// Terraform links to a provider in a mirror directory rather
			// than copying it.
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			os.Remove(target)
			return os.Symlink(link, target)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, info.Mode().Perm())
	})
}

// backport is a commit of upstream's, later than the one the fork is based
// on, that the fork also carries, and that is therefore applied to upstream
// before the two are compared.
type backport struct {
	Commit  string `json:"commit"`
	Subject string `json:"subject"`
}

// backportedTree returns the tree that results from applying the changes
// made by each of the given commits, in order, to the tree of the given
// commit. It writes a tree object and changes nothing else in the
// repository.
func backportedTree(repo, work, commit string, commits []string) (string, []backport, error) {
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", nil, err
	}
	index := filepath.Join(work, "reference.index")
	os.Remove(index)
	defer os.Remove(index)
	env := append(os.Environ(), "GIT_INDEX_FILE="+index)
	if _, err := mustCommand(repo, env, "git", "read-tree", commit); err != nil {
		return "", nil, err
	}
	var backports []backport
	for _, c := range commits {
		full, err := gitOutput(repo, "rev-parse", "--verify", c+"^{commit}")
		if err != nil {
			return "", nil, fmt.Errorf("the backported commit %s isn't in this repository; fetch it and its parent with\n  git fetch --depth=2 https://github.com/hashicorp/terraform %s\n%w", c, c, err)
		}
		subject, err := gitOutput(repo, "log", "-1", "--format=%s", full)
		if err != nil {
			return "", nil, err
		}
		patch, err := mustCommand(repo, nil, "git", "diff-tree", "-p", "--binary", "--full-index", "--no-renames", full+"^", full)
		if err != nil {
			return "", nil, fmt.Errorf("the parent of the backported commit %s isn't in this repository: %w", c, err)
		}
		apply := exec.Command("git", "apply", "--cached", "--whitespace=nowarn", "-")
		apply.Dir, apply.Env, apply.Stdin = repo, env, bytes.NewReader(patch)
		if out, err := apply.CombinedOutput(); err != nil {
			return "", nil, fmt.Errorf("the backported commit %s (%s) doesn't apply to upstream at %s. If upstream now includes it, take it out of UPSTREAM_BACKPORTS.\n%s", short(full), subject, short(commit), tail(out, 10))
		}
		backports = append(backports, backport{Commit: full, Subject: subject})
	}
	tree, err := mustCommand(repo, env, "git", "write-tree")
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(string(tree)), backports, nil
}

type buildOptions struct {
	work, upstreamRef, forkRef string
	// backports are the commits of upstream's to apply to upstreamRef
	// before comparing the fork with it.
	backports            []string
	upstreamBin, forkBin string
	coverage, skipBuild  bool

	// plannerRef is the commit of the full fork that saves the plans of
	// the two actors that check a build for applying them, or empty.
	plannerRef string
}

// prepare builds both versions of Terraform, the provider and the component
// drivers, and writes the CLI configuration that makes Terraform install the
// provider from a directory rather than from a registry.
func prepare(opts buildOptions, logf func(string, ...any)) (*toolchain, error) {
	t := &toolchain{}
	var err error
	if t.work, err = filepath.Abs(opts.work); err != nil {
		return nil, err
	}
	if t.self, err = os.Getwd(); err != nil {
		return nil, err
	}
	if t.repo, err = gitOutput(t.self, "rev-parse", "--show-toplevel"); err != nil {
		return nil, err
	}

	t.upstreamRef = opts.upstreamRef
	if t.upstreamCommit, err = gitOutput(t.repo, "rev-parse", "--verify", t.upstreamRef+"^{commit}"); err != nil {
		return nil, fmt.Errorf("the upstream commit %s isn't in this repository; fetch it with\n  git fetch https://github.com/hashicorp/terraform %s\n%w", t.upstreamRef, t.upstreamRef, err)
	}
	t.upstreamTree = t.upstreamCommit
	if len(opts.backports) != 0 {
		if t.upstreamTree, t.backports, err = backportedTree(t.repo, t.work, t.upstreamCommit, opts.backports); err != nil {
			return nil, err
		}
	}
	t.forkRef = opts.forkRef
	if t.forkRef == "WORKTREE" {
		// The tracked files as they are now, committed or not. This makes
		// a commit object but changes neither the index nor any branch.
		if t.forkCommit, err = gitOutput(t.repo, "rev-parse", "HEAD"); err != nil {
			return nil, err
		}
		stash, err := gitOutput(t.repo, "stash", "create")
		if err != nil {
			return nil, err
		}
		t.forkTree, t.forkDirty = t.forkCommit, false
		if stash != "" {
			t.forkTree, t.forkDirty = stash, true
		}
	} else {
		if t.forkCommit, err = gitOutput(t.repo, "rev-parse", "--verify", t.forkRef+"^{commit}"); err != nil {
			return nil, err
		}
		t.forkTree = t.forkCommit
	}
	if opts.plannerRef != "" {
		if t.plannerCommit, err = gitOutput(t.repo, "rev-parse", "--verify", opts.plannerRef+"^{commit}"); err != nil {
			return nil, fmt.Errorf("the full fork's commit %s isn't in this repository: %w", opts.plannerRef, err)
		}
	}

	goVersion, err := mustCommand(t.self, nil, "go", "version")
	if err != nil {
		return nil, err
	}
	t.goVersion = strings.TrimSpace(string(goVersion))

	bin := filepath.Join(t.work, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return nil, err
	}
	t.upstream = filepath.Join(bin, "terraform-upstream")
	t.fork = filepath.Join(bin, "terraform-fork")
	t.driverUpstream = filepath.Join(bin, "driver-upstream")
	t.driverFork = filepath.Join(bin, "driver-fork")
	if opts.coverage {
		t.cover = filepath.Join(bin, "terraform-fork-cover")
		t.driverCover = filepath.Join(bin, "driver-fork-cover")
	}
	mirror := filepath.Join(t.work, "mirror")
	provider := filepath.Join(mirror, "registry.terraform.io", "hashicorp", "fp", "1.0.0", runtime.GOOS+"_"+runtime.GOARCH, "terraform-provider-fp_v1.0.0")

	t.upstreamSrc = filepath.Join(t.work, "src", "upstream")
	t.forkSrc = filepath.Join(t.work, "src", "fork")
	if t.plannerCommit != "" {
		t.planner = filepath.Join(bin, "terraform-planner")
		t.plannerSrc = filepath.Join(t.work, "src", "planner")
	}

	if !opts.skipBuild {
		logf("extracting upstream %s and fork %s", short(t.upstreamCommit), short(t.forkTree))
		for _, b := range t.backports {
			logf("upstream has %s applied to it: %s", short(b.Commit), b.Subject)
		}
		if err := exportTree(t.repo, t.upstreamTree, t.upstreamSrc); err != nil {
			return nil, err
		}
		if err := exportTree(t.repo, t.forkTree, t.forkSrc); err != nil {
			return nil, err
		}
		if t.planner != "" {
			logf("extracting the full fork %s, which saves the plans that two more actors apply", short(t.plannerCommit))
			if err := exportTree(t.repo, t.plannerCommit, t.plannerSrc); err != nil {
				return nil, err
			}
		}
		// The provider is built only in upstream's tree, and the driver in
		// both. Neither tree has anything else added to it.
		overlay := filepath.Join(t.self, "_overlay")
		if err := copyTree(filepath.Join(overlay, "provider"), filepath.Join(t.upstreamSrc, "internal", "zzforkproof", "provider")); err != nil {
			return nil, err
		}
		for _, src := range []string{t.upstreamSrc, t.forkSrc} {
			if err := copyTree(filepath.Join(overlay, "driver"), filepath.Join(src, "internal", "zzforkproof", "driver")); err != nil {
				return nil, err
			}
		}

		env := append(os.Environ(), "CGO_ENABLED=0")
		build := func(what, src, out string, args ...string) error {
			logf("building %s", what)
			args = append([]string{"build", "-trimpath", "-buildvcs=false", "-o", out}, args...)
			_, err := mustCommand(src, env, "go", args...)
			return err
		}
		if err := os.MkdirAll(filepath.Dir(provider), 0o755); err != nil {
			return nil, err
		}
		if opts.upstreamBin == "" {
			if err := build("upstream terraform", t.upstreamSrc, t.upstream, "."); err != nil {
				return nil, err
			}
		}
		if opts.forkBin == "" {
			if err := build("fork terraform", t.forkSrc, t.fork, "."); err != nil {
				return nil, err
			}
		}
		if t.planner != "" {
			// Built the same way as the others, so that it records the same
			// version in its plans as they accept.
			if err := build("full fork terraform, which plans", t.plannerSrc, t.planner, "."); err != nil {
				return nil, err
			}
		}
		if err := build("provider, from upstream's source", t.upstreamSrc, provider, "./internal/zzforkproof/provider"); err != nil {
			return nil, err
		}
		if err := build("component driver, from upstream's source", t.upstreamSrc, t.driverUpstream, "./internal/zzforkproof/driver"); err != nil {
			return nil, err
		}
		if err := build("component driver, from the fork's source", t.forkSrc, t.driverFork, "./internal/zzforkproof/driver"); err != nil {
			return nil, err
		}
		if opts.coverage {
			coverpkg := "-coverpkg=github.com/hashicorp/terraform/..."
			if err := build("fork terraform, instrumented for coverage", t.forkSrc, t.cover, "-cover", coverpkg, "."); err != nil {
				return nil, err
			}
			if err := build("component driver, instrumented for coverage", t.forkSrc, t.driverCover, "-cover", coverpkg, "./internal/zzforkproof/driver"); err != nil {
				return nil, err
			}
		}
	}
	if opts.upstreamBin != "" {
		if t.upstream, err = filepath.Abs(opts.upstreamBin); err != nil {
			return nil, err
		}
	}
	if opts.forkBin != "" {
		if t.fork, err = filepath.Abs(opts.forkBin); err != nil {
			return nil, err
		}
	}
	executables := []string{t.upstream, t.fork, provider, t.driverUpstream, t.driverFork}
	if t.planner != "" {
		executables = append(executables, t.planner)
	}
	for _, path := range executables {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("%s hasn't been built: %w", path, err)
		}
	}

	t.cliConfig = filepath.Join(t.work, "terraformrc")
	config := fmt.Sprintf(`# Written by the fork's integrity proof. The provider is installed from
# a directory, and nothing is fetched from anywhere else.
disable_checkpoint = true
provider_installation {
  filesystem_mirror {
    path    = %q
    include = ["registry.terraform.io/hashicorp/fp"]
  }
  direct {
    exclude = ["registry.terraform.io/hashicorp/fp"]
  }
}
`, mirror)
	if err := os.WriteFile(t.cliConfig, []byte(config), 0o644); err != nil {
		return nil, err
	}
	return t, nil
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// fileDigest returns the SHA-256 of a file.
func fileDigest(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}

// treeDigest returns a digest of the names and content of every file under
// the given paths, which are relative to the directory of the proof. It
// identifies the corpus and the rules that a report was produced with.
func treeDigest(root string, paths ...string) (string, error) {
	var files []string
	for _, p := range paths {
		err := filepath.WalkDir(filepath.Join(root, p), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sort.Strings(files)
	h := sha256.New()
	for _, path := range files {
		rel, _ := filepath.Rel(root, path)
		fmt.Fprintf(h, "%s\x00%s\n", filepath.ToSlash(rel), fileDigest(path))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
