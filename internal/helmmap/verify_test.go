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
	"github.com/sserkanml/backflow/internal/rendermap"
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
	altRegion bool // the top level key "region", when set, replaces global.region
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

func setPath(m map[string]any, p rendermap.KeyPath, v any) {
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

func (d *demoRenderer) Render(ctx context.Context, app rendermap.App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
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
			return nil, fmt.Errorf("%w: %s: %v", rendermap.ErrRenderFailed, name, err)
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
	if r := get(values, "region"); d.altRegion && r != nil {
		region = str(r)
	}
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
	appDemo   = rendermap.App{Name: "demo-helm", Namespace: "argocd", Project: "default", DestNamespace: "demo"}
	keyConfig = rendermap.ResourceKey{Kind: "ConfigMap", Namespace: "demo", Name: "demo-helm-config"}
	keySide   = rendermap.ResourceKey{Kind: "ConfigMap", Namespace: "demo", Name: "demo-helm-sidecar"}
)

// drift builds a Field for data.<key> going from desired to live, with the
// candidates found for desired.
func drift(t *testing.T, src *Source, files map[string][]byte, index int, key, desired, live string) rendermap.Field {
	t.Helper()
	d, l := strconv.Quote(desired), strconv.Quote(live)
	found, err := src.Find(files, desired)
	if err != nil {
		t.Fatal(err)
	}
	return rendermap.Field{
		Index:  index,
		Change: v1alpha1.FieldChange{Path: "/data/" + key, Op: v1alpha1.OpReplace, Desired: d, Live: l},
		Found:  found.Found(),
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

func (e demoEnv) input(res rendermap.ResourceKey, fields ...rendermap.Field) Input {
	return Input{App: appDemo, Resource: res, Fields: fields, Source: e.src, Files: e.files}
}

func TestVerifyDemoCases(t *testing.T) {
	const dev = "helm/demo/values-dev.yaml"
	feature := Spec{Path: "helm/demo", ValueFiles: []string{"values-dev.yaml"},
		Parameters: []Parameter{{Name: "config.feature", Value: "beta"}}}

	tests := []struct {
		name string
		spec Spec
		res  rendermap.ResourceKey
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
			wantReason: rendermap.ReasonValueAffectsOtherFields, wantMsg: []string{"config.logLevel", "also changes", "LOG_LEVEL_UP"},
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
			wantReason: rendermap.ReasonAmbiguousCandidates,
			wantMsg:    []string{"config.minA", "config.minB", "/data/AMBIGUOUS"},
		},
		{
			name: "TIMEOUT is only in the chart defaults", res: keyConfig, fields: [][3]string{{"TIMEOUT", "30", "45"}},
			wantReason: rendermap.ReasonNoVerifiedCandidate, wantMsg: []string{"chart's own defaults", "config.timeout"},
		},
		{
			name: "FEATURE is set by a parameter", res: keyConfig, spec: feature, fields: [][3]string{{"FEATURE", "beta", "gamma"}},
			wantReason: rendermap.ReasonNoVerifiedCandidate, wantMsg: []string{"helm.parameters", "config.feature"},
		},
		{
			name: "LOG_LEVEL_UP is derived, no value matches", res: keyConfig, fields: [][3]string{{"LOG_LEVEL_UP", "DEBUG", "TRACE"}},
			wantReason: rendermap.ReasonNoVerifiedCandidate, wantMsg: []string{"derived"},
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
			wantReason: rendermap.ReasonValueAffectsOtherFields, wantMsg: []string{"also changes", "demo-helm-other"},
		},
		{
			name: "two fields, mapped independently", res: keyConfig,
			fields:   [][3]string{{"REGION", "eu-central-1", "us-west-2"}, {"EQUAL_A", "green", "blue"}},
			wantKeys: []string{dev + " global.region", dev + " config.colorA"}, renders: 2 + 1 + 2 + 1,
		},
		{
			name: "two fields, one cannot be mapped, the whole proposal is unmapped", res: keyConfig,
			fields:     [][3]string{{"REGION", "eu-central-1", "us-west-2"}, {"TIMEOUT", "30", "45"}},
			wantReason: rendermap.ReasonNoVerifiedCandidate, wantMsg: []string{"/data/TIMEOUT"},
		},
		{
			name: "two fields that interact", res: keyConfig, rend: func(r *demoRenderer) { r.interact = true },
			fields:     [][3]string{{"EQUAL_A", "green", "blue"}, {"REGION", "eu-central-1", "mars"}},
			wantReason: rendermap.ReasonEditsInteract, wantMsg: []string{"BOTH"},
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
			wantReason: rendermap.ReasonNondeterministicField,
		},
		{
			name: "the render does not give the old value", res: keyConfig, fields: [][3]string{{"LOG_LEVEL", "info", "trace"}},
			wantReason: rendermap.ReasonRenderMismatch, wantMsg: []string{"\"debug\""},
		},
		{
			name: "the field is not in the render", res: keyConfig, fields: [][3]string{{"MISSING", "x", "y"}},
			wantReason: rendermap.ReasonRenderMismatch, wantMsg: []string{"no such field"},
		},
		{
			name: "the resource is not in the render", res: rendermap.ResourceKey{Kind: "ConfigMap", Namespace: "demo", Name: "gone"},
			fields:     [][3]string{{"LOG_LEVEL", "debug", "trace"}},
			wantReason: rendermap.ReasonRenderMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newDemo(t, tt.spec)
			rend := &demoRenderer{src: env.src}
			if tt.rend != nil {
				tt.rend(rend)
			}
			var fields []rendermap.Field
			for i, f := range tt.fields {
				fields = append(fields, drift(t, env.src, env.files, i, f[0], f[1], f[2]))
			}
			out, err := Verify(context.Background(), rend, env.input(tt.res, fields...), rendermap.Options{})
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
	out, err := Verify(context.Background(), rend, env.input(keyConfig, f1, f2), rendermap.Options{})
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
	in := env.input(rendermap.ResourceKey{Kind: "Secret", Namespace: "demo", Name: "s"}, f)
	out, err := Verify(context.Background(), rend, in, rendermap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Mapped() || out.Unmapped.Reason != rendermap.ReasonSecretNotVerifiable || rend.calls != 0 {
		t.Errorf("out = %+v, renders = %d", out.Unmapped, rend.calls)
	}
	// A Secret of another API group is not a core Secret.
	in.Resource = rendermap.ResourceKey{Group: "example.com", Kind: "Secret", Namespace: "demo", Name: "s"}
	out, _ = Verify(context.Background(), rend, in, rendermap.Options{})
	if out.Unmapped == nil || out.Unmapped.Reason == rendermap.ReasonSecretNotVerifiable {
		t.Errorf("a Secret of another group was taken for a core Secret: %+v", out.Unmapped)
	}
}

func TestVerifyUnsupportedChanges(t *testing.T) {
	env := newDemo(t, Spec{})
	rend := &demoRenderer{src: env.src}
	tests := map[string]v1alpha1.FieldChange{
		"null":           {Path: "/data/LOG_LEVEL", Op: v1alpha1.OpReplace, Desired: `null`, Live: `"x"`},
		"null live":      {Path: "/data/NEW", Op: v1alpha1.OpAdd, Live: `null`},
		"not json":       {Path: "/data/LOG_LEVEL", Op: v1alpha1.OpReplace, Desired: `debug`, Live: `"x"`},
		"whole resource": {Path: "", Op: v1alpha1.OpAdd, Live: `{"a":"b"}`},
		"unknown op":     {Path: "/data/LOG_LEVEL", Op: "Move", Desired: `"debug"`, Live: `"x"`},
	}
	for name, ch := range tests {
		t.Run(name, func(t *testing.T) {
			out, err := Verify(context.Background(), rend, env.input(keyConfig, rendermap.Field{Change: ch, Found: &rendermap.Found{}}), rendermap.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if out.Mapped() || out.Unmapped.Reason != rendermap.ReasonUnsupportedChange {
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
		f.Found.Candidates = nil
		for i := range 11 {
			f.Found.Candidates = append(f.Found.Candidates, rendermap.Candidate{File: "helm/demo/values-dev.yaml", Path: rendermap.KeyPath{"k", strconv.Itoa(i)}})
		}
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), rendermap.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Mapped() || out.Unmapped.Reason != rendermap.ReasonTooManyCandidates || rend.calls != 2 {
			t.Errorf("out = %+v, renders = %d: none of the keys fits the field, and only the unmodified files are rendered", out.Unmapped, rend.calls)
		}
	})

	t.Run("a tie at the top is not cut", func(t *testing.T) {
		rend := &demoRenderer{src: env.src}
		f := drift(t, env.src, env.files, 0, "LOG_LEVEL", "debug", "trace")
		f.Found.Candidates = nil
		for i := range 11 {
			f.Found.Candidates = append(f.Found.Candidates, rendermap.Candidate{File: "helm/demo/values-dev.yaml", Path: rendermap.KeyPath{"logLevel" + strconv.Itoa(i)}})
		}
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), rendermap.Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonTooManyCandidates {
			t.Errorf("out = %+v, err = %v: eleven keys fit equally well", out, err)
		}
	})

	t.Run("the limit is configurable", func(t *testing.T) {
		rend := &demoRenderer{src: env.src}
		f := drift(t, env.src, env.files, 0, "EQUAL_A", "green", "blue") // two candidates
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), rendermap.Options{MaxCandidates: 1})
		if err != nil {
			t.Fatal(err)
		}
		if out.Mapped() || out.Unmapped.Reason != rendermap.ReasonTooManyCandidates {
			t.Errorf("out = %+v", out.Unmapped)
		}
	})

	t.Run("time budget", func(t *testing.T) {
		rend := &demoRenderer{src: env.src, delay: 40 * time.Millisecond}
		f := drift(t, env.src, env.files, 0, "EQUAL_A", "green", "blue")
		start := time.Now()
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), rendermap.Options{Budget: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if out.Mapped() || out.Unmapped.Reason != rendermap.ReasonRenderBudgetExceeded {
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
		if _, err := Verify(ctx, rend, env.input(keyConfig, f), rendermap.Options{}); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
}

// TestVerifyBoundsUntried: keys left out by the bound on their number.
func TestVerifyBoundsUntried(t *testing.T) {
	// The keys the tests leave out are k.0 ... k.10. A template reads the one
	// that the test names, or none of them.
	withTemplate := func(t *testing.T, text string) demoEnv {
		t.Helper()
		e := newDemo(t, Spec{})
		e.files["helm/demo/templates/cm.yaml"] = []byte(text)
		return e
	}
	narrowed := func(e demoEnv) *rendermap.Outcome {
		rend := &demoRenderer{src: e.src, noDerived: true}
		f := drift(t, e.src, e.files, 0, "LOG_LEVEL", "debug", "trace")
		for i := range 11 {
			f.Found.Candidates = append(f.Found.Candidates, rendermap.Candidate{File: "helm/demo/values-dev.yaml", Path: rendermap.KeyPath{"k", strconv.Itoa(i)}})
		}
		out, err := Verify(context.Background(), rend, e.input(keyConfig, f), rendermap.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	t.Run("too many candidates are narrowed down by the names of the keys, when the ones left out are not read", func(t *testing.T) {
		out := narrowed(withTemplate(t, "level: {{ .Values.config.logLevel }}\n"))
		if !out.Mapped() {
			t.Fatalf("out = %+v", out.Unmapped)
		}
		if decodePointer(out.Results[0].Edits[0].Location) != "config.logLevel" || out.Renders != 3 {
			t.Errorf("edits = %+v, renders = %d: only the key that fits the field is rendered", out.Results[0].Edits, out.Renders)
		}
	})

	t.Run("keys left out that a template may read are rendered to check, and none of these verifies", func(t *testing.T) {
		// The keys k.N are not in the file: an edit of them cannot even be written.
		out := narrowed(withTemplate(t, "level: {{ .Values.config.logLevel }}\nother: {{ .Values.k.3 }}\n"))
		if !out.Mapped() || decodePointer(out.Results[0].Edits[0].Location) != "config.logLevel" {
			t.Fatalf("out = %+v", out.Unmapped)
		}
	})

	t.Run("a template that reads every key proves nothing, so the keys left out are checked by rendering", func(t *testing.T) {
		out := narrowed(withTemplate(t, "all: {{ toYaml .Values }}\n"))
		if !out.Mapped() {
			t.Fatalf("out = %+v", out.Unmapped)
		}
	})

	t.Run("more keys to check than may be rendered: too many candidates", func(t *testing.T) {
		e := withTemplate(t, "all: {{ toYaml .Values }}\n")
		rend := &demoRenderer{src: e.src, noDerived: true}
		f := drift(t, e.src, e.files, 0, "LOG_LEVEL", "debug", "trace")
		for i := range 11 {
			f.Found.Candidates = append(f.Found.Candidates, rendermap.Candidate{File: "helm/demo/values-dev.yaml", Path: rendermap.KeyPath{"k", strconv.Itoa(i)}})
		}
		out, err := Verify(context.Background(), rend, e.input(keyConfig, f), rendermap.Options{MaxConfirmations: 5})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonTooManyCandidates ||
			!strings.Contains(out.Unmapped.Message, "more than the 5 that are rendered to check") {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
	})

	t.Run("without templates nothing is proven either, the keys are checked by rendering", func(t *testing.T) {
		out := narrowed(newDemo(t, Spec{}))
		if !out.Mapped() {
			t.Fatalf("out = %+v", out.Unmapped)
		}
	})

	t.Run("a key of a later stage left out does not compete with an edit of an earlier one", func(t *testing.T) {
		e := withTemplate(t, "level: {{ .Values.config.logLevel }}\nother: {{ .Values.x.3 }}\n")
		rend := &demoRenderer{src: e.src, noDerived: true}
		f := drift(t, e.src, e.files, 0, "LOG_LEVEL", "debug", "trace")
		f.Fragments = &rendermap.Found{}
		for i := range 30 {
			f.Fragments.Candidates = append(f.Fragments.Candidates, rendermap.Candidate{File: "helm/demo/values-dev.yaml", Path: rendermap.KeyPath{"x", strconv.Itoa(i)}, Fragment: true, NewValue: "t"})
		}
		out, err := Verify(context.Background(), rend, e.input(keyConfig, f), rendermap.Options{MaxConfirmations: 1})
		if err != nil || !out.Mapped() {
			t.Fatalf("out = %+v, err = %v: fragments are only tried when no whole-value edit verifies", out, err)
		}
	})

	t.Run("a field that no tried key explains does not claim that none exists", func(t *testing.T) {
		e := withTemplate(t, "x: {{ .Values.k.3 }}\n")
		rend := &demoRenderer{src: e.src}
		f := drift(t, e.src, e.files, 0, "LOG_LEVEL", "debug", "trace") // config.logLevel also moves LOG_LEVEL_UP
		for i := range 11 {
			f.Found.Candidates = append(f.Found.Candidates, rendermap.Candidate{File: "helm/demo/values-dev.yaml", Path: rendermap.KeyPath{"k", strconv.Itoa(i)}})
		}
		out, err := Verify(context.Background(), rend, e.input(keyConfig, f), rendermap.Options{})
		if err != nil || out.Mapped() || !strings.Contains(out.Unmapped.Message, "were not tried") {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
	})

	t.Run("many keys that hold a part of the value do not fail a field that has a key for the whole value", func(t *testing.T) {
		env := withTemplate(t, "level: {{ .Values.config.logLevel }}\n")
		rend := &demoRenderer{src: env.src, noDerived: true}
		f := drift(t, env.src, env.files, 0, "LOG_LEVEL", "debug", "trace")
		f.Fragments = &rendermap.Found{}
		for i := range 30 {
			f.Fragments.Candidates = append(f.Fragments.Candidates, rendermap.Candidate{File: "helm/demo/values-dev.yaml", Path: rendermap.KeyPath{"x", strconv.Itoa(i)}, Fragment: true, NewValue: "t"})
		}
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), rendermap.Options{})
		if err != nil || !out.Mapped() || decodePointer(out.Results[0].Edits[0].Location) != "config.logLevel" {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
	})

}

func TestVerifyRenderErrors(t *testing.T) {
	env := newDemo(t, Spec{})
	f := drift(t, env.src, env.files, 0, "LOG_LEVEL", "debug", "trace")

	t.Run("the chart does not render", func(t *testing.T) {
		rend := &demoRenderer{src: env.src, baseErr: fmt.Errorf("%w: nil pointer", rendermap.ErrRenderFailed)}
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), rendermap.Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonRenderFailed {
			t.Errorf("out = %+v, err = %v", out, err)
		}
	})
	t.Run("files too large", func(t *testing.T) {
		rend := &demoRenderer{src: env.src, baseErr: rendermap.ErrFilesTooLarge}
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), rendermap.Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonFilesTooLarge {
			t.Errorf("out = %+v, err = %v", out, err)
		}
	})
	t.Run("a transient error is returned, not turned into Unmapped", func(t *testing.T) {
		boom := errors.New("argocd: unreachable")
		rend := &demoRenderer{src: env.src, baseErr: boom}
		if _, err := Verify(context.Background(), rend, env.input(keyConfig, f), rendermap.Options{}); !errors.Is(err, boom) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a candidate the chart cannot render is rejected, not fatal", func(t *testing.T) {
		rend := &demoRenderer{src: env.src, failWhen: func(v map[string]any) error {
			if get(v, "config", "colorA") == "blue" {
				return fmt.Errorf("%w: bad color", rendermap.ErrRenderFailed)
			}
			return nil
		}}
		ff := drift(t, env.src, env.files, 0, "EQUAL_A", "green", "blue")
		out, err := Verify(context.Background(), rend, env.input(keyConfig, ff), rendermap.Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonNoVerifiedCandidate ||
			!strings.Contains(out.Unmapped.Message, "does not render") {
			t.Errorf("out = %+v, err = %v", out.Unmapped, err)
		}
	})
}

func TestVerifyNearMiss(t *testing.T) {
	t.Run("a derived field moves with the edit", func(t *testing.T) {
		env := newDemo(t, Spec{})
		f := drift(t, env.src, env.files, 0, "LOG_LEVEL", "debug", "trace")
		out, err := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, f), rendermap.Options{})
		if err != nil || out.Mapped() {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
		if out.Unmapped.Reason != rendermap.ReasonValueAffectsOtherFields || len(out.Unmapped.Fields) != 1 {
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
		out, err := Verify(context.Background(), &demoRenderer{src: env.src, tagLabel: true}, env.input(keyConfig, f), rendermap.Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonValueAffectsOtherFields {
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
		out, _ := Verify(context.Background(), &demoRenderer{src: env.src}, env.input(keyConfig, f), rendermap.Options{})
		if out.Unmapped.Fields[0].NearMiss != nil {
			t.Errorf("near miss = %+v", out.Unmapped.Fields[0].NearMiss)
		}
	})

}

// deploymentObject renders the Deployment of the chart: its image is
// "repository:tag", and the tag goes to other places by the flags.
func (d *demoRenderer) deploymentObject(app rendermap.App, values map[string]any) *unstructured.Unstructured {
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

var keyDeploy = rendermap.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "demo-helm"}

const (
	imagePointer = "/spec/template/spec/containers/0/image"
	oldImage     = "nginxinc/nginx-unprivileged:1.27-alpine"
	newImage     = "nginxinc/nginx-unprivileged:1.28-alpine"
)

// imageDrift is the Field of a changed image: the keys that hold the whole
// value, and those that hold a part of it.
func imageDrift(t *testing.T, env demoEnv, live string) rendermap.Field {
	t.Helper()
	f := drift(t, env.src, env.files, 0, "X", oldImage, live)
	f.Change.Path = imagePointer
	frag, err := env.src.FindFragments(env.files, oldImage, live)
	if err != nil {
		t.Fatal(err)
	}
	f.Fragments = frag.Found()
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
		out, err := Verify(context.Background(), rend, env.input(keyDeploy, f), rendermap.Options{})
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
		out, err := Verify(context.Background(), rend, env.input(keyDeploy, f), rendermap.Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonNoVerifiedCandidate ||
			!strings.Contains(out.Unmapped.Message, "chart's own defaults") || !strings.Contains(out.Unmapped.Message, "image.repository") {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
	})

	t.Run("a key that holds the whole value is preferred: fragments are not even rendered", func(t *testing.T) {
		env := newDemo(t, Spec{})
		rend := &demoRenderer{src: env.src}
		f := drift(t, env.src, env.files, 0, "REGION", "eu-central-1", "us-west-2")
		// A fragment candidate that would be rendered if it were tried.
		f.Fragments = (&Result{Candidates: []rendermap.Candidate{{File: dev, Path: rendermap.KeyPath{"replicaCount"}, Fragment: true, NewValue: "x"}}}).Found()
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), rendermap.Options{})
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
		f.Fragments = frag.Found()
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), rendermap.Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonValueAffectsOtherFields {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
	})

	t.Run("the bound counts fragments too", func(t *testing.T) {
		env := newDemo(t, Spec{})
		rend := &demoRenderer{src: env.src, deployment: true}
		f := imageDrift(t, env, newImage)
		f.Found = &rendermap.Found{}
		for i := range 8 {
			f.Found.Candidates = append(f.Found.Candidates, rendermap.Candidate{File: dev, Path: rendermap.KeyPath{"k", strconv.Itoa(i)}})
		}
		for i := range 3 {
			f.Fragments.Candidates = append(f.Fragments.Candidates, rendermap.Candidate{File: dev, Path: rendermap.KeyPath{"f", strconv.Itoa(i)}, Fragment: true, NewValue: "x"})
		}
		out, err := Verify(context.Background(), rend, env.input(keyDeploy, f), rendermap.Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonTooManyCandidates {
			t.Fatalf("out = %+v, err = %v", out.Unmapped, err)
		}
		if rend.calls > 2+8 {
			t.Errorf("%d renders: fragments were rendered although the bound was exceeded", rend.calls)
		}
	})
}

func collateralPointers(c []rendermap.Collateral) []string {
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
	labels := []string{rendermap.CollateralLabels}
	both := []string{rendermap.CollateralLabels, rendermap.CollateralAnnotations}
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
			rend: func(r *demoRenderer) { r.tagLabel2 = true }, allow: []string{rendermap.CollateralAnnotations},
			nearMiss: []string{versionLabel, podLabel},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newDemo(t, Spec{})
			rend := &demoRenderer{src: env.src, deployment: true}
			tt.rend(rend)
			f := imageDrift(t, env, newImage)
			out, err := Verify(context.Background(), rend, env.input(keyDeploy, f), rendermap.Options{AllowCollateral: tt.allow})
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
			if out.Unmapped.Reason != rendermap.ReasonValueAffectsOtherFields {
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
		out, err := Verify(context.Background(), rend, env.input(keyConfig, f), rendermap.Options{AllowCollateral: both})
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
		f2 := rendermap.Field{Index: 1, Found: found.Found(), Change: v1alpha1.FieldChange{Path: "/spec/replicas", Op: v1alpha1.OpReplace, Desired: "2", Live: "3"}}
		out, err := Verify(context.Background(), rend, env.input(keyDeploy, f1, f2), rendermap.Options{AllowCollateral: labels})
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
