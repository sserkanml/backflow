//go:build coverage

package coverage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/drift"
	"github.com/sserkanml/backflow/internal/kustomize"
	"github.com/sserkanml/backflow/internal/kustomize/kbuild"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// The source types of the cases.
const (
	sourceHelm      = "Helm"
	sourceKustomize = "Kustomize"
)

// kustomizeApp is one Kustomize Application under test: an overlay of the
// repository in testdata/kustomize, with the options of its spec.source.kustomize.
type kustomizeApp struct {
	name    string
	path    string
	options kustomize.Options
}

var kustomizeApps = []kustomizeApp{
	{name: "kustomize-dev", path: "overlays/dev"},
	{name: "kustomize-prod", path: "overlays/prod"},
	// The Application sets the image itself, on top of Git.
	{name: "kustomize-dev-images", path: "overlays/dev", options: kustomize.Options{Images: []string{"nginx=nginx:1.99"}}},
}

// kustomizeScenarios are the scenarios of the Helm cases that fit a plain
// Deployment, Service and ConfigMap, under the same names.
var kustomizeScenarios = []string{
	"image-tag", "scale", "env-change", "env-remove", "pod-annotation", "resources",
	"configmap-data", "toleration-remove", "pull-policy", "service-port",
}

// kustomizeModes are what a BackflowPolicy for Kustomize can allow: the
// defaults (spec.kustomize.addPatches on, editBases off), editBases on, and
// addPatches off.
func kustomizeModes() []policySet {
	return []policySet{
		{name: "default", pol: kustomize.Policy{AddPatches: true}},
		{name: "editBases", pol: kustomize.Policy{AddPatches: true, EditBases: true}},
		{name: "no-addPatches", pol: kustomize.Policy{}},
	}
}

// kustomizeBin is the kustomize binary (v5.8.1, the version of Argo CD) that
// `make kustomize` installs.
func kustomizeBin() string {
	if b := os.Getenv("KUSTOMIZE_BIN"); b != "" {
		return b
	}
	return filepath.Join("..", "..", "bin", "kustomize")
}

type kustomizeLoaded struct {
	app   kustomizeApp
	src   *kustomize.Source
	files map[string][]byte
	rend  *kbuild.Renderer
	loc   *kustomize.Located
	ra    rendermap.App
}

func loadKustomize(ctx context.Context, a kustomizeApp, slots chan struct{}) (*kustomizeLoaded, error) {
	fsys := os.DirFS(filepath.Join("testdata", "kustomize"))
	src, err := kustomize.NewSource(fsys, a.path, a.options)
	if err != nil {
		return nil, fmt.Errorf("%s: source: %w", a.name, err)
	}
	files, err := kustomize.CollectFiles(fsys, src)
	if err != nil {
		return nil, fmt.Errorf("%s: files: %w", a.name, err)
	}
	l := &kustomizeLoaded{app: a, src: src, files: files,
		rend: &kbuild.Renderer{Bin: kustomizeBin(), Dir: a.path, Options: a.options, Slots: slots},
		ra:   rendermap.App{Name: a.name, Namespace: "argocd", Project: "default", DestNamespace: "demo"}}
	if l.loc, err = kustomize.Locate(ctx, l.rend, src, files, l.ra); err != nil {
		return nil, fmt.Errorf("%s: locate: %w", a.name, err)
	}
	return l, nil
}

func (l *kustomizeLoaded) runAll(ctx context.Context) ([]caseResult, error) {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		out  []caseResult
	)
	for _, key := range sortedKeysOf(l.loc.Objects) {
		obj := l.loc.Objects[key]
		for _, id := range kustomizeScenarios {
			sc := scenarioByID(id)
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
			res := caseResult{Source: sourceKustomize, Chart: l.app.name, Resource: obj.GetKind() + " " + obj.GetName(),
				Scenario: sc.id, Changes: changes, ByPolicy: map[string]outcome{}}
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, m := range kustomizeModes() {
					o, err := l.mapChanges(ctx, obj, changes, m)
					mu.Lock()
					if err != nil {
						errs = append(errs, fmt.Errorf("%s %s %s: %w", res.Chart, res.Resource, res.Scenario, err))
					} else {
						res.ByPolicy[m.name] = o
					}
					mu.Unlock()
				}
				mu.Lock()
				out = append(out, res)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	return out, errors.Join(errs...)
}

func sortedKeysOf(m map[rendermap.ResourceKey]*unstructured.Unstructured) []rendermap.ResourceKey {
	keys := make([]rendermap.ResourceKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j].Kind+keys[j].Name < keys[j-1].Kind+keys[j-1].Name; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// mapChanges is the production path of mapping a Kustomize proposal.
func (l *kustomizeLoaded) mapChanges(ctx context.Context, obj *unstructured.Unstructured, changes []v1alpha1.FieldChange, m policySet) (outcome, error) {
	gv := obj.GroupVersionKind()
	pol := m.pol
	pol.Application = l.app.name
	out, err := kustomize.Verify(ctx, l.rend, kustomize.Input{
		App:      l.ra,
		Resource: rendermap.ResourceKey{Group: gv.Group, Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName()},
		Changes:  changes, Source: l.src, Files: l.files, Located: l.loc, Policy: pol,
	}, rendermap.Options{})
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
	if t := kustomizeTiers(out); t != "" {
		o.Keys += " [" + t + "]"
	}
	return o, nil
}

// kustomizeTiers names the tiers of the edits of an outcome, e.g. "setting".
func kustomizeTiers(out *rendermap.Outcome) string {
	names := map[int]string{kustomize.TierSetting: "setting", kustomize.TierPatch: "patch", kustomize.TierResource: "resource file",
		kustomize.TierBase: "base", kustomize.TierNewPatch: "new patch"}
	var parts []string
	for _, t := range kustomize.Describe(out).Tiers {
		parts = append(parts, names[t])
	}
	return strings.Join(parts, ", ")
}
