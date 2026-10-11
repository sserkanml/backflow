package controller

import (
	"regexp"

	"github.com/sserkanml/backflow/internal/scm"
)

// credentialsInURL matches the user information of a URL inside a text:
// "https://user:password@host" and "https://token@host".
var credentialsInURL = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^/\s@]+@`)

// redactText hides credentials embedded in any URL of a text, so they do not
// reach a status, an Event or a log line.
func redactText(s string) string {
	return credentialsInURL.ReplaceAllString(s, "${1}REDACTED@")
}

// hasCredentials reports whether a repository URL carries a credential (see
// scm.URLHasCredentials).
func hasCredentials(repoURL string) bool { return scm.URLHasCredentials(repoURL) }
