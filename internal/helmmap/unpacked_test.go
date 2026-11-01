package helmmap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/sserkanml/backflow/internal/metrics/metricstest"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// chartFile makes the files of a small chart: one ConfigMap, named after the release
// and the chart, with a value from values.yaml.
func depTestChart(dir, name string, deps ...string) map[string][]byte {
	chart := "apiVersion: v2\nname: " + name + "\nversion: 0.1.0\n"
	if len(deps) > 0 {
		chart += "dependencies:\n" + strings.Join(deps, "")
	}
	return map[string][]byte{
		dir + "/Chart.yaml":  []byte(chart),
		dir + "/values.yaml": []byte("greeting: hello\nenabled: true\n"),
		dir + "/templates/cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-" + name +
			"\ndata:\n  greeting: {{ .Values.greeting | quote }}\n"),
	}
}

func depLine(name, repo string, extra ...string) string {
	return "  - name: " + name + "\n    version: 0.1.0\n    repository: " + repo + "\n" + strings.Join(extra, "")
}

func mergeFiles(sets ...map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for _, s := range sets {
		maps.Copy(out, s)
	}
	return out
}

// argoSim renders the way Argo CD's repo server does: `helm template`, and only when Helm
// says a dependency is missing in charts/, `helm dependency build` and `helm template` again.
type argoSim struct {
	t       testing.TB
	version string

	mu      sync.Mutex
	renders int
	builds  int
}

func (a *argoSim) ArgoVersion(context.Context) (string, error) { return a.version, nil }

func (a *argoSim) Render(ctx context.Context, _ rendermap.App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	requireHelm(a.t)
	dir := a.t.TempDir()
	for name, data := range files {
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return nil, err
		}
	}
	a.mu.Lock()
	a.renders++
	a.mu.Unlock()
	chart := filepath.Join(dir, "apps", "app")
	out, err := helm(ctx, "template", "rel", chart)
	if err != nil && (strings.Contains(err.Error(), "found in Chart.yaml, but missing in charts/ directory") ||
		strings.Contains(err.Error(), "found in requirements.yaml, but missing in charts")) {
		a.mu.Lock()
		a.builds++
		a.mu.Unlock()
		if _, berr := helm(ctx, "dependency", "build", chart); berr != nil {
			return nil, fmt.Errorf("%w: %v", rendermap.ErrRenderFailed, berr)
		}
		out, err = helm(ctx, "template", "rel", chart)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", rendermap.ErrRenderFailed, err)
	}
	var objs []*unstructured.Unstructured
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		if len(m) > 0 {
			objs = append(objs, &unstructured.Unstructured{Object: m})
		}
	}
	return objs, nil
}

// helm runs the helm binary with no repositories configured, like the repo server of an
// Argo CD that has none: `helm dependency build` refreshes every repository of the user
// otherwise, which takes seconds and needs the network.
func helm(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "helm", args...)
	home := helmHome()
	cmd.Env = append(os.Environ(), "HELM_CONFIG_HOME="+home, "HELM_CACHE_HOME="+home, "HELM_DATA_HOME="+home,
		"HELM_REPOSITORY_CONFIG="+filepath.Join(home, "repositories.yaml"), "HELM_REPOSITORY_CACHE="+filepath.Join(home, "repository"))
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("helm %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func init() { logf.SetLogger(logr.Discard()) }

var (
	helmHomeOnce sync.Once
	helmHomeDir  string
)

// helmHome is an empty directory for Helm's configuration, made once per test run.
func helmHome() string {
	helmHomeOnce.Do(func() {
		helmHomeDir, _ = os.MkdirTemp("", "backflow-helm-home-")
	})
	return helmHomeDir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if helmHomeDir != "" {
		_ = os.RemoveAll(helmHomeDir)
	}
	os.Exit(code)
}

func requireHelm(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatal("the helm binary is not installed; CI must install it")
		}
		t.Skip("the helm binary is not installed")
	}
}

func objNames(objs []*unstructured.Unstructured) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.GetName())
	}
	slices.Sort(out)
	return out
}

func renderTwice(t *testing.T, files map[string][]byte, sim *argoSim) (first, second []*unstructured.Unstructured) {
	t.Helper()
	deps := NewUnpackedDeps()
	r := deps.Wrap(sim, &Source{ChartDir: "apps/app"})
	var err error
	if first, err = r.Render(t.Context(), rendermap.App{}, files); err != nil {
		t.Fatal(err)
	}
	if second, err = r.Render(t.Context(), rendermap.App{}, files); err != nil {
		t.Fatal(err)
	}
	return first, second
}

