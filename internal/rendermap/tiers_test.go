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

// tierWriter writes to the files of lineSource. A candidate whose Variant is
// "new" creates the file named by its File and lists it in list.txt, the way a
// new patch file is listed in a kustomization.
type tierWriter struct{ lineWriter }

func (w tierWriter) Write(c Candidate, content []byte, value string) (*Written, error) {
	if c.Variant != "new" {
		return w.lineWriter.Write(c, content, value)
	}
	return &Written{
		Result: mapping.Result{File: c.File, Edited: []byte(c.Path[0] + "=" + value),
			Edits: []mapping.Edit{{File: c.File, Location: c.Path.Pointer(), Value: fmt.Sprintf("%q", value)}}},
		Also: []mapping.Result{{File: "list.txt", Edited: []byte("include=" + c.File),
			Edits: []mapping.Edit{{File: "list.txt", Location: "/include", Value: `"` + c.File + `"`}}}},
	}, nil
}

// tierRenderer renders the lines of a.txt and b.txt, and then those of the file
// that list.txt includes, which win.
type tierRenderer struct{ calls int }

func (r *tierRenderer) Render(ctx context.Context, app App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	r.calls++
	merged := map[string][]byte{"a.txt": files["a.txt"], "b.txt": files["b.txt"]}
	for l := range bytes.SplitSeq(files["list.txt"], []byte("\n")) {
		if name, ok := strings.CutPrefix(string(l), "include="); ok {
			merged["b.txt"] = append(append(merged["b.txt"], '\n'), files[name]...)
		}
	}
	return lineRenderer{}.Render(ctx, app, merged)
}

func tierInput(files map[string][]byte, found *Found) Input {
	in := lineInput(files, found)
	in.Writer = tierWriter{}
	return in
}

func sizeCand(file string) Candidate { return Candidate{File: file, Path: KeyPath{"size"}} }

