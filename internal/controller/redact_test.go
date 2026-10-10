package controller

import "testing"

func TestRedactText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://oauth2:glpat-secret@gitlab.com/x/y.git", "https://REDACTED@gitlab.com/x/y.git"},
		{"https://token@github.com/o/r", "https://REDACTED@github.com/o/r"},
		{`Get "https://u:pw@git.example.com/x/info/refs": dial tcp: refused`, `Get "https://REDACTED@git.example.com/x/info/refs": dial tcp: refused`},
		{"two https://a:b@h1/x and http://c@h2/y", "two https://REDACTED@h1/x and http://REDACTED@h2/y"},
		{"https://gitlab.com/x/y.git", "https://gitlab.com/x/y.git"},
		{"mail me@example.com, not a URL", "mail me@example.com, not a URL"},
		{"a path https://gitlab.com/x/@y/z is kept", "a path https://gitlab.com/x/@y/z is kept"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := redactText(tt.in); got != tt.want {
			t.Errorf("redactText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHasCredentials(t *testing.T) {
	for in, want := range map[string]bool{
		"https://gitlab.com/x/y.git":               false,
		"https://oauth2:glpat-secret@gitlab.com/x": true,
		"https://token@github.com/o/r":             true,
		"http://u:p@git.internal/x":                true,
		"ssh://git@gitlab.com/x/y.git":             false,
		"git@github.com:o/r.git":                   false,
		"":                                         false,
	} {
		if got := hasCredentials(in); got != want {
			t.Errorf("hasCredentials(%q) = %v, want %v", in, got, want)
		}
	}
}
