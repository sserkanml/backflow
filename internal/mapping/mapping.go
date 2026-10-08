package mapping

import (
	"fmt"
	"io/fs"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

// Edit is one change landed in a file.
type Edit struct {
	// File path relative to the repository root.
	File string
	// Location is the JSON pointer of the field inside the document.
	Location string
	// Value is the JSON-encoded new value; empty for a removed field.
	Value string
	// ChangeIndex is the index into the proposal's changes.
	ChangeIndex int
}

// Result is a verified mapping of a proposal onto one file.
type Result struct {
	File     string
	Document int
	Edits    []Edit
	// Edited is the whole edited file. It is only returned for a verified edit.
	Edited []byte
	// Diff is the unified diff of the file.
	Diff string
}

// Map locates the document that defines res under appPath, applies changes,
// verifies the result and renders the diff. It returns an error wrapping
// ErrNotFound, ErrAmbiguous, ErrCannotApply, ErrUnsupportedFileFormat, ErrInvalidGlob or
// ErrVerificationFailed when the change cannot be traced back with certainty.
func Map(fsys fs.FS, appPath string, opts DirectoryOptions, destNamespace string, res ResourceID,
	changes []v1alpha1.FieldChange) (*Result, error) {
	loc, err := Locate(fsys, appPath, opts, destNamespace, res)
	if err != nil {
		return nil, err
	}
	original, err := fs.ReadFile(fsys, loc.File)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", loc.File, err)
	}
	edited, err := Apply(loc.File, original, loc.Document, changes)
	if err != nil {
		return nil, err
	}
	if err := Verify(original, edited, loc.Document, changes); err != nil {
		return nil, err
	}
	edits := make([]Edit, len(changes))
	for i, ch := range changes {
		edits[i] = Edit{File: loc.File, Location: ch.Path, Value: ch.Live, ChangeIndex: i}
	}
	return &Result{
		File:     loc.File,
		Document: loc.Document,
		Edits:    edits,
		Edited:   edited,
		Diff:     UnifiedDiff(loc.File, original, edited),
	}, nil
}