func TestLaterTiers(t *testing.T) {
	files := func() map[string][]byte {
		return map[string][]byte{"a.txt": []byte("size=1"), "b.txt": []byte("size=1")}
	}
	t.Run("a later tier is tried only when the first candidates verify nothing", func(t *testing.T) {
		// b.txt wins in the render, so the edit of a.txt changes nothing.
		found := &Found{
			Candidates: []Candidate{sizeCand("a.txt")},
			Later:      []Tier{{Name: "the other file", Candidates: []Candidate{sizeCand("b.txt")}}},
		}
		rend := &tierRenderer{}
		out, err := Verify(context.Background(), rend, tierInput(files(), found), Options{})
		if err != nil || !out.Mapped() {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
		if len(out.Results) != 1 || out.Results[0].File != "b.txt" || string(out.Results[0].Edited) != "size=2" {
			t.Errorf("results = %+v", out.Results)
		}
		if rend.calls != 4 {
			t.Errorf("renders = %d, want the base twice, the first candidate and the later one", rend.calls)
		}
		if len(out.Applied) != 1 || out.Applied[0].Candidate.File != "b.txt" || out.Applied[0].Value != "2" {
			t.Errorf("applied = %+v", out.Applied)
		}
	})

	t.Run("a later tier is not rendered when an earlier candidate verifies", func(t *testing.T) {
		found := &Found{
			Candidates: []Candidate{sizeCand("b.txt")},
			Later:      []Tier{{Name: "never", Candidates: []Candidate{sizeCand("a.txt")}}},
		}
		rend := &tierRenderer{}
		out, err := Verify(context.Background(), rend, tierInput(files(), found), Options{})
		if err != nil || !out.Mapped() || out.Results[0].File != "b.txt" {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
		if rend.calls != 3 {
			t.Errorf("renders = %d, want 3", rend.calls)
		}
	})

	t.Run("tiers are tried in order", func(t *testing.T) {
		found := &Found{
			Candidates: []Candidate{sizeCand("a.txt")},
			Later: []Tier{
				{Name: "second", Candidates: []Candidate{sizeCand("a.txt")}}, // still changes nothing
				{Name: "third", Candidates: []Candidate{sizeCand("b.txt")}},
			},
		}
		rend := &tierRenderer{}
		out, err := Verify(context.Background(), rend, tierInput(files(), found), Options{})
		if err != nil || !out.Mapped() || out.Results[0].File != "b.txt" {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
		if rend.calls != 4 {
			t.Errorf("renders = %d: the same edit in a later tier is not rendered again", rend.calls)
		}
	})

	t.Run("two edits that verify in one tier are ambiguous", func(t *testing.T) {
		both := map[string][]byte{"a.txt": []byte("size=1"), "b.txt": []byte("size=1")}
		// A tier's candidates that both change the render the same way are two ways.
		found := &Found{Later: []Tier{{Name: "t", Candidates: []Candidate{
			sizeCand("b.txt"),
			{File: "n.txt", Path: KeyPath{"size"}, Variant: "new", NewFile: true},
		}}}}
		out, err := Verify(context.Background(), &tierRenderer{}, tierInput(both, found), Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonAmbiguousCandidates {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
	})

	t.Run("a tier bigger than the bound is not tried", func(t *testing.T) {
		cands := make([]Candidate, 0, 4)
		for i := range 4 {
			cands = append(cands, Candidate{File: fmt.Sprintf("n%d.txt", i), Path: KeyPath{"size"}, Variant: "new", NewFile: true})
		}
		rend := &tierRenderer{}
		out, err := Verify(context.Background(), rend, tierInput(files(), &Found{Later: []Tier{{Name: "patches", Candidates: cands}}}),
			Options{MaxCandidates: 3})
		if err != nil || out.Mapped() || out.Unmapped.Reason != ReasonTooManyCandidates || rend.calls != 2 {
			t.Fatalf("out = %+v, err = %v, renders = %d", out, err, rend.calls)
		}
	})
}

func TestNewFilesAndAlsoFiles(t *testing.T) {
	files := map[string][]byte{"a.txt": []byte("size=1"), "b.txt": []byte("size=1")}
	found := &Found{Later: []Tier{{Name: "new file", Candidates: []Candidate{
		{File: "n.txt", Path: KeyPath{"size"}, Variant: "new", NewFile: true},
	}}}}
	out, err := Verify(context.Background(), &tierRenderer{}, tierInput(files, found), Options{})
	if err != nil || !out.Mapped() {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("results = %+v: the new file and the file that lists it", out.Results)
	}
	byFile := map[string]mapping.Result{}
	for _, r := range out.Results {
		byFile[r.File] = r
	}
	n, l := byFile["n.txt"], byFile["list.txt"]
	if string(n.Edited) != "size=2" || len(n.Edits) != 1 || n.Edits[0].ChangeIndex != 0 || !strings.Contains(n.Diff, "+size=2") {
		t.Errorf("n.txt = %+v", n)
	}
	if string(l.Edited) != "include=n.txt" || len(l.Edits) != 1 || l.Edits[0].Location != "/include" || l.Edits[0].ChangeIndex != 0 {
		t.Errorf("list.txt = %+v", l)
	}
}

// countingWriter counts the edits the engine asks a writer for.
type countingWriter struct {
	tierWriter
	writes *int
}

func (w countingWriter) Write(c Candidate, content []byte, value string) (*Written, error) {
	*w.writes++
	return w.tierWriter.Write(c, content, value)
}

func TestVariantsAreDifferentEdits(t *testing.T) {
	files := map[string][]byte{"a.txt": []byte("size=1"), "b.txt": []byte("other=1")}
	run := func(cands ...Candidate) int {
		writes := 0
		in := tierInput(files, &Found{Candidates: cands})
		in.Writer = countingWriter{writes: &writes}
		if _, err := Verify(context.Background(), &tierRenderer{}, in, Options{}); err != nil {
			t.Fatal(err)
		}
		return writes
	}
	same := run(sizeCand("a.txt"), sizeCand("a.txt"))
	other := run(sizeCand("a.txt"), Candidate{File: "a.txt", Path: KeyPath{"size"}, Variant: "x"})
	if other <= same {
		t.Errorf("edits written: the same candidate twice = %d, two variants = %d; a variant is a different edit", same, other)
	}
}

func TestBlockedField(t *testing.T) {
	files := map[string][]byte{"a.txt": []byte("size=1\nname=a"), "b.txt": []byte("other=1")}
	blocked := &Found{
		Candidates: []Candidate{sizeCand("a.txt")},
		Blocked:    &Blocked{Reason: ReasonSetByApplication, Message: "set by spec.source.kustomize.images of Application x; Git cannot change it"},
	}
	t.Run("a blocked field is Unmapped with the source's reason, without rendering", func(t *testing.T) {
		rend := &tierRenderer{}
		out, err := Verify(context.Background(), rend, tierInput(files, blocked), Options{})
		if err != nil || out.Mapped() {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
		if out.Unmapped.Reason != ReasonSetByApplication || !strings.Contains(out.Unmapped.Message, "Git cannot change it") ||
			len(out.Unmapped.Fields) != 1 || out.Unmapped.Fields[0].Pointer != "/data/size" {
			t.Errorf("unmapped = %+v", out.Unmapped)
		}
		if rend.calls != 0 {
			t.Errorf("%d renders for a field that cannot be mapped", rend.calls)
		}
	})
	t.Run("with a partial mapping the other fields are still mapped", func(t *testing.T) {
		in := tierInput(files, &Found{Candidates: []Candidate{sizeCand("a.txt")}})
		in.Fields = append(in.Fields, Field{Index: 1, Found: blocked, Change: v1alpha1.FieldChange{
			Path: "/data/name", Op: v1alpha1.OpReplace, Desired: `"a"`, Live: `"b"`}})
		out, err := Verify(context.Background(), &tierRenderer{}, in, Options{AllowPartial: true})
		if err != nil || !out.Mapped() {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
		if len(out.LeftOut) != 1 || out.LeftOut[0].Reason != ReasonSetByApplication || out.LeftOut[0].Pointer != "/data/name" {
			t.Errorf("left out = %+v", out.LeftOut)
		}
		if len(out.Results) != 1 || string(out.Results[0].Edited) != "size=2\nname=a" {
			t.Errorf("results = %+v", out.Results)
		}
	})
}
