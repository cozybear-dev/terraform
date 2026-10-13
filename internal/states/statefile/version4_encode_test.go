// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statefile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/states"
)

// testStateV4 builds a pseudo-random stateV4 with a variety of shapes of
// resource and instance, including the ones that exercise omitempty rules.
func testStateV4(rnd *rand.Rand, resources int) *stateV4 {
	sV4 := &stateV4{
		TerraformVersion: "1.2.3",
		Serial:           uint64(rnd.Intn(100)),
		Lineage:          "00000000-0000-0000-0000-000000000000",
		RootOutputs: map[string]outputStateV4{
			"resources": {
				ValueRaw:     json.RawMessage(`{"resources":[],"instances":[]}`),
				ValueTypeRaw: json.RawMessage(`["object",{"instances":["list","string"],"resources":["list","string"]}]`),
			},
			"html": {
				ValueRaw:     json.RawMessage(`"<a href=\"x\">&</a>"`),
				ValueTypeRaw: json.RawMessage(`"string"`),
				Sensitive:    true,
			},
		},
		Resources: []resourceStateV4{},
	}
	if rnd.Intn(2) == 0 {
		sV4.CheckResults = []checkResultsV4{
			{ObjectKind: "resource", ConfigAddr: "test_thing.a", Status: "pass", Objects: []checkResultsObjectV4{
				{ObjectAddr: "test_thing.a", Status: "fail", FailureMessages: []string{"\n  \"resources\": [],\n"}},
			}},
		}
	}
	sharedDependencies := []string{"test_thing.shared_a", "module.m.test_thing.shared_b", "data.test_thing.shared_c"}
	for i := 0; i < resources; i++ {
		rs := resourceStateV4{
			Mode:           "managed",
			Type:           "test_thing",
			Name:           fmt.Sprintf("r%d", i),
			ProviderConfig: `provider["registry.terraform.io/hashicorp/test"]`,
		}
		if rnd.Intn(2) == 0 {
			rs.Module = fmt.Sprintf("module.m[%q]", "<&>")
		}
		if rnd.Intn(3) == 0 {
			rs.EachMode = "map"
		}
		switch rnd.Intn(8) {
		case 0:
			rs.Instances = nil
		case 1:
			rs.Instances = []instanceObjectStateV4{}
		default:
			for j, n := 0, 1+rnd.Intn(5); j < n; j++ {
				is := instanceObjectStateV4{
					SchemaVersion: uint64(rnd.Intn(3)),
				}
				switch rnd.Intn(3) {
				case 0:
					is.IndexKey = j
				case 1:
					is.IndexKey = fmt.Sprintf("key<%d>", j)
				}
				switch rnd.Intn(7) {
				case 0:
					is.AttributesFlat = map[string]string{"id": "foo", "b.#": "1", "a": "\n      \"instances\": []\n    }"}
				case 1:
					is.AttributesRaw = json.RawMessage(`{}`)
				case 2:
					// Large enough to be formatted directly, in the indented
					// form that attributes have when read from a state file,
					// and with content that could be mistaken for the
					// structure around it.
					is.AttributesRaw = json.RawMessage(fmt.Sprintf(
						"{\n            \"id\": \"i-%d\",\n            \"attributes\": 0,\n            \"nested\": {\n              \"attributes\": 0,\n              \"list\": [\n                1,\n                2.5e-3,\n                true,\n                null,\n                [],\n                {}\n              ]\n            },\n            \"text\": %q,\n            \"padding\": %q\n          }",
						j, "\n          \"attributes\": 0,\n <&> \u2028 \u2029 \u00e9 \U0001f389 \t \\ \" /", strings.Repeat("x", 300),
					))
				case 3:
					// The same kind of thing in compact form.
					is.AttributesRaw = json.RawMessage(fmt.Sprintf(
						`{"id":"i-%d","attributes":0,"html":"<a href=\"x\">&amp;</a>","dependencies":["a","b"],"create_before_destroy":true,"list":[{"a":[[]],"b":{}},{"a":[[1,2],[3]],"b":{"c":null}}],"padding":%q}`,
						j, strings.Repeat("y ", 200),
					))
				default:
					is.AttributesRaw = json.RawMessage(fmt.Sprintf(
						`{"id":"i-%d","nested":{"list":[1,2.5,true,null,[],{}],"html":"<&> ","instances":[]},"empty":"","tags":null}`, j,
					))
				}
				if rnd.Intn(2) == 0 {
					is.AttributeSensitivePaths = json.RawMessage(`[[{"type":"get_attr","value":"id"}]]`)
				}
				if rnd.Intn(2) == 0 {
					is.IdentityRaw = json.RawMessage(`{"id":"i"}`)
					is.IdentitySchemaVersion = 1
				}
				if rnd.Intn(2) == 0 {
					is.PrivateRaw = []byte("private\x00bytes")
				}
				switch rnd.Intn(4) {
				case 0:
					is.Dependencies = newDependenciesV4([]string{"test_thing.a", "module.m.test_thing.b"})
				case 1:
					is.Dependencies = newDependenciesV4(sharedDependencies)
				case 2:
					is.Dependencies = newDependenciesV4(sharedDependencies[:1])
				}
				if rnd.Intn(4) == 0 {
					is.Status = "tainted"
				}
				if rnd.Intn(4) == 0 {
					is.Deposed = "00000001"
				}
				is.CreateBeforeDestroy = rnd.Intn(2) == 0
				rs.Instances = append(rs.Instances, is)
			}
		}
		sV4.Resources = append(sV4.Resources, rs)
	}
	return sV4
}

