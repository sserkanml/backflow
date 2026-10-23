package helmmap

import "github.com/sserkanml/backflow/internal/rendermap"

// ValuesWriter writes candidates to Helm value files.
type ValuesWriter struct{}

var _ rendermap.Writer = ValuesWriter{}

// Write implements rendermap.Writer. It sets the key of a candidate in the
// content of its file, or adds it when the candidate overrides a chart default.
func (ValuesWriter) Write(c rendermap.Candidate, content []byte, value string) (*rendermap.Written, error) {
	if e := c.Structured; e != nil {
		if e.Kind == rendermap.EditRemoveKey {
			return RemoveKey(c.File, content, c.Path)
		}
		return SetNode(c.File, content, c.Path, e.Value)
	}
	if c.Override {
		tag := ""
		if c.Default != nil {
			tag = c.Default.Tag
		}
		return AddValue(c.File, content, c.Path, value, tag)
	}
	return SetValue(c.File, content, c.Path, value)
}
