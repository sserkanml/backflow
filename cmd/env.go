package main

import (
	"flag"
	"fmt"
	"strings"
)

// envPrefix starts the name of the environment variable of every flag: --argocd-server is BACKFLOW_ARGOCD_SERVER.
const envPrefix = "BACKFLOW_"

// envName is the environment variable of a flag.
func envName(flagName string) string {
	return envPrefix + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(flagName))
}

// applyEnv sets, from the environment, every flag that was not given on the command line, so that an installation
// without Helm (OLM: Subscription.spec.config.env) can configure the operator. A flag wins over the environment. An
// empty
// variable is the same as an unset one. A variable for a flag that can be repeated (--argocd-default-access,
// --argocd-allowed-servers, ...) may hold several values separated by ";" or by new lines; the values of the comma
// separated flags are separated by commas as usual. An invalid value stops the operator at start-up, naming the
// variable.
func applyEnv(fs *flag.FlagSet, getenv func(string) string) error {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var err error
	fs.VisitAll(func(f *flag.Flag) {
		if err != nil || given[f.Name] {
			return
		}
		value := strings.TrimSpace(getenv(envName(f.Name)))
		if value == "" {
			return
		}
		for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ';' || r == '\n' }) {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			if setErr := fs.Set(f.Name, part); setErr != nil {
				err = fmt.Errorf("%s=%q: %w", envName(f.Name), part, setErr)
				return
			}
		}
	})
	return err
}

// secretReadNamespaces are the namespaces whose Secrets the operator reads: the ones asked for, with its own namespace
// and
// the one of the Argo CD token, which it must be able to read. Empty when none were asked for (any).
func secretReadNamespaces(asked []string, operatorNamespace, argoCDTokenNamespace string) []string {
	if len(asked) == 0 {
		return nil
	}
	out := append([]string{}, asked...)
	for _, ns := range []string{operatorNamespace, argoCDTokenNamespace} {
		if ns != "" {
			out = append(out, ns)
		}
	}
	return out
}
