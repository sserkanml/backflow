package allowlist

import "testing"

func TestHosts(t *testing.T) {
	list := Hosts{"gitlab.com", "GitHub.com", "*.corp.example.com", ""}
	tests := []struct {
		host string
		want bool
	}{
		{"gitlab.com", true},
		{"GITLAB.COM", true},
		{"gitlab.com.", true},
		{"github.com", true},
		{"git.corp.example.com", true},
		{"a.b.corp.example.com", true},
		{"corp.example.com", false},
		{"evilcorp.example.com", false},
		{"gitlab.com.evil.io", false},
		{"notgitlab.com", false},
		{"", false},
		{"169.254.169.254", false},
		{"localhost", false},
	}
	for _, tt := range tests {
		if got := list.Allows(tt.host); got != tt.want {
			t.Errorf("Allows(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
	if (Hosts{}).Allows("gitlab.com") || (Hosts(nil)).Allows("gitlab.com") {
		t.Error("an empty list must allow nothing")
	}
}

func TestHostsAllowsURL(t *testing.T) {
	list := Hosts{"gitlab.com"}
	for raw, want := range map[string]bool{
		"https://gitlab.com":                  true,
		"https://gitlab.com:8443/x/y":         true,
		"https://GitLab.com/api/v4":           true,
		"https://user:pw@gitlab.com/x":        true,
		"https://gitlab.com@evil.io/":         false,
		"https://evil.io/gitlab.com":          false,
		"https://gitlab.com.evil.io":          false,
		"gitlab.com":                          false,
		"":                                    false,
		"http://[::1]:80/":                    false,
		"https://169.254.169.254/latest/meta": false,
	} {
		if got := list.AllowsURL(raw); got != want {
			t.Errorf("AllowsURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestServers(t *testing.T) {
	list := Servers{"https://argocd.example.com", "http://localhost:8080", "https://*.argo.corp:8443", "ftp://nope", "not a url"}
	tests := []struct {
		url  string
		want bool
	}{
		{"https://argocd.example.com", true},
		{"https://argocd.example.com/", true},
		{"https://argocd.example.com:443", true},
		{"https://ARGOCD.example.com/api/v1", true},
		{"https://argocd.example.com:8443", false},
		{"http://argocd.example.com", false},
		{"http://localhost:8080", true},
		{"http://localhost:9090", false},
		{"http://localhost", false},
		{"https://localhost:8080", false},
		{"https://prod.argo.corp:8443", true},
		{"https://argo.corp:8443", false},
		{"https://prod.argo.corp", false},
		{"https://evil.io", false},
		{"https://argocd.example.com.evil.io", false},
		{"https://169.254.169.254", false},
		{"ftp://nope", false},
		{"argocd.example.com", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := list.Allows(tt.url); got != tt.want {
			t.Errorf("Allows(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
	if (Servers{}).Allows("https://argocd.example.com") {
		t.Error("an empty list must allow nothing")
	}
}
