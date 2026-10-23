package helmmap

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// removed is a field Helm values cannot express: an element or a key that is
// gone from the cluster.
func removed(index int, path, was string) rendermap.Field {
	return rendermap.Field{Index: index, Found: &rendermap.Found{},
		Change: v1alpha1.FieldChange{Path: path, Op: v1alpha1.OpRemove, Desired: was}}
}

func pointersOfFailures(ff []rendermap.FieldFailure) []string {
	out := make([]string, len(ff))
	for i, f := range ff {
		out[i] = f.Pointer
	}
	return out
}

func TestVerifyPartialLeavesOutFieldsTheRenderDoesNotHave(t *testing.T) {
	env := newDemo(t, Spec{})
	region := drift(t, env.src, env.files, 0, "REGION", "eu-central-1", "us-west-2")
	gone := removed(1, "/data/EXTRA", `"x"`)

	t.Run("without AllowPartial the whole proposal is unmapped", func(t *testing.T) {
		out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, region, gone), rendermap.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Mapped() || out.Unmapped.Reason != rendermap.ReasonRenderMismatch {
			t.Fatalf("out = %+v", out)
		}
	})

	t.Run("with it the mappable field is proposed and the other is listed", func(t *testing.T) {
		rend := &demoRenderer{src: env.src}
		out, err := Verify(context.Background(), rend, env.input(keyConfig, region, gone), rendermap.Options{AllowPartial: true})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Mapped() {
			t.Fatalf("unmapped: %+v", out.Unmapped)
		}
		if len(out.Results) != 1 || len(out.Results[0].Edits) != 1 || out.Results[0].Edits[0].ChangeIndex != 0 ||
			decodePointer(out.Results[0].Edits[0].Location) != "global.region" {
			t.Errorf("results = %+v", out.Results)
		}
		if len(out.LeftOut) != 1 || out.LeftOut[0].Pointer != "/data/EXTRA" || out.LeftOut[0].Reason != rendermap.ReasonRenderMismatch {
			t.Errorf("left out = %+v", out.LeftOut)
		}
		want := strings.Replace(string(env.files["helm/demo/values-dev.yaml"]), "region: eu-central-1", "region: us-west-2", 1)
		if string(out.Results[0].Edited) != want {
			t.Errorf("only the region line may change:\n%s", out.Results[0].Diff)
		}
		if rend.calls != 3 {
			t.Errorf("renders = %d: the unsupported field must not cost any", rend.calls)
		}
	})

	t.Run("nothing mappable stays unmapped", func(t *testing.T) {
		out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, gone), rendermap.Options{AllowPartial: true})
		if err != nil {
			t.Fatal(err)
		}
		if out.Mapped() || out.Unmapped.Reason != rendermap.ReasonRenderMismatch || len(out.LeftOut) != 0 {
			t.Errorf("out = %+v", out)
		}
	})
}

func TestVerifyPartialLeavesOutFieldsWithoutAVerifiedKey(t *testing.T) {
	env := newDemo(t, Spec{})
	region := drift(t, env.src, env.files, 0, "REGION", "eu-central-1", "us-west-2")
	timeout := drift(t, env.src, env.files, 1, "TIMEOUT", "30", "45") // only in the chart's defaults
	colorA := drift(t, env.src, env.files, 2, "EQUAL_A", "green", "blue")

	t.Run("without AllowPartial the whole proposal is unmapped", func(t *testing.T) {
		out, _ := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, region, timeout, colorA), rendermap.Options{})
		if out.Mapped() || out.Unmapped.Reason != rendermap.ReasonNoVerifiedCandidate {
			t.Fatalf("out = %+v", out)
		}
	})

	t.Run("with it the others are mapped and keep the index of their change", func(t *testing.T) {
		out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, region, timeout, colorA), rendermap.Options{AllowPartial: true})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Mapped() {
			t.Fatalf("unmapped: %+v", out.Unmapped)
		}
		edits := out.Results[0].Edits
		keys := make([]string, 0, len(edits))
		idx := make([]int, 0, len(edits))
		for _, e := range edits {
			keys = append(keys, decodePointer(e.Location))
			idx = append(idx, e.ChangeIndex)
		}
		if !slices.Equal(keys, []string{"global.region", "config.colorA"}) || !slices.Equal(idx, []int{0, 2}) {
			t.Errorf("edits: keys %v, indexes %v", keys, idx)
		}
		if got := pointersOfFailures(out.LeftOut); !slices.Equal(got, []string{"/data/TIMEOUT"}) {
			t.Fatalf("left out = %v", got)
		}
		if out.LeftOut[0].Reason != rendermap.ReasonNoVerifiedCandidate || !strings.Contains(out.LeftOut[0].Detail, "chart's own defaults") {
			t.Errorf("left out = %+v", out.LeftOut[0])
		}
	})

	t.Run("every field failing is unmapped, naming all of them", func(t *testing.T) {
		gone := removed(1, "/data/EXTRA", `"x"`)
		out, _ := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, timeout, gone), rendermap.Options{AllowPartial: true})
		if out.Mapped() {
			t.Fatalf("mapped: %+v", out.Results)
		}
		if got := pointersOfFailures(out.Unmapped.Fields); !slices.Equal(got, []string{"/data/EXTRA", "/data/TIMEOUT"}) {
			t.Errorf("fields = %v: %s", got, out.Unmapped.Message)
		}
	})
}

