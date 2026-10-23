//go:build coverage

package coverage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/drift"
	"github.com/sserkanml/backflow/internal/helmmap"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// Results of one mapping attempt.
const (
	Mapped   = "Mapped"
	Partial  = "Partial"
	Unmapped = "Unmapped"
)

// policySet is a set of the policy options the mapping runs with.
type policySet struct {
	name string
	opts helmmap.Options
}

// The suite maps every case under three policies: the defaults of a
// BackflowPolicy, a policy that allows everything that is optional (partial
// proposals, label and annotation changes beside an edit), and the defaults
// with spec.helm.addOverrides turned off, which is how mapping worked before
// overrides existed.
func policySets() []policySet {
	return []policySet{
		{name: "default", opts: helmmap.Options{AddOverrides: true}},
		{name: "permissive", opts: helmmap.Options{
			AddOverrides:    true,
			AllowPartial:    true,
			AllowCollateral: []string{helmmap.CollateralLabels, helmmap.CollateralAnnotations},
		}},
		{name: "no-overrides", opts: helmmap.Options{}},
	}
}

// outcome is what mapping one case gave.
type outcome struct {
	Result string // Mapped, Partial or Unmapped
	Reason string // for Unmapped: the reason; for Partial: the reasons of the fields left out
	Keys   string // the edited keys, "file: key" joined with ", "
}

func (o outcome) String() string {
	switch o.Result {
	case Mapped:
		return "Mapped " + o.Keys
	case Partial:
		return "Partial " + o.Keys + " | left out: " + o.Reason
	}
	return "Unmapped " + o.Reason
}

// rank orders the results: a lower rank is a regression.
func (o outcome) rank() int {
	switch o.Result {
	case Mapped:
		return 2
	case Partial:
		return 1
	}
	return 0
}

// caseResult is one scenario on one resource of one chart.
type caseResult struct {
	Chart, Resource, Scenario string
	Changes                   []v1alpha1.FieldChange
	ByPolicy                  map[string]outcome
}

// loaded is a chart in its repository, ready to be mapped.
type loaded struct {
	c      chartCase
	files  map[string][]byte // CollectFiles
	src    *helmmap.Source
	rend   *helmRenderer
	app    rendermap.App
	base   []*unstructured.Unstructured
	policy []policySet
}

func load(ctx context.Context, c chartCase, slots chan struct{}) (*loaded, error) {
	repo, err := c.repo()
	if err != nil {
		return nil, err
	}
	spec := helmmap.Spec{Path: c.chartPath(), ValueFiles: c.valueFiles}
	src, err := helmmap.NewSource(repo, spec)
	if err != nil {
		return nil, fmt.Errorf("%s: source: %w", c.name, err)
	}
	files, err := helmmap.CollectFiles(repo, src)
	if err != nil {
		return nil, fmt.Errorf("%s: files: %w", c.name, err)
	}
	rend := &helmRenderer{chartDir: c.chartPath(), valueFiles: src.ValueFiles, release: c.release, namespace: c.namespace, slots: slots}
	app := rendermap.App{Name: c.release, Namespace: "argocd", Project: "default", DestNamespace: c.namespace, ReleaseName: c.release}
	base, err := rend.Render(ctx, app, files)
	if err != nil {
		return nil, fmt.Errorf("%s: render: %w", c.name, err)
	}
	return &loaded{c: c, files: files, src: src, rend: rend, app: app, base: base, policy: policySets()}, nil
}

// runAll applies every scenario to every resource it fits and maps the result.
// The cases run concurrently; the renderer bounds the helm processes.
func (l *loaded) runAll(ctx context.Context) ([]caseResult, error) {
	type job struct {
		obj *unstructured.Unstructured
		sc  scenario
		res caseResult
	}
	var jobs []*job
	for _, obj := range l.base {
		for _, sc := range scenarios {
			if !contains(sc.kinds, obj.GetKind()) {
				continue
			}
			live := obj.DeepCopy()
			if !sc.apply(live.Object) {
				continue
			}
			changes := drift.Diff(obj.Object, live.Object)
			if len(changes) == 0 {
				continue
			}
			jobs = append(jobs, &job{obj: obj, sc: sc, res: caseResult{
				Chart: l.c.name, Resource: obj.GetKind() + " " + strings.TrimPrefix(obj.GetName(), l.c.release+"-"),
				Scenario: sc.id, Changes: changes, ByPolicy: map[string]outcome{},
			}})
		}
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, p := range l.policy {
				o, err := l.mapChanges(ctx, j.obj, j.res.Changes, p.opts)
				mu.Lock()
				if err != nil {
					errs = append(errs, fmt.Errorf("%s %s %s: %w", j.res.Chart, j.res.Resource, j.sc.id, err))
				} else {
					j.res.ByPolicy[p.name] = o
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	out := make([]caseResult, len(jobs))
	for i, j := range jobs {
		out[i] = j.res
	}
	return out, nil
}

// mapChanges is the production path of mapping a Helm proposal: the fields of
// the changes, then Verify (which renders).
func (l *loaded) mapChanges(ctx context.Context, obj *unstructured.Unstructured, changes []v1alpha1.FieldChange,
	opts helmmap.Options) (outcome, error) {
	fields, err := helmmap.FieldsFor(l.src, l.files, changes)
	if err != nil {
		return outcome{Result: Unmapped, Reason: helmmap.ReasonInvalidInlineValues}, nil //nolint:nilerr // a verdict
	}
	gv := obj.GroupVersionKind()
	out, err := helmmap.Verify(ctx, l.rend, helmmap.Input{
		App:      l.app,
		Resource: helmmap.ResourceKey{Group: gv.Group, Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName()},
		Fields:   fields, Provider: l.src, Writer: helmmap.ValuesWriter{}, Files: l.files,
	}, opts)
	if err != nil {
		return outcome{}, err
	}
	if !out.Mapped() {
		return outcome{Result: Unmapped, Reason: out.Unmapped.Reason}, nil
	}
	o := outcome{Result: Mapped, Keys: editedKeys(out)}
	if len(out.LeftOut) > 0 {
		o.Result = Partial
		reasons := map[string]bool{}
		for _, f := range out.LeftOut {
			reasons[f.Reason] = true
		}
		o.Reason = joinSorted(reasons)
	}
	return o, nil
}

func editedKeys(out *helmmap.Outcome) string {
	var keys []string
	for _, r := range out.Results {
		for _, e := range r.Edits {
			keys = append(keys, e.File+": "+rendermap.KeyPathFromPointer(e.Location).String())
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func joinSorted(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
