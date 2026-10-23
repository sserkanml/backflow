package helmmap

import (
	"reflect"
	"testing"

	"github.com/sserkanml/backflow/internal/rendermap"
)

func TestParseStrvals(t *testing.T) {
	tests := []struct {
		name    string
		want    rendermap.KeyPath
		wantErr bool
	}{
		{"config.feature", rendermap.KeyPath{"config", "feature"}, false},
		{"image.tag", rendermap.KeyPath{"image", "tag"}, false},
		{"a.b[0].c", rendermap.KeyPath{"a", "b", "0", "c"}, false},
		{"list[2]", rendermap.KeyPath{"list", "2"}, false},
		{`podLabels.app\.kubernetes\.io/name`, rendermap.KeyPath{"podLabels", "app.kubernetes.io/name"}, false},
		{"a[x]", nil, true},
		{"a[0", nil, true},
		{"", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStrvals(tt.name)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
