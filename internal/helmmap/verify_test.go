package helmmap

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

// demoRenderer stands in for Argo CD: it renders the demo chart of
// testdata/repo/helm/demo (templates/configmap.yaml and the sidecar's) from
// the files it is given, so a test can change a value file and see what the
// chart would do with it.
type demoRenderer struct {
	src *Source

	mu    sync.Mutex
	calls int
	// delay is how long every render takes.
	delay time.Duration
	// baseErr is returned by every render when set.
	baseErr error
	// failWhen returns an error for renders whose merged values satisfy it.
	failWhen func(values map[string]any) error

	// Behaviours of the chart that the real demo chart does not have, to
	// build the cases a verifier must catch.
	noDerived bool // data.LOG_LEVEL_UP, which derives from config.logLevel, is left out
	alsoOther bool // a second ConfigMap that also follows global.region
	random    bool // data.RANDOM changes on every render
	randomCM  bool // a ConfigMap whose name changes on every render
	interact  bool // data.BOTH is "yes" only if colorA is blue and region is mars
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		sm, sIsMap := v.(map[string]any)
		dm, dIsMap := dst[k].(map[string]any)
		switch {
		case v == nil:
			delete(dst, k)
		case sIsMap && dIsMap:
			deepMerge(dm, sm)
		default:
			dst[k] = v
		}
	}
}

