// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package getmodules

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	getter "github.com/hashicorp/go-getter"
)

// fakeGCS is just enough of the storage service to list and download the
// objects of one bucket.
type fakeGCS struct {
	mu       sync.Mutex
	objects  map[string][]byte
	fail     map[string]bool // object names whose downloads fail
	requests atomic.Int64
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()

	const listPath = "/storage/v1/b/mods/o"
	var name string
	switch {
	case r.URL.Path == listPath:
		prefix := r.URL.Query().Get("prefix")
		var names []string
		for name := range f.objects {
			if strings.HasPrefix(name, prefix) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		type item struct {
			Name   string `json:"name"`
			Bucket string `json:"bucket"`
			Size   string `json:"size"`
		}
		resp := struct {
			Kind  string `json:"kind"`
			Items []item `json:"items"`
		}{Kind: "storage#objects"}
		for _, name := range names {
			resp.Items = append(resp.Items, item{name, "mods", fmt.Sprint(len(f.objects[name]))})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	case strings.HasPrefix(r.URL.Path, listPath+"/"):
		name, _ = url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), listPath+"/"))
	case strings.HasPrefix(r.URL.Path, "/mods/"):
		name = strings.TrimPrefix(r.URL.Path, "/mods/")
	default:
		http.Error(w, "unexpected request "+r.URL.String(), http.StatusNotImplemented)
		return
	}
	data, ok := f.objects[name]
	if !ok || f.fail[name] {
		http.Error(w, "no such object", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}

func newFakeGCS(t *testing.T) *fakeGCS {
	t.Helper()
	moduleFiles := func(label string) map[string]string {
		return map[string]string{
			"main.tf":         "# main of " + label + "\n",
			"variables.tf":    "# variables of " + label + "\n",
			"nested/main.tf":  "# nested of " + label + "\n",
			"nested/a/b/c.tf": "# deep of " + label + "\n",
		}
	}
	fake := &fakeGCS{objects: map[string][]byte{}, fail: map[string]bool{}}
	for i := range 6 {
		for name, content := range moduleFiles(fmt.Sprintf("dir %d", i)) {
			fake.objects[fmt.Sprintf("dirs/mod%d/%s", i, name)] = []byte(content)
		}
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for name, content := range moduleFiles(fmt.Sprintf("zip %d", i)) {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			w.Write([]byte(content))
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		fake.objects[fmt.Sprintf("zips/mod%d.zip", i)] = buf.Bytes()
	}
	// A directory with many objects, to exercise concurrent downloads.
	for i := range 40 {
		fake.objects[fmt.Sprintf("dirs/big/file%02d.tf", i)] = []byte(fmt.Sprintf("# file %d\n", i))
	}
	fake.objects["dirs/empty/"] = nil

	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	t.Setenv("STORAGE_EMULATOR_HOST", strings.TrimPrefix(server.URL, "http://"))

	// Each test gets its own client, for its own server.
	gcsClients.Lock()
	gcsClients.client = nil
	gcsClients.Unlock()
	return fake
}

// readTree returns the content and mode of every file under the given
// directory, keyed by path.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	ret := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			ret[rel+"/"] = info.Mode().String()
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		ret[rel] = info.Mode().String() + " " + string(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ret
}

func sameTree(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("got %d entries; want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
		return
	}
	for name, wantEntry := range want {
		if got[name] != wantEntry {
			t.Errorf("wrong %s\ngot:  %q\nwant: %q", name, got[name], wantEntry)
		}
	}
}

const gcsTestAddr = "gcs::https://www.googleapis.com/storage/v1/mods/"

// Our getter must fetch exactly what go-getter's own fetches, and fail when
// it fails.
func TestGCSGetter(t *testing.T) {
	fake := newFakeGCS(t)

	fetch := func(t *testing.T, src string, gcs getter.Getter) (map[string]string, error) {
		t.Helper()
		dst := filepath.Join(t.TempDir(), "dst")
		getters := map[string]getter.Getter{"gcs": gcs}
		if err := fetchWithGoGetter(context.Background(), dst, src, getters); err != nil {
			return nil, err
		}
		return readTree(t, dst), nil
	}

	tests := map[string]struct {
		src     string
		wantErr bool
	}{
		"directory":           {gcsTestAddr + "dirs/mod1", false},
		"directory with many": {gcsTestAddr + "dirs/big", false},
		"archive":             {gcsTestAddr + "zips/mod2.zip", false},
		"missing archive":     {gcsTestAddr + "zips/missing.zip", true},
		"not a bucket URL":    {"gcs::https://example.com/storage/v1/mods/dirs/mod1", true},
		"too few parts":       {"gcs::https://www.googleapis.com/storage/v1/mods", true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			want, wantErr := fetch(t, test.src, new(getter.GCSGetter))
			got, err := fetch(t, test.src, new(gcsGetter))
			if (wantErr != nil) != test.wantErr {
				t.Fatalf("go-getter failed = %t (%v); the test expects %t", wantErr != nil, wantErr, test.wantErr)
			}
			if (err != nil) != (wantErr != nil) {
				t.Fatalf("wrong error\ngot:  %v\nwant: %v", err, wantErr)
			}
			if err != nil {
				return
			}
			if len(want) < 2 {
				t.Fatalf("go-getter fetched only %v, so this isn't testing anything", want)
			}
			sameTree(t, got, want)
		})
	}

	t.Run("one object fails", func(t *testing.T) {
		fake.mu.Lock()
		fake.fail["dirs/big/file17.tf"] = true
		fake.mu.Unlock()
		defer func() {
			fake.mu.Lock()
			delete(fake.fail, "dirs/big/file17.tf")
			fake.mu.Unlock()
		}()
		if _, err := fetch(t, gcsTestAddr+"dirs/big", new(getter.GCSGetter)); err == nil {
			t.Fatal("go-getter succeeded; the test expects it to fail")
		}
		if _, err := fetch(t, gcsTestAddr+"dirs/big", new(gcsGetter)); err == nil {
			t.Fatal("succeeded; want error")
		}
	})

	t.Run("existing destination is replaced", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "dst")
		if err := os.MkdirAll(filepath.Join(dst, "stale"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, "stale", "old.tf"), []byte("old"), 0644); err != nil {
			t.Fatal(err)
		}
		getters := map[string]getter.Getter{"gcs": new(gcsGetter)}
		if err := fetchWithGoGetter(context.Background(), dst, gcsTestAddr+"dirs/mod3", getters); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dst, "stale")); !os.IsNotExist(err) {
			t.Fatalf("stale content is still there: %v", err)
		}
	})
}

