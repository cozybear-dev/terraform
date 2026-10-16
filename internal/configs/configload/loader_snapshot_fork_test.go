// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package configload

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/afero"
)

// scanOpen is how snapshotFS.Open found a file before it had a table of the
// modules' directories: by looking through every module of the snapshot for
// each file. It's kept here to compare the two.
func scanOpen(snap *Snapshot, name string) (afero.File, error) {
	dir := filepath.Dir(name)
	fn := filepath.Base(name)
	directDir := filepath.Clean(name)

	for _, candidate := range snap.Modules {
		modDir := filepath.Clean(candidate.Dir)
		if modDir == directDir {
			filenames := slices.Sorted(maps.Keys(candidate.Files))
			return &snapshotDir{
				filenames: filenames,
			}, nil
		}
	}

	var modSnap *SnapshotModule
	for _, candidate := range snap.Modules {
		modDir := filepath.Clean(candidate.Dir)
		if modDir == dir {
			modSnap = candidate
			break
		}
	}
	if modSnap == nil {
		return nil, os.ErrNotExist
	}

	src, exists := modSnap.Files[fn]
	if !exists {
		return nil, os.ErrNotExist
	}

	return &snapshotFile{
		src: src,
	}, nil
}

// describeOpen says what Open returned, in a form that can be compared.
func describeOpen(f afero.File, err error) string {
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "does not exist"
		}
		return "error: " + err.Error()
	}
	switch f := f.(type) {
	case *snapshotDir:
		return "directory of " + strings.Join(f.filenames, ", ")
	case *snapshotFile:
		return "file containing " + string(f.src)
	default:
		return fmt.Sprintf("unexpected %T", f)
	}
}

// snapshotForTest returns a snapshot with modules in the arrangements that
// the lookup has to tell apart: nested in each other's directories, in
// directories written in ways that filepath.Clean changes, and two calls of
// one local module, which Terraform records as two modules with the same
// directory and the same files. Each file's content says which module and
// file it is, so that finding the wrong one shows.
func snapshotForTest() *Snapshot {
	files := func(module string, names ...string) map[string][]byte {
		ret := make(map[string][]byte, len(names))
		for _, name := range names {
			ret[name] = []byte(module + "/" + name)
		}
		return ret
	}
	return &Snapshot{
		Modules: map[string]*SnapshotModule{
			"":              {Dir: ".", Files: files("root", "main.tf", "variables.tf", "outputs.tf")},
			"first":         {Dir: "modules/shared", Files: files("shared", "main.tf")},
			"second":        {Dir: "./modules/shared", Files: files("shared", "main.tf")},
			"first.nested":  {Dir: "modules/shared/nested", Files: files("nested", "main.tf", "outputs.tf")},
			"remote":        {Dir: ".terraform/modules/remote", Files: files("remote", "main.tf", "versions.tf")},
			"remote.child":  {Dir: ".terraform/modules/remote/modules/child/", Files: files("child", "child.tf")},
			"remote.uncln":  {Dir: ".terraform/modules/remote/../uncleaned", Files: files("uncleaned", "u.tf")},
			"empty":         {Dir: "modules/empty", Files: files("empty")},
			"module.tf.dir": {Dir: "modules/x.tf", Files: files("dotted", "main.tf")},
		},
	}
}

// pathsForTest returns the names Open is asked for: every module directory
// and file written several ways, files that aren't there, directories that
// aren't modules, and odd names.
func pathsForTest(snap *Snapshot) []string {
	var names []string
	for _, key := range slices.Sorted(maps.Keys(snap.Modules)) {
		mod := snap.Modules[key]
		for _, dir := range []string{mod.Dir, filepath.Clean(mod.Dir), mod.Dir + "/", "./" + mod.Dir, filepath.Join(mod.Dir, "..")} {
			names = append(names, dir)
			for _, name := range slices.Sorted(maps.Keys(mod.Files)) {
				names = append(names, dir+"/"+name, filepath.Join(dir, name), dir+"//"+name)
			}
			names = append(names, filepath.Join(dir, "missing.tf"), filepath.Join(dir, "nested"))
		}
	}
	names = append(names,
		"", ".", "./", "/", "..", "../main.tf", "main.tf", "./main.tf", "missing.tf",
		"modules", "modules/", ".terraform", ".terraform/modules", ".terraform/modules/uncleaned/u.tf",
		"modules/x.tf/main.tf", "modules/x.tf", "nothing/at/all.tf",
	)
	return names
}

