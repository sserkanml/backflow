package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/sserkanml/backflow/internal/metrics/metricstest"
)

func TestSecretReadsAreCounted(t *testing.T) {
	reads := func(result string) float64 {
		return metricstest.Value("backflow_secret_reads_total", map[string]string{"result": result})
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "s"}}
	fakeClient := fake.NewClientBuilder().WithObjects(secret).Build()
	c := NewSecretClient(fakeClient, fakeClient, time.Minute)

	okBefore, missingBefore, forbiddenBefore := reads("ok"), reads("notfound"), reads("forbidden")
	for range 3 { // the cache answers the second and the third
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "s"}, &corev1.Secret{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := reads("ok") - okBefore; got != 1 {
		t.Errorf("ok = %v, want 1: only a read of the API server is counted", got)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "gone"}, &corev1.Secret{}); err == nil {
		t.Fatal("no error for a missing Secret")
	}
	if got := reads("notfound") - missingBefore; got != 1 {
		t.Errorf("notfound = %v, want 1", got)
	}
	f := NewSecretClient(fakeClient, forbiddingReader{fakeClient}, 0)
	_ = f.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "s"}, &corev1.Secret{})
	if got := reads("forbidden") - forbiddenBefore; got != 1 {
		t.Errorf("forbidden = %v, want 1", got)
	}
}
