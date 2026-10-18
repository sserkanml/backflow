package helmmap

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	// deployment adds the Deployment demo-helm, whose image is "repository:tag".
	deployment bool
	// Where else image.tag goes in that Deployment.
	tagLabel2 bool // metadata.labels and spec.template.metadata.labels app.kubernetes.io/version
	tagSelect bool // spec.selector.matchLabels and the pod template labels: the selector
	tagAnnot  bool // metadata.annotations example.com/version
	tagLabel  bool // data.IMAGE_TAG and the label app.kubernetes.io/version both follow image.tag
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
	if d.tagLabel {
		data["IMAGE_TAG"] = str(get(values, "image", "tag"))
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
	if d.tagLabel {
		objs[0].SetLabels(map[string]string{"app.kubernetes.io/version": str(get(values, "image", "tag"))})
	}
	if d.deployment {
		objs = append(objs, d.deploymentObject(app, values))
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
			// Both fields come from config.logLevel, so one edit reproduces them.
			name: "LOG_LEVEL and LOG_LEVEL_UP drifted together", res: keyConfig,
			fields:   [][3]string{{"LOG_LEVEL", "debug", "trace"}, {"LOG_LEVEL_UP", "DEBUG", "TRACE"}},
			wantKeys: []string{dev + " config.logLevel"}, renders: 3,
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

func TestVerifyNearMiss(t *testing.T) {
	t.Run("a derived field moves with the edit", func(t *testing.T) {
		env := newDemo(t, Spec{})
		f := drift(t, env.src, env.files, 0, "LOG_LEVEL", "debug", "trace")
		out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, f), Options{})
		if err != nil || out.Mapped() {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
		if out.Unmapped.Reason != ReasonNoVerifiedCandidate || len(out.Unmapped.Fields) != 1 {
			t.Fatalf("unmapped = %+v", out.Unmapped)
		}
		nm := out.Unmapped.Fields[0].NearMiss
		if nm == nil {
			t.Fatal("no near miss")
		}
		if nm.File != "helm/demo/values-dev.yaml" || nm.Path.String() != "config.logLevel" || nm.Value != "trace" ||
			len(nm.Covers) != 1 || nm.Covers[0] != "/data/LOG_LEVEL" || nm.CollateralTotal != 1 || len(nm.Collateral) != 1 {
			t.Fatalf("near miss = %+v", nm)
		}
		c := nm.Collateral[0]
		if c.Resource != keyConfig || c.Pointer != "/data/LOG_LEVEL_UP" || c.Current != `"DEBUG"` || c.After != `"TRACE"` {
			t.Errorf("collateral = %+v", c)
		}
		for _, want := range []string{"config.logLevel", "/data/LOG_LEVEL_UP", `current "DEBUG"`, `after the edit "TRACE"`} {
			if !strings.Contains(out.Unmapped.Message, want) {
				t.Errorf("message %q does not contain %q", out.Unmapped.Message, want)
			}
		}
	})

	t.Run("image.tag also feeds the version label", func(t *testing.T) {
		env := newDemo(t, Spec{})
		f := drift(t, env.src, env.files, 0, "IMAGE_TAG", "1.27-alpine", "1.28-alpine")
		out, err := Verify(context.Background(), &demoRenderer{src: env.src, tagLabel: true}, env.input(keyConfig, f), Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonNoVerifiedCandidate {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
		nm := out.Unmapped.Fields[0].NearMiss
		if nm == nil || nm.Path.String() != "image.tag" || len(nm.Collateral) != 1 {
			t.Fatalf("near miss = %+v", nm)
		}
		if c := nm.Collateral[0]; c.Pointer != "/metadata/labels/app.kubernetes.io~1version" ||
			c.Current != `"1.27-alpine"` || c.After != `"1.28-alpine"` {
			t.Errorf("collateral = %+v", c)
		}
	})

	t.Run("no near miss when nothing reproduces the field", func(t *testing.T) {
		env := newDemo(t, Spec{})
		f := drift(t, env.src, env.files, 0, "TIMEOUT", "30", "45")
		out, _ := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, f), Options{})
		if out.Unmapped.Fields[0].NearMiss != nil {
			t.Errorf("near miss = %+v", out.Unmapped.Fields[0].NearMiss)
		}
	})

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
		fields := []parsedField{{Field: Field{Change: v1alpha1.FieldChange{Path: "/data/A"}, Found: &Result{}}}}
		cand := func(key string, n int) *tryout {
			return &tryout{cand: Candidate{File: "f.yaml", Path: KeyPath{key}}, origin: []int{0}, covers: []int{0}, collateral: make([]Collateral, n)}
		}
		bad := uncovered(fields, []*tryout{cand("many", 3), cand("few", 1), cand("some", 2)}, nil, collateralRule{})
		if len(bad) != 1 || bad[0].NearMiss == nil || bad[0].NearMiss.Path.String() != "few" {
			t.Errorf("bad = %+v", bad)
		}
	})
}