func TestUnpackedDependenciesSkipTheBuild(t *testing.T) {
	requireHelm(t)
	checks := func() float64 {
		return metricstest.Value("backflow_deps_equivalence_checks_total", map[string]string{"result": "identical"})
	}
	before := checks()
	sim := &argoSim{t: t, version: "v3.5.3"}
	files := mergeFiles(
		depTestChart("apps/app", "app", depLine("kv", "file://../kv")),
		depTestChart("apps/kv", "kv"),
	)
	first, second := renderTwice(t, files, sim)
	if got := strings.Join(objNames(first), ","); got != "rel-app,rel-kv" {
		t.Errorf("render = %s", got)
	}
	if !sameManifests(first, second) {
		t.Error("the unpacked render differs from the build")
	}
	// The first render is made both ways (one build); the second only the quick way.
	if sim.builds != 1 || sim.renders != 3 {
		t.Errorf("builds = %d, renders = %d, want 1 build and 3 renders (2 for the check, 1 quick)", sim.builds, sim.renders)
	}
	if got := checks() - before; got != 1 {
		t.Errorf("identical checks = %v, want 1", got)
	}
}

func TestAliasesStayInTheParentChart(t *testing.T) {
	requireHelm(t)
	sim := &argoSim{t: t, version: "v3.5.3"}
	files := mergeFiles(
		depTestChart("apps/app", "app", depLine("kv", "file://../kv", "    alias: cache\n"), depLine("kv", "file://../kv", "    alias: store\n")),
		depTestChart("apps/kv", "kv"),
	)
	plan := planUnpack(files, "apps/app")
	if plan.Extra == nil {
		t.Fatalf("not unpacked: %s", plan.Reason)
	}
	for p := range plan.Extra {
		if strings.Contains(p, "cache") || strings.Contains(p, "store") {
			t.Errorf("an alias became a directory: %s", p)
		}
	}
	if _, ok := plan.Extra["apps/app/charts/kv/Chart.yaml"]; !ok {
		t.Errorf("the chart is not under its own name: %v", slices.Sorted(pathKeys(plan.Extra)))
	}
	first, second := renderTwice(t, files, sim)
	if !sameManifests(first, second) || sim.builds != 1 {
		t.Errorf("builds = %d, same = %v", sim.builds, sameManifests(first, second))
	}
}

func pathKeys(m map[string][]byte) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func TestConditionsAndTagsDoNotChangeTheResult(t *testing.T) {
	requireHelm(t)
	sim := &argoSim{t: t, version: "v3.5.3"}
	files := mergeFiles(
		depTestChart("apps/app", "app",
			depLine("kv", "file://../kv", "    condition: kv.enabled\n"),
			depLine("tagged", "file://../tagged", "    tags:\n      - extras\n")),
		depTestChart("apps/kv", "kv"),
		depTestChart("apps/tagged", "tagged"),
	)
	files["apps/app/values.yaml"] = []byte("kv:\n  enabled: false\ntags:\n  extras: true\n")
	first, second := renderTwice(t, files, sim)
	if got := strings.Join(objNames(first), ","); got != "rel-app,rel-tagged" {
		t.Errorf("render = %s: the disabled dependency must stay out", got)
	}
	if !sameManifests(first, second) || sim.builds != 1 {
		t.Errorf("builds = %d, same = %v", sim.builds, sameManifests(first, second))
	}
}

