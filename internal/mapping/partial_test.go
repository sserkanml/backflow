package mapping

import (
	"errors"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

const mountedDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo
  namespace: demo
spec:
  # Keep it small.
  replicas: 1
  template:
    spec:
      containers:
        - name: demo
          image: nginx:1.27
          volumeMounts:
            - name: data
              mountPath: /data
            - name: cache
              mountPath: /cache
`

func mountedRepo() map[string]string {
	return map[string]string{"apps/demo/deployment.yaml": mountedDeployment, "README.md": "not a manifest"}
}

var (
	deployment    = ResourceID{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "demo"}
	scaleUp       = change("/spec/replicas", v1alpha1.OpReplace, `1`, `3`)
	removedMount  = change("/spec/template/spec/containers/0/volumeMounts/1", v1alpha1.OpRemove, `{"name":"cache","mountPath":"/cache"}`, "")
	scaleUpWithIt = []v1alpha1.FieldChange{scaleUp, removedMount}
)

func TestMapPartialLeavesOutWhatCannotBeWritten(t *testing.T) {
	// Map is all or nothing.
	if _, err := Map(files(mountedRepo()), "apps/demo", DirectoryOptions{}, "demo", deployment, scaleUpWithIt); !errors.Is(err, ErrCannotApply) {
		t.Fatalf("Map: err = %v, want ErrCannotApply", err)
	}

	res, err := MapPartial(files(mountedRepo()), "apps/demo", DirectoryOptions{}, "demo", deployment, scaleUpWithIt)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(mountedDeployment, "replicas: 1", "replicas: 3", 1); string(res.Edited) != want {
		t.Errorf("edited file =\n%s\nwant only the replicas changed:\n%s", res.Edited, want)
	}
	if len(res.Edits) != 1 || res.Edits[0].Location != "/spec/replicas" || res.Edits[0].Value != "3" || res.Edits[0].ChangeIndex != 0 {
		t.Errorf("edits = %+v", res.Edits)
	}
	if len(res.LeftOut) != 1 || res.LeftOut[0].ChangeIndex != 1 || res.LeftOut[0].Path != removedMount.Path {
		t.Fatalf("left out = %+v", res.LeftOut)
	}
	if !errors.Is(res.LeftOut[0].Err, ErrCannotApply) {
		t.Errorf("left-out error = %v", res.LeftOut[0].Err)
	}
	if msg := res.LeftOut[0].Message(); !strings.Contains(msg, "array items cannot be removed") || strings.Contains(msg, "change 0") {
		t.Errorf("message = %q", msg)
	}
	if strings.Contains(res.Diff, "cache") || !strings.Contains(res.Diff, "+  replicas: 3") {
		t.Errorf("diff =\n%s", res.Diff)
	}
}

func TestMapPartialKeepsTheIndexesOfTheProposal(t *testing.T) {
	res, err := MapPartial(files(mountedRepo()), "apps/demo", DirectoryOptions{}, "demo", deployment,
		[]v1alpha1.FieldChange{removedMount, scaleUp})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Edits) != 1 || res.Edits[0].ChangeIndex != 1 {
		t.Errorf("edits = %+v, want the replicas edit with ChangeIndex 1", res.Edits)
	}
	if len(res.LeftOut) != 1 || res.LeftOut[0].ChangeIndex != 0 {
		t.Errorf("left out = %+v, want ChangeIndex 0", res.LeftOut)
	}
}

func TestMapPartialIsMapWhenEverythingCanBeWritten(t *testing.T) {
	changes := []v1alpha1.FieldChange{scaleUp}
	want, err := Map(files(mountedRepo()), "apps/demo", DirectoryOptions{}, "demo", deployment, changes)
	if err != nil {
		t.Fatal(err)
	}
	got, err := MapPartial(files(mountedRepo()), "apps/demo", DirectoryOptions{}, "demo", deployment, changes)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Edited) != string(want.Edited) || got.Diff != want.Diff || len(got.LeftOut) != 0 || len(got.Edits) != len(want.Edits) {
		t.Errorf("partial = %+v, want %+v", got, want)
	}
}

func TestMapPartialWithNothingToWriteIsNotMapped(t *testing.T) {
	_, err := MapPartial(files(mountedRepo()), "apps/demo", DirectoryOptions{}, "demo", deployment,
		[]v1alpha1.FieldChange{removedMount})
	if !errors.Is(err, ErrCannotApply) {
		t.Errorf("err = %v, want ErrCannotApply", err)
	}
}

func TestMapPartialEditsAJSONManifest(t *testing.T) {
	repo := map[string]string{"apps/demo/deployment.json": `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"demo","namespace":"demo"},"spec":{"replicas":1}}`}
	res, err := MapPartial(files(repo), "apps/demo", DirectoryOptions{}, "demo", deployment, scaleUpWithIt)
	if err != nil {
		t.Fatalf("a JSON manifest is edited like a YAML one: %v", err)
	}
	if !strings.Contains(string(res.Edited), `"replicas":3`) {
		t.Errorf("edited = %s", res.Edited)
	}
}

func TestMapPartialDoesNotFindWhatIsNotThere(t *testing.T) {
	_, err := MapPartial(files(mountedRepo()), "apps/demo", DirectoryOptions{}, "demo",
		ResourceID{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "other"}, scaleUpWithIt)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
