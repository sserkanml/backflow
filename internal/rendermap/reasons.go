package rendermap

import "errors"

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
	// ReasonSetByApplication: the value is set by an option of the Application
	// (spec.source.kustomize.images, say), which Argo CD applies on top of Git,
	// so a change of Git cannot change it.
	ReasonSetByApplication = "SetByApplication"
)

// ReasonOf returns the reason for an error from a Renderer, or "" when the
// error is none of those.
func ReasonOf(err error) string {
	switch {
	case errors.Is(err, ErrRenderFailed):
		return ReasonRenderFailed
	case errors.Is(err, ErrFilesTooLarge):
		return ReasonFilesTooLarge
	}
	return ""
}
