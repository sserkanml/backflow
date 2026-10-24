package kustomize

import (
	"errors"

	"github.com/sserkanml/backflow/internal/rendermap"
)

// Reasons are machine-readable causes for a proposal that stays unmapped.
const (
	// ReasonSourceNotClosed: the build reads something that cannot be uploaded
	// (a remote base, helmCharts, a missing file), so a render of edited files
	// would not be the render Argo CD makes.
	ReasonSourceNotClosed = "KustomizeSourceNotClosed"
	// ReasonBaseNotEditable: the value comes from a file outside the
	// Application's path and spec.kustomize.editBases is off.
	ReasonBaseNotEditable = "BaseNotEditable"
)

// ErrNotClosed means the files a Kustomize build reads are not a closed set.
var ErrNotClosed = errors.New("the kustomization reads files that cannot be uploaded")

// ReasonOf returns the reason for an error from NewSource or a Renderer, or ""
// when the error is none of those.
func ReasonOf(err error) string {
	if errors.Is(err, ErrNotClosed) {
		return ReasonSourceNotClosed
	}
	return rendermap.ReasonOf(err)
}
