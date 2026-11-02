package chart

import (
	"slices"
	"strings"
	"testing"
)

const migrationTemplate = "templates/migration.yaml"

// withClaim and withEmptyDir are the values that turn the migration hook on.
var (
	withClaim    = []string{"--set", "migration.enabled=true", "--set", "migration.backup.existingClaim=bk"}
	withEmptyDir = []string{"--set", "migration.enabled=true", "--set", "migration.backup.emptyDir=true"}
)

func TestMigrationHookIsOffByDefault(t *testing.T) {
	if objs := renderTemplates(t, []string{migrationTemplate}); len(objs) != 0 {
		t.Errorf("the migration renders %d objects without migration.enabled", len(objs))
	}
}

func TestMigrationHookNeedsABackupPlace(t *testing.T) {
	renderFails(t, migrationTemplate, "migration.backup.existingClaim", "--set", "migration.enabled=true")
	both := append(slices.Clone(withClaim), "--set", "migration.backup.emptyDir=true")
	renderFails(t, migrationTemplate, "not both", both...)
}

// jobs returns the Jobs of the migration by name.
func jobs(t *testing.T, args ...string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, o := range renderTemplates(t, []string{migrationTemplate}, args...) {
		if o["kind"] == "Job" {
			out[o["metadata"].(map[string]any)["name"].(string)] = o
		}
	}
	return out
}

func podSpec(job map[string]any) map[string]any {
	return job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
}

func jobArgs(job map[string]any) []string {
	raw := podSpec(job)["containers"].([]any)[0].(map[string]any)["args"].([]any)
	out := make([]string, 0, len(raw))
	for _, a := range raw {
		out = append(out, a.(string))
	}
	return out
}

func flagValue(args []string, name string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}

func TestMigrationHookStopsTheOperatorBeforeAndRestoresItAfter(t *testing.T) {
	args := append(slices.Clone(withClaim), "--set", "migration.extraArgs={--skip-finished}")
	all := jobs(t, args...)
	pre, post := all["backflow-migrate-pre"], all["backflow-migrate-post"]
	if pre == nil || post == nil || len(all) != 2 {
		t.Fatalf("jobs = %v", all)
	}
	hook := func(o map[string]any) any {
		return o["metadata"].(map[string]any)["annotations"].(map[string]any)["helm.sh/hook"]
	}
	if hook(pre) != "pre-upgrade" || hook(post) != "post-upgrade" {
		t.Errorf("hooks: %v %v", hook(pre), hook(post))
	}
	preArgs, postArgs := jobArgs(pre), jobArgs(post)
	for _, want := range []string{"migrate", "--backup-dir=/backup", "--stop-operator", "--operator-namespace=backflow-system",
		"--skip-finished"} {
		if !slices.Contains(preArgs, want) {
			t.Errorf("pre-upgrade args %v lack %s", preArgs, want)
		}
	}
	if !slices.Contains(postArgs, "--restore-operator") || slices.Contains(postArgs, "--stop-operator") ||
		slices.Contains(postArgs, "--backup-dir=/backup") {
		t.Errorf("post-upgrade args = %v", postArgs)
	}
	// The selector is the one of the Deployment, which the chart labels.
	sel := flagValue(preArgs, "--operator-selector")
	for _, want := range []string{"app.kubernetes.io/instance=backflow", "app.kubernetes.io/name=backflow"} {
		if !strings.Contains(sel, want) {
			t.Errorf("selector = %q lacks %s", sel, want)
		}
	}
}

func TestMigrationHookBackupVolume(t *testing.T) {
	volume := func(args []string) map[string]any {
		return podSpec(jobs(t, args...)["backflow-migrate-pre"])["volumes"].([]any)[0].(map[string]any)
	}
	if v := volume(withClaim); v["persistentVolumeClaim"] == nil {
		t.Errorf("volume = %v, want the claim", v)
	}
	if v := volume(withEmptyDir); v["emptyDir"] == nil {
		t.Errorf("volume = %v, want an emptyDir", v)
	}
}

func TestMigrationHookRBACIsRemovedWithTheHooks(t *testing.T) {
	for _, o := range renderTemplates(t, []string{migrationTemplate}, withEmptyDir...) {
		meta := o["metadata"].(map[string]any)
		ann := meta["annotations"].(map[string]any)
		policy, _ := ann["helm.sh/hook-delete-policy"].(string)
		if ann["helm.sh/hook"] == nil || !strings.Contains(policy, "hook-succeeded") {
			t.Errorf("%s %v: the permissions to rewrite CRDs must not outlive the hook", o["kind"], meta["name"])
		}
	}
}

func TestMigrationHookSelectorFollowsNameOverride(t *testing.T) {
	all := jobs(t, append(slices.Clone(withEmptyDir), "--set", "nameOverride=bf")...)
	sel := flagValue(jobArgs(all["backflow-bf-migrate-pre"]), "--operator-selector")
	if !strings.Contains(sel, "app.kubernetes.io/name=bf") {
		t.Errorf("selector = %q (jobs %v)", sel, all)
	}
}
