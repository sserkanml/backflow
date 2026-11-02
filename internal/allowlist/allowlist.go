// Package allowlist decides whether the operator may talk to an address a tenant named. The lists
// are set by whoever installs the operator (flags and chart values), never by a custom resource:
// a tenant who can create a BackflowPolicy or a ScmConnection must not be able to point the
// operator at an arbitrary URL.
package allowlist

import (
	"net/url"
	"strings"
)

// Hosts is a list of host names. An entry is an exact host ("gitlab.com") or a wildcard that
// matches any subdomain but not the domain itself ("*.example.com"). Matching ignores case and a
// port. An empty list allows nothing.
type Hosts []string

// Allows reports whether host is in the list.
func (h Hosts) Allows(host string) bool {
	host = canonicalHost(host)
	if host == "" {
		return false
	}
	for _, entry := range h {
		entry = canonicalHost(entry)
		switch {
		case entry == "":
		case entry == host:
			return true
		case strings.HasPrefix(entry, "*.") && strings.HasSuffix(host, entry[1:]) && len(host) > len(entry)-1:
			return true
		}
	}
	return false
}

// AllowsURL reports whether the host of a URL is in the list. A URL that does not parse, or has
// no host, is not allowed.
func (h Hosts) AllowsURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return false
	}
	return h.Allows(u.Hostname())
}

// Servers is a list of server URLs. An entry names a scheme, a host and optionally a port
// ("https://argocd.example.com", "https://argocd.example.com:8443", "http://localhost:8080");
// the host may be a wildcard as in Hosts. A URL is allowed when its scheme and host match, and its
// port too when the entry has one (otherwise the default port of the scheme). The path and query
// are not looked at. An empty list allows nothing.
type Servers []string

// Allows reports whether the server URL raw is in the list.
func (s Servers) Allows(raw string) bool {
	u, ok := parse(raw)
	if !ok {
		return false
	}
	for _, e := range s {
		entry, ok := parse(e)
		if !ok || entry.scheme != u.scheme || entry.port != u.port {
			continue
		}
		if (Hosts{entry.host}).Allows(u.host) {
			return true
		}
	}
	return false
}

type server struct{ scheme, host, port string }

func parse(raw string) (server, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return server{}, false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return server{}, false
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	return server{scheme: scheme, host: canonicalHost(u.Hostname()), port: port}, true
}

func canonicalHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	return strings.TrimPrefix(strings.TrimPrefix(h, "["), "]")
}
