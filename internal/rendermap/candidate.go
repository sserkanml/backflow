package rendermap

import "github.com/sserkanml/backflow/internal/mapping"

// Candidate is an edit that might bring a drifted field to its live value: set
// a key of a file, or add it.
type Candidate struct {
	// File is the repository path of the file to edit.
	File string
	Path KeyPath
	// Fragment is set when only part of the drifted value is the key's value:
	// the key holds the "1.27-alpine" of "nginx:1.27-alpine". NewValue is then
	// what the key must become. A candidate for the whole value has neither;
	// the value to write is the new value of the field.
	Fragment bool
	NewValue string
	// Override is set when File does not define Path: the candidate adds the
	// key to override a default. Default says which.
	Override bool
	Default  *Default
	// Structured is set when the candidate is not the change of one scalar:
	// a map or a list set at Path, or a key removed.
	Structured *StructuredEdit
	// Variant tells apart edits of one file at one Path that are not the same
	// edit (a source with several kinds of files uses it); Meta is what its
	// Writer needs to make the edit, opaque to the engine. NewFile is set when
	// File does not exist and the Writer creates it.
	Variant string
	Meta    any
	NewFile bool
}

// Default is a default, outside the files that are edited, that feeds a drifted
// field and that a Candidate with Override overrides: the values.yaml of a
// Helm chart or of one of its dependencies.
type Default struct {
	// File is the file that defines it.
	File string
	// Key is the key path in that file. For a dependency it is the path in the
	// dependency's own values, without its name or alias.
	Key KeyPath
	// Value is the default as a template renders it.
	Value string
	// Tag is the YAML tag of the default (!!str, !!int, !!float, !!bool).
	Tag string
}

// The kinds of StructuredEdit.
const (
	// EditSet sets a key to a map, a list or a scalar, creating it when the
	// file does not have it.
	EditSet = "set"
	// EditRemoveKey removes a key from the file.
	EditRemoveKey = "removeKey"
)

// StructuredEdit is the edit of a Candidate that is not the change of one
// scalar.
type StructuredEdit struct {
	Kind string
	// Value is the decoded JSON to set; nil for EditRemoveKey.
	Value any
}

// Text is the edit as one comparable string.
func (e *StructuredEdit) Text() string {
	t, _ := JSONText(e.Value)
	return e.Kind + ":" + t
}

// Found is what a source finds for one drifted field: the candidates that hold
// the value in Git.
type Found struct {
	// Candidates are the editable matches, in the order of the files and of the
	// keys in each file.
	Candidates []Candidate
	// Overrides are the ways to set a default that is not edited: the key added
	// to the file that receives overrides. They are only tried when no key a
	// file already defines works.
	Overrides []Candidate
	// Later are further candidates, in the order they are tried: a set only when
	// no edit set of the candidates before it verifies (Candidates and the keys
	// of Fragments first). A source that has places to look in an order of
	// preference, a setting before a file before a new patch, lists them here.
	Later []Tier
	// Blocked says the field cannot be mapped whatever is tried, and why.
	Blocked *Blocked
	// Diagnostics explains what the source knows about the value when nothing
	// verifies; nil when it knows nothing.
	Diagnostics Diagnostics
}

// Tier is a named set of candidates that is tried after the ones before it.
type Tier struct {
	Name       string
	Candidates []Candidate
}

// Blocked is a field that no edit can bring to its live value: the source knows
// where the value comes from, and Git does not decide it (a setting of the
// Application, a hashed generator).
type Blocked struct {
	// Reason is machine readable (a Reason constant); Message is for people.
	Reason, Message string
	// Detail is what the source knows about the cause, opaque to the engine (the
	// option of the Application that sets the value, for example).
	Detail any
}

// Diagnostics lets a source explain why a field has no verified candidate. Both
// methods get the Found as the engine left it, after bounding its candidates.
type Diagnostics interface {
	// Explain returns what the value comes from other than an edit that could
	// have worked: a default that is not edited, a setting of the Application.
	Explain(f *Found, overrides bool) []string
	// ExplainEmpty returns what to say when f offers no candidate at all.
	ExplainEmpty(f *Found) []string
}

// Written is the outcome of writing one candidate to a file.
type Written struct {
	// Result holds the edited file and its diff. Document is 0.
	mapping.Result
	// Line is the 1-based line of the edited key in the new file.
	Line int
	// Text is that line as it reads in the new file.
	Text string
	// Also are the other files the edit changes, in full: a new patch file is
	// listed in the kustomization that uses it. Edits of an entry are the edits
	// of that file.
	Also []mapping.Result
}

// Writer edits a file for a candidate. It is the file writer seam of a source
// kind: values files for Helm, the files of an overlay for Kustomize. value is
// the text to write for a scalar candidate (see Candidate.Fragment).
type Writer interface {
	Write(c Candidate, content []byte, value string) (*Written, error)
}

// ContextWriter is a Writer whose edit depends on files other than the one it
// edits (a new patch file is listed in a kustomization that an earlier edit of
// the same set may have changed). When a Writer is one, the engine calls
// WriteWith with the current files of the set, and Write is not used.
type ContextWriter interface {
	Writer
	WriteWith(c Candidate, files map[string][]byte, value string) (*Written, error)
}

// Provider supplies what the engine cannot know about a source kind. It is the
// candidate seam: the scalar candidates come with each Field (see Found), the
// structured ones are made from the layers a Provider reports.
type Provider interface {
	// Layers decodes the layers of settings a render reads, lowest precedence
	// first.
	Layers(files map[string][]byte) ([]Layer, error)
	// OverrideFile is the file that receives an overriding key, or the reason
	// there is none.
	OverrideFile() (file, reason string)
	// Controlled reports whether a setting that is not edited decides the key,
	// so editing a file would not change the render.
	Controlled(key KeyPath) bool
	// References scans the files for what the render reads, to tell keys that
	// no template reads from keys that might feed a field.
	References(files map[string][]byte) References
}

// Layer is one layer of settings as a decoded tree.
type Layer struct {
	Name string
	Tree map[string]any
	// IsDefault is set for a layer that is read but not edited.
	IsDefault bool
	// Prefix is the key the layer's keys have in the parent chart, for a
	// dependency's defaults; empty otherwise.
	Prefix string
}

// References tells whether a render may read a key.
type References interface {
	// Mentions reports whether a template may read the key or something below
	// or above it.
	Mentions(key KeyPath) bool
}
