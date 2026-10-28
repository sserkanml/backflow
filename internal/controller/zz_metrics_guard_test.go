package controller

import (
	"testing"

	"github.com/sserkanml/backflow/internal/metrics/metricstest"
)

// TestMetricsStayWithinTheirLabels runs after the controller suite (the file name
// sorts last), when the reconcilers have recorded what they did: no label holds a
// name of a resource, a file or a branch, and no metric has more series than a
// small installation can explain.
func TestMetricsStayWithinTheirLabels(t *testing.T) {
	metricstest.Guard(t, 200)
}