func TestNestedFileDependencies(t *testing.T) {
	requireHelm(t)
	checks := func(result string) float64 {
		return metricstest.Value("backflow_deps_equivalence_checks_total", map[string]string{"result": result})
	}

	t.Run("the plan reaches the dependencies of dependencies", func(t *testing.T) {
		files := mergeFiles(
			depTestChart("apps/app", "app", depLine("mid", "file://../mid")),
			depTestChart("apps/mid", "mid", depLine("leaf", "file://../leaf")),
			depTestChart("apps/leaf", "leaf"),
		)
		plan := planUnpack(files, "apps/app")
		for _, want := range []string{"apps/app/charts/mid/Chart.yaml", "apps/app/charts/mid/charts/leaf/Chart.yaml"} {
			if _, ok := plan.Extra[want]; !ok {
				t.Errorf("%s is not in the plan: %v", want, slices.Sorted(pathKeys(plan.Extra)))
			}
		}
	})

	// Argo CD's `helm dependency build` packages mid without the leaf it depends on (Helm
	// does not build the dependencies of a dependency), so the build renders no leaf. The
	// unpacked set would render it: that is a different result, and it is not used.
	t.Run("what Argo CD does not resolve is not resolved either", func(t *testing.T) {
		before := checks("different")
		sim := &argoSim{t: t, version: "v3.5.3"}
		files := mergeFiles(
			depTestChart("apps/app", "app", depLine("mid", "file://../mid")),
			depTestChart("apps/mid", "mid", depLine("leaf", "file://../leaf")),
			depTestChart("apps/leaf", "leaf"),
		)
		first, second := renderTwice(t, files, sim)
		if got := strings.Join(objNames(first), ","); got != "rel-app,rel-mid" {
			t.Errorf("render = %s, want the build's", got)
		}
		if !sameManifests(first, second) {
			t.Error("the answer changed between renders")
		}
		if got := checks("different") - before; got != 1 {
			t.Errorf("different checks = %v, want 1", got)
		}
	})

	t.Run("a dependency that has its own dependency committed", func(t *testing.T) {
		before := checks("identical")
		sim := &argoSim{t: t, version: "v3.5.3"}
		files := mergeFiles(
			depTestChart("apps/app", "app", depLine("mid", "file://../mid")),
			depTestChart("apps/mid", "mid", depLine("leaf", "file://../leaf")),
			depTestChart("apps/mid/charts/leaf", "leaf"),
			depTestChart("apps/leaf", "leaf"),
		)
		first, second := renderTwice(t, files, sim)
		if got := strings.Join(objNames(first), ","); got != "rel-app,rel-leaf,rel-mid" {
			t.Errorf("render = %s", got)
		}
		if !sameManifests(first, second) || sim.builds != 1 {
			t.Errorf("builds = %d, same = %v", sim.builds, sameManifests(first, second))
		}
		if got := checks("identical") - before; got != 1 {
			t.Errorf("identical checks = %v, want 1", got)
		}
	})
}

func TestADependencyOutsideTheChartDirectory(t *testing.T) {
	requireHelm(t)
	sim := &argoSim{t: t, version: "v3.5.3"}
	// file://../common is a sibling of the chart, not below it.
	files := mergeFiles(
		depTestChart("apps/app", "app", depLine("common", "file://../common")),
		depTestChart("apps/common", "common"),
	)
	first, second := renderTwice(t, files, sim)
	if !sameManifests(first, second) || sim.builds != 1 {
		t.Errorf("builds = %d, same = %v", sim.builds, sameManifests(first, second))
	}
	// And one that leaves the repository is never unpacked.
	files = depTestChart("apps/app", "app", depLine("x", "file://../../../x"))
	if plan := planUnpack(files, "apps/app"); plan.Extra != nil || !strings.Contains(plan.Reason, "outside the repository") {
		t.Errorf("plan = %+v", plan)
	}
}

func TestNeverASecondCopyOfWhatIsInCharts(t *testing.T) {
	kv := depTestChart("apps/kv", "kv")
	other := depTestChart("apps/other", "other")
	app := depTestChart("apps/app", "app", depLine("kv", "file://../kv"), depLine("other", "file://../other"))

	t.Run("a committed tgz", func(t *testing.T) {
		files := mergeFiles(app, kv, other, map[string][]byte{"apps/app/charts/kv-0.1.0.tgz": []byte("packaged")})
		plan := planUnpack(files, "apps/app")
		if plan.Extra == nil {
			t.Fatal(plan.Reason)
		}
		for p := range plan.Extra {
			if strings.HasPrefix(p, "apps/app/charts/kv/") {
				t.Errorf("a second copy of kv: %s", p)
			}
		}
		if _, ok := plan.Extra["apps/app/charts/other/Chart.yaml"]; !ok {
			t.Error("the dependency that is not in charts/ is not unpacked")
		}
	})
	t.Run("a committed directory under another name", func(t *testing.T) {
		files := mergeFiles(app, kv, other, depTestChart("apps/app/charts/vendored-kv", "kv"))
		plan := planUnpack(files, "apps/app")
		for p := range plan.Extra {
			if strings.Contains(p, "charts/kv/") {
				t.Errorf("a second copy of kv: %s", p)
			}
		}
	})
	t.Run("a tgz of a chart with a longer name is not kv", func(t *testing.T) {
		files := mergeFiles(app, kv, other, map[string][]byte{"apps/app/charts/kv-extra-0.1.0.tgz": []byte("packaged")})
		if _, ok := planUnpack(files, "apps/app").Extra["apps/app/charts/kv/Chart.yaml"]; !ok {
			t.Error("kv-extra is not kv, but kv was skipped")
		}
	})
	t.Run("everything is there already", func(t *testing.T) {
		files := mergeFiles(app, kv, other,
			map[string][]byte{"apps/app/charts/kv-0.1.0.tgz": []byte("a"), "apps/app/charts/other-0.1.0.tgz": []byte("b")})
		if plan := planUnpack(files, "apps/app"); plan.Extra != nil || plan.Reason != "" || !plan.HasFileDeps {
			t.Errorf("plan = %+v: nothing to add, and no reason to fall back", plan)
		}
	})
}

