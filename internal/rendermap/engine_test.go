package rendermap

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

func TestCollateralRuleAllows(t *testing.T) {
	res := ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "web"}
	other := ResourceKey{Kind: "Service", Namespace: "demo", Name: "web"}
	rule := newCollateralRule([]string{CollateralLabels, CollateralAnnotations}, res, []parsedField{{Field: Field{Change: v1alpha1.FieldChange{Path: "/metadata/labels/x"}}}}, nil)
	labelsOnly := newCollateralRule([]string{CollateralLabels}, res, nil, nil)
	tests := []struct {
		name string
		rule collateralRule
		c    Collateral
		want bool
	}{
		{"a label", rule, Collateral{Resource: res, Pointer: "/metadata/labels/app"}, true},
		{"a label of another resource", rule, Collateral{Resource: other, Pointer: "/metadata/labels/app"}, true},
		{"a pod template label", rule, Collateral{Resource: res, Pointer: "/spec/template/metadata/labels/app"}, true},
		{"an annotation", rule, Collateral{Resource: res, Pointer: "/metadata/annotations/a"}, true},
		{"a pod template annotation", rule, Collateral{Resource: res, Pointer: "/spec/template/metadata/annotations/a"}, true},
		{"an empty labels map", rule, Collateral{Resource: res, Pointer: "/metadata/labels"}, true},
		{"a pod template label of a CronJob", rule, Collateral{Resource: res, Pointer: "/spec/jobTemplate/spec/template/metadata/labels/app"}, true},
		{"a pod template annotation of a CronJob", rule, Collateral{Resource: res, Pointer: "/spec/jobTemplate/spec/template/metadata/annotations/a"}, true},
		{"a CronJob pod template annotation when only labels are allowed", labelsOnly, Collateral{Resource: res, Pointer: "/spec/jobTemplate/spec/template/metadata/annotations/a"}, false},
		{"the selector of a CronJob's job", rule, Collateral{Resource: res, Pointer: "/spec/jobTemplate/spec/selector/matchLabels/app"}, false},
		{"the pod spec of a CronJob", rule, Collateral{Resource: res, Pointer: "/spec/jobTemplate/spec/template/spec/containers/0/image"}, false},
		{"the labels of the job template itself are not a pod template", rule, Collateral{Resource: res, Pointer: "/spec/jobTemplate/metadata/labels/app"}, false},
		{"an annotation when only labels are allowed", labelsOnly, Collateral{Resource: res, Pointer: "/metadata/annotations/a"}, false},
		{"the selector of a Deployment", rule, Collateral{Resource: res, Pointer: "/spec/selector/matchLabels/app"}, false},
		{"matchExpressions", rule, Collateral{Resource: res, Pointer: "/spec/selector/matchExpressions/0/key"}, false},
		{"the selector of a Service", rule, Collateral{Resource: other, Pointer: "/spec/selector/app"}, false},
		{"a label named like a selector is only a label", rule, Collateral{Resource: res, Pointer: "/metadata/labels/selector"}, true},
		{"a field that is not metadata", rule, Collateral{Resource: res, Pointer: "/spec/replicas"}, false},
		{"a field of the pod spec", rule, Collateral{Resource: res, Pointer: "/spec/template/spec/containers/0/image"}, false},
		{"a drifted field that did not end at its live value", rule, Collateral{Resource: res, Pointer: "/metadata/labels/x"}, false},
		{"the same pointer on another resource is not that drifted field", rule, Collateral{Resource: other, Pointer: "/metadata/labels/x"}, true},
		{"a whole resource that appears", rule, Collateral{Resource: other}, false},
		{"labels of a path that only begins like labels", rule, Collateral{Resource: res, Pointer: "/metadata/labelsX/a"}, false},
		{"nothing is allowed by the zero rule", collateralRule{}, Collateral{Resource: res, Pointer: "/metadata/labels/app"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rule.allows(tt.c); got != tt.want {
				t.Errorf("allows(%+v) = %v, want %v", tt.c, got, tt.want)
			}
		})
	}
}

