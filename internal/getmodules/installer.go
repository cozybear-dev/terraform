// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package getmodules

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// PackageFetcher is a low-level utility for fetching remote module packages
// into local filesystem directories in preparation for use by higher-level
// module installer functionality implemented elsewhere.
//
// A PackageFetcher works only with entire module packages and never with
// the individual modules within a package.
//
// A particular PackageFetcher instance remembers the target directory of
// any successfully-installed package so that it can optimize future calls
// that have the same package address by copying the local directory tree,
// rather than fetching the package from its origin repeatedly. There is
// no way to reset this cache, so a particular PackageFetcher instance should
// live only for the duration of a single initialization process.
type PackageFetcher struct {
	getter reusingGetter

	// The remaining fields are for packages that are being fetched ahead
	// of when they're asked for. See Prefetch.
	mu         sync.Mutex
	prefetched map[string]*prefetchedPackage
	prefetches sync.WaitGroup
	slots      chan struct{}
	staged     int
}

// prefetchedPackage is a package that Prefetch is fetching or has fetched.
type prefetchedPackage struct {
	// dir is where the package is being fetched to, and err is the result
	// of doing so, both of which are available once done is closed.
	dir  string
	done chan struct{}
	err  error
}

// prefetchConcurrency is how many packages are fetched ahead at once.
const prefetchConcurrency = 16

func NewPackageFetcher() *PackageFetcher {
	return &PackageFetcher{
		getter:     reusingGetter{},
		prefetched: make(map[string]*prefetchedPackage),
		slots:      make(chan struct{}, prefetchConcurrency),
	}
}

// Prefetch starts fetching the packages at the given addresses into
// subdirectories of the given staging directory, in the background and
// several at a time, so that a later call to FetchPackage for any of them
// only has to move the result into place.
//
// Installing modules is otherwise entirely sequential, because which
// modules a module calls can only be known once it has been fetched. For a
// configuration with many module calls that makes the time taken the sum of
// all of the individual fetches, nearly all of which is spent waiting for
// the network. The calls in one module are all known at once, though, so
// the caller can tell us about them as soon as it has loaded that module.
//
// Only packages of kinds that are known to be safe to fetch concurrently
// are fetched ahead, and the rest are ignored here, as are any that have
// already been fetched or are being fetched. The addresses are in the same
// form as FetchPackage expects.
//
// The caller must call Close once it has finished with the fetcher, and
// must not use the staging directory for anything else.
func (f *PackageFetcher) Prefetch(ctx context.Context, stagingDir string, packageAddrs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, packageAddr := range packageAddrs {
		if !canPrefetch(packageAddr) {
			continue
		}
		if _, exists := f.getter[packageAddr]; exists {
			continue
		}
		if _, exists := f.prefetched[packageAddr]; exists {
			continue
		}
		f.staged++
		p := &prefetchedPackage{
			dir:  filepath.Join(stagingDir, strconv.Itoa(f.staged)),
			done: make(chan struct{}),
		}
		f.prefetched[packageAddr] = p
		f.prefetches.Add(1)
		go func() {
			defer f.prefetches.Done()
			defer close(p.done)
			select {
			case f.slots <- struct{}{}:
				defer func() { <-f.slots }()
			case <-ctx.Done():
				p.err = ctx.Err()
				return
			}
			p.err = fetchWithGoGetter(ctx, p.dir, packageAddr, prefetchGetters())
		}()
	}
}

// Close waits for any fetches that Prefetch started and that nothing has
// asked for, whose results the caller is then free to delete.
func (f *PackageFetcher) Close() {
	f.prefetches.Wait()
}

// FetchPackage downloads or otherwise retrieves the filesystem inside the
// package at the given address into the given local installation directory.
//
// packageAddr must be formatted as if it were the result of an
// addrs.ModulePackage.String() call. It's only defined as a raw string here
// because the getmodules package can't import the addrs package due to
// that creating a package dependency cycle.
//
// PackageFetcher only works with entire packages. If the caller is processing
// a module source address which includes a subdirectory portion then the
// caller must resolve that itself, possibly with the help of the
// getmodules.SplitPackageSubdir and getmodules.ExpandSubdirGlobs functions.
func (f *PackageFetcher) FetchPackage(ctx context.Context, instDir string, packageAddr string) error {
	f.mu.Lock()
	_, fetched := f.getter[packageAddr]
	p := f.prefetched[packageAddr]
	f.mu.Unlock()

	if p != nil && !fetched {
		// This is the first request for a package that we started fetching
		// earlier, so we wait for that and then move the result into place.
		// From then on it's like any other package we've already fetched.
		<-p.done
		if p.err != nil {
			return p.err
		}
		if err := os.Rename(p.dir, instDir); err != nil {
			return fmt.Errorf("failed to move %s to %s: %s", p.dir, instDir, err)
		}
		f.mu.Lock()
		f.getter[packageAddr] = instDir
		f.mu.Unlock()
		return nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getter.getWithGoGetter(ctx, instDir, packageAddr)
}
