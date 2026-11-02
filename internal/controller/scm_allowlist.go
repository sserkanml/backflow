package controller

import (
	"fmt"
	"sync/atomic"

	backflowv1beta1 "github.com/sserkanml/backflow/api/v1beta1"
	"github.com/sserkanml/backflow/internal/allowlist"
	"github.com/sserkanml/backflow/internal/scm"
)

// scmHosts is the list of Git hosts the installer allows ScmConnections for (--scm-allowed-hosts,
// chart scm.allowedHosts). It is set once at start-up. It is not in the CRD: a tenant who can
// create a ScmConnection must not be able to point the operator, with a token of their own, at
// any host. An empty list allows no host.
var scmHosts atomic.Pointer[allowlist.Hosts]

// SetScmAllowedHosts sets the hosts ScmConnections may name.
func SetScmAllowedHosts(hosts []string) {
	h := allowlist.Hosts(hosts)
	scmHosts.Store(&h)
}

// scmHostAllowed reports whether the host of a URL is allowed.
func scmHostAllowed(rawURL string) bool {
	h := scmHosts.Load()
	return h != nil && h.AllowsURL(rawURL)
}

// checkScmHost returns a HostNotAllowed error for a connection whose spec.url names a host the
// installer did not allow. No request is made to such a host, and its token Secret is not read.
func checkScmHost(conn *backflowv1beta1.ScmConnection) error {
	if scmHostAllowed(conn.Spec.URL) {
		return nil
	}
	return &checkError{reason: backflowv1beta1.ReasonHostNotAllowed, msg: fmt.Sprintf(
		"the host of %s is not in the operator's list of allowed Git hosts (--scm-allowed-hosts, chart value scm.allowedHosts); no request is made",
		scm.RedactURL(conn.Spec.URL))}
}
