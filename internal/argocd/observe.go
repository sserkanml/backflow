package argocd

import (
	"errors"
	"strings"
	"time"

	"github.com/sserkanml/backflow/internal/metrics"
)

// The endpoints of the requests, as the metrics name them.
const (
	endpointManagedResources = "managed_resources"
	endpointManifestsUpload  = "manifests_with_files"
	endpointManifestsRevs    = "manifests_by_revisions"
	endpointAppDetails       = "app_details"
)

// observe records a request to the Argo CD API when the returned function runs:
//
//	defer c.observe(endpoint)(&err)
//
// The class of the answer follows the error: no error is 2xx; a refusal, a missing
// Application, a source that does not render and files that are too large are 4xx
// (what Argo CD said about the request, not a failure of Argo CD); an Argo CD that
// could not be reached is an error, or 5xx when it answered so.
func (c *Client) observe(endpoint string) func(err *error) {
	start := time.Now()
	return func(err *error) {
		metrics.ArgoCDRequest(endpoint, statusOf(*err), *err, time.Since(start))
	}
}

// statusOf is the HTTP status that stands for the outcome of a request.
func statusOf(err error) int {
	switch {
	case err == nil:
		return 200
	case errors.Is(err, ErrUnauthorized):
		return 403
	case errors.Is(err, ErrNotFound):
		return 404
	case errors.Is(err, ErrRenderFailed), errors.Is(err, ErrArchiveTooLarge), errors.Is(err, ErrUnsupported), errors.Is(err, ErrAppDetailsFailed):
		return 422
	case errors.Is(err, ErrUnreachable) && strings.Contains(err.Error(), "HTTP 5"):
		return 502
	}
	return 0
}
