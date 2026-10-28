package controller

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/sserkanml/backflow/internal/metrics"
)

var _ = Describe("Metrics endpoint", func() {
	It("serves the Backflow series", func() {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		addr := l.Addr().String()
		Expect(l.Close()).To(Succeed())

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: addr},
		})
		Expect(err).NotTo(HaveOccurred())
		metrics.SetBuildInfo("v0.0.0-test", "abcdef123456")
		Expect(metrics.RegisterProposals(ProposalCounts(mgr.GetCache()))).To(Succeed())

		mctx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mctx)).To(Succeed())
		}()

		Eventually(func(g Gomega) string {
			resp, err := http.Get("http://" + addr + "/metrics") //nolint:noctx // a local test server
			g.Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			g.Expect(err).NotTo(HaveOccurred())
			return string(body)
		}, 30*time.Second, 200*time.Millisecond).Should(And(
			ContainSubstring(`backflow_build_info{commit="abcdef123456",version="v0.0.0-test"} 1`),
		))
	})
})
