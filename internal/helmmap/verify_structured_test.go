package helmmap

import (
	"context"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

func structuredChange(path string, op v1alpha1.ChangeOperation, desired, live string) Field {
	return Field{Change: v1alpha1.FieldChange{Path: path, Op: op, Desired: desired, Live: live}, Found: &Result{}}
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

func TestVerifyStructuredBaseline(t *testing.T) {
	env := newDemo(t, Spec{})
	rend := &demoRenderer{src: env.src}
	t.Run("the render must have what the proposal says is in Git", func(t *testing.T) {
		out, err := Verify(context.Background(), rend, env.input(keyConfig,
			structuredChange("/data/NEW", v1alpha1.OpRemove, `"x"`, "")), Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonRenderMismatch ||
			!strings.Contains(out.Unmapped.Message, "the proposal expects /data/NEW") {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
	})
	t.Run("an Add of a field the render already has is a mismatch", func(t *testing.T) {
		out, err := Verify(context.Background(), rend, env.input(keyConfig,
			structuredChange("/data/LOG_LEVEL", v1alpha1.OpAdd, "", `"x"`)), Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonRenderMismatch {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
	})
	t.Run("a structured field below which a leaf changes on every render is nondeterministic", func(t *testing.T) {
		out, err := Verify(context.Background(), &demoRenderer{src: env.src, random: true}, env.input(keyConfig,
			structuredChange("/data", v1alpha1.OpReplace, `{"RANDOM":"r1"}`, `{"RANDOM":"x"}`)), Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonNondeterministicField {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
	})
	t.Run("without candidates a structured field has no verified key", func(t *testing.T) {
		out, err := Verify(context.Background(), rend, env.input(keyConfig,
			structuredChange("/data/NEW", v1alpha1.OpAdd, "", `"x"`)), Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonNoVerifiedCandidate {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
	})
}

func TestVerifyValueAffectsOtherFieldsWinsOverOtherRejections(t *testing.T) {
	env := newDemo(t, Spec{})
	f := drift(t, env.src, env.files, 0, "LOG_LEVEL", "debug", "trace")
	// config.logLevel reproduces the field but also moves LOG_LEVEL_UP; the other
	// key does not reproduce it at all.
	f.Found.Candidates = append(f.Found.Candidates, Candidate{File: "helm/demo/values-dev.yaml", Path: KeyPath{"config", "minA"}})
	out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, f), Options{})
	if err != nil || out.Mapped() {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	if out.Unmapped.Reason != ReasonValueAffectsOtherFields {
		t.Fatalf("reason = %s: a key that reproduces the field and moves others decides the reason", out.Unmapped.Reason)
	}
	for _, want := range []string{"config.logLevel", "would reproduce /data/LOG_LEVEL", "LOG_LEVEL_UP", "1 other candidate(s) were rejected for other reasons"} {
		if !strings.Contains(out.Unmapped.Message, want) {
			t.Errorf("message %q lacks %q", out.Unmapped.Message, want)
		}
	}
}
