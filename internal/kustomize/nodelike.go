package kustomize

import "go.yaml.in/yaml/v3"

// nodeLike is a parsed YAML node, as far as steps need it.
type nodeLike interface {
	child(key string) nodeLike
	item(mergeKey, value string) (nodeLike, int)
	scalar() (string, bool)
}

type yamlNode struct{ n *yaml.Node }

func (y yamlNode) child(key string) nodeLike {
	if c := mapValue(y.n, key); c != nil {
		return yamlNode{c}
	}
	return nil
}

func (y yamlNode) item(mergeKey, value string) (nodeLike, int) {
	if y.n.Kind != yaml.SequenceNode {
		return nil, 0
	}
	for i, it := range y.n.Content {
		if v, _ := scalarText(mapValue(it, mergeKey)); v == value {
			return yamlNode{it}, i
		}
	}
	return nil, 0
}

func (y yamlNode) scalar() (string, bool) { return scalarText(y.n) }
