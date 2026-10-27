package mapping

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

// LeftOut is a change a partial mapping does not make.
type LeftOut struct {
	// ChangeIndex is the index into the proposal's changes.
	ChangeIndex int
	// Path is the JSON pointer of the field.
	Path string
	// Err says why it cannot be written; it wraps ErrCannotApply.
	Err error
}

// changePrefix is the "change 0 (Remove /spec/x): " Apply puts before the cause.
var changePrefix = regexp.MustCompile(`^change \d+ \([^)]*\): `)

// Message is the cause for people.
func (l LeftOut) Message() string { return changePrefix.ReplaceAllString(l.Err.Error(), "") }

// MapPartial is Map for a policy that allows partial proposals. A change that
// cannot be written to the file (an element removed from a list, say) is left
// out instead of failing the whole proposal; the others are applied together
// and verified as a set. The verification compares the edited document with the
// original and requires exactly the mapped changes, so every left-out field is
// untouched. When no change can be written, or the set does not verify, the
// error is the one Map gives. Result.LeftOut lists the changes not made, and
// each Edit's ChangeIndex points into changes.
func MapPartial(fsys fs.FS, appPath string, opts DirectoryOptions, destNamespace string, res ResourceID,
	changes []v1alpha1.FieldChange) (*Result, error) {
	loc, err := Locate(fsys, appPath, opts, destNamespace, res)
	if err != nil {
		return nil, err
	}
	original, err := fs.ReadFile(fsys, loc.File)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", loc.File, err)
	}

	var mapped []v1alpha1.FieldChange
	var index []int // index[i] is the position of mapped[i] in changes
	var left []LeftOut
	for i, ch := range changes {
		_, err := Apply(loc.File, original, loc.Document, []v1alpha1.FieldChange{ch})
		switch {
		case err == nil:
			mapped = append(mapped, ch)
			index = append(index, i)
		case errors.Is(err, ErrCannotApply):
			left = append(left, LeftOut{ChangeIndex: i, Path: ch.Path, Err: err})
		default:
			return nil, err // the file as a whole cannot be edited
		}
	}
	if len(mapped) == 0 {
		return nil, left[0].Err
	}

	edited, err := Apply(loc.File, original, loc.Document, mapped)
	if err != nil {
		return nil, err
	}
	if err := VerifyFile(loc.File, original, edited, loc.Document, mapped); err != nil {
		return nil, err
	}
	edits := make([]Edit, len(mapped))
	for i, ch := range mapped {
		edits[i] = Edit{File: loc.File, Location: ch.Path, Value: ch.Live, ChangeIndex: index[i]}
	}
	return &Result{
		File: loc.File, Document: loc.Document, Edits: edits, Edited: edited,
		Diff: UnifiedDiff(loc.File, original, edited), LeftOut: left,
	}, nil
}
