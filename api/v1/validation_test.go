package v1_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	k8s client.Client
	ctx = context.Background()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("KUBEBUILDER_ASSETS is not set: skipping the CRD validation tests")
		os.Exit(0)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		fmt.Println("cannot start envtest:", err)
		os.Exit(1)
	}
	k8s, err = client.New(cfg, client.Options{Scheme: runtime.NewScheme()})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

type obj = map[string]any

func newObject(group, version, kind string, spec obj) *unstructured.Unstructured {
	// ints become float64 here, which Unstructured can copy
	raw, _ := json.Marshal(spec)
	var plain obj
	_ = json.Unmarshal(raw, &plain)
	u := &unstructured.Unstructured{Object: obj{"spec": plain}}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
	u.SetNamespace("default")
	u.SetGenerateName("v-")
	return u
}

// check creates the object as a dry run (nothing is stored) and says whether the API server accepted it.
// The returned object holds the defaults the server filled in.
func check(t *testing.T, u *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	t.Helper()
	c := u.DeepCopy()
	err := k8s.Create(ctx, c, client.DryRunAll)
	return c, err
}

type tc struct {
	name   string
	mutate func(spec obj)
	// bad is a part of the error message; empty means the object must be accepted.
	bad string
}

func run(t *testing.T, group, kind string, base func() obj, cases []tc) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := base()
			if c.mutate != nil {
				c.mutate(spec)
			}
			version := "v1"
			if group == "internal.backflow.io" {
				version = "v1alpha1"
			}
			_, err := check(t, newObject(group, version, kind, spec))
			switch {
			case c.bad == "" && err != nil:
				t.Fatalf("a valid %s was refused: %v", kind, err)
			case c.bad != "" && err == nil:
				t.Fatalf("an invalid %s was accepted", kind)
			case c.bad != "" && !strings.Contains(err.Error(), c.bad):
				t.Fatalf("refused for the wrong reason: %v (want %q)", err, c.bad)
			}
		})
	}
}

// plain makes a value Unstructured can copy: ints become float64.
func plain(v obj) obj {
	raw, _ := json.Marshal(v)
	var out obj
	_ = json.Unmarshal(raw, &out)
	return out
}

func many(n int, f func(i int) any) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = f(i)
	}
	return out
}

func secretRef() obj { return obj{"name": "tok", "key": "token"} }

func TestScmConnection(t *testing.T) {
	base := func() obj {
		return obj{"provider": "GitLab", "url": "https://gitlab.com", "tokenSecretRef": secretRef()}
	}
	run(t, "backflow.io", "ScmConnection", base, []tc{
		{"minimal", nil, ""},
		{"all fields", func(s obj) {
			s["hosts"] = []any{"gitlab.com", "git.example.com"}
			s["caSecretRef"] = secretRef()
			s["committer"] = obj{"name": "Backflow", "email": "bf@example.com"}
		}, ""},
		{"lower case provider", func(s obj) { s["provider"] = "gitlab" }, "Unsupported value"},
		{"unknown provider", func(s obj) { s["provider"] = "Gitea" }, "Unsupported value"},
		{"url without a scheme", func(s obj) { s["url"] = "gitlab.com" }, "should match"},
		{"url with another scheme", func(s obj) { s["url"] = "ftp://gitlab.com" }, "should match"},
		{"url with a space", func(s obj) { s["url"] = "https://git lab.com" }, "should match"},
		{"url too long", func(s obj) { s["url"] = "https://" + strings.Repeat("a", 2050) }, "may not be more than 2048"},
		{"host with a bad first character", func(s obj) { s["hosts"] = []any{"-gitlab.com"} }, "should match"},
		{"host with a path", func(s obj) { s["hosts"] = []any{"gitlab.com/x"} }, "should match"},
		{"17 hosts", func(s obj) { s["hosts"] = many(17, func(i int) any { return fmt.Sprintf("h%d.example.com", i) }) }, "must have at most 16 items"},
		{"duplicate hosts", func(s obj) { s["hosts"] = []any{"a.com", "a.com"} }, "Duplicate value"},
		{"token secret without a key", func(s obj) { s["tokenSecretRef"] = obj{"name": "tok"} }, "key"},
		{"token secret with an empty name", func(s obj) { s["tokenSecretRef"] = obj{"name": "", "key": "token"} }, "at least 1"},
		{"no token secret", func(s obj) { delete(s, "tokenSecretRef") }, "tokenSecretRef"},
		{"committer without a name", func(s obj) { s["committer"] = obj{"name": "", "email": "a@b.co"} }, "at least 1"},
		{"committer with a bad email", func(s obj) { s["committer"] = obj{"name": "x", "email": "nope"} }, "email"},
	})
}

