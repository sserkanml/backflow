package rendermap

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
		if got := Related(tt.p, tt.q); got != tt.related {
			t.Errorf("Related(%v, %v) = %v", tt.p, tt.q, got)
		}
	}
}

func TestKeyPathPointer(t *testing.T) {
	got := KeyPath{"podLabels", "app.kubernetes.io/name", "a~b", "0"}.Pointer()
	if want := "/podLabels/app.kubernetes.io~1name/a~0b/0"; got != want {
		t.Fatalf("pointer() = %q, want %q", got, want)
	}
}

func TestKeyPathFromPointer(t *testing.T) {
	for _, p := range []KeyPath{{"a"}, {"podLabels", "app.kubernetes.io/name", "a~b", "0"}, {"x", "y"}} {
		if got := KeyPathFromPointer(p.Pointer()); !reflect.DeepEqual(got, p) {
			t.Errorf("KeyPathFromPointer(%q) = %q, want %q", p.Pointer(), got, p)
		}
	}
	if KeyPathFromPointer("") != nil {
		t.Error("the empty pointer is not a key")
	}
}
