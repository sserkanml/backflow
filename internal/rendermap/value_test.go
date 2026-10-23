package rendermap

import (
	"encoding/json"
	"testing"
)

func TestRenderValue(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{"3", "3"},
		{3, "3"},
		{int64(3), "3"},
		{float64(3), "3"},
		{1.5, "1.5"},
		{float64(1000000), "1000000"},
		{json.Number("3"), "3"},
		{json.Number("3.0"), "3"},
		{json.Number("2.50"), "2.5"},
		{true, "true"},
		{false, "false"},
		{"", ""},
		{nil, ""},
		{map[string]any{"a": 1}, ""},
	}
	for _, tt := range tests {
		if got := RenderValue(tt.in); got != tt.want {
			t.Errorf("RenderValue(%#v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