// Fetching ahead must give exactly what fetching on demand gives, with
// fewer fetches than requests when a package is used more than once.
func TestPackageFetcherPrefetch(t *testing.T) {
	fake := newFakeGCS(t)
	ctx := context.Background()

	addrs := []string{
		gcsTestAddr + "dirs/mod0", gcsTestAddr + "zips/mod0.zip", gcsTestAddr + "dirs/mod1",
		gcsTestAddr + "zips/mod1.zip", gcsTestAddr + "dirs/big", gcsTestAddr + "zips/mod2.zip",
	}
	// Some packages are asked for twice, as when two calls use one module.
	requests := append(append([]string{}, addrs...), addrs[0], addrs[3])

	install := func(t *testing.T, prefetch bool) []map[string]string {
		t.Helper()
		root := t.TempDir()
		staging := filepath.Join(root, ".prefetch")
		f := NewPackageFetcher()
		if prefetch {
			// Including things that can't be fetched ahead, which are ignored.
			f.Prefetch(ctx, staging, append([]string{"git::https://example.com/unused.git", "./local"}, addrs...))
			// Asking again changes nothing.
			f.Prefetch(ctx, staging, addrs)
		}
		var trees []map[string]string
		for i, addr := range requests {
			dst := filepath.Join(root, fmt.Sprintf("inst%d", i))
			if err := f.FetchPackage(ctx, dst, addr); err != nil {
				t.Fatalf("%s: %s", addr, err)
			}
			trees = append(trees, readTree(t, dst))
		}
		f.Close()
		if entries, _ := os.ReadDir(staging); len(entries) != 0 {
			t.Errorf("%d packages left in the staging directory", len(entries))
		}
		return trees
	}

	want := install(t, false)
	got := install(t, true)
	for i := range want {
		sameTree(t, got[i], want[i])
	}

	t.Run("failure", func(t *testing.T) {
		root := t.TempDir()
		f := NewPackageFetcher()
		defer f.Close()
		missing := gcsTestAddr + "zips/missing.zip"
		f.Prefetch(ctx, filepath.Join(root, ".prefetch"), []string{missing, addrs[0]})
		wantErr := NewPackageFetcher().FetchPackage(ctx, filepath.Join(root, "plain"), missing)
		err := f.FetchPackage(ctx, filepath.Join(root, "a"), missing)
		if err == nil || wantErr == nil {
			t.Fatalf("succeeded; want error (got %v, plain fetch got %v)", err, wantErr)
		}
		// The other package is unaffected.
		if err := f.FetchPackage(ctx, filepath.Join(root, "b"), addrs[0]); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unused", func(t *testing.T) {
		// Packages that nothing asks for are left in the staging directory
		// for the caller to remove, once Close has returned.
		root := t.TempDir()
		staging := filepath.Join(root, ".prefetch")
		f := NewPackageFetcher()
		f.Prefetch(ctx, staging, addrs)
		f.Close()
		entries, err := os.ReadDir(staging)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != len(addrs) {
			t.Errorf("%d packages in the staging directory; want %d", len(entries), len(addrs))
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		root := t.TempDir()
		f := NewPackageFetcher()
		f.Prefetch(ctx, filepath.Join(root, ".prefetch"), addrs)
		if err := f.FetchPackage(ctx, filepath.Join(root, "a"), addrs[0]); err == nil {
			t.Fatal("succeeded with a cancelled context")
		}
		f.Close()
	})

	if fake.requests.Load() == 0 {
		t.Fatal("the fake bucket was never used")
	}
}
