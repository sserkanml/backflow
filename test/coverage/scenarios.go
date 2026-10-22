//go:build coverage

package coverage

import (
	"fmt"
	"strings"
)

// A scenario simulates one change somebody makes in the cluster to a resource
// of a chart. It patches the rendered manifest (the live state); drift.Diff
// against the render then gives the changes Backflow would see.
type scenario struct {
	id   string
	desc string
	// kinds are the resource kinds it applies to.
	kinds []string
	// apply patches the object in place and reports whether it applied.
	apply func(obj map[string]any) bool
}

var workloadKinds = []string{"Deployment", "StatefulSet", "DaemonSet"}

var scenarios = []scenario{
	{id: "image-tag", desc: "set image tag (every container that runs the same image)", kinds: workloadKinds, apply: func(o map[string]any) bool {
		img, _ := container(o)["image"].(string)
		slash := strings.LastIndex(img, "/")
		colon := strings.LastIndex(img, ":")
		if colon < 0 || colon < slash || strings.Contains(img, "@") {
			return false
		}
		next := img[:colon+1] + "9.9.9"
		spec := podTemplate(o, "spec")
		for _, key := range []string{"containers", "initContainers"} {
			cs, _ := spec[key].([]any)
			for _, c := range cs {
				if m, _ := c.(map[string]any); m != nil && m["image"] == img {
					m["image"] = next
				}
			}
		}
		return true
	}},
	{id: "scale", desc: "scale (replicas + 1)", kinds: workloadKinds, apply: func(o map[string]any) bool {
		spec, _ := o["spec"].(map[string]any)
		n, ok := spec["replicas"].(float64)
		if !ok {
			if i, isInt := spec["replicas"].(int64); isInt {
				spec["replicas"] = i + 1
				return true
			}
			return false
		}
		spec["replicas"] = n + 1
		return true
	}},
	{id: "env-change", desc: "change an env value", kinds: workloadKinds, apply: func(o map[string]any) bool {
		env, _ := container(o)["env"].([]any)
		for _, e := range env {
			item, _ := e.(map[string]any)
			if v, ok := item["value"].(string); ok {
				item["value"] = "bf-" + v
				return true
			}
		}
		return false
	}},
	{id: "env-add", desc: "add an env var", kinds: workloadKinds, apply: func(o map[string]any) bool {
		c := container(o)
		env, _ := c["env"].([]any)
		c["env"] = append(env, map[string]any{"name": "BF_EXTRA", "value": "1"})
		return true
	}},
	{id: "resources", desc: "change resources limits", kinds: workloadKinds, apply: func(o map[string]any) bool {
		c := container(o)
		res, _ := c["resources"].(map[string]any)
		if res == nil {
			res = map[string]any{}
			c["resources"] = res
		}
		limits, _ := res["limits"].(map[string]any)
		if limits == nil {
			res["limits"] = map[string]any{"memory": "2Gi"}
			return true
		}
		for _, k := range []string{"memory", "cpu", "ephemeral-storage"} {
			if v, ok := limits[k].(string); ok {
				limits[k] = v + "0"
				return true
			}
		}
		limits["memory"] = "2Gi"
		return true
	}},
	{id: "pod-annotation", desc: "add a pod annotation", kinds: workloadKinds, apply: func(o map[string]any) bool {
		meta := podTemplate(o, "metadata")
		ann, _ := meta["annotations"].(map[string]any)
		if ann == nil {
			ann = map[string]any{}
			meta["annotations"] = ann
		}
		ann["backflow.io/test"] = "1"
		return true
	}},
	{id: "toleration", desc: "add a toleration", kinds: workloadKinds, apply: func(o map[string]any) bool {
		spec := podTemplate(o, "spec")
		tol, _ := spec["tolerations"].([]any)
		spec["tolerations"] = append(tol, map[string]any{"key": "dedicated", "operator": "Equal", "value": "bf", "effect": "NoSchedule"})
		return true
	}},
	{id: "pull-policy", desc: "change imagePullPolicy", kinds: workloadKinds, apply: func(o map[string]any) bool {
		c := container(o)
		if c["imagePullPolicy"] == "Always" {
			c["imagePullPolicy"] = "Never"
		} else {
			c["imagePullPolicy"] = "Always"
		}
		return true
	}},
	{id: "probe-delay", desc: "change a probe's initialDelaySeconds", kinds: workloadKinds, apply: func(o map[string]any) bool {
		c := container(o)
		for _, k := range []string{"livenessProbe", "readinessProbe", "startupProbe"} {
			if p, ok := c[k].(map[string]any); ok {
				if n, ok := p["initialDelaySeconds"].(float64); ok {
					p["initialDelaySeconds"] = n + 7
					return true
				}
			}
		}
		return false
	}},
	{id: "service-port", desc: "change a service port", kinds: []string{"Service"}, apply: func(o map[string]any) bool {
		spec, _ := o["spec"].(map[string]any)
		ports, _ := spec["ports"].([]any)
		if len(ports) == 0 {
			return false
		}
		p, _ := ports[0].(map[string]any)
		n, ok := p["port"].(float64)
		if !ok {
			return false
		}
		p["port"] = n + 1
		return true
	}},
	{id: "configmap-data", desc: "change ConfigMap data", kinds: []string{"ConfigMap"}, apply: func(o map[string]any) bool {
		data, _ := o["data"].(map[string]any)
		for _, k := range sortedKeys(data) {
			if v, ok := data[k].(string); ok && !strings.Contains(v, "\n") {
				data[k] = v + "-bf"
				return true
			}
		}
		for _, k := range sortedKeys(data) {
			if v, ok := data[k].(string); ok {
				data[k] = v + "\n# bf\n"
				return true
			}
		}
		return false
	}},
}

func container(o map[string]any) map[string]any {
	spec := podTemplate(o, "spec")
	cs, _ := spec["containers"].([]any)
	if len(cs) == 0 {
		panic(fmt.Sprintf("%v has no container", o["kind"]))
	}
	return cs[0].(map[string]any)
}

// podTemplate returns spec.template.<part> of a workload, creating it.
func podTemplate(o map[string]any, part string) map[string]any {
	spec, _ := o["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	p, _ := tpl[part].(map[string]any)
	if p == nil {
		p = map[string]any{}
		tpl[part] = p
	}
	return p
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// Scenarios that combine others, to see partial proposals.
func init() {
	for _, other := range []string{"pod-annotation", "toleration"} {
		second := scenarioByID(other)
		first := scenarioByID("scale")
		scenarios = append(scenarios, scenario{
			id: "scale+" + other, desc: "scale and " + second.desc + " together", kinds: workloadKinds,
			apply: func(o map[string]any) bool { return first.apply(o) && second.apply(o) },
		})
	}
}

func scenarioByID(id string) scenario {
	for _, s := range scenarios {
		if s.id == id {
			return s
		}
	}
	panic("no scenario " + id)
}
