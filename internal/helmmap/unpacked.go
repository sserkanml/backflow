package helmmap

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/sserkanml/backflow/internal/metrics"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// Argo CD runs `helm template` on the uploaded chart first, and only when Helm
// answers that a dependency of Chart.yaml (or requirements.yaml) is "missing in
// charts/" does it run `helm dependency build` (reposerver/repository, runHelmBuild;
// the marker file that makes it run once per checkout does not outlive the
// temporary directory of an upload). A chart whose file:// dependencies are already
// in charts/ never gets there. Uploading them unpacked under charts/<name>/ is
// therefore enough to skip the build, which takes most of the time of a render.
//
// The unpacked set is only used where it was shown to render the same as Argo CD's
// build: see UnpackedDeps.

// depEntry is a dependency of Chart.yaml or requirements.yaml.
type depEntry struct {
	Name       string `yaml:"name"`
	Repository string `yaml:"repository"`
}

// readDeps returns the dependencies of the chart in dir: Chart.yaml (apiVersion v2)
// and requirements.yaml (v1), in the order they are written.
func readDeps(files map[string][]byte, dir string) ([]depEntry, error) {
	var out []depEntry
	for _, name := range []string{chartYAMLName, requirementsName} {
		data, ok := files[path.Join(dir, name)]
		if !ok {
			continue
		}
		var doc struct {
			Dependencies []depEntry `yaml:"dependencies"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", path.Join(dir, name), err)
		}
		out = append(out, doc.Dependencies...)
	}
	return out, nil
}

// chartName is the name in the Chart.yaml of dir.
func chartName(files map[string][]byte, dir string) string {
	var doc struct {
		Name string `yaml:"name"`
	}
	if data, ok := files[path.Join(dir, chartYAMLName)]; ok {
		_ = yaml.Unmarshal(data, &doc)
	}
	return doc.Name
}

// tgzOf matches the file `helm dependency build` writes for a chart: name-version.tgz.
func tgzOf(name string) *regexp.Regexp {
	return regexp.MustCompile("^" + regexp.QuoteMeta(name) + `-v?[0-9][^/]*\.tgz$`)
}

// inCharts reports whether the chart called name is already in the charts/ directory
// of dir: as a directory (whatever it is called, its Chart.yaml says), or as a
// packaged chart.
func inCharts(files map[string][]byte, dir, name string) bool {
	prefix := path.Join(dir, "charts") + "/"
	tgz := tgzOf(name)
	seen := map[string]bool{}
	for p := range files {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		first, below, isDir := strings.Cut(rest, "/")
		if !isDir {
			if tgz.MatchString(first) {
				return true
			}
			continue
		}
		_ = below
		if seen[first] {
			continue
		}
		seen[first] = true
		if chartName(files, path.Join(dir, "charts", first)) == name {
			return true
		}
	}
	return false
}

// unpackPlan is what to add to an upload so that Argo CD does not build.
type unpackPlan struct {
	// Extra are the files to add, by repository path.
	Extra map[string][]byte
	// Structural are the paths of the files whose content decides whether the
	// unpacked set renders like the build (the chart and its dependencies).
	Structural []string
	// HasFileDeps: some chart of the tree has a file:// dependency.
	HasFileDeps bool
	// Reason says why Extra is empty although there are file:// dependencies.
	Reason string
}

type unpacker struct {
	files map[string][]byte
	plan  *unpackPlan
	done  map[string]bool
}

const maxUnpackDepth = 8

// planUnpack works out which files to add for the chart in chartDir. Either every
// dependency of every chart is provided (in charts/ already, or unpacked by the plan)
// and Argo CD's `helm template` succeeds without a build, or nothing is added: a
// chart with a remote dependency that is not in charts/ gets built anyway, and a
// half-unpacked charts/ directory would only get in the build's way.
func planUnpack(files map[string][]byte, chartDir string) *unpackPlan {
	u := &unpacker{files: files, plan: &unpackPlan{Extra: map[string][]byte{}}, done: map[string]bool{}}
	for p := range files {
		if chartDir == "." || strings.HasPrefix(p, chartDir+"/") {
			u.plan.Structural = append(u.plan.Structural, p)
		}
	}
	if err := u.chart(chartDir, chartDir, 0); err != nil {
		u.plan.Extra = nil
		u.plan.Reason = err.Error()
	} else if len(u.plan.Extra) == 0 {
		u.plan.Extra = nil
	}
	return u.plan
}

// chart provides the dependencies of the chart whose files are in srcDir, to be
// placed in the charts/ directory of dstDir (the same directory for the chart itself,
// a copy under charts/ for a dependency).
func (u *unpacker) chart(srcDir, dstDir string, depth int) error {
	if depth > maxUnpackDepth {
		return fmt.Errorf("file:// dependencies nested deeper than %d", maxUnpackDepth)
	}
	deps, err := readDeps(u.files, srcDir)
	if err != nil {
		return err
	}
	for _, d := range deps {
		if d.Name == "" {
			return fmt.Errorf("a dependency of %s has no name", srcDir)
		}
		rel, isFile := strings.CutPrefix(d.Repository, "file://")
		if !isFile {
			if !inCharts(u.files, srcDir, d.Name) {
				return fmt.Errorf("the dependency %s of %s is not a file:// one and is not in charts/", d.Name, srcDir)
			}
			continue
		}
		u.plan.HasFileDeps = true
		if inCharts(u.files, srcDir, d.Name) {
			continue // already there: never a second copy
		}
		target := path.Join(srcDir, rel)
		if path.IsAbs(rel) || target == ".." || strings.HasPrefix(target, "../") {
			return fmt.Errorf("the dependency %s of %s is outside the repository", d.Name, srcDir)
		}
		if got := chartName(u.files, target); got != d.Name {
			return fmt.Errorf("the dependency %s of %s is the chart %q in %s", d.Name, srcDir, got, target)
		}
		dst := path.Join(dstDir, "charts", d.Name)
		if u.done[dst] {
			continue // the same chart under another alias
		}
		u.done[dst] = true
		for p, data := range u.files {
			if rest, ok := strings.CutPrefix(p, target+"/"); ok {
				u.plan.Extra[path.Join(dst, rest)] = data
				u.plan.Structural = append(u.plan.Structural, p)
			}
		}
		if err := u.chart(target, dst, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// structuralKey identifies the chart for the decision: Argo CD's version, and the
// content of the chart and of its file:// dependencies, but not of the value files
// (a proposal edits those, and they cannot change how the dependencies are provided).
func structuralKey(version string, files map[string][]byte, plan *unpackPlan, valueFiles []string) string {
	skip := map[string]bool{}
	for _, f := range valueFiles {
		skip[f] = true
	}
	paths := slices.Clone(plan.Structural)
	slices.Sort(paths)
	paths = slices.Compact(paths)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d:%s", len(version), version)
	for _, p := range paths {
		base := path.Base(p)
		if skip[p] || (strings.HasPrefix(base, "values") && (strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml"))) {
			continue
		}
		_, _ = fmt.Fprintf(h, "|%d:%s|%d:", len(p), p, len(files[p]))
		_, _ = h.Write(files[p])
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// sameManifests reports whether two renders produced the same resources.
func sameManifests(a, b []*unstructured.Unstructured) bool {
	if len(a) != len(b) {
		return false
	}
	order := func(in []*unstructured.Unstructured) []*unstructured.Unstructured {
		out := slices.Clone(in)
		slices.SortStableFunc(out, func(x, y *unstructured.Unstructured) int {
			return strings.Compare(identity(x), identity(y))
		})
		return out
	}
	a, b = order(a), order(b)
	for i := range a {
		if !reflect.DeepEqual(a[i].Object, b[i].Object) {
			return false
		}
	}
	return true
}

func identity(u *unstructured.Unstructured) string {
	return u.GroupVersionKind().String() + "/" + u.GetNamespace() + "/" + u.GetName()
}

// argoVersioner is a renderer that can tell the version of the Argo CD it renders
// with (rendermap.ArgoRenderer does).
type argoVersioner interface {
	ArgoVersion(ctx context.Context) (string, error)
}

// UnpackedDeps decides, for each chart with file:// dependencies, whether its
// renders may carry the dependencies unpacked under charts/. The first render of a
// chart (by the content of the chart and its dependencies, and the version of Argo CD)
// is made both ways and the resources are compared: only when they are identical are
// the following renders made the quick way. The decision is kept in memory. Whenever
// the check is not possible or does not agree, the render is the one Argo CD does by
// itself, which is always what the user gets.
//
// The zero value is not usable; use NewUnpackedDeps. A nil *UnpackedDeps does nothing.
type UnpackedDeps struct {
	mu      sync.Mutex
	decided map[string]bool
	logged  map[string]bool
}

// NewUnpackedDeps returns an empty set of decisions, to share between renders.
func NewUnpackedDeps() *UnpackedDeps {
	return &UnpackedDeps{decided: map[string]bool{}, logged: map[string]bool{}}
}

// Wrap returns a renderer that provides the file:// dependencies of src unpacked when
// that is safe. inner must be a renderer that uploads files and tells the version
// of Argo CD (rendermap.ArgoRenderer); any other, and a chart that is not in the
// repository, is returned as it is.
func (u *UnpackedDeps) Wrap(inner rendermap.Renderer, src *Source) rendermap.Renderer {
	if u == nil || src == nil || src.External != nil {
		return inner
	}
	v, ok := inner.(argoVersioner)
	if !ok {
		return inner
	}
	return &unpackedRenderer{inner: inner, version: v, src: src, deps: u}
}

type unpackedRenderer struct {
	inner   rendermap.Renderer
	version argoVersioner
	src     *Source
	deps    *UnpackedDeps
}

// Render implements rendermap.Renderer.
func (r *unpackedRenderer) Render(ctx context.Context, app rendermap.App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	plan := planUnpack(files, r.src.ChartDir)
	if !plan.HasFileDeps {
		return r.inner.Render(ctx, app, files)
	}
	built := func() ([]*unstructured.Unstructured, error) {
		return r.inner.Render(rendermap.WithDeps(ctx, metrics.DepsBuilt), app, files)
	}
	if plan.Extra == nil {
		r.deps.logOnce(ctx, "plan:"+r.src.ChartDir+":"+plan.Reason, r.src.ChartDir,
			"file:// dependencies are not uploaded unpacked: "+plan.Reason)
		return built()
	}
	version, err := r.version.ArgoVersion(ctx)
	if err != nil {
		return built() // no key to decide by: do what Argo CD does by itself
	}
	key := structuralKey(version, files, plan, slices.Concat(r.src.ValueFiles, r.src.FileParameterFiles))
	unpacked := func() ([]*unstructured.Unstructured, error) {
		merged := maps.Clone(files)
		for p, data := range plan.Extra {
			if _, exists := merged[p]; !exists {
				merged[p] = data
			}
		}
		return r.inner.Render(rendermap.WithDeps(ctx, metrics.DepsUnpacked), app, merged)
	}

	use, known := r.deps.decision(key)
	switch {
	case known && use:
		return unpacked()
	case known:
		return built()
	}

	// The first render of this chart: both ways, and the result is the build's.
	want, err := built()
	if err != nil {
		return nil, err // nothing to compare with; try again at the next render
	}
	got, err := unpacked()
	switch {
	case err != nil:
		metrics.DepsCheck(metrics.DepsCheckErr)
		r.deps.decide(key, false)
		r.deps.logOnce(ctx, key, r.src.ChartDir, "file:// dependencies are not uploaded unpacked: the render with them failed where Argo CD's build worked: "+err.Error())
	case !sameManifests(want, got):
		metrics.DepsCheck(metrics.DepsDifferent)
		r.deps.decide(key, false)
		r.deps.logOnce(ctx, key, r.src.ChartDir, "file:// dependencies are not uploaded unpacked: the resources differ from the ones Argo CD's build gives")
	default:
		metrics.DepsCheck(metrics.DepsIdentical)
		r.deps.decide(key, true)
	}
	return want, nil
}

func (u *UnpackedDeps) decision(key string) (use, known bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	use, known = u.decided[key]
	return use, known
}

func (u *UnpackedDeps) decide(key string, use bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.decided[key] = use
}

// logOnce logs a reason once per key, at V(0): it explains why a chart is slower than it could be.
func (u *UnpackedDeps) logOnce(ctx context.Context, key, chartDir, msg string) {
	u.mu.Lock()
	seen := u.logged[key]
	u.logged[key] = true
	u.mu.Unlock()
	if !seen {
		logf.FromContext(ctx).Info(msg, "chart", chartDir)
	}
}