func TestConflicts(t *testing.T) {
	mk := func(file, key, value string) *tryout {
		return &tryout{cand: Candidate{File: file, Path: KeyPath{key}}, value: value}
	}
	if !conflicts(mk("f.yaml", "k", "x"), mk("f.yaml", "k", "y")) {
		t.Error("one key set to two values was not reported")
	}
	for name, pair := range map[string][2]*tryout{
		"the same value": {mk("f.yaml", "k", "x"), mk("f.yaml", "k", "x")},
		"two keys":       {mk("f.yaml", "k", "x"), mk("f.yaml", "j", "y")},
		"two files":      {mk("f.yaml", "k", "x"), mk("g.yaml", "k", "y")},
	} {
		if conflicts(pair[0], pair[1]) {
			t.Errorf("%s reported as a conflict", name)
		}
	}
}

func TestCoverSets(t *testing.T) {
	mk := func(key string, covers ...int) *tryout {
		return &tryout{cand: Candidate{File: "f.yaml", Path: KeyPath{key}}, value: key, covers: covers}
	}
	tests := []struct {
		name     string
		n        int
		accepted []*tryout
		want     []string
	}{
		{"one edit", 1, []*tryout{mk("a", 0)}, []string{"f.yaml a"}},
		{"two edits for one field are two sets", 1, []*tryout{mk("a", 0), mk("b", 0)}, []string{"f.yaml a", "f.yaml b"}},
		{"one edit covers both fields", 2, []*tryout{mk("a", 0, 1)}, []string{"f.yaml a"}},
		{"a smaller set wins over a bigger one", 2, []*tryout{mk("a", 0, 1), mk("b", 0), mk("c", 1)}, []string{"f.yaml a", "f.yaml b + f.yaml c"}},
		{"a field nothing covers", 2, []*tryout{mk("a", 0)}, nil},
		{"the same key with two values never go together", 2,
			[]*tryout{{cand: Candidate{File: "f.yaml", Path: KeyPath{"k"}}, value: "x", covers: []int{0}},
				{cand: Candidate{File: "f.yaml", Path: KeyPath{"k"}}, value: "y", covers: []int{1}}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sets, tooMany := coverSets(tt.n, tt.accepted)
			if tooMany {
				t.Fatal("too many")
			}
			var got []string
			for _, s := range sets {
				got = append(got, describeSet(s))
			}
			slices.Sort(got)
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("sets = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNearMissReporting(t *testing.T) {
	t.Run("only three collateral changes are listed", func(t *testing.T) {
		nm := &NearMiss{File: "f.yaml", Path: KeyPath{"k"}, Value: "v", Covers: []string{"/data/A"}, CollateralTotal: 5}
		for i := range 3 {
			nm.Collateral = append(nm.Collateral, Collateral{Resource: keyConfig, Pointer: "/data/X" + strconv.Itoa(i), Current: "1", After: "2"})
		}
		got := nm.String()
		if !strings.Contains(got, "/data/X2") || !strings.Contains(got, "and 2 more") {
			t.Errorf("String() = %s", got)
		}
	})

	t.Run("the best near miss has the fewest other changes", func(t *testing.T) {
		fields := []parsedField{{Field: Field{Change: v1alpha1.FieldChange{Path: "/data/A"}, Found: &Found{}}}}
		cand := func(key string, n int) *tryout {
			return &tryout{cand: Candidate{File: "f.yaml", Path: KeyPath{key}}, origin: []int{0}, covers: []int{0}, collateral: make([]Collateral, n)}
		}
		bad := uncovered(fields, []*tryout{cand("many", 3), cand("few", 1), cand("some", 2)}, nil, collateralRule{}, false)
		if len(bad) != 1 || bad[0].NearMiss == nil || bad[0].NearMiss.Path.String() != "few" {
			t.Errorf("bad = %+v", bad)
		}
	})
}

func TestParseStructuredFields(t *testing.T) {
	tests := []struct {
		name       string
		f          Field
		structured bool
		desired    string
		live       string
		desiredLvs leaves
		liveLvs    leaves
	}{
		{"a scalar replaced by a scalar stays scalar", structuredChange("/data/A", v1alpha1.OpReplace, `"x"`, `"y"`), false, `"x"`, `"y"`, nil, nil},
		{"a list replaced by a longer list", structuredChange("/spec/env", v1alpha1.OpReplace, `[{"name":"A"}]`, `[{"name":"A"},{"name":"B"}]`), true,
			`[{"name":"A"}]`, `[{"name":"A"},{"name":"B"}]`,
			leaves{"/spec/env/0/name": `"A"`}, leaves{"/spec/env/0/name": `"A"`, "/spec/env/1/name": `"B"`}},
		{"a map that appears", structuredChange("/metadata/annotations", v1alpha1.OpAdd, "", `{"a/b":"1"}`), true, absentJSON, `{"a/b":"1"}`,
			leaves{}, leaves{"/metadata/annotations/a~1b": `"1"`}},
		{"a scalar key that appears", structuredChange("/data/NEW", v1alpha1.OpAdd, "", `"x"`), true, absentJSON, `"x"`,
			leaves{}, leaves{"/data/NEW": `"x"`}},
		{"a key that disappears", structuredChange("/data/OLD", v1alpha1.OpRemove, `"y"`, ""), true, `"y"`, absentJSON,
			leaves{"/data/OLD": `"y"`}, leaves{}},
		{"an empty map becomes a filled one", structuredChange("/metadata/labels", v1alpha1.OpReplace, `{}`, `{"a":"b"}`), true, `{}`, `{"a":"b"}`,
			leaves{"/metadata/labels": "{}"}, leaves{"/metadata/labels/a": `"b"`}},
		{"a scalar replaced by a map", structuredChange("/data/A", v1alpha1.OpReplace, `"x"`, `{"b":1}`), true, `"x"`, `{"b":1}`,
			leaves{"/data/A": `"x"`}, leaves{"/data/A/b": `1`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := parseField(tt.f)
			if err != nil {
				t.Fatal(err)
			}
			if p.structured != tt.structured || p.desired != tt.desired || p.live != tt.live {
				t.Errorf("structured = %v desired = %s live = %s", p.structured, p.desired, p.live)
			}
			if tt.structured && (!sameLeaves(p.desiredLeaves, tt.desiredLvs) || !sameLeaves(p.liveLeaves, tt.liveLvs)) {
				t.Errorf("leaves = %v -> %v, want %v -> %v", p.desiredLeaves, p.liveLeaves, tt.desiredLvs, tt.liveLvs)
			}
		})
	}
}

func TestStructuredFieldReached(t *testing.T) {
	p, err := parseField(structuredChange("/spec/env", v1alpha1.OpReplace, `[{"name":"A"}]`, `[{"name":"A"},{"name":"B"}]`))
	if err != nil {
		t.Fatal(err)
	}
	live := leaves{"/spec/env/0/name": `"A"`, "/spec/env/1/name": `"B"`, "/spec/other": "1"}
	if !p.reached(live) {
		t.Error("the live leaves under the path are the live state")
	}
	if p.reached(leaves{"/spec/env/0/name": `"A"`}) {
		t.Error("the base state is not the live state")
	}
	if p.reached(leaves{"/spec/env/0/name": `"A"`, "/spec/env/1/name": `"C"`}) {
		t.Error("another value is not the live state")
	}
	if p.reached(leaves{"/spec/env/0/name": `"A"`, "/spec/env/1/name": `"B"`, "/spec/env/2/name": `"C"`}) {
		t.Error("an extra item is not the live state")
	}
}

var keyConfig = ResourceKey{Kind: "ConfigMap", Namespace: "demo", Name: "demo-helm-config"}

func structuredChange(path string, op v1alpha1.ChangeOperation, desired, live string) Field {
	return Field{Change: v1alpha1.FieldChange{Path: path, Op: op, Desired: desired, Live: live}, Found: &Found{}}
}
