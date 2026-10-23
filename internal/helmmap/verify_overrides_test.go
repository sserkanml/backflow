package helmmap

import (
	"context"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/internal/rendermap"
)

const (
	ovDev      = "helm/demo/values-dev.yaml"
	ovDefaults = "helm/demo/values.yaml"
)

var ovOn = Options{AddOverrides: true}

// A default only the chart defines is overridden in the last value file.
func TestVerifyOverrideInLastValueFile(t *testing.T) {
	env := newDemo(t, Spec{})
	f := drift(t, env.src, env.files, 0, "TIMEOUT", "30", "45")
	out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, f), ovOn)
	if err != nil || !out.Mapped() {
		t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
	}
	if len(out.Results) != 1 || out.Results[0].File != ovDev || len(out.Results[0].Edits) != 1 {
		t.Fatalf("results = %+v", out.Results)
	}
	e := out.Results[0].Edits[0]
	if decodePointer(e.Location) != "config.timeout" || e.Value != "45" || e.ChangeIndex != 0 {
		t.Errorf("edit = %+v: an int default gets an int", e)
	}
	if len(out.Overrides) != 1 {
		t.Fatalf("overrides = %+v", out.Overrides)
	}
	o := out.Overrides[0]
	if o.File != ovDev || o.Path.String() != "config.timeout" || o.Value != "45" ||
		o.Default.File != ovDefaults || o.Default.Key.String() != "config.timeout" || o.Default.Value != "30" {
		t.Errorf("override = %+v", o)
	}
	edited := string(out.Results[0].Edited)
	if !strings.Contains(edited, "  timeout: 45\n") || !strings.Contains(edited, "# Log verbosity: debug, info, warn, error, trace\n  logLevel: debug\n") {
		t.Errorf("the key was not added to the config block of the file, or comments were lost:\n%s", edited)
	}
	if strings.Contains(out.Results[0].Diff, "-") && strings.Contains(strings.ReplaceAll(out.Results[0].Diff, "--- ", ""), "\n-") {
		t.Errorf("an override only adds lines:\n%s", out.Results[0].Diff)
	}
	if out.Renders != 3 {
		t.Errorf("renders = %d: the unmodified files twice and the one override", out.Renders)
	}
}

// Without the option the same drift stays unmapped, as before.
func TestVerifyOverridesOffKeepsUnmapped(t *testing.T) {
	env := newDemo(t, Spec{})
	f := drift(t, env.src, env.files, 0, "TIMEOUT", "30", "45")
	for name, opts := range map[string]Options{"zero value": {}, "explicitly off": {AddOverrides: false}} {
		out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, f), opts)
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonNoVerifiedCandidate ||
			!strings.Contains(out.Unmapped.Message, "chart defaults are not edited") {
			t.Errorf("%s: out = %+v, err = %v", name, out.Unmapped, err)
		}
		if out.Renders != 2 {
			t.Errorf("%s: renders = %d: no override is rendered", name, out.Renders)
		}
	}
}

// Edits of existing lines come first, overrides are not even rendered.
func TestVerifyOverridesAfterExistingEdits(t *testing.T) {
	env := newDemo(t, Spec{})
	f := drift(t, env.src, env.files, 0, "REGION", "eu-central-1", "us-west-2")
	// An override candidate that would be rendered if it were tried.
	f.Found.Overrides = []Candidate{{File: ovDev, Path: rendermap.KeyPath{"config", "timeout"}, Override: true,
		Default: &ChartDefault{File: ovDefaults, Key: rendermap.KeyPath{"config", "timeout"}, Value: "30", Tag: "!!int"}}}
	out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, f), ovOn)
	if err != nil || !out.Mapped() {
		t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
	}
	if decodePointer(out.Results[0].Edits[0].Location) != "global.region" || len(out.Overrides) != 0 {
		t.Errorf("edits = %+v, overrides = %+v", out.Results[0].Edits, out.Overrides)
	}
	if out.Renders != 3 {
		t.Errorf("renders = %d: the override must not be rendered when an edit verifies", out.Renders)
	}
}

// One field edits a line, the other adds an override.
func TestVerifyOverrideNextToEdit(t *testing.T) {
	env := newDemo(t, Spec{})
	fields := []Field{
		drift(t, env.src, env.files, 0, "REGION", "eu-central-1", "us-west-2"),
		drift(t, env.src, env.files, 1, "TIMEOUT", "30", "45"),
	}
	out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, fields...), ovOn)
	if err != nil || !out.Mapped() {
		t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
	}
	got := make([]string, 0, len(out.Results[0].Edits))
	for _, e := range out.Results[0].Edits {
		got = append(got, decodePointer(e.Location))
	}
	if strings.Join(got, ",") != "global.region,config.timeout" {
		t.Errorf("edits = %v", got)
	}
	if len(out.Overrides) != 1 || out.Overrides[0].Path.String() != "config.timeout" {
		t.Errorf("overrides = %+v", out.Overrides)
	}
}