func TestBackflowPolicySpec(t *testing.T) {
	base := func() obj { return obj{"applications": obj{"names": []any{"demo-app"}}} }
	ptrs := func(s ...string) []any {
		out := make([]any, len(s))
		for i, v := range s {
			out[i] = v
		}
		return out
	}
	run(t, "backflow.io", "BackflowPolicy", base, []tc{
		{"minimal", nil, ""},
		{"selector only", func(s obj) { s["applications"] = obj{"selector": obj{"matchLabels": obj{"env": "test"}}} }, ""},
		{"names and selector", func(s obj) {
			s["applications"] = obj{"names": []any{"a"}, "selector": obj{"matchLabels": obj{"env": "test"}}}
		}, ""},
		{"no application selection", func(s obj) { s["applications"] = obj{} }, "set names, selector, or both"},
		{"empty names and no selector", func(s obj) { s["applications"] = obj{"names": []any{}} }, "set names, selector, or both"},
		{"101 names", func(s obj) {
			s["applications"] = obj{"names": many(101, func(i int) any { return fmt.Sprintf("app-%d", i) })}
		}, "must have at most 100 items"},
		{"an empty name", func(s obj) { s["applications"] = obj{"names": []any{""}} }, "at least 1"},
		{"duplicate names", func(s obj) { s["applications"] = obj{"names": []any{"a", "a"}} }, "Duplicate value"},
		{"mode ReportOnly", func(s obj) { s["mode"] = "ReportOnly" }, ""},
		{"unknown mode", func(s obj) { s["mode"] = "Merge" }, "Unsupported value"},
		{"batchWindow 0s", func(s obj) { s["batchWindow"] = "0s" }, ""},
		{"batchWindow 24h", func(s obj) { s["batchWindow"] = "24h" }, ""},
		{"batchWindow over 24h", func(s obj) { s["batchWindow"] = "25h" }, "between 0s and 24h"},
		{"batchWindow negative", func(s obj) { s["batchWindow"] = "-1s" }, "between 0s and 24h"},
		{"batchWindow not a duration", func(s obj) { s["batchWindow"] = "soon" }, "duration"},
		{"batchWindow a number", func(s obj) { s["batchWindow"] = 30 }, "must be of type duration"},
		{"retention 0s keeps for ever", func(s obj) { s["retention"] = obj{"finishedProposals": "0s"} }, ""},
		{"retention a year", func(s obj) { s["retention"] = obj{"finishedProposals": "8760h"} }, ""},
		{"retention over a year", func(s obj) { s["retention"] = obj{"finishedProposals": "8761h"} }, "between 0s and 8760h"},
		{"retention negative", func(s obj) { s["retention"] = obj{"finishedProposals": "-1h"} }, "between 0s and 8760h"},
		{"include and exclude", func(s obj) {
			s["include"] = []any{obj{"group": "apps", "kind": "Deployment"}, obj{"kind": "ConfigMap"}}
			s["exclude"] = []any{obj{"group": "*", "kind": "*"}}
		}, ""},
		{"include without a kind", func(s obj) { s["include"] = []any{obj{"group": "apps"}} }, "kind"},
		{"include with an empty kind", func(s obj) { s["include"] = []any{obj{"kind": ""}} }, "at least 1"},
		{"65 excludes", func(s obj) { s["exclude"] = many(65, func(i int) any { return obj{"kind": fmt.Sprintf("K%d", i)} }) }, "must have at most 64 items"},
		{"ignoreFields", func(s obj) {
			s["ignoreFields"] = []any{obj{"group": "apps", "kind": "Deployment", "name": "demo", "jsonPointers": ptrs("/spec/replicas")}}
		}, ""},
		{"ignoreFields pointer without a slash", func(s obj) {
			s["ignoreFields"] = []any{obj{"kind": "Deployment", "jsonPointers": ptrs("spec/replicas")}}
		}, "should match"},
		{"ignoreFields without pointers", func(s obj) {
			s["ignoreFields"] = []any{obj{"kind": "Deployment", "jsonPointers": []any{}}}
		}, "at least 1"},
		{"ignoreFields 33 pointers", func(s obj) {
			s["ignoreFields"] = []any{obj{"kind": "Deployment", "jsonPointers": many(33, func(i int) any { return fmt.Sprintf("/spec/f%d", i) })}}
		}, "must have at most 32 items"},
		{"argoCD server", func(s obj) {
			s["argoCD"] = obj{"namespace": "openshift-gitops", "server": obj{
				"url": "https://argocd.example.com", "tokenSecretRef": secretRef(), "caSecretRef": secretRef(), "insecureSkipTLSVerify": true}}
		}, ""},
		{"argoCD server url without a scheme", func(s obj) {
			s["argoCD"] = obj{"server": obj{"url": "argocd.example.com", "tokenSecretRef": secretRef()}}
		}, "should match"},
		{"argoCD server without a token", func(s obj) { s["argoCD"] = obj{"server": obj{"url": "https://a.example.com"}} }, "tokenSecretRef"},
		{"argoCD namespace too long", func(s obj) { s["argoCD"] = obj{"namespace": strings.Repeat("n", 64)} }, "may not be more than 63"},
		{"helm options", func(s obj) {
			s["helm"] = obj{"mappingMode": "ExplicitOnly", "targetValuesFile": "values-prod.yaml", "allowApplicationOverride": true,
				"allowValueOverrides": false, "allowCollateralChanges": []any{"Labels", "Annotations"},
				"mappings": []any{obj{"kind": "Deployment", "fieldPointer": "/spec/replicas", "valuesPath": "api.replicaCount"}}}
		}, ""},
		{"helm mappingMode unknown", func(s obj) { s["helm"] = obj{"mappingMode": "Manual"} }, "Unsupported value"},
		{"helm collateral unknown", func(s obj) { s["helm"] = obj{"allowCollateralChanges": []any{"Selectors"}} }, "Unsupported value"},
		{"helm collateral duplicate", func(s obj) { s["helm"] = obj{"allowCollateralChanges": []any{"Labels", "Labels"}} }, "Duplicate value"},
		{"helm mapping pointer without a slash", func(s obj) {
			s["helm"] = obj{"mappings": []any{obj{"kind": "Deployment", "fieldPointer": "spec/replicas", "valuesPath": "a"}}}
		}, "should match"},
		{"helm mapping without a values path", func(s obj) {
			s["helm"] = obj{"mappings": []any{obj{"kind": "Deployment", "fieldPointer": "/spec/replicas", "valuesPath": ""}}}
		}, "at least 1"},
		{"65 helm mappings", func(s obj) {
			s["helm"] = obj{"mappings": many(65, func(i int) any {
				return obj{"kind": "Deployment", "fieldPointer": "/spec/replicas", "valuesPath": fmt.Sprintf("v%d", i)}
			})}
		}, "must have at most 64 items"},
		{"kustomize options", func(s obj) { s["kustomize"] = obj{"allowBaseEdits": true, "allowNewPatches": false} }, ""},
		{"mapping options", func(s obj) { s["mapping"] = obj{"allowPartial": true, "scratchBranches": "Never"} }, ""},
		{"scratchBranches unknown", func(s obj) { s["mapping"] = obj{"scratchBranches": "Sometimes"} }, "Unsupported value"},
		{"git options", func(s obj) {
			s["git"] = obj{"branchPrefix": "bf/", "targetBranch": "main", "mergeRequest": obj{
				"labels": ptrs("backflow"), "reviewers": ptrs("alice", "bob"), "assignActor": false}}
		}, ""},
		{"17 labels", func(s obj) {
			s["git"] = obj{"mergeRequest": obj{"labels": many(17, func(i int) any { return fmt.Sprintf("l%d", i) })}}
		}, "must have at most 16 items"},
		{"an empty reviewer", func(s obj) { s["git"] = obj{"mergeRequest": obj{"reviewers": ptrs("")}} }, "at least 1"},
		{"duplicate reviewers", func(s obj) { s["git"] = obj{"mergeRequest": obj{"reviewers": ptrs("a", "a")}} }, "Duplicate value"},
		{"branchPrefix too long", func(s obj) { s["git"] = obj{"branchPrefix": strings.Repeat("b", 65)} }, "may not be more than 64"},
		{"the old argoCDNamespace is pruned, not an error", func(s obj) { s["argoCDNamespace"] = "argocd" }, ""},
	})
}