func TestEncodeStateV4(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for _, resources := range []int{0, 1, 5, 30, 100, 1000} {
		t.Run(fmt.Sprintf("%d resources", resources), func(t *testing.T) {
			sV4 := testStateV4(rnd, resources)

			want, err := json.MarshalIndent(sV4, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, '\n')

			var got bytes.Buffer
			if err := encodeStateV4(sV4, &got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("wrong result\ngot:\n%s\nwant:\n%s", got.Bytes(), want)
			}
		})
	}
}

func TestEncodeStateV4_invalid(t *testing.T) {
	invalid := map[string]string{
		"small":     `{invalid`,
		"large":     `{"padding":"` + strings.Repeat("x", 1000) + `",invalid}`,
		"truncated": `{"padding":"` + strings.Repeat("x", 1000) + `"`,
		"trailing":  `{"padding":"` + strings.Repeat("x", 1000) + `"}}`,
	}
	for name, attrs := range invalid {
		t.Run(name, func(t *testing.T) {
			sV4 := testStateV4(rand.New(rand.NewSource(1)), 200)
			sV4.Resources[len(sV4.Resources)-1].Instances = []instanceObjectStateV4{
				{AttributesRaw: json.RawMessage(attrs)},
			}

			_, wantErr := json.MarshalIndent(sV4, "", "  ")
			if wantErr == nil {
				t.Fatal("test state is unexpectedly valid")
			}
			err := encodeStateV4(sV4, &bytes.Buffer{})
			if err == nil {
				t.Fatal("succeeded; want error")
			}
			if got, want := err.Error(), wantErr.Error(); got != want {
				t.Fatalf("wrong error\ngot:  %s\nwant: %s", got, want)
			}
		})
	}
}

// Each object must be encoded exactly as the standard library would encode
// it, whichever way we go about that.
func TestMarshalInstanceObjectV4(t *testing.T) {
	sV4 := testStateV4(rand.New(rand.NewSource(2)), 2000)
	var dependencySrcs sync.Map
	direct := 0
	for i := range sV4.Resources {
		for j := range sV4.Resources[i].Instances {
			is := &sV4.Resources[i].Instances[j]
			want, err := json.MarshalIndent(is, "        ", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got, err := marshalInstanceObjectV4(is, &dependencySrcs)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("wrong result\ngot:\n%s\nwant:\n%s", got, want)
			}
			if len(is.AttributesRaw) >= minDirectAttributesV4 {
				direct++
			}
		}
	}
	if direct < 100 {
		t.Fatalf("only %d objects have attributes large enough to be formatted directly", direct)
	}
}

func BenchmarkEncodeStateV4(b *testing.B) {
	sV4 := testStateV4(rand.New(rand.NewSource(1)), 20000)
	b.Run("parallel", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var buf bytes.Buffer
			if err := encodeStateV4(sV4, &buf); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(buf.Len()))
		}
	})
	b.Run("simple", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var buf bytes.Buffer
			if err := encodeStateV4Simple(sV4, &buf); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(buf.Len()))
		}
	})
}