func TestRemoteDependenciesAreLeftAlone(t *testing.T) {
	remote := depLine("redis", "https://charts.example.com")
	app := depTestChart("apps/app", "app", depLine("kv", "file://../kv"), remote)
	files := mergeFiles(app, depTestChart("apps/kv", "kv"))

	plan := planUnpack(files, "apps/app")
	if plan.Extra != nil || !strings.Contains(plan.Reason, "redis") {
		t.Errorf("a remote dependency that is not in charts/ gets built anyway; nothing may be added: %+v", plan)
	}

	files["apps/app/charts/redis-1.0.0.tgz"] = []byte("packaged")
	plan = planUnpack(files, "apps/app")
	if _, ok := plan.Extra["apps/app/charts/kv/Chart.yaml"]; !ok || plan.Reason != "" {
		t.Errorf("with the remote one in charts/, the file:// one is unpacked: %+v", plan)
	}
	for p := range plan.Extra {
		if strings.Contains(p, "redis") {
			t.Errorf("a remote dependency was touched: %s", p)
		}
	}
}

func TestAChartWithoutFileDependenciesIsRenderedAsItIs(t *testing.T) {
	files := mergeFiles(depTestChart("apps/app", "app"))
	if plan := planUnpack(files, "apps/app"); plan.HasFileDeps || plan.Extra != nil {
		t.Errorf("plan = %+v", plan)
	}
}

func TestADependencyThatIsAnotherChartIsNotUnpacked(t *testing.T) {
	files := mergeFiles(depTestChart("apps/app", "app", depLine("kv", "file://../kv")), depTestChart("apps/kv", "something-else"))
	if plan := planUnpack(files, "apps/app"); plan.Extra != nil || !strings.Contains(plan.Reason, "something-else") {
		t.Errorf("plan = %+v", plan)
	}
}

// liar renders from the files it gets, and gives another answer when the dependency is
// unpacked, or fails, as the test chooses.
type liar struct {
	mu       sync.Mutex
	calls    int
	unpacked int
	version  string
	mode     string // "differs", "fails", "buildFails"
}

func (l *liar) ArgoVersion(context.Context) (string, error) { return l.version, nil }

func (l *liar) Render(_ context.Context, _ rendermap.App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	_, unpacked := files["apps/app/charts/kv/Chart.yaml"]
	if unpacked {
		l.unpacked++
	}
	cm := func(value string) []*unstructured.Unstructured {
		return []*unstructured.Unstructured{{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "cm"}, "data": map[string]any{"v": value},
		}}}
	}
	switch {
	case l.mode == "buildFails" && !unpacked:
		return nil, errors.New("the build failed")
	case unpacked && l.mode == "differs":
		return cm("from the unpacked chart"), nil
	case unpacked && l.mode == "fails":
		return nil, errors.New("helm template failed")
	}
	return cm("from the build"), nil
}

func fakeChart() map[string][]byte {
	return mergeFiles(depTestChart("apps/app", "app", depLine("kv", "file://../kv")), depTestChart("apps/kv", "kv"))
}

func TestFallsBackWhenTheUnpackedRenderDiffers(t *testing.T) {
	for _, mode := range []string{"differs", "fails"} {
		t.Run(mode, func(t *testing.T) {
			metric := "different"
			if mode == "fails" {
				metric = "error"
			}
			before := metricstest.Value("backflow_deps_equivalence_checks_total", map[string]string{"result": metric})
			l := &liar{version: "v3.5.3", mode: mode}
			r := NewUnpackedDeps().Wrap(l, &Source{ChartDir: "apps/app"})
			files := fakeChart()
			for i := range 3 {
				objs, err := r.Render(t.Context(), rendermap.App{}, files)
				if err != nil {
					t.Fatal(err)
				}
				if v, _, _ := unstructured.NestedString(objs[0].Object, "data", "v"); v != "from the build" {
					t.Errorf("render %d answered %q: the result must be the build's", i, v)
				}
			}
			// Checked once (one unpacked render), then never unpacked again.
			if l.unpacked != 1 || l.calls != 4 {
				t.Errorf("unpacked renders = %d, calls = %d, want 1 and 4", l.unpacked, l.calls)
			}
			if got := metricstest.Value("backflow_deps_equivalence_checks_total", map[string]string{"result": metric}) - before; got != 1 {
				t.Errorf("%s checks = %v, want 1", metric, got)
			}
		})
	}
}