func TestBackflowPolicyDefaults(t *testing.T) {
	get := func(u *unstructured.Unstructured, path ...string) any {
		v, _, _ := unstructured.NestedFieldNoCopy(u.Object, path...)
		return v
	}
	t.Run("a minimal policy gets every default, nested ones included", func(t *testing.T) {
		u, err := check(t, newObject("backflow.io", "v1", "BackflowPolicy", obj{"applications": obj{"names": []any{"a"}}, "kustomize": obj{}, "helm": obj{}}))
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"spec.mode": "MergeRequest", "spec.batchWindow": "30s", "spec.argoCD.namespace": "argocd",
			"spec.retention.finishedProposals": "336h", "spec.git.branchPrefix": "backflow/", "spec.git.mergeRequest.assignActor": true,
			"spec.kustomize.allowNewPatches": true, "spec.kustomize.allowBaseEdits": false,
			"spec.helm.allowValueOverrides": true, "spec.helm.mappingMode": "Auto", "spec.helm.allowApplicationOverride": false,
		}
		for path, w := range want {
			if got := get(u, strings.Split(path, ".")...); got != w {
				t.Errorf("%s = %v (%T), want %v", path, got, got, w)
			}
		}
	})
	t.Run("a false that was set is kept", func(t *testing.T) {
		u, err := check(t, newObject("backflow.io", "v1", "BackflowPolicy", obj{
			"applications": obj{"names": []any{"a"}}, "git": obj{"mergeRequest": obj{"assignActor": false}}, "kustomize": obj{"allowNewPatches": false}, "helm": obj{"allowValueOverrides": false}}))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"spec.git.mergeRequest.assignActor", "spec.kustomize.allowNewPatches", "spec.helm.allowValueOverrides"} {
			if got := get(u, strings.Split(p, ".")...); got != false {
				t.Errorf("%s = %v, want false", p, got)
			}
		}
	})
	t.Run("an explicit 0s is kept", func(t *testing.T) {
		u, err := check(t, newObject("backflow.io", "v1", "BackflowPolicy", obj{
			"applications": obj{"names": []any{"a"}}, "batchWindow": "0s", "retention": obj{"finishedProposals": "0s"}}))
		if err != nil {
			t.Fatal(err)
		}
		if get(u, "spec", "batchWindow") != "0s" || get(u, "spec", "retention", "finishedProposals") != "0s" {
			t.Errorf("0s was replaced: %v", u.Object["spec"])
		}
	})
}