func setPath(m map[string]any, p KeyPath, v any) {
	for _, k := range p[:len(p)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[p[len(p)-1]] = v
}

func get(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func str(v any) string { return fmt.Sprint(v) }

func (d *demoRenderer) Render(ctx context.Context, app App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	d.mu.Lock()
	d.calls++
	n := d.calls
	d.mu.Unlock()
	if d.delay > 0 {
		select {
		case <-time.After(d.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if d.baseErr != nil {
		return nil, d.baseErr
	}
	values := map[string]any{}
	for _, name := range append([]string{d.src.ChartValues}, d.src.ValueFiles...) {
		data, ok := files[name]
		if !ok {
			continue
		}
		var layer map[string]any
		if err := yaml.Unmarshal(data, &layer); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrRenderFailed, name, err)
		}
		deepMerge(values, layer)
	}
	for _, in := range d.src.Inline {
		if in.HasValue {
			setPath(values, in.Path, in.Value)
		}
	}
	if d.failWhen != nil {
		if err := d.failWhen(values); err != nil {
			return nil, err
		}
	}

	logLevel := str(get(values, "config", "logLevel"))
	region := str(get(values, "global", "region"))
	data := map[string]any{
		"LOG_LEVEL":    logLevel,
		"REGION":       region,
		"LOG_LEVEL_UP": strings.ToUpper(logLevel),
		"TIMEOUT":      str(get(values, "config", "timeout")),
		"FEATURE":      str(get(values, "config", "feature")),
		"EQUAL_A":      str(get(values, "config", "colorA")),
		"EQUAL_B":      str(get(values, "config", "colorB")),
	}
	if d.noDerived {
		delete(data, "LOG_LEVEL_UP")
	}
	minA, _ := strconv.Atoi(str(get(values, "config", "minA")))
	minB, _ := strconv.Atoi(str(get(values, "config", "minB")))
	data["AMBIGUOUS"] = strconv.Itoa(max(minA, minB))
	if d.random {
		data["RANDOM"] = fmt.Sprintf("r%d", n)
	}
	if d.interact {
		data["BOTH"] = "no"
		if str(get(values, "config", "colorA")) == "blue" && region == "mars" {
			data["BOTH"] = "yes"
		}
	}
	objs := []*unstructured.Unstructured{
		configMap(app.DestNamespace, "demo-helm-config", data),
		configMap(app.DestNamespace, "demo-helm-sidecar", map[string]any{
			"TIMEOUT_SECONDS": str(get(values, "demo-sidecar", "timeoutSeconds")),
		}),
	}
	if d.alsoOther {
		objs = append(objs, configMap(app.DestNamespace, "demo-helm-other", map[string]any{"REGION": region}))
	}
	if d.randomCM {
		objs = append(objs, configMap(app.DestNamespace, fmt.Sprintf("random-%d", n), map[string]any{"a": "b"}))
	}
	return objs, nil
}

func configMap(ns, name string, data map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"data":     data,
	}}
}

var (
	appDemo   = App{Name: "demo-helm", Namespace: "argocd", Project: "default", DestNamespace: "demo"}
	keyConfig = ResourceKey{Kind: "ConfigMap", Namespace: "demo", Name: "demo-helm-config"}
	keySide   = ResourceKey{Kind: "ConfigMap", Namespace: "demo", Name: "demo-helm-sidecar"}
)

// drift builds a Field for data.<key> going from desired to live, with the
// candidates found for desired.
func drift(t *testing.T, src *Source, files map[string][]byte, index int, key, desired, live string) Field {
	t.Helper()
	d, l := strconv.Quote(desired), strconv.Quote(live)
	found, err := src.Find(files, desired)
	if err != nil {
		t.Fatal(err)
	}
	return Field{
		Index:  index,
		Change: v1alpha1.FieldChange{Path: "/data/" + key, Op: v1alpha1.OpReplace, Desired: d, Live: l},
		Found:  found,
	}
}

type demoEnv struct {
	src   *Source
	files map[string][]byte
}

func newDemo(t *testing.T, spec Spec) demoEnv {
	t.Helper()
	fsys := demoRepo(t)
	if spec.Path == "" {
		spec.Path = "helm/demo"
		spec.ValueFiles = []string{"values-dev.yaml"}
	}
	src, err := NewSource(fsys, spec)
	if err != nil {
		t.Fatal(err)
	}
	files, err := CollectFiles(fsys, src)
	if err != nil {
		t.Fatal(err)
	}
	return demoEnv{src, files}
}

func (e demoEnv) input(res ResourceKey, fields ...Field) Input {
	return Input{App: appDemo, Resource: res, Fields: fields, Source: e.src, Files: e.files}
}

func TestVerifyDemoCases(t *testing.T) {
	const dev = "helm/demo/values-dev.yaml"
	feature := Spec{Path: "helm/demo", ValueFiles: []string{"values-dev.yaml"},
		Parameters: []Parameter{{Name: "config.feature", Value: "beta"}}}

	tests := []struct {
		name string
		spec Spec
		res  ResourceKey
		// fields: key, desired, live
		fields [][3]string
		rend   func(r *demoRenderer)

		wantKeys   []string // "file key" of each edit, in order
		wantReason string
		wantMsg    []string // substrings of the message
		renders    int      // 0: not checked
	}{
		{
			name: "LOG_LEVEL, where the chart has no derived key", res: keyConfig, fields: [][3]string{{"LOG_LEVEL", "debug", "trace"}},
			rend:     func(r *demoRenderer) { r.noDerived = true },
			wantKeys: []string{dev + " config.logLevel"}, renders: 3,
		},
		{
			// config.logLevel feeds LOG_LEVEL_UP too, so no edit of it changes
			// LOG_LEVEL alone.
			name: "LOG_LEVEL, which LOG_LEVEL_UP derives from, is not mapped", res: keyConfig,
			fields:     [][3]string{{"LOG_LEVEL", "debug", "trace"}},
			wantReason: ReasonNoVerifiedCandidate, wantMsg: []string{"config.logLevel", "also changes", "LOG_LEVEL_UP"},
		},
		{
			name: "REGION comes from global", res: keyConfig, fields: [][3]string{{"REGION", "eu-central-1", "us-west-2"}},
			wantKeys: []string{dev + " global.region"},
		},
		{
			name: "a key of the subchart", res: keySide, fields: [][3]string{{"TIMEOUT_SECONDS", "45", "60"}},
			wantKeys: []string{dev + " demo-sidecar.timeoutSeconds"},
		},
		{
			name: "EQUAL_A: two candidates, only colorA verifies", res: keyConfig, fields: [][3]string{{"EQUAL_A", "green", "blue"}},
			wantKeys: []string{dev + " config.colorA"}, renders: 4,
		},
		{
			name: "EQUAL_B: only colorB verifies", res: keyConfig, fields: [][3]string{{"EQUAL_B", "green", "blue"}},
			wantKeys: []string{dev + " config.colorB"},
		},
		{
			name: "AMBIGUOUS: both candidates verify", res: keyConfig, fields: [][3]string{{"AMBIGUOUS", "3", "5"}},
			wantReason: ReasonAmbiguousCandidates,
			wantMsg:    []string{"config.minA", "config.minB", "/data/AMBIGUOUS"},
		},
		{
			name: "TIMEOUT is only in the chart defaults", res: keyConfig, fields: [][3]string{{"TIMEOUT", "30", "45"}},
			wantReason: ReasonNoVerifiedCandidate, wantMsg: []string{"chart's own defaults", "config.timeout"},
		},
		{
			name: "FEATURE is set by a parameter", res: keyConfig, spec: feature, fields: [][3]string{{"FEATURE", "beta", "gamma"}},
			wantReason: ReasonNoVerifiedCandidate, wantMsg: []string{"helm.parameters", "config.feature"},
		},
		{
			name: "LOG_LEVEL_UP is derived, no value matches", res: keyConfig, fields: [][3]string{{"LOG_LEVEL_UP", "DEBUG", "TRACE"}},
			wantReason: ReasonNoVerifiedCandidate, wantMsg: []string{"derived"},
		},
		{
			name: "a candidate that also changes another resource is rejected", res: keyConfig,
			fields: [][3]string{{"REGION", "eu-central-1", "us-west-2"}}, rend: func(r *demoRenderer) { r.alsoOther = true },
			wantReason: ReasonNoVerifiedCandidate, wantMsg: []string{"also changes", "demo-helm-other"},
		},
		{
			name: "two fields, mapped independently", res: keyConfig,
			fields:   [][3]string{{"REGION", "eu-central-1", "us-west-2"}, {"EQUAL_A", "green", "blue"}},
			wantKeys: []string{dev + " global.region", dev + " config.colorA"}, renders: 2 + 1 + 2 + 1,
		},
		{
			name: "two fields, one cannot be mapped, the whole proposal is unmapped", res: keyConfig,
			fields:     [][3]string{{"REGION", "eu-central-1", "us-west-2"}, {"TIMEOUT", "30", "45"}},
			wantReason: ReasonNoVerifiedCandidate, wantMsg: []string{"/data/TIMEOUT"},
		},
		{
			name: "two fields that interact", res: keyConfig, rend: func(r *demoRenderer) { r.interact = true },
			fields:     [][3]string{{"EQUAL_A", "green", "blue"}, {"REGION", "eu-central-1", "mars"}},
			wantReason: ReasonEditsInteract, wantMsg: []string{"BOTH"},
		},
		{
			name: "a nondeterministic key elsewhere does not matter", res: keyConfig,
			rend: func(r *demoRenderer) { r.random = true }, fields: [][3]string{{"REGION", "eu-central-1", "us-west-2"}},
			wantKeys: []string{dev + " global.region"},
		},
		{
			name: "a resource with a random name does not matter", res: keyConfig,
			rend: func(r *demoRenderer) { r.randomCM = true }, fields: [][3]string{{"REGION", "eu-central-1", "us-west-2"}},
			wantKeys: []string{dev + " global.region"},
		},
		{
			name: "the drifted field is nondeterministic", res: keyConfig,
			rend: func(r *demoRenderer) { r.random = true }, fields: [][3]string{{"RANDOM", "r1", "x"}},
			wantReason: ReasonNondeterministicField,
		},
		{
			name: "the render does not give the old value", res: keyConfig, fields: [][3]string{{"LOG_LEVEL", "info", "trace"}},
			wantReason: ReasonRenderMismatch, wantMsg: []string{"\"debug\""},
		},
		{
			name: "the field is not in the render", res: keyConfig, fields: [][3]string{{"MISSING", "x", "y"}},
			wantReason: ReasonRenderMismatch, wantMsg: []string{"no such field"},
		},
		{
			name: "the resource is not in the render", res: ResourceKey{Kind: "ConfigMap", Namespace: "demo", Name: "gone"},
			fields:     [][3]string{{"LOG_LEVEL", "debug", "trace"}},
			wantReason: ReasonRenderMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newDemo(t, tt.spec)
			rend := &demoRenderer{src: env.src}
			if tt.rend != nil {
				tt.rend(rend)
			}
			var fields []Field
			for i, f := range tt.fields {
				fields = append(fields, drift(t, env.src, env.files, i, f[0], f[1], f[2]))
			}
			out, err := Verify(context.Background(), rend, env.input(tt.res, fields...), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if tt.renders != 0 && out.Renders != tt.renders {
				t.Errorf("renders = %d, want %d", out.Renders, tt.renders)
			}
			if tt.wantReason != "" {
				if out.Mapped() {
					t.Fatalf("mapped: %+v", out.Results)
				}
				if out.Unmapped.Reason != tt.wantReason {
					t.Errorf("reason = %s, want %s\n%s", out.Unmapped.Reason, tt.wantReason, out.Unmapped.Message)
				}
				for _, s := range tt.wantMsg {
					if !strings.Contains(out.Unmapped.Message, s) {
						t.Errorf("message %q does not contain %q", out.Unmapped.Message, s)
					}
				}
				return
			}
			if !out.Mapped() {
				t.Fatalf("unmapped: %s: %s", out.Unmapped.Reason, out.Unmapped.Message)
			}
			var got []string
			for _, r := range out.Results {
				for _, e := range r.Edits {
					got = append(got, r.File+" "+decodePointer(e.Location))
				}
				if r.Diff == "" || len(r.Edited) == 0 {
					t.Errorf("result for %s has no diff or file", r.File)
				}
			}
			if strings.Join(got, "|") != strings.Join(tt.wantKeys, "|") {
				t.Errorf("edits = %v, want %v", got, tt.wantKeys)
			}
		})
	}
}

// decodePointer turns /config/logLevel into config.logLevel.
func decodePointer(p string) string {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	return strings.Join(parts, ".")
}

func TestVerifyResultContent(t *testing.T) {
	env := newDemo(t, Spec{})
	rend := &demoRenderer{src: env.src}
	f1 := drift(t, env.src, env.files, 0, "REGION", "eu-central-1", "us-west-2")
	f2 := drift(t, env.src, env.files, 1, "EQUAL_A", "green", "blue")
	out, err := Verify(context.Background(), rend, env.input(keyConfig, f1, f2), Options{})
	if err != nil || !out.Mapped() {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results = %d, want one file", len(out.Results))
	}
	r := out.Results[0]
	if r.File != "helm/demo/values-dev.yaml" || len(r.Edits) != 2 {
		t.Fatalf("result = %+v", r)
	}
	if r.Edits[0].ChangeIndex != 0 || r.Edits[1].ChangeIndex != 1 {
		t.Errorf("change indexes = %d, %d", r.Edits[0].ChangeIndex, r.Edits[1].ChangeIndex)
	}
	want := strings.NewReplacer("colorA: green", "colorA: blue", "region: eu-central-1", "region: us-west-2").
		Replace(string(env.files[r.File]))
	if string(r.Edited) != want {
		t.Errorf("edited file is not the original with the two lines replaced:\n%s", r.Diff)
	}
	if !strings.Contains(r.Diff, "-  region: eu-central-1") || !strings.Contains(r.Diff, "+  region: us-west-2") {
		t.Errorf("diff = %s", r.Diff)
	}
	if string(env.files[r.File]) == string(r.Edited) {
		t.Error("the input files were modified in place")
	}
}

func TestVerifySecretIsNeverRendered(t *testing.T) {
	env := newDemo(t, Spec{})
	rend := &demoRenderer{src: env.src}
	f := drift(t, env.src, env.files, 0, "LOG_LEVEL", "debug", "trace")
	in := env.input(ResourceKey{Kind: "Secret", Namespace: "demo", Name: "s"}, f)
	out, err := Verify(context.Background(), rend, in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Mapped() || out.Unmapped.Reason != ReasonSecretNotVerifiable || rend.calls != 0 {
		t.Errorf("out = %+v, renders = %d", out.Unmapped, rend.calls)
	}
	// A Secret of another API group is not a core Secret.
	in.Resource = ResourceKey{Group: "example.com", Kind: "Secret", Namespace: "demo", Name: "s"}
	out, _ = Verify(context.Background(), rend, in, Options{})
	if out.Unmapped == nil || out.Unmapped.Reason == ReasonSecretNotVerifiable {
		t.Errorf("a Secret of another group was taken for a core Secret: %+v", out.Unmapped)
	}
}

func TestVerifyUnsupportedChanges(t *testing.T) {
	env := newDemo(t, Spec{})
	rend := &demoRenderer{src: env.src}
	tests := map[string]v1alpha1.FieldChange{
		"remove":       {Path: "/data/LOG_LEVEL", Op: v1alpha1.OpRemove, Desired: `"debug"`},
		"add":          {Path: "/data/NEW", Op: v1alpha1.OpAdd, Live: `"x"`},
		"object value": {Path: "/data", Op: v1alpha1.OpReplace, Desired: `{"a":"b"}`, Live: `{"a":"c"}`},
		"null":         {Path: "/data/LOG_LEVEL", Op: v1alpha1.OpReplace, Desired: `null`, Live: `"x"`},
		"not json":     {Path: "/data/LOG_LEVEL", Op: v1alpha1.OpReplace, Desired: `debug`, Live: `"x"`},
	}
	for name, ch := range tests {
		t.Run(name, func(t *testing.T) {
			out, err := Verify(context.Background(), rend, env.input(keyConfig, Field{Change: ch, Found: &Result{}}), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if out.Mapped() || out.Unmapped.Reason != ReasonUnsupportedChange {
				t.Errorf("out = %+v", out.Unmapped)
			}
		})
	}
	if rend.calls != 0 {
		t.Errorf("%d renders for changes that cannot be verified", rend.calls)
	}
}

func TestVerifyBounds(t *testing.T) {
	env := newDemo(t, Spec{})

	t.Run("too many candidates", func(t *testing.T) {
		rend := &demoRenderer{src: env.src}
		f := drift(t, env.src, env.files, 0, "LOG_LEVEL", "debug", "trace")
		for i := range 11 {
			f.Found.Candidates = append(f.Found.Candidates, Candidate{File: "helm/demo/values-dev.yaml", Path: KeyPath{"k", strconv.Itoa(i)}})
		}
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Mapped() || out.Unmapped.Reason != ReasonTooManyCandidates || rend.calls != 0 {
			t.Errorf("out = %+v, renders = %d", out.Unmapped, rend.calls)
		}
	})

	t.Run("the limit is configurable", func(t *testing.T) {
		rend := &demoRenderer{src: env.src}
		f := drift(t, env.src, env.files, 0, "EQUAL_A", "green", "blue") // two candidates
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), Options{MaxCandidates: 1})
		if err != nil {
			t.Fatal(err)
		}
		if out.Mapped() || out.Unmapped.Reason != ReasonTooManyCandidates {
			t.Errorf("out = %+v", out.Unmapped)
		}
	})

	t.Run("time budget", func(t *testing.T) {
		rend := &demoRenderer{src: env.src, delay: 40 * time.Millisecond}
		f := drift(t, env.src, env.files, 0, "EQUAL_A", "green", "blue")
		start := time.Now()
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), Options{Budget: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if out.Mapped() || out.Unmapped.Reason != ReasonRenderBudgetExceeded {
			t.Fatalf("out = %+v", out.Unmapped)
		}
		if time.Since(start) > time.Second {
			t.Errorf("took %v", time.Since(start))
		}
		if out.Renders >= 4 {
			t.Errorf("%d renders: the budget did not stop the work", out.Renders)
		}
	})

	t.Run("the caller's context wins", func(t *testing.T) {
		rend := &demoRenderer{src: env.src, delay: 40 * time.Millisecond}
		f := drift(t, env.src, env.files, 0, "LOG_LEVEL", "debug", "trace")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Verify(ctx, rend, env.input(keyConfig, f), Options{}); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
}

func TestVerifyRenderErrors(t *testing.T) {
	env := newDemo(t, Spec{})
	f := drift(t, env.src, env.files, 0, "LOG_LEVEL", "debug", "trace")

	t.Run("the chart does not render", func(t *testing.T) {
		rend := &demoRenderer{src: env.src, baseErr: fmt.Errorf("%w: nil pointer", ErrRenderFailed)}
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonRenderFailed {
			t.Errorf("out = %+v, err = %v", out, err)
		}
	})
	t.Run("files too large", func(t *testing.T) {
		rend := &demoRenderer{src: env.src, baseErr: ErrFilesTooLarge}
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonFilesTooLarge {
			t.Errorf("out = %+v, err = %v", out, err)
		}
	})
	t.Run("a transient error is returned, not turned into Unmapped", func(t *testing.T) {
		boom := errors.New("argocd: unreachable")
		rend := &demoRenderer{src: env.src, baseErr: boom}
		if _, err := Verify(context.Background(), rend, env.input(keyConfig, f), Options{}); !errors.Is(err, boom) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a candidate the chart cannot render is rejected, not fatal", func(t *testing.T) {
		rend := &demoRenderer{src: env.src, failWhen: func(v map[string]any) error {
			if get(v, "config", "colorA") == "blue" {
				return fmt.Errorf("%w: bad color", ErrRenderFailed)
			}
			return nil
		}}
		ff := drift(t, env.src, env.files, 0, "EQUAL_A", "green", "blue")
		out, err := Verify(context.Background(), rend, env.input(keyConfig, ff), Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonNoVerifiedCandidate ||
			!strings.Contains(out.Unmapped.Message, "does not render") {
			t.Errorf("out = %+v, err = %v", out.Unmapped, err)
		}
	})
}

func TestConflicts(t *testing.T) {
	a := parsedField{Field: Field{Change: v1alpha1.FieldChange{Path: "/data/A"}}, newValue: "x"}
	b := parsedField{Field: Field{Change: v1alpha1.FieldChange{Path: "/data/B"}}, newValue: "y"}
	same := Candidate{File: "f.yaml", Path: KeyPath{"k"}}
	other := Candidate{File: "f.yaml", Path: KeyPath{"j"}}
	if got := conflicts([]parsedField{a, b}, []Candidate{same, same}); got == "" {
		t.Error("one key set to two values was not reported")
	}
	b.newValue = "x"
	if got := conflicts([]parsedField{a, b}, []Candidate{same, same}); got != "" {
		t.Errorf("one key set to the same value twice: %s", got)
	}
	b.newValue = "y"
	if got := conflicts([]parsedField{a, b}, []Candidate{same, other}); got != "" {
		t.Errorf("two keys: %s", got)
	}
}