func TestSnapshotFSOpenSameAsScan(t *testing.T) {
	snap := snapshotForTest()
	fs := newSnapshotFS(snap)
	for _, name := range pathsForTest(snap) {
		got := describeOpen(fs.Open(name))
		want := describeOpen(scanOpen(snap, name))
		if got != want {
			t.Errorf("Open(%q)\ngot:  %s\nwant: %s", name, got, want)
		}

		// Stat and OpenFile are made from Open.
		info, statErr := fs.Stat(name)
		_, wantErr := scanOpen(snap, name)
		if (statErr != nil) != (wantErr != nil) {
			t.Errorf("Stat(%q) returned error %v, and the scan %v", name, statErr, wantErr)
		}
		if statErr == nil {
			_, wantDir := mustScan(t, snap, name).(*snapshotDir)
			if info.IsDir() != wantDir {
				t.Errorf("Stat(%q).IsDir() is %t, want %t", name, info.IsDir(), wantDir)
			}
		}
		if got := describeOpen(fs.OpenFile(name, os.O_RDONLY, 0)); got != want {
			t.Errorf("OpenFile(%q)\ngot:  %s\nwant: %s", name, got, want)
		}
	}
}

func mustScan(t *testing.T, snap *Snapshot, name string) afero.File {
	t.Helper()
	f, err := scanOpen(snap, name)
	if err != nil {
		t.Fatalf("scanOpen(%q): %s", name, err)
	}
	return f
}

// With modules of their own for each module call, which is what modules
// from a registry or a bucket are, there are as many directories as calls.
func TestSnapshotFSOpenManyModules(t *testing.T) {
	snap := &Snapshot{Modules: map[string]*SnapshotModule{}}
	for i := range 2000 {
		key := fmt.Sprintf("app_%04d", i)
		snap.Modules[key] = &SnapshotModule{
			Dir:   ".terraform/modules/" + key,
			Files: map[string][]byte{"main.tf": []byte(key)},
		}
		snap.Modules[key+".inner"] = &SnapshotModule{
			Dir:   ".terraform/modules/" + key + "/inner",
			Files: map[string][]byte{"main.tf": []byte(key + " inner")},
		}
	}
	fs := newSnapshotFS(snap)
	for _, name := range []string{
		".terraform/modules/app_0000/main.tf",
		".terraform/modules/app_1999/inner/main.tf",
		".terraform/modules/app_1234",
		".terraform/modules/app_1234/inner",
		".terraform/modules/app_2000/main.tf",
		".terraform/modules/app_0001/other.tf",
	} {
		got := describeOpen(fs.Open(name))
		want := describeOpen(scanOpen(snap, name))
		if got != want {
			t.Errorf("Open(%q)\ngot:  %s\nwant: %s", name, got, want)
		}
	}
}

// Where two modules have the same directory and, unlike in a snapshot
// that Terraform writes, different files, the scan's answer depends on the
// order in which Go iterates over the map. The table's is always that of
// the module with the first key.
func TestSnapshotFSSameDirectoryTakesFirstKey(t *testing.T) {
	snap := &Snapshot{Modules: map[string]*SnapshotModule{
		"b": {Dir: "modules/m", Files: map[string][]byte{"main.tf": []byte("b"), "b.tf": []byte("b")}},
		"a": {Dir: "./modules/m/", Files: map[string][]byte{"main.tf": []byte("a")}},
	}}
	for range 20 {
		fs := newSnapshotFS(snap)
		if got, want := describeOpen(fs.Open("modules/m/main.tf")), "file containing a"; got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
		if got, want := describeOpen(fs.Open("modules/m")), "directory of main.tf"; got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
		if got, want := describeOpen(fs.Open("modules/m/b.tf")), "does not exist"; got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
}

func BenchmarkSnapshotFSOpen(b *testing.B) {
	snap := &Snapshot{Modules: map[string]*SnapshotModule{}}
	for i := range 5000 {
		key := fmt.Sprintf("app_%04d", i)
		snap.Modules[key] = &SnapshotModule{
			Dir:   ".terraform/modules/" + key,
			Files: map[string][]byte{"main.tf": nil, "variables.tf": nil, "outputs.tf": nil},
		}
	}
	fs := newSnapshotFS(snap)
	b.ResetTimer()
	for i := range b.N {
		key := fmt.Sprintf("app_%04d", i%5000)
		if _, err := fs.Open(".terraform/modules/" + key + "/main.tf"); err != nil {
			b.Fatal(err)
		}
	}
}
