package helmmap

import "errors"

// Reasons are machine-readable causes for a proposal that stays unmapped. They
// are meant for a condition reason, so they are CamelCase constants.
const (
	// Value file resolution.
	ReasonSymlinkedValueFile   = "SymlinkedValueFile"
	ReasonMissingValueFile     = "MissingValueFile"
	ReasonValueFileOutsideRepo = "ValueFileOutsideRepository"
	ReasonUnsupportedValueFile = "UnsupportedValueFile"
	ReasonInvalidInlineValues  = "InvalidInlineValues"
)

// SourceReason returns the reason for an error from NewSource, or "" when the
// error is not one of the source errors.
func SourceReason(err error) string {
	switch {
	case errors.Is(err, ErrSymlinkedValueFile):
		return ReasonSymlinkedValueFile
	case errors.Is(err, ErrMissingValueFile):
		return ReasonMissingValueFile
	case errors.Is(err, ErrOutsideRepository):
		return ReasonValueFileOutsideRepo
	case errors.Is(err, ErrUnsupportedValueFile):
		return ReasonUnsupportedValueFile
	case errors.Is(err, ErrInvalidInline):
		return ReasonInvalidInlineValues
	}
	return ""
}