func TestVerifyPartialDoesNotWorkAroundWhatIsNotAboutOneField(t *testing.T) {
	env := newDemo(t, Spec{})

	t.Run("fields that interact", func(t *testing.T) {
		a := drift(t, env.src, env.files, 0, "EQUAL_A", "green", "blue")
		b := drift(t, env.src, env.files, 1, "REGION", "eu-central-1", "mars")
		gone := removed(2, "/data/EXTRA", `"x"`)
		out, _ := Verify(context.Background(), &demoRenderer{src: env.src, interact: true}, env.input(keyConfig, a, b, gone), rendermap.Options{AllowPartial: true})
		if out.Mapped() || out.Unmapped.Reason != rendermap.ReasonEditsInteract {
			t.Errorf("out = %+v", out)
		}
	})

	t.Run("ambiguous candidates of several fields", func(t *testing.T) {
		amb := drift(t, env.src, env.files, 0, "AMBIGUOUS", "3", "5")
		region := drift(t, env.src, env.files, 1, "REGION", "eu-central-1", "us-west-2")
		out, _ := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, amb, region), rendermap.Options{AllowPartial: true})
		if out.Mapped() {
			t.Fatalf("an ambiguous field is left out, not guessed: %+v", out.Results)
		}
	})
}

func TestVerifyPartialNeverChangesALeftOutField(t *testing.T) {
	// The chart derives LOG_LEVEL_UP from config.logLevel. LOG_LEVEL_UP drifted
	// too but is left out (a removal): editing config.logLevel would change it,
	// so the edit cannot be proposed, partial or not.
	env := newDemo(t, Spec{})
	logLevel := drift(t, env.src, env.files, 0, "LOG_LEVEL", "debug", "trace")
	derived := removed(1, "/data/LOG_LEVEL_UP", `"DEBUG"`)
	out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, logLevel, derived), rendermap.Options{AllowPartial: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Mapped() {
		t.Fatalf("an edit that changes a left-out field was proposed: %+v", out.Results)
	}
	if got := pointersOfFailures(out.Unmapped.Fields); !slices.Contains(got, "/data/LOG_LEVEL") {
		t.Errorf("fields = %v", got)
	}
}

func TestVerifyPartialCollateralNeverTouchesALeftOutField(t *testing.T) {
	const annotation = "/metadata/annotations/example.com~1version"
	both := []string{rendermap.CollateralLabels, rendermap.CollateralAnnotations}
	run := func(t *testing.T, fields func(f rendermap.Field) []rendermap.Field) *rendermap.Outcome {
		t.Helper()
		env := newDemo(t, Spec{})
		rend := &demoRenderer{src: env.src, deployment: true, tagAnnot: true}
		out, err := Verify(context.Background(), rend, env.input(keyDeploy, fields(imageDrift(t, env, newImage))...),
			rendermap.Options{AllowCollateral: both, AllowPartial: true})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	t.Run("an annotation the tag also changes is allowed collateral", func(t *testing.T) {
		out := run(t, func(f rendermap.Field) []rendermap.Field { return []rendermap.Field{f} })
		if !out.Mapped() || !slices.Contains(collateralPointers(out.Collateral), annotation) {
			t.Fatalf("out = %+v", out)
		}
	})

	t.Run("the same annotation, drifted and left out, is not", func(t *testing.T) {
		out := run(t, func(f rendermap.Field) []rendermap.Field {
			return []rendermap.Field{f, removed(1, annotation, `"1.27-alpine"`)}
		})
		if out.Mapped() {
			t.Fatalf("the edit changes a left-out field but was proposed: collateral %v", collateralPointers(out.Collateral))
		}
	})
}

func TestVerifyPartiallySharesRenders(t *testing.T) {
	// LOG_LEVEL_UP derives from a value and has no key of its own: it is left out
	// and the others are verified again. The second attempt must not render the
	// unmodified files, or the edit of REGION, a second time.
	env := newDemo(t, Spec{})
	rend := &demoRenderer{src: env.src}
	fields := []rendermap.Field{
		drift(t, env.src, env.files, 0, "REGION", "eu-central-1", "us-west-2"),
		drift(t, env.src, env.files, 1, "LOG_LEVEL_UP", "DEBUG", "TRACE"),
	}
	out, err := Verify(context.Background(), rend, env.input(keyConfig, fields...), rendermap.Options{AllowPartial: true})
	if err != nil || !out.Mapped() || len(out.LeftOut) != 1 {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	if out.Renders != 3 || rend.calls != 3 {
		t.Errorf("renders = %d (calls %d), want 3: the unmodified files twice and the edit once", out.Renders, rend.calls)
	}

	t.Run("a field that changes on every render is still found in the later attempt", func(t *testing.T) {
		rend := &demoRenderer{src: env.src, random: true}
		fields := []rendermap.Field{
			drift(t, env.src, env.files, 0, "RANDOM", "r1", "x"),
			drift(t, env.src, env.files, 1, "REGION", "eu-central-1", "us-west-2"),
			drift(t, env.src, env.files, 2, "LOG_LEVEL_UP", "DEBUG", "TRACE"),
		}
		out, err := Verify(context.Background(), rend, env.input(keyConfig, fields...), rendermap.Options{AllowPartial: true})
		if err != nil || !out.Mapped() || len(out.LeftOut) != 2 {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
		reasons := map[string]string{}
		for _, f := range out.LeftOut {
			reasons[f.Pointer] = f.Reason
		}
		if reasons["/data/RANDOM"] != rendermap.ReasonNondeterministicField {
			t.Errorf("left out = %v", reasons)
		}
	})
}
