package helmmap

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	driftdiff "github.com/sserkanml/backflow/internal/drift"
	"github.com/sserkanml/backflow/internal/rendermap"
)

const structDefaults = `# Defaults of the chart.
replicaCount: 1
port: 8080
image:
  repository: example/web
  tag: "1.0"
podAnnotations: {}
podLabels: {}
tolerations: []
nodeSelector: {}
extraEnvVars: []
resourcesPreset: small
resources: {}
`

// structRenderer renders a small chart that has the shapes real charts have:
// maps and lists next to what the chart generates itself (a checksum annotation,
// a BASE env var, resources from a preset).
type structRenderer struct {
	src   *Source
	calls int
}

func (r *structRenderer) Render(_ context.Context, app rendermap.App, files map[string][]byte) ([]*unstructured.Unstructured, error) {
	r.calls++
	layers, err := r.src.Layers(files)
	if err != nil {
		return nil, err
	}
	v := mergeValues(layers)
	mapOf := func(key string) map[string]any { m, _ := v[key].(map[string]any); return m }
	listOf := func(key string) []any { l, _ := v[key].([]any); return l }

	annotations := map[string]any{"checksum/config": "abc"}
	maps.Copy(annotations, mapOf("podAnnotations"))
	labels := map[string]any{"app": "web"}
	maps.Copy(labels, mapOf("podLabels"))
	env := append([]any{map[string]any{"name": "BASE", "value": "1"}}, listOf("extraEnvVars")...)
	resources := mapOf("resources")
	if len(resources) == 0 {
		resources = map[string]any{"limits": map[string]any{"memory": "512Mi"}, "requests": map[string]any{"memory": "256Mi"}}
	}
	image, _ := v["image"].(map[string]any)
	pod := map[string]any{"containers": []any{map[string]any{
		"name": "web", "image": image["repository"].(string) + ":" + str(image["tag"]), "env": env, "resources": resources,
		"ports": []any{map[string]any{"containerPort": v["port"]}},
	}}}
	tolerations := slices.Concat(listOf("tolerations"), webTolerations(v))
	if len(tolerations) > 0 {
		pod["tolerations"] = tolerations
	}
	if n := mapOf("nodeSelector"); len(n) > 0 {
		pod["nodeSelector"] = n
	}
	dep := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": app.DestNamespace},
		"spec": map[string]any{
			"replicas": v["replicaCount"],
			"selector": map[string]any{"matchLabels": map[string]any{"app": "web"}},
			"template": map[string]any{"metadata": map[string]any{"labels": labels, "annotations": annotations}, "spec": pod},
		},
	}
	svc := map[string]any{
		"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "web", "namespace": app.DestNamespace},
		"spec": map[string]any{"ports": []any{map[string]any{"port": v["port"]}}},
	}
	out := []*unstructured.Unstructured{{Object: dep}, {Object: svc}}
	// Through JSON, as a render comes back from Argo CD.
	for i, o := range out {
		b, _ := json.Marshal(o.Object)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		out[i] = &unstructured.Unstructured{Object: m}
	}
	return out, nil
}

// webTolerations are the tolerations of the key web.tolerations, which a few
// tests add next to tolerations: two keys that feed the same list.
func webTolerations(v map[string]any) []any {
	web, _ := v["web"].(map[string]any)
	l, _ := web["tolerations"].([]any)
	return l
}

// structTemplate is what the chart's templates read: the static check of keys
// that were left out depends on it. Nothing in it mentions legacy.
const structTemplate = `# The Deployment of the chart.
replicas: {{ .Values.replicaCount }}
image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
port: {{ .Values.port }}
annotations:
  {{- with .Values.podAnnotations }}{{ toYaml . | nindent 2 }}{{- end }}
labels:
  {{- with .Values.podLabels }}{{ toYaml . | nindent 2 }}{{- end }}
{{- with .Values.tolerations }}
tolerations: {{ toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.web.tolerations }}
tolerations: {{ toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.app.tolerations }}
unused: {{ toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.nodeSelector }}
nodeSelector: {{ toYaml . | nindent 2 }}
{{- end }}
env: {{ toYaml .Values.extraEnvVars | nindent 2 }}
resources: {{- if .Values.resources }}{{ toYaml .Values.resources | nindent 2 }}{{- else }}{{ .Values.resourcesPreset }}{{- end }}
`

var (
	appStruct = rendermap.App{Name: "web-prod", Namespace: "argocd", Project: "default", DestNamespace: "prod"}
	keyWeb    = ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "prod", Name: "web"}
)

type structCase struct {
	t      *testing.T
	repo   fstest.MapFS
	src    *Source
	files  map[string][]byte
	rend   *structRenderer
	base   *unstructured.Unstructured
	fields []Field
}

// newStructCase builds the repository with the given environment values and
// renders the base.
func newStructCase(t *testing.T, env string) *structCase {
	t.Helper()
	c := &structCase{t: t, repo: fstest.MapFS{
		"chart/Chart.yaml":                &fstest.MapFile{Data: []byte("apiVersion: v2\nname: web\nversion: 1.0.0\n")},
		"chart/values.yaml":               &fstest.MapFile{Data: []byte(structDefaults)},
		"chart/env.yaml":                  &fstest.MapFile{Data: []byte(env)},
		"chart/templates/deployment.yaml": &fstest.MapFile{Data: []byte(structTemplate)},
	}}
	var err error
	if c.src, err = NewSource(c.repo, Spec{Path: "chart", ValueFiles: []string{"env.yaml"}}); err != nil {
		t.Fatal(err)
	}
	if c.files, err = CollectFiles(c.repo, c.src); err != nil {
		t.Fatal(err)
	}
	c.rend = &structRenderer{src: c.src}
	objs, err := c.rend.Render(context.Background(), appStruct, c.files)
	if err != nil {
		t.Fatal(err)
	}
	c.base = objs[0]
	return c
}

// drift patches the live state of the Deployment and returns the outcome of
// mapping what changed.
func (c *structCase) drift(mutate func(live map[string]any), opts Options) *Outcome {
	c.t.Helper()
	live := c.base.DeepCopy()
	mutate(live.Object)
	changes := driftdiff.Diff(c.base.Object, live.Object)
	if len(changes) == 0 {
		c.t.Fatal("the patch changes nothing")
	}
	fields, err := FieldsFor(c.src, c.files, changes)
	if err != nil {
		c.t.Fatal(err)
	}
	c.fields = fields
	out, err := Verify(context.Background(), c.rend, Input{App: appStruct, Resource: keyWeb, Fields: fields, Provider: c.src, Writer: ValuesWriter{}, Files: c.files}, opts)
	if err != nil {
		c.t.Fatal(err)
	}
	return out
}

func podSpecOf(o map[string]any) map[string]any {
	return o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
}

func podMeta(o map[string]any) map[string]any {
	return o["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)
}

func containerOf(o map[string]any) map[string]any {
	return podSpecOf(o)["containers"].([]any)[0].(map[string]any)
}

// edited returns the new content of the one edited file.
func edited(t *testing.T, out *Outcome) string {
	t.Helper()
	if !out.Mapped() {
		t.Fatalf("unmapped: %s: %s", out.Unmapped.Reason, out.Unmapped.Message)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results = %d", len(out.Results))
	}
	return string(out.Results[0].Edited)
}

func mustContain(t *testing.T, got string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(got, p) {
			t.Errorf("missing %q in\n%s", p, got)
		}
	}
}
