// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package lang

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// Scopes that share a results table share a function table, and one scope
// can be calling a template function from that table while another is still
// in the middle of obtaining it. This is for the race detector: without it
// the test passes whatever happens.
func TestScopeFunctions_sharedTableConcurrentUse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "greeting.tftpl"), []byte("hello ${upper(name)}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		results := NewFunctionResultsTable(nil)
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				scope := &Scope{BaseDir: dir, FunctionResults: results}
				fn := scope.Functions()["templatefile"]
				got, err := fn.Call([]cty.Value{
					cty.StringVal("greeting.tftpl"),
					cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("world")}),
				})
				if err != nil {
					t.Error(err)
					return
				}
				if want := cty.StringVal("hello WORLD"); !got.RawEquals(want) {
					t.Errorf("got %#v; want %#v", got, want)
				}
			}()
		}
		wg.Wait()
	}
}
