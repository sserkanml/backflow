package rendermap

import "testing"

func TestTokens(t *testing.T) {
	for in, want := range map[string][]string{
		"extraEnvVars":         {"env", "var"},
		"podAnnotations":       {"pod", "annotation"},
		"replica.replicaCount": {"replica", "count"},
		"redis-prod-master":    {"redi", "prod", "master"},
		"backflow.io/test":     {"backflow", "test"},
		"spec":                 nil,
		"BITNAMI_DEBUG":        {"bitnami", "debug"},
		"nodeSelector":         {"node", "selector"},
		"tolerations":          {"toleration"},
		"resourcesPreset":      {"resource", "preset"},
		"ports":                {"port"},
		"class":                {"class"},
		"HTTPProxy":            {"httpproxy"},
	} {
		got := tokens(in)
		if len(got) != len(want) {
			t.Errorf("%s: %v, want %v", in, got, want)
			continue
		}
		for _, w := range want {
			if !got[w] {
				t.Errorf("%s: %v lacks %s", in, got, w)
			}
		}
	}
}

func TestAffinity(t *testing.T) {
	if n := affinity(tokens("annotations"), tokens("podAnnotations")); n != 1 {
		t.Errorf("annotations ~ podAnnotations = %d", n)
	}
	if n := affinity(tokens("replicas", "redis-prod-replicas"), tokens("replica", "replicaCount")); n != 1 {
		t.Errorf("replicas of redis-replicas ~ replica.replicaCount = %d", n)
	}
	if n := affinity(tokens("tolerations"), tokens("podAnnotations")); n != 0 {
		t.Errorf("tolerations ~ podAnnotations = %d", n)
	}
}

func TestAffinitySynonyms(t *testing.T) {
	if n := affinity(tokens("replicas"), tokens("master.count")); n != 1 {
		t.Errorf("replicas ~ master.count = %d: a chart calls the replicas of a workload a count", n)
	}
	if n := affinity(tokens("count"), tokens("replicaCount")); n < 1 {
		t.Errorf("count ~ replicaCount = %d", n)
	}
}
