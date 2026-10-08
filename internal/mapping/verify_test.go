package mapping

import (
	"errors"
	"strings"
	"testing"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

func change(path string, op v1alpha1.ChangeOperation, desired, live string) v1alpha1.FieldChange {
	return v1alpha1.FieldChange{Path: path, Op: op, Desired: desired, Live: live}
}

func TestVerify(t *testing.T) {
	logLevel := change("/data/LOG_LEVEL", v1alpha1.OpReplace, `"info"`, `"debug"`)
	edited := strings.Replace(configMapYAML, "LOG_LEVEL: info", "LOG_LEVEL: debug", 1)

	tests := []struct {
		name    string
		orig    string
		edited  string
		doc     int
		changes []v1alpha1.FieldChange
		wantErr bool
	}{
		{"the edit is exactly the proposal", configMapYAML, edited, 0, []v1alpha1.FieldChange{logLevel}, false},
		{"nothing expected and nothing changed", configMapYAML, configMapYAML, 0, nil, false},
		{"the edit does less than the proposal", configMapYAML, configMapYAML, 0, []v1alpha1.FieldChange{logLevel}, true},
		{
			"the edit does more than the proposal",
			configMapYAML, strings.Replace(edited, "TIMEOUT: 30", "TIMEOUT: 31", 1), 0,
			[]v1alpha1.FieldChange{logLevel}, true,
		},
		{
			"the edit writes another value",
			configMapYAML, strings.Replace(configMapYAML, "LOG_LEVEL: info", "LOG_LEVEL: trace", 1), 0,
			[]v1alpha1.FieldChange{logLevel}, true,
		},
		{
			"a replace of a field the file does not have, added with the live value",
			configMapYAML, configMapYAML + "  EXTRA: x\n", 0,
			[]v1alpha1.FieldChange{change("/data/EXTRA", v1alpha1.OpReplace, `"y"`, `"x"`)}, false,
		},
		{
			"a replace of a field the file does not have, added with another value",
			configMapYAML, configMapYAML + "  EXTRA: z\n", 0,
			[]v1alpha1.FieldChange{change("/data/EXTRA", v1alpha1.OpReplace, `"y"`, `"x"`)}, true,
		},
		{"an add", configMapYAML, configMapYAML + "  EXTRA: x\n", 0,
			[]v1alpha1.FieldChange{change("/data/EXTRA", v1alpha1.OpAdd, "", `"x"`)}, false},
		{"a remove", configMapYAML, strings.Replace(configMapYAML, "  TIMEOUT: 30\n", "", 1), 0,
			[]v1alpha1.FieldChange{change("/data/TIMEOUT", v1alpha1.OpRemove, `30`, "")}, false},
		{"a number written as a string is a change", configMapYAML, strings.Replace(configMapYAML, "TIMEOUT: 30", `TIMEOUT: "30"`, 1), 0,
			nil, true},
		{
			"another document was modified",
			multiDocYAML, strings.Replace(multiDocYAML, "- port: 80", "- port: 81", 1), 1,
			nil, true,
		},
		{
			"a document was added",
			multiDocYAML, multiDocYAML + "---\nkind: x\n", 1, nil, true,
		},
		{"the edited document is not valid YAML", configMapYAML, "kind: [oops\n", 0, []v1alpha1.FieldChange{logLevel}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify([]byte(tt.orig), []byte(tt.edited), tt.doc, tt.changes)
			if tt.wantErr != errors.Is(err, ErrVerificationFailed) || (err != nil) != tt.wantErr {
				t.Errorf("err = %v, want failure = %v", err, tt.wantErr)
			}
		})
	}
}

func TestApplyThenVerify(t *testing.T) {
	changes := []v1alpha1.FieldChange{
		change("/data/FEATURE", v1alpha1.OpReplace, `"on"`, `"off"`),
		change("/data/LOG_LEVEL", v1alpha1.OpReplace, `"info"`, `"debug"`),
		change("/data/NEW", v1alpha1.OpAdd, "", `"x"`),
		change("/data/TIMEOUT", v1alpha1.OpRemove, `30`, ""),
		// Diff reports a whole missing map as one Add at the first missing key.
		change("/metadata/annotations", v1alpha1.OpAdd, "", `{"example.com/owner":"me"}`),
		change("/metadata/labels/app.kubernetes.io~1name", v1alpha1.OpReplace, `"demo"`, `"other"`),
	}
	// Diff orders by path; the proposal's changes come in that order.
	got, err := Apply("cm.yaml", []byte(configMapYAML), 0, changes)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify([]byte(configMapYAML), got, 0, changes); err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
}

