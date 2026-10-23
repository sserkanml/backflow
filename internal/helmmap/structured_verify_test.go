package helmmap

import (
	"strings"
	"testing"

	"github.com/sserkanml/backflow/internal/rendermap"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

const structEnv = `# Production.
replicaCount: 2

podAnnotations:
  team: platform # who to call
`

func TestStructuredAddKeyToMapTheEnvironmentHas(t *testing.T) {
	c := newStructCase(t, structEnv)
	out := c.drift(func(o map[string]any) { podMeta(o)["annotations"].(map[string]any)["backflow.io/test"] = "1" }, rendermap.Options{})
	got := edited(t, out)
	if want := strings.Replace(structEnv, "  team: platform # who to call\n", "  team: platform # who to call\n  backflow.io/test: \"1\"\n", 1); got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
	if len(out.Overrides) != 0 {
		t.Errorf("a map the environment has is edited, not overridden: %+v", out.Overrides)
	}
	if e := out.Results[0].Edits[0]; e.Location != "/podAnnotations/backflow.io~1test" || e.Value != `"1"` {
		t.Errorf("edit = %+v", e)
	}
}

func TestStructuredRemoveKeyFromMap(t *testing.T) {
	c := newStructCase(t, structEnv+"  extra: \"1\"\n")
	out := c.drift(func(o map[string]any) { delete(podMeta(o)["annotations"].(map[string]any), "extra") }, rendermap.Options{})
	if got, want := edited(t, out), structEnv; got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
}

func TestStructuredEmptyDefaultBecomesOverride(t *testing.T) {
	const env = "replicaCount: 2\n"
	t.Run("a map that appears: the empty default is overridden in the last value file", func(t *testing.T) {
		c := newStructCase(t, env)
		out := c.drift(func(o map[string]any) { podMeta(o)["annotations"].(map[string]any)["a/b"] = "1" }, rendermap.Options{AddOverrides: true})
		if got, want := edited(t, out), env+"podAnnotations:\n  a/b: \"1\"\n"; got != want {
			t.Errorf("file =\n%s\nwant\n%s", got, want)
		}
		if len(out.Overrides) != 1 || out.Overrides[0].Path.String() != "podAnnotations.a/b" || out.Overrides[0].Default.File != "chart/values.yaml" ||
			out.Overrides[0].Default.Value != "{}" {
			t.Errorf("overrides = %+v", out.Overrides)
		}
	})
	t.Run("without overrides it stays unmapped", func(t *testing.T) {
		c := newStructCase(t, env)
		out := c.drift(func(o map[string]any) { podMeta(o)["annotations"].(map[string]any)["a/b"] = "1" }, rendermap.Options{})
		if out.Mapped() || out.Unmapped.Reason != rendermap.ReasonNoVerifiedCandidate {
			t.Fatalf("out = %+v", out)
		}
	})
	t.Run("a list that appears", func(t *testing.T) {
		c := newStructCase(t, env)
		tol := map[string]any{"key": "dedicated", "operator": "Equal", "value": "bf", "effect": "NoSchedule"}
		out := c.drift(func(o map[string]any) { podSpecOf(o)["tolerations"] = []any{tol} }, rendermap.Options{AddOverrides: true})
		mustContain(t, edited(t, out), "tolerations:\n  - effect: NoSchedule\n    key: dedicated\n    operator: Equal\n    value: bf\n")
	})
	t.Run("a map that appears whole where the key is a node selector", func(t *testing.T) {
		c := newStructCase(t, env)
		out := c.drift(func(o map[string]any) { podSpecOf(o)["nodeSelector"] = map[string]any{"disk": "ssd"} }, rendermap.Options{AddOverrides: true})
		mustContain(t, edited(t, out), "nodeSelector:\n  disk: ssd\n")
	})
}

func TestStructuredEnvList(t *testing.T) {
	envVar := func(name, value string) map[string]any { return map[string]any{"name": name, "value": value} }
	appendEnv := func(o map[string]any, e map[string]any) {
		c := containerOf(o)
		c["env"] = append(c["env"].([]any), e)
	}

	t.Run("an env var is added to the list that follows the generated ones", func(t *testing.T) {
		c := newStructCase(t, "replicaCount: 2\n")
		out := c.drift(func(o map[string]any) { appendEnv(o, envVar("EXTRA", "1")) }, rendermap.Options{AddOverrides: true})
		mustContain(t, edited(t, out), "extraEnvVars:\n  - name: EXTRA\n    value: \"1\"\n")
	})
	t.Run("one more is added to the entries the environment has", func(t *testing.T) {
		env := "extraEnvVars:\n  - name: A\n    value: \"1\" # first\n"
		c := newStructCase(t, env)
		out := c.drift(func(o map[string]any) { appendEnv(o, envVar("B", "true")) }, rendermap.Options{})
		if got := edited(t, out); !strings.HasPrefix(got, "extraEnvVars:\n  - name: A\n") ||
			!strings.Contains(got, "  - name: B\n    value: \"true\"\n") {
			t.Errorf("file =\n%s", got)
		}
		if len(out.Overrides) != 0 {
			t.Errorf("overrides = %+v", out.Overrides)
		}
	})
	t.Run("one is removed from them", func(t *testing.T) {
		env := "extraEnvVars:\n  - name: A\n    value: \"1\"\n  - name: B\n    value: \"2\"\n"
		c := newStructCase(t, env)
		out := c.drift(func(o map[string]any) {
			cc := containerOf(o)
			cc["env"] = cc["env"].([]any)[:2]
		}, rendermap.Options{})
		if got, want := edited(t, out), "extraEnvVars:\n  - name: A\n    value: \"1\"\n"; got != want {
			t.Errorf("file =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("the last one is removed: the list becomes empty", func(t *testing.T) {
		env := "replicaCount: 2\nextraEnvVars:\n  - name: A\n    value: \"1\"\n"
		c := newStructCase(t, env)
		out := c.drift(func(o map[string]any) {
			cc := containerOf(o)
			cc["env"] = cc["env"].([]any)[:1]
		}, rendermap.Options{})
		if got, want := edited(t, out), "replicaCount: 2\nextraEnvVars: []\n"; got != want {
			t.Errorf("file =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("an env var the chart generates cannot be changed from the values", func(t *testing.T) {
		c := newStructCase(t, "replicaCount: 2\n")
		out := c.drift(func(o map[string]any) { containerOf(o)["env"].([]any)[0].(map[string]any)["value"] = "changed" }, rendermap.Options{AddOverrides: true})
		if out.Mapped() {
			t.Fatalf("mapped: %+v", out.Results)
		}
	})
}

func TestStructuredAncestorOfAScalar(t *testing.T) {
	t.Run("a limit the preset generates: the resources key takes the whole live state", func(t *testing.T) {
		c := newStructCase(t, "replicaCount: 2\n")
		out := c.drift(func(o map[string]any) {
			containerOf(o)["resources"].(map[string]any)["limits"].(map[string]any)["memory"] = "1Gi"
		}, rendermap.Options{AddOverrides: true})
		mustContain(t, edited(t, out), "resources:\n  limits:\n    memory: 1Gi\n  requests:\n    memory: 256Mi\n")
		if len(out.Overrides) != 1 || out.Overrides[0].Path.String() != "resources" {
			t.Errorf("overrides = %+v", out.Overrides)
		}
	})
	t.Run("without overrides it stays unmapped", func(t *testing.T) {
		c := newStructCase(t, "replicaCount: 2\n")
		out := c.drift(func(o map[string]any) {
			containerOf(o)["resources"].(map[string]any)["limits"].(map[string]any)["memory"] = "1Gi"
		}, rendermap.Options{})
		if out.Mapped() {
			t.Fatalf("mapped: %+v", out.Results)
		}
	})
	t.Run("a key of its own wins over the ancestor", func(t *testing.T) {
		c := newStructCase(t, "port: 8080\nreplicaCount: 2\n")
		out := c.drift(func(o map[string]any) { o["spec"].(map[string]any)["replicas"] = float64(3) }, rendermap.Options{AddOverrides: true})
		if got, want := edited(t, out), "port: 8080\nreplicaCount: 3\n"; got != want {
			t.Errorf("file =\n%s", got)
		}
	})
}

func TestStructuredLimits(t *testing.T) {
	t.Run("two fields: a scalar and a map key, together", func(t *testing.T) {
		c := newStructCase(t, structEnv)
		out := c.drift(func(o map[string]any) {
			o["spec"].(map[string]any)["replicas"] = float64(3)
			podMeta(o)["annotations"].(map[string]any)["a/b"] = "1"
		}, rendermap.Options{})
		got := edited(t, out)
		mustContain(t, got, "replicaCount: 3\n", "  a/b: \"1\"\n", "# who to call")
	})
	t.Run("partial: the field that cannot be mapped is left out", func(t *testing.T) {
		c := newStructCase(t, structEnv)
		out := c.drift(func(o map[string]any) {
			o["spec"].(map[string]any)["replicas"] = float64(3)
			containerOf(o)["env"].([]any)[0].(map[string]any)["value"] = "changed"
		}, rendermap.Options{AllowPartial: true, AddOverrides: true})
		if !out.Mapped() || len(out.LeftOut) != 1 || !strings.Contains(out.LeftOut[0].Pointer, "/env/0/value") {
			t.Fatalf("out = %+v", out)
		}
	})
	t.Run("a Secret is never proposed, structured or not", func(t *testing.T) {
		out, err := Verify(t.Context(), &structRenderer{}, Input{Resource: rendermap.ResourceKey{Kind: "Secret", Name: "s"},
			Fields: []rendermap.Field{structuredChange("/data", v1alpha1.OpAdd, "", `{"a":"b"}`)}}, rendermap.Options{})
		if err != nil || out.Mapped() || out.Unmapped.Reason != rendermap.ReasonSecretNotVerifiable {
			t.Fatalf("out = %+v, err = %v", out, err)
		}
	})
}

// Two keys feed the same list of tolerations. The Deployment is named web, so
// the key web.tolerations fits it better than tolerations does.
const twoTolerationKeys = "tolerations: []\nweb:\n  tolerations: []\n"

var dedicated = map[string]any{"key": "dedicated", "operator": "Equal", "value": "bf", "effect": "NoSchedule"}

func addToleration(o map[string]any) { podSpecOf(o)["tolerations"] = []any{dedicated} }

func TestStructuredTwoKeysThatGiveTheSameRenderStayAmbiguous(t *testing.T) {
	c := newStructCase(t, twoTolerationKeys)
	out := c.drift(addToleration, rendermap.Options{})
	if out.Mapped() || out.Unmapped.Reason != rendermap.ReasonAmbiguousCandidates {
		t.Fatalf("out = %+v: both keys give the same render, but different files", out)
	}
	for _, key := range []string{"tolerations", "web.tolerations"} {
		if !strings.Contains(out.Unmapped.Message, "chart/env.yaml "+key) {
			t.Errorf("message does not name %s: %s", key, out.Unmapped.Message)
		}
	}
}

func TestStructuredEditsThatGiveTheSameFilesAreOneMapping(t *testing.T) {
	// Adding the key to the map and setting the whole map write the same file.
	c := newStructCase(t, structEnv)
	out := c.drift(func(o map[string]any) { podMeta(o)["annotations"].(map[string]any)["a/b"] = "1" }, rendermap.Options{})
	if !out.Mapped() {
		t.Fatalf("out = %+v: edits that write the same bytes must not make the proposal ambiguous", out.Unmapped)
	}
}

func TestStructuredBoundDoesNotHideAmbiguity(t *testing.T) {
	t.Run("a key that is read is left out by the bound: it is rendered to check, and it verifies too", func(t *testing.T) {
		c := newStructCase(t, twoTolerationKeys)
		out := c.drift(addToleration, rendermap.Options{MaxCandidates: 1})
		if out.Mapped() {
			t.Fatalf("mapped: %+v. Both keys verify without the bound, so one of them was hidden", out.Results)
		}
		if out.Unmapped.Reason != rendermap.ReasonAmbiguousCandidates {
			t.Fatalf("reason = %s: %s", out.Unmapped.Reason, out.Unmapped.Message)
		}
		for _, want := range []string{"chart/env.yaml web.tolerations", "chart/env.yaml tolerations"} {
			if !strings.Contains(out.Unmapped.Message, want) {
				t.Errorf("message %q lacks %q", out.Unmapped.Message, want)
			}
		}
	})
	t.Run("keys left out that cannot be rendered to check: too many candidates, the best ones named", func(t *testing.T) {
		c := newStructCase(t, twoTolerationKeys+"app:\n  tolerations: []\n")
		out := c.drift(addToleration, rendermap.Options{MaxCandidates: 1, MaxConfirmations: 1})
		if out.Mapped() || out.Unmapped.Reason != rendermap.ReasonTooManyCandidates {
			t.Fatalf("out = %+v", out)
		}
		for _, want := range []string{"an edit verifies", "2 more value keys", "more than the 1 that are rendered to check"} {
			if !strings.Contains(out.Unmapped.Message, want) {
				t.Errorf("message %q lacks %q", out.Unmapped.Message, want)
			}
		}
	})
	t.Run("the key left out is not read by any template: the mapping stands", func(t *testing.T) {
		// legacy.tolerations fits as well as tolerations does and is cut, but no
		// template reads it, so it cannot be the key.
		c := newStructCase(t, "web:\n  tolerations: []\nlegacy:\n  tolerations: []\n")
		out := c.drift(addToleration, rendermap.Options{MaxCandidates: 1})
		got := edited(t, out)
		mustContain(t, got, "web:\n  tolerations:\n    - effect: NoSchedule\n")
		if strings.Contains(got, "legacy:\n  tolerations:\n    -") {
			t.Errorf("the unread key was edited:\n%s", got)
		}
	})
	t.Run("with a bound that holds both, they are both tried and the result is ambiguity", func(t *testing.T) {
		c := newStructCase(t, twoTolerationKeys)
		if out := c.drift(addToleration, rendermap.Options{MaxCandidates: 2}); out.Mapped() || out.Unmapped.Reason != rendermap.ReasonAmbiguousCandidates {
			t.Fatalf("out = %+v", out)
		}
	})
}
