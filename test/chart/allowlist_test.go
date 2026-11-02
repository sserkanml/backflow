package chart

import (
	"slices"
	"testing"
)

func TestAllowlistsReachTheOperator(t *testing.T) {
	flag := func(args []string, prefix string) []string {
		var out []string
		for _, a := range args {
			if len(a) > len(prefix) && a[:len(prefix)] == prefix {
				out = append(out, a[len(prefix):])
			}
		}
		return out
	}
	deployment := func(extra ...string) []string {
		args, _ := managerArgs(t, renderTemplates(t, []string{"templates/deployment.yaml"}, with(extra...)...))
		return args
	}

	t.Run("gitlab.com and github.com are allowed by default, no other Argo CD server", func(t *testing.T) {
		args := deployment()
		if got := flag(args, "--scm-allowed-hosts="); !slices.Equal(got, []string{"gitlab.com", "github.com"}) {
			t.Errorf("--scm-allowed-hosts = %v", got)
		}
		if got := flag(args, "--argocd-allowed-servers="); len(got) != 0 {
			t.Errorf("--argocd-allowed-servers = %v, want none", got)
		}
	})
	t.Run("the values are the flags", func(t *testing.T) {
		args := deployment(
			"--set", "argocd.allowedServers={https://argocd.team-a.example.com,https://*.argo.corp:8443}",
			"--set", "scm.allowedHosts={git.example.com,*.corp.example.com}")
		wantServers := []string{"https://argocd.team-a.example.com", "https://*.argo.corp:8443"}
		if got := flag(args, "--argocd-allowed-servers="); !slices.Equal(got, wantServers) {
			t.Errorf("--argocd-allowed-servers = %v", got)
		}
		if got := flag(args, "--scm-allowed-hosts="); !slices.Equal(got, []string{"git.example.com", "*.corp.example.com"}) {
			t.Errorf("--scm-allowed-hosts = %v", got)
		}
	})
	t.Run("an empty list allows no Git host", func(t *testing.T) {
		if got := flag(deployment("--set", "scm.allowedHosts={}"), "--scm-allowed-hosts="); len(got) != 0 {
			t.Errorf("--scm-allowed-hosts = %v, want none", got)
		}
	})
}