// deploymentObject renders the Deployment of the chart: its image is
// "repository:tag", and the tag goes to other places by the flags.
func (d *demoRenderer) deploymentObject(app App, values map[string]any) *unstructured.Unstructured {
	tag := str(get(values, "image", "tag"))
	replicas := int64(2)
	if n, ok := get(values, "replicaCount").(int); ok {
		replicas = int64(n)
	}
	podLabels := map[string]any{"app": "demo-helm"}
	obj := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "demo-helm", "namespace": app.DestNamespace},
		"spec": map[string]any{
			"replicas": replicas,
			"selector": map[string]any{"matchLabels": map[string]any{"app": "demo-helm"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": podLabels},
				"spec": map[string]any{"containers": []any{map[string]any{
					"name": "web", "image": str(get(values, "image", "repository")) + ":" + tag,
				}}},
			},
		},
	}
	meta := obj["metadata"].(map[string]any)
	spec := obj["spec"].(map[string]any)
	if d.tagLabel2 {
		meta["labels"] = map[string]any{"app.kubernetes.io/version": tag}
		podLabels["app.kubernetes.io/version"] = tag
	}
	if d.tagSelect {
		spec["selector"] = map[string]any{"matchLabels": map[string]any{"app": "demo-helm", "app.kubernetes.io/version": tag}}
		podLabels["app.kubernetes.io/version"] = tag
	}
	if d.tagAnnot {
		meta["annotations"] = map[string]any{"example.com/version": tag}
	}
	return &unstructured.Unstructured{Object: obj}
}

var keyDeploy = ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "demo-helm"}

const (
	imagePointer = "/spec/template/spec/containers/0/image"
	oldImage     = "nginxinc/nginx-unprivileged:1.27-alpine"
	newImage     = "nginxinc/nginx-unprivileged:1.28-alpine"
)

// imageDrift is the Field of a changed image: the keys that hold the whole
// value, and those that hold a part of it.
func imageDrift(t *testing.T, env demoEnv, live string) Field {
	t.Helper()
	f := drift(t, env.src, env.files, 0, "X", oldImage, live)
	f.Change.Path = imagePointer
	frag, err := env.src.FindFragments(env.files, oldImage, live)
	if err != nil {
		t.Fatal(err)
	}
	f.Fragments = frag
	return f
}