func change(op string, extra obj) obj {
	c := obj{"path": "/spec/replicas", "op": op}
	maps.Copy(c, extra)
	return c
}

func proposalBase() obj {
	return obj{
		"policyRef":      obj{"name": "p"},
		"applicationRef": obj{"name": "demo-app", "namespace": "argocd"},
		"resourceRef":    obj{"group": "apps", "version": "v1", "kind": "Deployment", "namespace": "demo", "name": "demo"},
		"sources": []any{obj{"position": 1, "role": "Render", "repoURL": "https://gitlab.com/x/y.git", "revision": "abc",
			"targetRevision": "main", "path": "apps/demo", "type": "Directory"}},
		"changes":    []any{change("Replace", obj{"desired": "1", "live": "2"})},
		"detectedAt": "2026-11-02T09:02:03Z",
	}
}

func TestDriftProposalSpec(t *testing.T) {
	changes := func(n int) []any {
		return many(n, func(i int) any {
			return obj{"path": fmt.Sprintf("/data/k%d", i), "op": "Replace", "desired": `"a"`, "live": `"b"`}
		})
	}
	run(t, "backflow.io", "DriftProposal", proposalBase, []tc{
		{"minimal", nil, ""},
		{"add, remove, replace", func(s obj) {
			s["changes"] = []any{change("Add", obj{"live": "3"}), change("Remove", obj{"desired": "1"}), change("Replace", obj{"desired": "1", "live": "2"})}
		}, ""},
		{"256 changes", func(s obj) { s["changes"] = changes(256) }, ""},
		{"257 changes", func(s obj) { s["changes"] = changes(257) }, "must have at most 256 items"},
		{"truncated", func(s obj) { s["changes"] = changes(256); s["truncated"] = true }, ""},
		{"no changes", func(s obj) { s["changes"] = []any{} }, "at least 1"},
		{"an Add with a desired value", func(s obj) { s["changes"] = []any{change("Add", obj{"desired": "1", "live": "2"})} }, "an Add has no desired value"},
		{"a Remove with a live value", func(s obj) { s["changes"] = []any{change("Remove", obj{"desired": "1", "live": "2"})} }, "a Remove has no live value"},
		{"unknown operation", func(s obj) { s["changes"] = []any{change("Patch", nil)} }, "Unsupported value"},
		{"path without a slash", func(s obj) { s["changes"] = []any{obj{"path": "spec/replicas", "op": "Replace"}} }, "should match"},
		{"a value over 16 KiB", func(s obj) {
			s["changes"] = []any{change("Replace", obj{"desired": strings.Repeat("x", 16385), "live": "1"})}
		}, "may not be more than 16384"},
		{"no sources", func(s obj) { s["sources"] = []any{} }, "at least 1"},
		{"17 sources", func(s obj) {
			s["sources"] = many(17, func(i int) any {
				return obj{"position": i + 1, "role": "Reference", "repoURL": "https://x.example/r.git", "revision": "abc"}
			})
		}, "must have at most 16 items"},
		{"a Plugin source", func(s obj) {
			s["sources"] = []any{obj{"position": 1, "role": "Render", "repoURL": "https://x.example/r.git", "revision": "abc", "type": "Plugin"}}
		}, ""},
		{"unknown source role", func(s obj) {
			s["sources"] = []any{obj{"position": 1, "role": "Source", "repoURL": "https://x.example/r.git", "revision": "abc"}}
		}, "Unsupported value"},
		{"unknown source type", func(s obj) {
			s["sources"] = []any{obj{"position": 1, "role": "Render", "repoURL": "https://x.example/r.git", "revision": "abc", "type": "Jsonnet"}}
		}, "Unsupported value"},
		{"position 0", func(s obj) {
			s["sources"] = []any{obj{"position": 0, "role": "Render", "repoURL": "https://x.example/r.git", "revision": "abc"}}
		}, "greater than or equal to 1"},
		{"actor", func(s obj) { s["actor"] = obj{"kind": "Person", "username": "alice", "groups": []any{"devs"}} }, ""},
		{"actor kind in lower case", func(s obj) { s["actor"] = obj{"kind": "person"} }, "Unsupported value"},
		{"actor with 17 groups", func(s obj) {
			s["actor"] = obj{"groups": many(17, func(i int) any { return fmt.Sprintf("g%d", i) })}
		}, "must have at most 16 items"},
		{"the removed userAgent is pruned, not an error", func(s obj) { s["actor"] = obj{"kind": "Person", "userAgent": "kubectl/1.35"} }, ""},
		{"managers", func(s obj) {
			s["managers"] = []any{obj{"manager": "kubectl-patch", "operation": "Update", "time": "2026-11-02T09:01:58Z", "fields": []any{"/spec/replicas"}}}
		}, ""},
		{"a manager without a time", func(s obj) { s["managers"] = []any{obj{"manager": "kubectl", "operation": "Update"}} }, ""},
		{"6 managers", func(s obj) {
			s["managers"] = many(6, func(i int) any { return obj{"manager": fmt.Sprintf("m%d", i)} })
		}, "must have at most 5 items"},
		{"a manager operation unknown", func(s obj) { s["managers"] = []any{obj{"manager": "m", "operation": "Patch"}} }, "Unsupported value"},
		{"a manager without a name", func(s obj) { s["managers"] = []any{obj{"manager": ""}} }, "at least 1"},
		{"no policyRef", func(s obj) { delete(s, "policyRef") }, "policyRef"},
		{"application without a namespace", func(s obj) { s["applicationRef"] = obj{"name": "a"} }, "namespace"},
		{"the old field names are not accepted", func(s obj) {
			delete(s, "policyRef")
			s["policyName"] = "p"
		}, "policyRef"},
	})
}

