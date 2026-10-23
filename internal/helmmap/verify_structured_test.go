package helmmap

import (
	"context"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/internal/rendermap"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

func structuredChange(path string, op v1alpha1.ChangeOperation, desired, live string) rendermap.Field {
	return rendermap.Field{Change: v1alpha1.FieldChange{Path: path, Op: op, Desired: desired, Live: live}, Found: &rendermap.Found{}}
}

func TestVerifyStructuredBaseline(t *testing.T) {
	env := newDemo(t, Spec{})
	rend := &demoRenderer{src: env.src}
	t.Run("the render must have what the proposal says is in Git", func(t *testing.T) {
		out, err := Verify(context.Background(), rend, env.input(keyConfig,
			structuredChange("/data/NEW", v1alpha1.OpRemove, `"x"`, "")), rendermap.Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonRenderMismatch ||
			!strings.Contains(out.Unmapped.Message, "the proposal expects /data/NEW") {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
	})
	t.Run("an Add of a field the render already has is a mismatch", func(t *testing.T) {
		out, err := Verify(context.Background(), rend, env.input(keyConfig,
			structuredChange("/data/LOG_LEVEL", v1alpha1.OpAdd, "", `"x"`)), rendermap.Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonRenderMismatch {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
	})
	t.Run("a structured field below which a leaf changes on every render is nondeterministic", func(t *testing.T) {
		out, err := Verify(context.Background(), &demoRenderer{src: env.src, random: true}, env.input(keyConfig,
			structuredChange("/data", v1alpha1.OpReplace, `{"RANDOM":"r1"}`, `{"RANDOM":"x"}`)), rendermap.Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonNondeterministicField {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
	})
	t.Run("without candidates a structured field has no verified key", func(t *testing.T) {
		out, err := Verify(context.Background(), rend, env.input(keyConfig,
			structuredChange("/data/NEW", v1alpha1.OpAdd, "", `"x"`)), rendermap.Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonNoVerifiedCandidate {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
	})
}

func TestVerifyValueAffectsOtherFieldsWinsOverOtherRejections(t *testing.T) {
	env := newDemo(t, Spec{})
	f := drift(t, env.src, env.files, 0, "LOG_LEVEL", "debug", "trace")
	// config.logLevel reproduces the field but also moves LOG_LEVEL_UP; the other
	// key does not reproduce it at all.
	f.Found.Candidates = append(f.Found.Candidates, rendermap.Candidate{File: "helm/demo/values-dev.yaml", Path: rendermap.KeyPath{"config", "minA"}})
	out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, f), rendermap.Options{})
	if err != nil || out.Mapped() {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	if out.Unmapped.Reason != rendermap.ReasonValueAffectsOtherFields {
		t.Fatalf("reason = %s: a key that reproduces the field and moves others decides the reason", out.Unmapped.Reason)
	}
	for _, want := range []string{"config.logLevel", "would reproduce /data/LOG_LEVEL", "LOG_LEVEL_UP", "1 other candidate(s) were rejected for other reasons"} {
		if !strings.Contains(out.Unmapped.Message, want) {
			t.Errorf("message %q lacks %q", out.Unmapped.Message, want)
		}
	}
}