// Shapes that make Apply fall back to re-rendering a bigger entry must still
// produce an edit that verifies.
func TestFallbackEditsVerify(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		doc     int
		changes []v1alpha1.FieldChange
	}{
		{
			"remove the first key of a list item",
			multiDocYAML, 1,
			[]v1alpha1.FieldChange{change("/spec/template/spec/containers/0/name", v1alpha1.OpRemove, `"demo"`, "")},
		},
		{
			"remove from a flow map",
			edgeYAML, 0,
			[]v1alpha1.FieldChange{change("/metadata/labels/a", v1alpha1.OpRemove, `"b"`, "")},
		},
		{
			"replace a whole list",
			multiDocYAML, 1,
			[]v1alpha1.FieldChange{change("/spec/template/spec/containers", v1alpha1.OpReplace,
				`[{"args":["a","b"],"image":"nginx:1.0","name":"demo"},{"image":"busybox","name":"side"}]`,
				`[{"image":"x","name":"demo"}]`)},
		},
		{
			"replace a map by a scalar",
			configMapYAML, 0,
			[]v1alpha1.FieldChange{change("/metadata/labels", v1alpha1.OpReplace, `{"app.kubernetes.io/name":"demo"}`, `"none"`)},
		},
		{
			"replace a scalar by a map",
			configMapYAML, 0,
			[]v1alpha1.FieldChange{change("/data/LOG_LEVEL", v1alpha1.OpReplace, `"info"`, `{"a":1,"b":[1,2]}`)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Apply("f.yaml", []byte(tt.input), tt.doc, tt.changes)
			if err != nil {
				t.Fatal(err)
			}
			if err := Verify([]byte(tt.input), got, tt.doc, tt.changes); err != nil {
				t.Fatalf("%v\n%s", err, got)
			}
		})
	}
}

func TestVerifyDefaultedFieldsStayStrict(t *testing.T) {
	deploy := multiDocYAML
	withLimit := strings.Replace(deploy, "  replicas: 1 # keep small\n", "  replicas: 1 # keep small\n  revisionHistoryLimit: 5\n", 1)
	defaulted := change("/spec/revisionHistoryLimit", v1alpha1.OpReplace, `10`, `5`)
	// Verify compares whole documents; index 1 is the Deployment.
	tests := []struct {
		name    string
		edited  string
		changes []v1alpha1.FieldChange
		wantErr bool
	}{
		{"the field is added with the live value", withLimit, []v1alpha1.FieldChange{defaulted}, false},
		{"the edit does not add the field", deploy, []v1alpha1.FieldChange{defaulted}, true},
		{
			"the field is added with another value",
			strings.Replace(withLimit, "revisionHistoryLimit: 5", "revisionHistoryLimit: 6", 1),
			[]v1alpha1.FieldChange{defaulted}, true,
		},
		{
			"the edit also adds something else",
			strings.Replace(withLimit, "revisionHistoryLimit: 5\n", "revisionHistoryLimit: 5\n  paused: true\n", 1),
			[]v1alpha1.FieldChange{defaulted}, true,
		},
		{
			"the edit also changes a present field",
			strings.Replace(withLimit, "replicas: 1", "replicas: 2", 1),
			[]v1alpha1.FieldChange{defaulted}, true,
		},
		{
			"a missing parent holding more than the proposal asked for",
			strings.Replace(deploy, "  replicas: 1 # keep small\n", "  replicas: 1 # keep small\n  strategy:\n    type: Recreate\n    rollingUpdate:\n      maxSurge: 2\n", 1),
			[]v1alpha1.FieldChange{change("/spec/strategy/type", v1alpha1.OpReplace, `"RollingUpdate"`, `"Recreate"`)}, true,
		},
		{
			"a missing parent holding exactly what the proposal asked for",
			strings.Replace(deploy, "  replicas: 1 # keep small\n", "  replicas: 1 # keep small\n  strategy:\n    type: Recreate\n", 1),
			[]v1alpha1.FieldChange{change("/spec/strategy/type", v1alpha1.OpReplace, `"RollingUpdate"`, `"Recreate"`)}, false,
		},
		{
			"a defaulted object value",
			strings.Replace(deploy, "  replicas: 1 # keep small\n", "  replicas: 1 # keep small\n  selectorX:\n    a: 1\n", 1),
			[]v1alpha1.FieldChange{change("/spec/selectorX", v1alpha1.OpReplace, `{"a":0}`, `{"a":1}`)}, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify([]byte(deploy), []byte(tt.edited), 1, tt.changes)
			if (err != nil) != tt.wantErr || (err != nil && !errors.Is(err, ErrVerificationFailed)) {
				t.Errorf("err = %v, want failure = %v", err, tt.wantErr)
			}
		})
	}
}