func TestDriftProposalIsImmutableAndStatusIsSeparate(t *testing.T) {
	u := newObject("backflow.io", "v1", "DriftProposal", proposalBase())
	if err := k8s.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, u) })

	t.Run("an unchanged spec can be written again", func(t *testing.T) {
		c := u.DeepCopy()
		c.SetLabels(map[string]string{"a": "b"})
		if err := k8s.Update(ctx, c); err != nil {
			t.Fatalf("a metadata change was refused: %v", err)
		}
	})
	t.Run("a changed spec is refused", func(t *testing.T) {
		c := u.DeepCopy()
		_ = k8s.Get(ctx, client.ObjectKeyFromObject(u), c)
		_ = unstructured.SetNestedField(c.Object, "other", "spec", "policyRef", "name")
		err := k8s.Update(ctx, c)
		if err == nil || !strings.Contains(err.Error(), "spec is immutable") {
			t.Fatalf("a spec change was not refused: %v", err)
		}
	})
	t.Run("status is written through the subresource", func(t *testing.T) {
		c := u.DeepCopy()
		_ = k8s.Get(ctx, client.ObjectKeyFromObject(u), c)
		st := obj{
			"phase": "Proposed", "message": "Merge request !12 is open.", "finishedAt": "2026-11-02T10:00:00Z",
			"mapping":    obj{"strategy": "Direct", "verified": true, "edits": []any{obj{"file": "a.yaml", "location": "/spec/replicas", "value": "2", "changeIndex": 0}}},
			"git":        obj{"branch": "backflow/x", "mergeRequest": obj{"url": "https://gitlab.com/x/-/merge_requests/12", "number": 12, "state": "opened"}, "push": obj{"rejections": 1}},
			"signals":    obj{"supersededBy": "other", "reverted": "gone", "liveRevertedAtRevision": "abc", "accessRevoked": true, "rejectionReported": true},
			"conditions": []any{obj{"type": "Mapped", "status": "True", "reason": "Mapped", "message": "ok", "lastTransitionTime": "2026-11-02T09:02:10Z"}},
		}
		_ = unstructured.SetNestedField(c.Object, plain(st), "status")
		if err := k8s.Status().Update(ctx, c); err != nil {
			t.Fatalf("a valid status was refused: %v", err)
		}
	})
	t.Run("a bad status is refused", func(t *testing.T) {
		for name, st := range map[string]obj{
			"unknown phase":          {"phase": "Done"},
			"unknown strategy":       {"mapping": obj{"strategy": "Unmapped"}},
			"61 edits":               {"mapping": obj{"edits": many(65, func(i int) any { return obj{"file": "a", "location": "/a", "value": "1", "changeIndex": 0} })}},
			"diff over 32 KiB":       {"mapping": obj{"diff": strings.Repeat("d", 32769)}},
			"an unmapped reason":     {"mapping": obj{"unmapped": []any{obj{"path": "/a", "reason": "lower case", "message": "m"}}}},
			"unknown parent state":   {"mapping": obj{"applicationOption": obj{"applicationRef": obj{"name": "a", "namespace": "n"}, "option": "o", "parent": obj{"state": "Lost"}}}},
			"condition without type": {"conditions": []any{obj{"status": "True", "reason": "R", "message": "m", "lastTransitionTime": "2026-11-02T09:02:10Z"}}},
			"duplicate condition": {"conditions": []any{
				obj{"type": "Mapped", "status": "True", "reason": "R", "message": "m", "lastTransitionTime": "2026-11-02T09:02:10Z"},
				obj{"type": "Mapped", "status": "False", "reason": "R", "message": "m", "lastTransitionTime": "2026-11-02T09:02:10Z"}}},
		} {
			c := u.DeepCopy()
			_ = k8s.Get(ctx, client.ObjectKeyFromObject(u), c)
			_ = unstructured.SetNestedField(c.Object, plain(st), "status")
			if err := k8s.Status().Update(ctx, c); err == nil || !apierrors.IsInvalid(err) {
				t.Errorf("%s: want an Invalid error, got %v", name, err)
			}
		}
	})
}