// An override that works as well as an existing key is shadowed, not ambiguous.
func TestVerifyOverrideShadowed(t *testing.T) {
	env := newDemo(t, Spec{})
	// The chart also reads region from a top level key that only its defaults
	// know. Both edits reproduce REGION; the edit of the existing line wins.
	region := drift(t, env.src, env.files, 0, "REGION", "eu-central-1", "us-west-2")
	region.Found.Overrides = []Candidate{{File: ovDev, Path: rendermap.KeyPath{"region"}, Override: true,
		Default: &ChartDefault{File: ovDefaults, Key: rendermap.KeyPath{"region"}, Value: "eu-central-1", Tag: "!!str"}}}
	timeout := drift(t, env.src, env.files, 1, "TIMEOUT", "30", "45")
	out, err := Verify(context.Background(), &demoRenderer{src: env.src, altRegion: true}, env.input(keyConfig, region, timeout), ovOn)
	if err != nil || !out.Mapped() {
		t.Fatalf("out = %+v, err = %v (the shadowed override must not make the proposal ambiguous)", out.Unmapped, err)
	}
	got := make([]string, 0, len(out.Results[0].Edits))
	for _, e := range out.Results[0].Edits {
		got = append(got, decodePointer(e.Location))
	}
	if strings.Join(got, ",") != "global.region,config.timeout" || len(out.Overrides) != 1 || out.Overrides[0].Path.String() != "config.timeout" {
		t.Errorf("edits = %v, overrides = %+v", got, out.Overrides)
	}
}

// A part of a string default is overridden through the fragment.
func TestVerifyOverrideFragment(t *testing.T) {
	env := newDemo(t, Spec{})
	f := imageDrift(t, env, "other/nginx-unprivileged:1.27-alpine")
	out, err := Verify(context.Background(), &demoRenderer{src: env.src, deployment: true}, env.input(keyDeploy, f), ovOn)
	if err != nil || !out.Mapped() {
		t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
	}
	if len(out.Overrides) != 1 || out.Overrides[0].Path.String() != "image.repository" ||
		out.Overrides[0].Value != `"other/nginx-unprivileged"` || out.Overrides[0].Default.Value != "nginxinc/nginx-unprivileged" {
		t.Fatalf("overrides = %+v", out.Overrides)
	}
	if !strings.Contains(string(out.Results[0].Edited), "image:\n  tag: 1.27-alpine\n  repository: other/nginx-unprivileged\n") {
		t.Errorf("edited =\n%s", out.Results[0].Edited)
	}
}

// An inline setting still decides the key: no override.
func TestVerifyOverrideInlineControlled(t *testing.T) {
	env := newDemo(t, Spec{Path: "helm/demo", ValueFiles: []string{"values-dev.yaml"},
		Parameters: []Parameter{{Name: "config.timeout", Value: "30"}}})
	f := drift(t, env.src, env.files, 0, "TIMEOUT", "30", "45")
	out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, f), ovOn)
	if err != nil || out.Mapped() || !strings.Contains(out.Unmapped.Message, "helm.parameters") {
		t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
	}
}

// An application without a value file has nowhere to add the override.
func TestVerifyOverrideNoValueFile(t *testing.T) {
	env := newDemo(t, Spec{Path: "helm/demo"})
	f := drift(t, env.src, env.files, 0, "TIMEOUT", "30", "45")
	out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, f), ovOn)
	if err != nil || out.Mapped() || !strings.Contains(out.Unmapped.Message, "no value file to add an override to") {
		t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
	}
}

// An override that changes more than the field is rejected, with the reason.
func TestVerifyOverrideRejected(t *testing.T) {
	env := newDemo(t, Spec{})
	f := drift(t, env.src, env.files, 0, "TIMEOUT", "30", "45")
	rend := &demoRenderer{src: env.src, failWhen: func(values map[string]any) error {
		if get(values, "config", "timeout") == 45 || str(get(values, "config", "timeout")) == "45" {
			return rendermap.ErrRenderFailed
		}
		return nil
	}}
	out, err := Verify(context.Background(), rend, env.input(keyConfig, f), ovOn)
	if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonNoVerifiedCandidate ||
		!strings.Contains(out.Unmapped.Message, "(a new key)") || !strings.Contains(out.Unmapped.Message, "does not render") {
		t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
	}
}

// With partial proposals the override covers its field and the others stay left out.
func TestVerifyOverridePartial(t *testing.T) {
	env := newDemo(t, Spec{})
	fields := []Field{
		drift(t, env.src, env.files, 0, "TIMEOUT", "30", "45"),
		drift(t, env.src, env.files, 1, "LOG_LEVEL_UP", "DEBUG", "TRACE"), // derived: no key holds it
	}
	out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, fields...),
		Options{AddOverrides: true, AllowPartial: true})
	if err != nil || !out.Mapped() {
		t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
	}
	if len(out.Overrides) != 1 || len(out.LeftOut) != 1 || out.LeftOut[0].Pointer != "/data/LOG_LEVEL_UP" {
		t.Errorf("overrides = %+v, left out = %+v", out.Overrides, out.LeftOut)
	}
}
