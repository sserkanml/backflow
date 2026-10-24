package rendermap

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/mapping"
)

// lineSource is a source kind that is not Helm: every file is a list of
// "key=value" lines, and the render is a ConfigMap whose data is the lines of
// all files (the last file wins). It shows what a source has to supply.
type lineSource struct{}

func (lineSource) Layers(map[string][]byte) ([]Layer, error) { return nil, nil }
func (lineSource) OverrideFile() (string, string)            { return "", "no override file" }
func (lineSource) Controlled(KeyPath) bool                   { return false }
func (lineSource) References(map[string][]byte) References   { return everything{} }

type everything struct{}

func (everything) Mentions(KeyPath) bool { return true }

type lineWriter struct{}

func (lineWriter) Write(c Candidate, content []byte, value string) (*Written, error) {
	want := c.Path[0] + "="
	lines := strings.Split(string(content), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, want) {
			lines[i] = want + value
			edited := []byte(strings.Join(lines, "\n"))
			return &Written{Result: mapping.Result{File: c.File, Edited: edited,
				Edits: []mapping.Edit{{File: c.File, Location: c.Path.Pointer(), Value: fmt.Sprintf("%q", value)}}}}, nil
		}
	}
	return nil, fmt.Errorf("%s has no %s", c.File, c.Path[0])
}

type lineRenderer struct{}

func (lineRenderer) Render(_ context.Context, app App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	data := map[string]any{}
	for _, name := range []string{"a.txt", "b.txt"} {
		for l := range bytes.SplitSeq(files[name], []byte("\n")) {
			if k, v, ok := strings.Cut(string(l), "="); ok {
				data[k] = v
			}
		}
	}
	return []*unstructured.Unstructured{{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "cm", "namespace": app.DestNamespace}, "data": data,
	}}}, nil
}

type noted struct{}

func (noted) Explain(*Found, bool) []string { return []string{"the value comes from somewhere else"} }
func (noted) ExplainEmpty(*Found) []string  { return nil }

func lineInput(files map[string][]byte, found *Found) Input {
	return Input{
		App:      App{Name: "x", DestNamespace: "ns"},
		Resource: ResourceKey{Kind: "ConfigMap", Namespace: "ns", Name: "cm"},
		Fields: []Field{{Change: v1alpha1.FieldChange{Path: "/data/size", Op: v1alpha1.OpReplace, Desired: `"1"`, Live: `"2"`},
			Found: found}},
		Provider: lineSource{}, Writer: lineWriter{}, Files: files,
	}
}

func TestVerifyWithAnotherSource(t *testing.T) {
	cand := func(file string) Candidate { return Candidate{File: file, Path: KeyPath{"size"}} }
	t.Run("one key that reproduces the drift is mapped", func(t *testing.T) {
		files := map[string][]byte{"a.txt": []byte("size=1\nname=a"), "b.txt": []byte("other=1")}
		out, err := Verify(context.Background(), lineRenderer{}, lineInput(files, &Found{Candidates: []Candidate{cand("a.txt")}}), Options{})
		if err != nil || !out.Mapped() {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
		if len(out.Results) != 1 || out.Results[0].File != "a.txt" || string(out.Results[0].Edited) != "size=2\nname=a" {
			t.Errorf("results = %+v", out.Results)
		}
		if out.Renders < 3 {
			t.Errorf("renders = %d, want the base twice and the edit", out.Renders)
		}
	})
	t.Run("of two files that hold the value, only the edit of the one the render reads verifies", func(t *testing.T) {
		files := map[string][]byte{"a.txt": []byte("size=1"), "b.txt": []byte("size=1")}
		// b.txt wins in the render, so an edit of a.txt changes nothing.
		found := &Found{Candidates: []Candidate{cand("b.txt"), cand("a.txt")}}
		out, err := Verify(context.Background(), lineRenderer{}, lineInput(files, found), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Mapped() || len(out.Results) != 1 || out.Results[0].File != "b.txt" {
			t.Errorf("out = %+v", out)
		}
	})
	t.Run("an edit that does not change the render is not accepted, and the source explains", func(t *testing.T) {
		files := map[string][]byte{"a.txt": []byte("size=1"), "b.txt": []byte("size=1")}
		found := &Found{Candidates: []Candidate{cand("a.txt")}, Diagnostics: noted{}}
		out, err := Verify(context.Background(), lineRenderer{}, lineInput(files, found), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Mapped() || out.Unmapped.Reason != ReasonNoVerifiedCandidate {
			t.Fatalf("out = %+v", out)
		}
		if got := out.Unmapped.Fields[0].Detail; !strings.Contains(got, "the value comes from somewhere else") {
			t.Errorf("detail = %q", got)
		}
	})
}

// preparingRenderer records what the engine tells it before a batch.
type preparingRenderer struct {
	lineRenderer
	batches [][]map[string][]byte
}

func (p *preparingRenderer) Prepare(_ context.Context, sets []map[string][]byte) error {
	p.batches = append(p.batches, sets)
	return nil
}

func TestVerifyPreparesTheRendersOfABatch(t *testing.T) {
	files := map[string][]byte{"a.txt": []byte("size=1"), "b.txt": []byte("size=1")}
	found := &Found{Candidates: []Candidate{
		{File: "a.txt", Path: KeyPath{"size"}}, {File: "b.txt", Path: KeyPath{"size"}},
	}}
	r := &preparingRenderer{}
	out, err := Verify(context.Background(), r, lineInput(files, found), Options{})
	if err != nil || !out.Mapped() {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	if len(r.batches) == 0 || len(r.batches[0]) != 2 {
		t.Fatalf("batches = %d, the first has %d sets, want both candidates in one batch", len(r.batches), len(r.batches[0]))
	}
	for _, set := range r.batches[0] {
		if string(set["a.txt"]) == "size=1" && string(set["b.txt"]) == "size=1" {
			t.Errorf("a set of the batch is the unmodified files: %v", set)
		}
	}
}