func TestInternalChangeRecord(t *testing.T) {
	base := func() obj {
		return obj{"uid": "d791a0ca-00e3-4c2e-9033-fe93e366ebd1", "group": "apps", "resource": "deployments", "subresource": "scale",
			"namespace": "demo", "name": "demo", "actorKind": "Person", "username": "alice", "groups": []any{"devs"},
			"paths": []any{"/spec/replicas"}, "firstSeen": "2026-11-02T09:01:58.231904Z", "lastSeen": "2026-11-02T09:01:58.231904Z", "count": 1}
	}
	run(t, "internal.backflow.io", "ChangeRecord", base, []tc{
		{"a record", nil, ""},
		{"a record of the resource itself", func(s obj) { delete(s, "subresource") }, ""},
		{"no names recorded", func(s obj) { delete(s, "username"); delete(s, "groups") }, ""},
		{"actor kind in lower case", func(s obj) { s["actorKind"] = "person" }, "Unsupported value"},
		{"no actor kind", func(s obj) { delete(s, "actorKind") }, "actorKind"},
		{"65 paths", func(s obj) { s["paths"] = many(65, func(i int) any { return fmt.Sprintf("/f%d", i) }) }, "must have at most 64 items"},
		{"a path over 1024", func(s obj) { s["paths"] = []any{"/" + strings.Repeat("p", 1024)} }, "may not be more than 1024"},
		{"17 groups", func(s obj) { s["groups"] = many(17, func(i int) any { return fmt.Sprintf("g%d", i) }) }, "must have at most 16 items"},
		{"no uid", func(s obj) { s["uid"] = "" }, "at least 1"},
		{"times need a date", func(s obj) { s["lastSeen"] = "yesterday" }, "lastSeen"},
	})
	t.Run("the old group no longer has the kind", func(t *testing.T) {
		u := newObject("backflow.io", "v1", "ChangeRecord", base())
		if err := k8s.Create(ctx, u, client.DryRunAll); err == nil {
			t.Fatal("backflow.io has a ChangeRecord")
		}
	})
}
