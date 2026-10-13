// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package marks

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestSameAsMarking(t *testing.T) {
	a, b, c := cty.GetAttrPath("a"), cty.GetAttrPath("b"), cty.GetAttrPath("c").IndexInt(0)
	sensitive := func(paths ...cty.Path) []cty.PathValueMarks {
		var ret []cty.PathValueMarks
		for _, path := range paths {
			ret = append(ret, cty.PathValueMarks{Path: path, Marks: cty.NewValueMarks(Sensitive)})
		}
		return ret
	}
	other := cty.PathValueMarks{Path: a, Marks: cty.NewValueMarks(Sensitive, "other")}

	tests := map[string]struct {
		want, pvms []cty.PathValueMarks
		sensitive  []cty.Path
		same       bool
	}{
		"nothing":                       {nil, nil, nil, true},
		"same marks":                    {sensitive(a, c), sensitive(c, a), nil, true},
		"all from schema":               {sensitive(a, c), nil, []cty.Path{c, a}, true},
		"some from each":                {sensitive(a, b), sensitive(b), []cty.Path{a}, true},
		"overlapping":                   {sensitive(a, b), sensitive(a, b), []cty.Path{a}, true},
		"missing mark":                  {sensitive(a, b), sensitive(b), nil, false},
		"additional mark":               {sensitive(a), sensitive(a, b), nil, false},
		"additional sensitive path":     {sensitive(a), sensitive(a), []cty.Path{b}, false},
		"different path":                {sensitive(a), sensitive(b), nil, false},
		"different marks at same path":  {[]cty.PathValueMarks{other}, sensitive(a), nil, false},
		"other mark kept with schema":   {[]cty.PathValueMarks{other}, []cty.PathValueMarks{{Path: a, Marks: cty.NewValueMarks("other")}}, []cty.Path{a}, true},
		"other mark without sensitive":  {[]cty.PathValueMarks{other}, []cty.PathValueMarks{{Path: a, Marks: cty.NewValueMarks("other")}}, nil, false},
		"unwanted marks, nothing given": {nil, sensitive(a), nil, false},
		"unwanted path, nothing given":  {nil, nil, []cty.Path{a}, false},
	}
	val := cty.ObjectVal(map[string]cty.Value{
		"a": cty.StringVal("a"),
		"b": cty.StringVal("b"),
		"c": cty.ListVal([]cty.Value{cty.StringVal("c")}),
	})
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := SameAsMarking(test.want, test.pvms, test.sensitive)
			if got != test.same {
				t.Errorf("wrong result %t; want %t", got, test.same)
			}
			// The result must agree with actually applying the marks.
			wantVal := val.MarkWithPaths(test.want)
			gotVal := MarkPaths(val.MarkWithPaths(test.pvms), Sensitive, test.sensitive)
			if actual := gotVal.RawEquals(wantVal); actual != test.same {
				t.Errorf("result %t disagrees with marking the value, which gives %t", got, actual)
			}
		})
	}
}
