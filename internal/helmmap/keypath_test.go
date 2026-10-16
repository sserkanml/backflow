package helmmap

import (
	"reflect"
	"testing"
)

func TestKeyPathStringRoundTrip(t *testing.T) {
	tests := []struct {
		path KeyPath
		want string
	}{
		{KeyPath{"config", "logLevel"}, "config.logLevel"},
		{KeyPath{"global", "region"}, "global.region"},
		{KeyPath{"podLabels", "app.kubernetes.io/name"}, `podLabels.app\.kubernetes\.io/name`},
		{KeyPath{"a", "0", "b"}, "a.0.b"},
		{KeyPath{`back\slash`, "x.y"}, `back\\slash.x\.y`},
		{KeyPath{"", "x"}, ".x"},
		{KeyPath{"only"}, "only"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.path.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
			back, err := ParseKeyPath(tt.want)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back, tt.path) {
				t.Fatalf("ParseKeyPath(%q) = %q, want %q", tt.want, back, tt.path)
			}
		})
	}
}

func TestParseKeyPathErrors(t *testing.T) {
	for _, in := range []string{"", `a\`} {
		if _, err := ParseKeyPath(in); err == nil {
			t.Errorf("ParseKeyPath(%q) succeeded", in)
		}
	}
}

func TestKeyPathPrefix(t *testing.T) {
	tests := []struct {
		p, q    KeyPath
		prefix  bool
		equal   bool
		related bool
	}{
		{KeyPath{"a", "b"}, KeyPath{"a"}, true, false, true},
		{KeyPath{"a"}, KeyPath{"a", "b"}, false, false, true},
		{KeyPath{"a", "b"}, KeyPath{"a", "b"}, true, true, true},
		{KeyPath{"a", "b"}, KeyPath{"a", "c"}, false, false, false},
		{KeyPath{"ab"}, KeyPath{"a"}, false, false, false},
	}
	for _, tt := range tests {
		if got := tt.p.HasPrefix(tt.q); got != tt.prefix {
			t.Errorf("%v.HasPrefix(%v) = %v", tt.p, tt.q, got)
		}
		if got := tt.p.Equal(tt.q); got != tt.equal {
			t.Errorf("%v.Equal(%v) = %v", tt.p, tt.q, got)
		}
		if got := related(tt.p, tt.q); got != tt.related {
			t.Errorf("related(%v, %v) = %v", tt.p, tt.q, got)
		}
	}
}

func TestKeyPathPointer(t *testing.T) {
	got := KeyPath{"podLabels", "app.kubernetes.io/name", "a~b", "0"}.pointer()
	if want := "/podLabels/app.kubernetes.io~1name/a~0b/0"; got != want {
		t.Fatalf("pointer() = %q, want %q", got, want)
	}
}

func TestParseStrvals(t *testing.T) {
	tests := []struct {
		name    string
		want    KeyPath
		wantErr bool
	}{
		{"config.feature", KeyPath{"config", "feature"}, false},
		{"image.tag", KeyPath{"image", "tag"}, false},
		{"a.b[0].c", KeyPath{"a", "b", "0", "c"}, false},
		{"list[2]", KeyPath{"list", "2"}, false},
		{`podLabels.app\.kubernetes\.io/name`, KeyPath{"podLabels", "app.kubernetes.io/name"}, false},
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
