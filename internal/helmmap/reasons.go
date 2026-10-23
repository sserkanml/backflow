package helmmap

import (
	"errors"

	"github.com/sserkanml/backflow/internal/rendermap"
)

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

// Reasons of verification and rendering.
const (
	ReasonRenderMismatch        = "RenderMismatch"
	ReasonNondeterministicField = "NondeterministicField"
	ReasonSecretNotVerifiable   = "SecretNotVerifiable"
	ReasonNoVerifiedCandidate   = "NoVerifiedCandidate"
	// ReasonValueAffectsOtherFields: every key of the value files that was tried
	// reproduces the drifted field, and each also changes other fields, which the
	// policy does not allow. The message names the nearest key and what else it
	// changes.
	ReasonValueAffectsOtherFields = "ValueAffectsOtherFields"
	ReasonAmbiguousCandidates     = "AmbiguousCandidates"
	ReasonEditsInteract           = "EditsInteract"
	ReasonTooManyCandidates       = "TooManyCandidates"
	ReasonRenderBudgetExceeded    = "RenderBudgetExceeded"
	ReasonRenderFailed            = "RenderFailed"
	ReasonFilesTooLarge           = "FilesTooLarge"
	ReasonUnsupportedChange       = "UnsupportedChange"
	ReasonSymlinkInChart          = "SymlinkInChart"
	ReasonDependencyOutsideRepo   = "DependencyOutsideRepository"
	ReasonArgoSourceOverrides     = "ArgoSourceOverrides"
)

// ReasonOf returns the reason for an error from NewSource, CollectFiles or a
// Renderer, or "" when the error is none of those.
func ReasonOf(err error) string {
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
	case errors.Is(err, ErrSymlinkInChart):
		return ReasonSymlinkInChart
	case errors.Is(err, ErrDependencyOutsideRepository):
		return ReasonDependencyOutsideRepo
	case errors.Is(err, ErrArgoSourceOverrides):
		return ReasonArgoSourceOverrides
	case errors.Is(err, rendermap.ErrRenderFailed):
		return ReasonRenderFailed
	case errors.Is(err, rendermap.ErrFilesTooLarge):
		return ReasonFilesTooLarge
	}
	return ""
}