func TestABuildThatFailsIsNotCachedAndIsTheAnswer(t *testing.T) {
	l := &liar{version: "v3.5.3", mode: "buildFails"}
	r := NewUnpackedDeps().Wrap(l, &Source{ChartDir: "apps/app"})
	for range 2 {
		if _, err := r.Render(t.Context(), rendermap.App{}, fakeChart()); err == nil || !strings.Contains(err.Error(), "the build failed") {
			t.Fatalf("err = %v, want the build's error", err)
		}
	}
	if l.unpacked != 0 {
		t.Errorf("the unpacked render ran %d times although there was nothing to compare with", l.unpacked)
	}
}

func TestTheDecisionDoesNotDependOnValueFilesButOnTheChartAndArgoCD(t *testing.T) {
	l := &liar{version: "v3.5.3", mode: "none"}
	src := &Source{ChartDir: "apps/app", ValueFiles: []string{"apps/app/values-dev.yaml"}}
	r := NewUnpackedDeps().Wrap(l, src)
	files := fakeChart()
	files["apps/app/values-dev.yaml"] = []byte("greeting: one\n")
	render := func() {
		t.Helper()
		if _, err := r.Render(t.Context(), rendermap.App{}, files); err != nil {
			t.Fatal(err)
		}
	}
	render() // checked: 2 calls
	files["apps/app/values-dev.yaml"] = []byte("greeting: two\n")
	files["apps/app/values.yaml"] = []byte("greeting: three\n")
	files["apps/kv/values.yaml"] = []byte("greeting: four\n")
	render() // the same chart, other values: no second check
	if l.calls != 3 {
		t.Errorf("calls = %d after edits of values, want 3 (a check of two, then one)", l.calls)
	}
	files["apps/kv/templates/cm.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: changed\n")
	render() // a template of the dependency changed: a new chart, checked again
	if l.calls != 5 {
		t.Errorf("calls = %d after a template changed, want 5", l.calls)
	}
	// Another Argo CD: the chart is checked again (its decision was made with the other).
	l.version = "v3.6.0"
	render()
	if l.calls != 7 {
		t.Errorf("calls = %d after Argo CD changed, want 7", l.calls)
	}
}

type versionless struct{ rendermap.Renderer }

func TestNothingChangesWithoutWhatItNeeds(t *testing.T) {
	l := &liar{version: "v3.5.3"}
	var none *UnpackedDeps
	if got := none.Wrap(l, &Source{ChartDir: "apps/app"}); got != rendermap.Renderer(l) {
		t.Error("a nil set of decisions wraps the renderer")
	}
	if got := NewUnpackedDeps().Wrap(versionless{l}, &Source{ChartDir: "apps/app"}); got == nil {
		t.Fatal("nil")
	} else if _, wrapped := got.(*unpackedRenderer); wrapped {
		t.Error("a renderer that cannot tell the version of Argo CD is wrapped")
	}
	ext := &Source{External: &ExternalChart{Name: "x"}}
	if got := NewUnpackedDeps().Wrap(l, ext); got != rendermap.Renderer(l) {
		t.Error("a chart that is not in the repository is wrapped")
	}
}

// BenchmarkUploadSets renders the same chart with its file:// dependency built by the
// Argo CD of the test (helm dependency build) and with it uploaded unpacked.
func BenchmarkUploadSets(b *testing.B) {
	requireHelm(b)
	files := fakeChart()
	plan := planUnpack(files, "apps/app")
	unpacked := mergeFiles(files, plan.Extra)
	for _, bc := range []struct {
		name  string
		files map[string][]byte
	}{{"built", files}, {"unpacked", unpacked}} {
		b.Run(bc.name, func(b *testing.B) {
			sim := &argoSim{t: b, version: "v3.5.3"}
			for b.Loop() {
				if _, err := sim.Render(context.Background(), rendermap.App{}, bc.files); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(sim.builds)/float64(max(sim.renders, 1)), "builds/render")
		})
	}
}
