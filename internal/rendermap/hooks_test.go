package rendermap

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// podinfoRenderer renders what podinfo's chart does: a ConfigMap fed by the
// files, and chart test Pods that are hooks (helm.sh/hook: test) and are named
// at random on every render. With cached set it answers the same files from a
// cache, as Argo CD does for a revision it rendered before; RenderFresh bypasses
// it.
type podinfoRenderer struct {
	mu     sync.Mutex
	n      int
	cached bool
	cache  map[string][]*unstructured.Unstructured
	// randomPlain adds a resource that is not a hook and has a random name.
	randomPlain bool
}

func (p *podinfoRenderer) build(files map[string][]byte) []*unstructured.Unstructured {
	p.n++
	data := map[string]any{}
	for _, name := range []string{"a.txt", "b.txt"} {
		for l := range strings.SplitSeq(string(files[name]), "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				data[k] = v
			}
		}
	}
	objs := []*unstructured.Unstructured{{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "cm", "namespace": "ns"}, "data": data,
	}}}
	for _, kind := range []string{"grpc", "jwt", "tcp"} {
		objs = append(objs, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{
				"name": fmt.Sprintf("podinfo-%s-test-%05d", kind, p.n), "namespace": "ns",
				"annotations": map[string]any{"helm.sh/hook": "test"},
			},
			"spec": map[string]any{"restartPolicy": "Never"},
		}})
	}
	if p.randomPlain {
		objs = append(objs, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": fmt.Sprintf("token-%05d", p.n), "namespace": "ns"},
			"type": "Opaque",
		}})
	}
	return objs
}

func (p *podinfoRenderer) Render(_ context.Context, _ App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached {
		key := filesKey(files)
		if objs, ok := p.cache[key]; ok {
			return objs, nil
		}
		objs := p.build(files)
		if p.cache == nil {
			p.cache = map[string][]*unstructured.Unstructured{}
		}
		p.cache[key] = objs
		return objs, nil
	}
	return p.build(files), nil
}

// freshPodinfo is a podinfoRenderer that can bypass its cache.
type freshPodinfo struct{ *podinfoRenderer }

func (f freshPodinfo) RenderFresh(_ context.Context, _ App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.build(files), nil
}

func TestHookResourcesAreNotCompared(t *testing.T) {
	files := map[string][]byte{"a.txt": []byte("size=1\nname=a"), "b.txt": []byte("other=1")}
	found := &Found{Candidates: []Candidate{{File: "a.txt", Path: KeyPath{"size"}}}}
	// Answered from a cache, as Argo CD does, the base renders show nothing random:
	// only leaving the hooks out keeps their names from counting as changes.
	out, err := Verify(context.Background(), &podinfoRenderer{cached: true}, lineInput(files, found), Options{})
	if err != nil || !out.Mapped() {
		t.Fatalf("out = %+v (%+v), err = %v: the test Pods of the chart are hooks and do not count as changes", out, out.Unmapped, err)
	}
	if len(out.Results) != 1 || string(out.Results[0].Edited) != "size=2\nname=a" {
		t.Errorf("results = %+v", out.Results)
	}
}

func TestIsHook(t *testing.T) {
	for name, tc := range map[string]struct {
		ann  map[string]string
		want bool
	}{
		"none":             {nil, false},
		"helm test":        {map[string]string{"helm.sh/hook": "test"}, true},
		"helm pre-install": {map[string]string{"helm.sh/hook": "pre-install,pre-upgrade"}, true},
		"argo presync":     {map[string]string{"argocd.argoproj.io/hook": "PreSync"}, true},
		"argo skip":        {map[string]string{"argocd.argoproj.io/hook": "Skip"}, true},
		"hook weight only": {map[string]string{"helm.sh/hook-weight": "5"}, false},
		"other":            {map[string]string{"app": "x"}, false},
	} {
		u := &unstructured.Unstructured{Object: map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "p"}}}
		u.SetAnnotations(tc.ann)
		if got := IsHook(u); got != tc.want {
			t.Errorf("%s: IsHook = %v, want %v", name, got, tc.want)
		}
	}
}

func TestResourceWithARandomNameIsIgnored(t *testing.T) {
	files := map[string][]byte{"a.txt": []byte("size=1"), "b.txt": []byte("other=1")}
	found := &Found{Candidates: []Candidate{{File: "a.txt", Path: KeyPath{"size"}}}}
	in := lineInput(files, found)
	t.Run("two renders that differ show the random name", func(t *testing.T) {
		out, err := Verify(context.Background(), &podinfoRenderer{randomPlain: true}, in, Options{})
		if err != nil || !out.Mapped() {
			t.Fatalf("out = %+v (%+v), err = %v", out, out.Unmapped, err)
		}
	})
	t.Run("a renderer that answers the same files from a cache hides it", func(t *testing.T) {
		out, err := Verify(context.Background(), &podinfoRenderer{randomPlain: true, cached: true}, in, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Mapped() || out.Unmapped.Reason != ReasonValueAffectsOtherFields {
			t.Fatalf("out = %+v: with the base answered twice from a cache, the Secret of the edited render looks added", out)
		}
	})
	t.Run("RenderFresh makes the two base renders real", func(t *testing.T) {
		r := freshPodinfo{&podinfoRenderer{randomPlain: true, cached: true}}
		out, err := Verify(context.Background(), r, in, Options{})
		if err != nil || !out.Mapped() {
			t.Fatalf("out = %+v (%+v), err = %v", out, out.Unmapped, err)
		}
	})
}

func TestBaseRendersUseRenderFreshWhenThereIsOne(t *testing.T) {
	files := map[string][]byte{"a.txt": []byte("size=1"), "b.txt": []byte("other=1")}
	found := &Found{Candidates: []Candidate{{File: "a.txt", Path: KeyPath{"size"}}}}
	rec := &recordingFresh{podinfoRenderer: &podinfoRenderer{}}
	if _, err := Verify(context.Background(), rec, lineInput(files, found), Options{}); err != nil {
		t.Fatal(err)
	}
	if rec.fresh != 2 || rec.plain != 1 {
		t.Errorf("fresh renders = %d, plain renders = %d, want 2 and 1 (the edit)", rec.fresh, rec.plain)
	}
}

type recordingFresh struct {
	*podinfoRenderer
	fresh, plain int
}

func (r *recordingFresh) Render(ctx context.Context, app App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	r.plain++
	return r.podinfoRenderer.Render(ctx, app, files)
}

func (r *recordingFresh) RenderFresh(ctx context.Context, app App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	r.fresh++
	return r.podinfoRenderer.Render(ctx, app, files)
}