func TestStatesDefinitelyDiffer(t *testing.T) {
	// A state as read from a file, and variations of it. Whenever
	// StatesDefinitelyDiffer says that two states differ, their
	// serializations must really differ.
	src := testStateV4Src(t, 300)
	read := func() *File {
		t.Helper()
		f, err := Read(bytes.NewReader(src))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	base := read().State
	marshal := func(s *states.State) string {
		var buf bytes.Buffer
		if err := Write(&File{State: s}, &buf); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	baseSrc := marshal(base)

	variants := map[string]func(s *states.State){
		"unchanged":     func(s *states.State) {},
		"separate copy": func(s *states.State) {},
		"whitespace only": func(s *states.State) {
			for _, ms := range s.Modules {
				for _, rs := range ms.Resources {
					for _, is := range rs.Instances {
						if is.Current != nil && len(is.Current.AttrsJSON) > 2 {
							is.Current.AttrsJSON = append([]byte(" "), is.Current.AttrsJSON...)
						}
					}
				}
			}
		},
		"attribute changed": func(s *states.State) {
			for _, ms := range s.Modules {
				for _, rs := range ms.Resources {
					for _, is := range rs.Instances {
						if is.Current != nil {
							is.Current.AttrsJSON = []byte(`{"id":"changed"}`)
							return
						}
					}
				}
			}
		},
		"status changed": func(s *states.State) {
			for _, ms := range s.Modules {
				for _, rs := range ms.Resources {
					for _, is := range rs.Instances {
						if is.Current != nil && is.Current.Status == states.ObjectReady {
							is.Current.Status = states.ObjectTainted
							return
						}
					}
				}
			}
		},
		"instance removed": func(s *states.State) {
			for _, ms := range s.Modules {
				for _, rs := range ms.Resources {
					for key := range rs.Instances {
						delete(rs.Instances, key)
						return
					}
				}
			}
		},
		"resource removed": func(s *states.State) {
			for _, ms := range s.Modules {
				for key := range ms.Resources {
					delete(ms.Resources, key)
					return
				}
			}
		},
		"output removed": func(s *states.State) {
			for name := range s.RootOutputValues {
				delete(s.RootOutputValues, name)
				return
			}
		},
		"dependencies changed": func(s *states.State) {
			for _, ms := range s.Modules {
				for _, rs := range ms.Resources {
					for _, is := range rs.Instances {
						if is.Current != nil {
							is.Current.Dependencies = nil
						}
					}
				}
			}
		},
		"private changed": func(s *states.State) {
			for _, ms := range s.Modules {
				for _, rs := range ms.Resources {
					for _, is := range rs.Instances {
						if is.Current != nil {
							is.Current.Private = []byte("changed")
							return
						}
					}
				}
			}
		},
		"create before destroy changed": func(s *states.State) {
			for _, ms := range s.Modules {
				for _, rs := range ms.Resources {
					for _, is := range rs.Instances {
						if is.Current != nil {
							is.Current.CreateBeforeDestroy = !is.Current.CreateBeforeDestroy
							return
						}
					}
				}
			}
		},
		"schema version changed": func(s *states.State) {
			for _, ms := range s.Modules {
				for _, rs := range ms.Resources {
					for _, is := range rs.Instances {
						if is.Current != nil {
							is.Current.SchemaVersion++
							return
						}
					}
				}
			}
		},
		"sensitive path added": func(s *states.State) {
			for _, ms := range s.Modules {
				for _, rs := range ms.Resources {
					for _, is := range rs.Instances {
						if is.Current != nil {
							is.Current.AttrSensitivePaths = append(is.Current.AttrSensitivePaths[:len(is.Current.AttrSensitivePaths):len(is.Current.AttrSensitivePaths)], cty.GetAttrPath("id"))
							return
						}
					}
				}
			}
		},
		"deposed object added": func(s *states.State) {
			for _, ms := range s.Modules {
				for _, rs := range ms.Resources {
					for _, is := range rs.Instances {
						if is.Current != nil {
							is.Deposed = map[states.DeposedKey]*states.ResourceInstanceObjectSrc{"00000001": is.Current.DeepCopy()}
							return
						}
					}
				}
			}
		},
		"identity changed": func(s *states.State) {
			for _, ms := range s.Modules {
				for _, rs := range ms.Resources {
					for _, is := range rs.Instances {
						if is.Current != nil {
							is.Current.IdentityJSON = []byte(`{"id":"changed"}`)
							return
						}
					}
				}
			}
		},
		"provider changed": func(s *states.State) {
			for _, ms := range s.Modules {
				for _, rs := range ms.Resources {
					rs.ProviderConfig.Alias = "changed"
					return
				}
			}
		},
		"check results removed": func(s *states.State) {
			s.CheckResults = nil
		},
		"check result changed": func(s *states.State) {
			for _, aggr := range s.CheckResults.ConfigResults.Iter() {
				for _, obj := range aggr.ObjectResults.Iter() {
					obj.FailureMessages = append(obj.FailureMessages, "changed")
					return
				}
			}
		},
		"output changed": func(s *states.State) {
			for _, ov := range s.RootOutputValues {
				ov.Sensitive = !ov.Sensitive
				return
			}
		},
	}
	for name, modify := range variants {
		t.Run(name, func(t *testing.T) {
			variant := base.DeepCopy()
			if name == "separate copy" {
				variant = read().State
			}
			modify(variant)

			reallyDiffer := marshal(variant) != baseSrc
			for _, pair := range [][2]*states.State{{base, variant}, {variant, base}} {
				if StatesDefinitelyDiffer(pair[0], pair[1]) && !reallyDiffer {
					t.Fatal("reported a difference between states that serialize identically")
				}
				if got, want := StatesMarshalEqual(pair[0], pair[1]), !reallyDiffer; got != want {
					t.Fatalf("StatesMarshalEqual returned %t; want %t", got, want)
				}
			}
			for _, pair := range [][2]*states.State{{base, variant}, {variant, base}} {
				if StatesDefinitelyMarshalEqual(pair[0], pair[1]) && reallyDiffer {
					t.Fatal("reported that states which serialize differently are the same")
				}
			}
			switch name {
			case "unchanged", "separate copy":
				if !StatesDefinitelyMarshalEqual(base, variant) {
					t.Error("didn't recognize states that it's intended to recognize as the same")
				}
			default:
				if name != "whitespace only" && !reallyDiffer {
					t.Error("variant doesn't change the serialization, so it isn't testing anything")
				}
			}
			switch name {
			case "attribute changed", "status changed", "instance removed", "resource removed", "output removed":
				if !StatesDefinitelyDiffer(base, variant) {
					t.Error("didn't detect a difference that it's intended to detect")
				}
			}
		})
	}
}
