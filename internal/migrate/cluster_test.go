package migrate

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	backflowv1beta1 "github.com/sserkanml/backflow/api/v1beta1"
)

// cluster is an API server with the CRDs of backflow.io in the state of the upgrade: the objects are stored as v1alpha1
// (the CRDs are installed as they were in the last release), and then the CRDs of this release are applied.
type cluster struct {
	t    *testing.T
	cfg  *rest.Config
	c    client.Client // what the tool uses
	typ  client.Client // typed v1beta1 objects, to see the result as the operator does
	crds map[string]*apiextensionsv1.CustomResourceDefinition
}

var crdFiles = map[string]string{
	"scmconnections.backflow.io":   "backflow.io_scmconnections.yaml",
	"backflowpolicies.backflow.io": "backflow.io_backflowpolicies.yaml",
	"driftproposals.backflow.io":   "backflow.io_driftproposals.yaml",
}

// lastRelease is the directory with the CRDs of the last release (chart 0.17.0), as `helm show crds` printed them.
const lastRelease = "testdata/crds-0.17.0"

func startCluster(t *testing.T) *cluster {
	t.Helper()
	logf.SetLogger(logr.Discard())
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set: skipping the tests with an API server")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, coordinationv1.AddToScheme, corev1.AddToScheme, apiextensionsv1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	tool, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ts := runtime.NewScheme()
	if err := backflowv1beta1.AddToScheme(ts); err != nil {
		t.Fatal(err)
	}
	typ, err := client.New(cfg, client.Options{Scheme: ts})
	if err != nil {
		t.Fatal(err)
	}
	cl := &cluster{t: t, cfg: cfg, c: tool, typ: typ, crds: map[string]*apiextensionsv1.CustomResourceDefinition{}}
	for _, ns := range []string{"team-a", "backflow-system"} {
		if err := tool.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	return cl
}

func (cl *cluster) establish(name string) {
	cl.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := cl.c.Get(context.Background(), types.NamespacedName{Name: name}, crd); err == nil {
			for _, c := range crd.Status.Conditions {
				if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	cl.t.Fatalf("the CRD %s was not established", name)
}

// installLastRelease installs the CRDs of the last release: v1alpha1 is the only version, served and stored. The ChangeRecord
// CRD of that release was in backflow.io.
func (cl *cluster) installLastRelease() {
	cl.t.Helper()
	files := map[string]string{changeRecordCRD: "backflow.io_changerecords.yaml"}
	maps.Copy(files, crdFiles)
	for name := range files {
		data, err := os.ReadFile(filepath.Join(lastRelease, files[name]))
		if err != nil {
			cl.t.Fatal(err)
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(data, crd); err != nil {
			cl.t.Fatal(err)
		}
		if err := cl.c.Create(context.Background(), crd); err != nil {
			cl.t.Fatal(err)
		}
		cl.establish(name)
	}
}

// create makes an object through the v1alpha1 endpoint, and writes its status.
func (cl *cluster) createOld(kind, text string) {
	cl.t.Helper()
	o := parse(cl.t, text)
	status := o["status"]
	delete(o, "status")
	u := &unstructured.Unstructured{Object: o}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: Group, Version: OldVersion, Kind: kind})
	if err := cl.c.Create(context.Background(), u); err != nil {
		cl.t.Fatalf("create %s: %v", kind, err)
	}
	if status != nil {
		u.Object["status"] = status
		if err := cl.c.Status().Update(context.Background(), u); err != nil {
			cl.t.Fatalf("status of %s: %v", kind, err)
		}
	}
}

func (cl *cluster) storedVersions(name string) []string {
	cl.t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := cl.c.Get(context.Background(), types.NamespacedName{Name: name}, crd); err != nil {
		cl.t.Fatal(err)
	}
	return crd.Status.StoredVersions
}

// raw reads an object through an endpoint, as the maps the tool sees.
// raw reads an object of the namespace team-a through an endpoint, as the maps the tool sees.
func (cl *cluster) raw(version, kind, name string) obj {
	const namespace = "team-a"
	cl.t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: Group, Version: version, Kind: kind})
	if err := cl.c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, u); err != nil {
		cl.t.Fatalf("get %s %s/%s as %s: %v", kind, namespace, name, version, err)
	}
	return u.Object
}

func (cl *cluster) exists(version, kind, namespace, name string) bool {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: Group, Version: version, Kind: kind})
	err := cl.c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, u)
	if err != nil && !apierrors.IsNotFound(err) {
		cl.t.Fatal(err)
	}
	return err == nil
}

// run runs the tool and returns its report and what it printed.
func (cl *cluster) run(opt Options) (*Report, string, error) {
	cl.t.Helper()
	var out bytes.Buffer
	opt.Out = &out
	opt.Now = func() time.Time { return time.Now() }
	rep, err := Run(context.Background(), cl.c, opt)
	return rep, out.String(), err
}

func asUnstructured(o obj) *unstructured.Unstructured { return &unstructured.Unstructured{Object: o} }

func podTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"a": "b"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "m", Image: "example.invalid/m:1"}}},
	}
}

func (cl *cluster) gvkOf(version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: Group, Version: version, Kind: kind}
}

// setServed serves or stops serving v1alpha1 in the CRDs that still have it, for a test that reads the old shape of an object.
func (cl *cluster) setServed(served bool) {
	cl.t.Helper()
	for name := range crdFiles {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := cl.c.Get(context.Background(), types.NamespacedName{Name: name}, crd); err != nil {
			cl.t.Fatal(err)
		}
		i, was := versionServed(crd)
		if i < 0 || was == served {
			continue
		}
		crd.Spec.Versions[i].Served = served
		if err := cl.c.Update(context.Background(), crd); err != nil {
			cl.t.Fatal(err)
		}
	}
	time.Sleep(3 * time.Second)
}

// versions are the names of the versions a CRD has.
func (cl *cluster) versions(name string) []string {
	cl.t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := cl.c.Get(context.Background(), types.NamespacedName{Name: name}, crd); err != nil {
		cl.t.Fatal(err)
	}
	out := make([]string, 0, len(crd.Spec.Versions))
	for _, v := range crd.Spec.Versions {
		out = append(out, v.Name)
	}
	return out
}

func (cl *cluster) crdExists(name string) bool {
	err := cl.c.Get(context.Background(), types.NamespacedName{Name: name}, &apiextensionsv1.CustomResourceDefinition{})
	if err != nil && !apierrors.IsNotFound(err) {
		cl.t.Fatal(err)
	}
	return err == nil
}