func TestVerifyFragments(t *testing.T) {
	const dev = "helm/demo/values-dev.yaml"

	t.Run("an image tag change maps to image.tag through the fragment", func(t *testing.T) {
		env := newDemo(t, Spec{})
		rend := &demoRenderer{src: env.src, deployment: true}
		f := imageDrift(t, env, newImage)
		if len(f.Found.Candidates) != 0 {
			t.Fatalf("no key holds the whole image, found %+v", f.Found.Candidates)
		}
		out, err := Verify(context.Background(), rend, env.input(keyDeploy, f), Options{})
		if err != nil || !out.Mapped() {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
		r := out.Results[0]
		if r.File != dev || len(r.Edits) != 1 || decodePointer(r.Edits[0].Location) != "image.tag" || r.Edits[0].Value != `"1.28-alpine"` {
			t.Errorf("result = %+v", r)
		}
		if !strings.Contains(r.Diff, "-  tag: 1.27-alpine") || !strings.Contains(r.Diff, "+  tag: 1.28-alpine") {
			t.Errorf("diff = %s", r.Diff)
		}
		if out.Renders != 3 {
			t.Errorf("renders = %d: the unmodified files twice and the one fragment", out.Renders)
		}
	})

	t.Run("a registry or repository that only the chart's defaults hold is reported", func(t *testing.T) {
		env := newDemo(t, Spec{})
		rend := &demoRenderer{src: env.src, deployment: true}
		f := imageDrift(t, env, "other/nginx-unprivileged:1.27-alpine")
		out, err := Verify(context.Background(), rend, env.input(keyDeploy, f), Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonNoVerifiedCandidate ||
			!strings.Contains(out.Unmapped.Message, "chart's own defaults") || !strings.Contains(out.Unmapped.Message, "image.repository") {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
	})

	t.Run("a key that holds the whole value is preferred: fragments are not even rendered", func(t *testing.T) {
		env := newDemo(t, Spec{})
		rend := &demoRenderer{src: env.src}
		f := drift(t, env.src, env.files, 0, "REGION", "eu-central-1", "us-west-2")
		// A fragment candidate that would be rendered if it were tried.
		f.Fragments = &Result{Candidates: []Candidate{{File: dev, Path: KeyPath{"replicaCount"}, Fragment: true, NewValue: "x"}}}
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), Options{})
		if err != nil || !out.Mapped() {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
		if decodePointer(out.Results[0].Edits[0].Location) != "global.region" {
			t.Errorf("edit = %+v", out.Results[0].Edits)
		}
		if out.Renders != 3 {
			t.Errorf("renders = %d, want the unmodified files twice and the one whole-value edit", out.Renders)
		}
	})

	t.Run("fragments follow when the whole-value edits do not verify", func(t *testing.T) {
		env := newDemo(t, Spec{})
		rend := &demoRenderer{src: env.src}
		// LOG_LEVEL: config.logLevel also moves LOG_LEVEL_UP, so it never verifies;
		// the fragment stage tries the same field with parts and fails the same way.
		f := drift(t, env.src, env.files, 0, "LOG_LEVEL", "debug", "trace")
		frag, _ := env.src.FindFragments(env.files, "debug", "trace")
		f.Fragments = frag
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonNoVerifiedCandidate {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
	})

	t.Run("the bound counts fragments too", func(t *testing.T) {
		env := newDemo(t, Spec{})
		rend := &demoRenderer{src: env.src, deployment: true}
		f := imageDrift(t, env, newImage)
		f.Found = &Result{}
		for i := range 8 {
			f.Found.Candidates = append(f.Found.Candidates, Candidate{File: dev, Path: KeyPath{"k", strconv.Itoa(i)}})
		}
		for i := range 3 {
			f.Fragments.Candidates = append(f.Fragments.Candidates, Candidate{File: dev, Path: KeyPath{"f", strconv.Itoa(i)}, Fragment: true, NewValue: "x"})
		}
		out, err := Verify(context.Background(), rend, env.input(keyDeploy, f), Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonTooManyCandidates {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
		if rend.calls > 2+8 {
			t.Errorf("%d renders: fragments were rendered although the bound was exceeded", rend.calls)
		}
	})
}

func collateralPointers(c []Collateral) []string {
	out := make([]string, len(c))
	for i, x := range c {
		out[i] = x.Pointer
	}
	return out
}

func TestVerifyCollateral(t *testing.T) {
	const (
		versionLabel = "/metadata/labels/app.kubernetes.io~1version"
		podLabel     = "/spec/template/metadata/labels/app.kubernetes.io~1version"
		selectorKey  = "/spec/selector/matchLabels/app.kubernetes.io~1version"
		annotation   = "/metadata/annotations/example.com~1version"
	)
	labels := []string{CollateralLabels}
	both := []string{CollateralLabels, CollateralAnnotations}
	tests := []struct {
		name      string
		rend      func(r *demoRenderer)
		allow     []string
		wantMap   bool
		collat    []string // allowed collateral, when mapped
		nearMiss  []string // the other changes of the near miss, when not
		renders   int
		reasonHas string
	}{
		{
			name:     "the tag also feeds a version label: strict stays unmapped with the near miss",
			rend:     func(r *demoRenderer) { r.tagLabel2 = true },
			nearMiss: []string{versionLabel, podLabel},
		},
		{
			name: "the same with Labels allowed is mapped, and the labels are listed",
			rend: func(r *demoRenderer) { r.tagLabel2 = true }, allow: labels,
			wantMap: true, collat: []string{versionLabel, podLabel}, renders: 3,
		},
		{
			name: "the tag also feeds a selector: unmapped even with Labels",
			rend: func(r *demoRenderer) { r.tagSelect = true }, allow: both,
			nearMiss: []string{selectorKey},
		},
		{
			name: "the tag also feeds an annotation, only Labels allowed: unmapped",
			rend: func(r *demoRenderer) { r.tagAnnot = true }, allow: labels,
			nearMiss: []string{annotation},
		},
		{
			name: "the tag feeds a label and an annotation, only Labels allowed: unmapped, the annotation is the problem",
			rend: func(r *demoRenderer) { r.tagLabel2, r.tagAnnot = true, true }, allow: labels,
			nearMiss: []string{annotation},
		},
		{
			name: "the same with Labels and Annotations: mapped",
			rend: func(r *demoRenderer) { r.tagLabel2, r.tagAnnot = true, true }, allow: both,
			wantMap: true, collat: []string{annotation, versionLabel, podLabel},
		},
		{
			name: "annotations alone are not labels",
			rend: func(r *demoRenderer) { r.tagLabel2 = true }, allow: []string{CollateralAnnotations},
			nearMiss: []string{versionLabel, podLabel},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newDemo(t, Spec{})
			rend := &demoRenderer{src: env.src, deployment: true}
			tt.rend(rend)
			f := imageDrift(t, env, newImage)
			out, err := Verify(context.Background(), rend, env.input(keyDeploy, f), Options{AllowCollateral: tt.allow})
			if err != nil {
				t.Fatal(err)
			}
			if out.Mapped() != tt.wantMap {
				t.Fatalf("mapped = %v: %+v", out.Mapped(), out.Unmapped)
			}
			if tt.wantMap {
				if decodePointer(out.Results[0].Edits[0].Location) != "image.tag" || len(out.Results[0].Edits) != 1 {
					t.Errorf("edits = %+v", out.Results[0].Edits)
				}
				got := collateralPointers(out.Collateral)
				slices.Sort(got)
				want := slices.Clone(tt.collat)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Errorf("collateral = %v, want %v", got, want)
				}
				for _, c := range out.Collateral {
					if c.Resource != keyDeploy || c.Current != `"1.27-alpine"` || c.After != `"1.28-alpine"` {
						t.Errorf("collateral = %+v", c)
					}
				}
				if tt.renders != 0 && out.Renders != tt.renders {
					t.Errorf("renders = %d, want %d: the stage with collateral renders nothing new", out.Renders, tt.renders)
				}
				return
			}
			if out.Unmapped.Reason != ReasonNoVerifiedCandidate {
				t.Fatalf("reason = %s: %s", out.Unmapped.Reason, out.Unmapped.Message)
			}
			nm := out.Unmapped.Fields[0].NearMiss
			if nm == nil || nm.Path.String() != "image.tag" {
				t.Fatalf("near miss = %+v", nm)
			}
			got := collateralPointers(nm.Collateral)
			slices.Sort(got)
			want := slices.Clone(tt.nearMiss)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("the near miss lists %v, want %v (what the policy does not allow)", got, want)
			}
		})
	}

	t.Run("strict is tried first: an edit that needs no collateral is used even when it is allowed", func(t *testing.T) {
		env := newDemo(t, Spec{})
		rend := &demoRenderer{src: env.src}
		f := drift(t, env.src, env.files, 0, "REGION", "eu-central-1", "us-west-2")
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), Options{AllowCollateral: both})
		if err != nil || !out.Mapped() || len(out.Collateral) != 0 {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
	})

	t.Run("a set of two edits lists the collateral of the combined render", func(t *testing.T) {
		env := newDemo(t, Spec{})
		rend := &demoRenderer{src: env.src, deployment: true, tagLabel2: true}
		f1 := imageDrift(t, env, newImage)
		found, err := env.src.Find(env.files, float64(2))
		if err != nil {
			t.Fatal(err)
		}
		f2 := Field{Index: 1, Found: found, Change: v1alpha1.FieldChange{Path: "/spec/replicas", Op: v1alpha1.OpReplace, Desired: "2", Live: "3"}}
		out, err := Verify(context.Background(), rend, env.input(keyDeploy, f1, f2), Options{AllowCollateral: labels})
		if err != nil || !out.Mapped() {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
		keys := make([]string, 0, len(out.Results[0].Edits))
		for _, e := range out.Results[0].Edits {
			keys = append(keys, decodePointer(e.Location))
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"image.tag", "replicaCount"}) {
			t.Errorf("edits = %v", keys)
		}
		got := collateralPointers(out.Collateral)
		slices.Sort(got)
		if !slices.Equal(got, []string{versionLabel, podLabel}) {
			t.Errorf("collateral = %v", got)
		}
	})
}

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
