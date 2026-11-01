package actorwebhook

import (
	"slices"
	"strings"
)

// Condition is a matchCondition of the webhook configuration: a CEL expression that must
// be true for the apiserver to call the webhook.
type Condition struct {
	Name       string
	Expression string
}

// Conditions are the match conditions of the webhook configuration, the one source of
// truth for the chart (a test compares them) and for the tests that evaluate them.
//
// A field that is absent is not empty: request.subResource does not exist for the main
// resource, so it is always tested with has(). A condition that fails to evaluate is
// not a match under failurePolicy Ignore, silently, which would switch the webhook off.
//
// excludeUsers are the user names whose changes are not wanted besides the system
// users (the Argo CD application controller, Backflow's own service account).
func Conditions(excludeUsers []string) []Condition {
	quoted := make([]string, len(excludeUsers))
	for i, u := range excludeUsers {
		quoted[i] = "'" + strings.ReplaceAll(strings.ReplaceAll(u, `\`, `\\`), "'", `\'`) + "'"
	}
	users := "!(request.userInfo.username in [" + strings.Join(slices.Sorted(slices.Values(quoted)), ", ") + "])"
	if len(excludeUsers) == 0 {
		users = "true"
	}
	return []Condition{
		{Name: "not-secrets", Expression: "request.resource.resource != 'secrets'"},
		{Name: "not-dry-run", Expression: "!has(request.dryRun) || request.dryRun == false"},
		{Name: "tracked-or-scale", Expression: "(has(request.subResource) && request.subResource == 'scale') || " +
			"(!has(request.subResource) && ((has(object.metadata.annotations) && " +
			"'argocd.argoproj.io/tracking-id' in object.metadata.annotations) || " +
			"(has(object.metadata.labels) && 'app.kubernetes.io/instance' in object.metadata.labels)))"},
		{Name: "not-system-or-gitops-users", Expression: "!request.userInfo.username.startsWith('system:serviceaccount:kube-system:') && " +
			"!request.userInfo.username.startsWith('system:node:') && " +
			"!(request.userInfo.username in ['system:kube-controller-manager', 'system:kube-scheduler']) && " + users},
	}
}
