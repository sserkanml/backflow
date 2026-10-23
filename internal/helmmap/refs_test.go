package helmmap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/sserkanml/backflow/internal/rendermap"
)

func refsOf(t *testing.T, texts ...string) *valueRefs {
	t.Helper()
	files := map[string][]byte{
		"c/Chart.yaml": []byte("name: c\ndependencies:\n  - name: redis\n    alias: store\n"),
	}
	for i, s := range texts {
		files["c/templates/f"+string(rune('a'+i))+".yaml"] = []byte(s)
	}
	return scanValueRefs(files)
}

func TestValueRefsMentions(t *testing.T) {
	tpl := `
{{- /* .Values.commented.out and a bare .Values in a comment */ -}}
image: {{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- with .Values.podAnnotations }}{{ toYaml . }}{{- end }}
{{ include "x" (dict "root" $ "v" $.Values.nodeSelector) }}
{{ index .Values "extra" "list" }}
{{ get .Values "flag" }}
{{ .context.Values.global.imageRegistry }}
`
	r := refsOf(t, tpl)
	if r.all {
		t.Fatal("no template uses .Values as a whole: the one in the comment does not count")
	}
	for key, want := range map[string]bool{
		"image.tag":            true, // read
		"image":                true, // the map it is in is read below
		"image.tag.deep":       true, // a key below a key that is read
		"podAnnotations.team":  true, // below a key that is read
		"nodeSelector":         true,
		"extra.list":           true,
		"extra.list.0":         true,
		"flag":                 true,
		"global.imageRegistry": true,
		"store.image.tag":      true,  // a dependency's alias: its templates read image.tag
		"commented.out":        false, // only in a comment
		"legacy.tolerations":   false,
		"metrics.resources":    false,
		"tolerations":          false,
	} {
		k, _ := rendermap.ParseKeyPath(key)
		if got := r.Mentions(k); got != want {
			t.Errorf("mentions(%s) = %v, want %v", key, got, want)
		}
	}
}

func TestValueRefsBareValuesMakesEveryKeyRead(t *testing.T) {
	for name, tpl := range map[string]string{
		"passed on":      `{{ include "x" (dict "values" .Values) }}`,
		"assigned":       `{{- $v := .Values }}{{ $v.a }}`,
		"computed index": `{{ index .Values $name }}`,
		"toYaml":         `{{ toYaml .Values }}`,
	} {
		r := refsOf(t, tpl)
		if !r.all || !r.Mentions(rendermap.KeyPath{"anything"}) {
			t.Errorf("%s: all = %v", name, r.all)
		}
	}
}

func TestValueRefsReadsPackagedCharts(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "image: {{ .Values.sub.setting }}"
	_ = tw.WriteHeader(&tar.Header{Name: "sub/templates/d.yaml", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()
	r := scanValueRefs(map[string][]byte{"c/charts/sub-1.0.0.tgz": buf.Bytes()})
	if !r.Mentions(rendermap.KeyPath{"sub", "setting"}) || r.Mentions(rendermap.KeyPath{"other"}) {
		t.Errorf("refs = %+v", r)
	}
	// What cannot be read could read anything.
	if bad := scanValueRefs(map[string][]byte{"c/charts/x-1.tgz": []byte("not gzip")}); !bad.all {
		t.Error("an archive that cannot be read must count as reading anything")
	}
}

func TestValueRefsReadsTplStringsInValueFiles(t *testing.T) {
	r := scanValueRefs(map[string][]byte{"c/values.yaml": []byte("greeting: \"{{ .Values.name }}\"\n")})
	if !r.Mentions(rendermap.KeyPath{"name"}) {
		t.Error("a tpl string in a value file reads .Values.name")
	}
}

func TestValueRefsWithoutTemplatesProveNothing(t *testing.T) {
	r := scanValueRefs(map[string][]byte{"c/values.yaml": []byte("a: 1\n")})
	if !r.all || !r.Mentions(rendermap.KeyPath{"a"}) || !r.Mentions(rendermap.KeyPath{"b"}) {
		t.Errorf("no template was found: every key may be read, refs = %+v", r)
	}
}
