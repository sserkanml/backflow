package drift

import (
	"encoding/json"
	"testing"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

func decode(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLookup(t *testing.T) {
	state := decode(t, `{"data":{"LOG_LEVEL":"warn","a/b":"x","n":0},"spec":{"ports":[{"port":80},{"port":443}]},"empty":null}`)
	tests := []struct {
		pointer string
		want    interface{}
		ok      bool
	}{
		{"/data/LOG_LEVEL", "warn", true},
		{"/data/a~1b", "x", true},
		{"/data/n", float64(0), true},
		{"/spec/ports/1/port", float64(443), true},
		{"/empty", nil, true},
		{"/data/missing", nil, false},
		{"/spec/ports/2", nil, false},
		{"/spec/ports/x", nil, false},
		{"/spec/ports/-1", nil, false},
		{"/data/LOG_LEVEL/deeper", nil, false},
		{"data", nil, false},
	}
	for _, tt := range tests {
		got, ok := Lookup(state, tt.pointer)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("Lookup(%q) = %v, %v; want %v, %v", tt.pointer, got, ok, tt.want, tt.ok)
		}
	}
}

func TestWhere(t *testing.T) {
	replace := v1alpha1.FieldChange{Path: "/data/LOG_LEVEL", Op: v1alpha1.OpReplace, Desired: `"warn"`, Live: `"error"`}
	add := v1alpha1.FieldChange{Path: "/data/NEW", Op: v1alpha1.OpAdd, Live: `"1"`}
	remove := v1alpha1.FieldChange{Path: "/data/OLD", Op: v1alpha1.OpRemove, Desired: `"y"`}

	tests := []struct {
		name              string
		state             string
		changes           []v1alpha1.FieldChange
		atDesired, atLive bool
	}{
		{"reset to Git", `{"data":{"LOG_LEVEL":"warn"}}`, []v1alpha1.FieldChange{replace}, true, false},
		{"still changed", `{"data":{"LOG_LEVEL":"error"}}`, []v1alpha1.FieldChange{replace}, false, true},
		{"a third value", `{"data":{"LOG_LEVEL":"debug"}}`, []v1alpha1.FieldChange{replace}, false, false},
		{"field gone", `{"data":{}}`, []v1alpha1.FieldChange{replace}, false, false},
		{"added field reset: absent again", `{"data":{}}`, []v1alpha1.FieldChange{add}, true, false},
		{"added field still there", `{"data":{"NEW":"1"}}`, []v1alpha1.FieldChange{add}, false, true},
		{"removed field reset: back", `{"data":{"OLD":"y"}}`, []v1alpha1.FieldChange{remove}, true, false},
		{"removed field still gone", `{"data":{}}`, []v1alpha1.FieldChange{remove}, false, true},
		{"all fields reset", `{"data":{"LOG_LEVEL":"warn","OLD":"y"}}`, []v1alpha1.FieldChange{replace, add, remove}, true, false},
		{"only some fields reset", `{"data":{"LOG_LEVEL":"warn"}}`, []v1alpha1.FieldChange{replace, remove}, false, false},
		{"no changes", `{}`, nil, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, l := Where(decode(t, tt.state), tt.changes)
			if d != tt.atDesired || l != tt.atLive {
				t.Errorf("Where = (%v, %v), want (%v, %v)", d, l, tt.atDesired, tt.atLive)
			}
		})
	}
}
