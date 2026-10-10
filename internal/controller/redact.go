package controller

import (
	"net/url"
	"regexp"
)

// credentialsInURL matches the user information of a URL inside a text:
// "https://user:password@host" and "https://token@host".
var credentialsInURL = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^/\s@]+@`)

// redactText hides credentials embedded in any URL of a text, so they do not
// reach a status, an Event or a log line.
func redactText(s string) string {
	return credentialsInURL.ReplaceAllString(s, "${1}REDACTED@")
}

// hasCredentials reports whether an http(s) repository URL carries user
// information (a user name, a token or a password).
func hasCredentials(repoURL string) bool {
	u, err := url.Parse(repoURL)
	if err != nil {
		// An unparsable URL may still hold a credential.
		return credentialsInURL.MatchString(repoURL)
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.User != nil
}
