package main

import (
	"flag"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sserkanml/backflow/internal/controller"
)

// operatorFlags is a flag set with one flag of each kind the operator has.
type operatorFlags struct {
	fs       *flag.FlagSet
	server   string
	insecure bool
	poll     time.Duration
	port     int
	hosts    stringList
	servers  stringList
	access   controller.AccessRules
}

func newFlags() *operatorFlags {
	o := &operatorFlags{fs: flag.NewFlagSet("test", flag.ContinueOnError)}
	o.fs.SetOutput(io.Discard)
	o.fs.StringVar(&o.server, "argocd-server", "", "")
	o.fs.BoolVar(&o.insecure, "argocd-insecure-skip-tls-verify", false, "")
	o.fs.DurationVar(&o.poll, "merge-request-poll-interval", 2*time.Minute, "")
	o.fs.IntVar(&o.port, "webhook-port", 9443, "")
	o.fs.Var(&o.hosts, "scm-allowed-hosts", "")
	o.fs.Var(&o.servers, "argocd-allowed-servers", "")
	o.fs.Var(&o.access, "argocd-default-access", "")
	return o
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestEnvName(t *testing.T) {
	for flagName, want := range map[string]string{
		"argocd-server": "BACKFLOW_ARGOCD_SERVER", "scm-allowed-hosts": "BACKFLOW_SCM_ALLOWED_HOSTS",
		"merge-request-poll-interval": "BACKFLOW_MERGE_REQUEST_POLL_INTERVAL", "zap-log-level": "BACKFLOW_ZAP_LOG_LEVEL",
		"leader-elect": "BACKFLOW_LEADER_ELECT",
	} {
		if got := envName(flagName); got != want {
			t.Errorf("envName(%q) = %q, want %q", flagName, got, want)
		}
	}
}

func TestEnvSetsWhatTheCommandLineDoesNot(t *testing.T) {
	o := newFlags()
	if err := o.fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	err := applyEnv(o.fs, env(map[string]string{
		"BACKFLOW_ARGOCD_SERVER":                   "https://argocd.example.com",
		"BACKFLOW_ARGOCD_INSECURE_SKIP_TLS_VERIFY": "true",
		"BACKFLOW_MERGE_REQUEST_POLL_INTERVAL":     "30s",
		"BACKFLOW_WEBHOOK_PORT":                    "9444",
		"BACKFLOW_SCM_ALLOWED_HOSTS":               "gitlab.example.com, *.corp.example.com",
		"BACKFLOW_ARGOCD_ALLOWED_SERVERS":          "https://a.example.com,https://b.example.com",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if o.server != "https://argocd.example.com" || !o.insecure || o.poll != 30*time.Second || o.port != 9444 {
		t.Errorf("scalars: %q %v %v %d", o.server, o.insecure, o.poll, o.port)
	}
	if got := strings.Join(o.hosts.values, "|"); got != "gitlab.example.com|*.corp.example.com" {
		t.Errorf("hosts = %q", got)
	}
	if len(o.servers.values) != 2 {
		t.Errorf("servers = %v", o.servers.values)
	}
}

func TestFlagsWinOverTheEnvironment(t *testing.T) {
	o := newFlags()
	args := []string{"--argocd-server=https://flag.example.com", "--scm-allowed-hosts=flag.example.com"}
	if err := o.fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	err := applyEnv(o.fs, env(map[string]string{
		"BACKFLOW_ARGOCD_SERVER": "https://env.example.com", "BACKFLOW_SCM_ALLOWED_HOSTS": "env.example.com",
		"BACKFLOW_WEBHOOK_PORT": "9444",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if o.server != "https://flag.example.com" {
		t.Errorf("server = %q: the flag must win", o.server)
	}
	// A flag that can be repeated is replaced, not added to, by the command line.
	if len(o.hosts.values) != 1 || o.hosts.values[0] != "flag.example.com" {
		t.Errorf("hosts = %v: the environment must not add to a flag that was given", o.hosts.values)
	}
	if o.port != 9444 {
		t.Errorf("a flag that was not given takes the environment: %d", o.port)
	}
}

func TestEnvRepeatableFlagTakesSeveralValues(t *testing.T) {
	o := newFlags()
	if err := o.fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	err := applyEnv(o.fs, env(map[string]string{
		"BACKFLOW_ARGOCD_DEFAULT_ACCESS": "team-a,team-b:team-a;platform:*\nops:ops-project",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(o.access) != 3 {
		t.Fatalf("access = %v, want three rules", o.access)
	}
	if !slices.Contains(o.access[0].PolicyNamespaces, "team-b") || o.access[1].Projects[0] != "*" ||
		o.access[2].PolicyNamespaces[0] != "ops" {
		t.Errorf("access = %v", o.access)
	}
}

func TestEnvEmptyIsUnsetAndUnknownVariablesAreIgnored(t *testing.T) {
	o := newFlags()
	if err := o.fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"BACKFLOW_ARGOCD_SERVER": "  ", "BACKFLOW_NOT_A_FLAG": "x", "ARGOCD_SERVER": "y"}
	if err := applyEnv(o.fs, env(vars)); err != nil {
		t.Fatal(err)
	}
	if o.server != "" {
		t.Errorf("server = %q", o.server)
	}
}

func TestEnvInvalidValueNamesTheVariable(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"a duration":     {"BACKFLOW_MERGE_REQUEST_POLL_INTERVAL": "soon"},
		"a number":       {"BACKFLOW_WEBHOOK_PORT": "many"},
		"a bool":         {"BACKFLOW_ARGOCD_INSECURE_SKIP_TLS_VERIFY": "maybe"},
		"an access rule": {"BACKFLOW_ARGOCD_DEFAULT_ACCESS": "no-colon"},
	} {
		t.Run(name, func(t *testing.T) {
			o := newFlags()
			if err := o.fs.Parse(nil); err != nil {
				t.Fatal(err)
			}
			err := applyEnv(o.fs, env(vars))
			if err == nil {
				t.Fatal("an invalid value was accepted")
			}
			for k := range vars {
				if !strings.Contains(err.Error(), k) {
					t.Errorf("error %q does not name %s", err, k)
				}
			}
		})
	}
}

func TestSecretReadNamespaces(t *testing.T) {
	if got := secretReadNamespaces(nil, "backflow-system", "openshift-gitops"); got != nil {
		t.Errorf("nothing asked for means any namespace, got %v", got)
	}
	got := secretReadNamespaces([]string{"team-a"}, "backflow-system", "openshift-gitops")
	if strings.Join(got, ",") != "team-a,backflow-system,openshift-gitops" {
		t.Errorf("got %v: the operator's namespace and the Argo CD token's must be included", got)
	}
	if got := secretReadNamespaces([]string{"team-a"}, "", ""); len(got) != 1 {
		t.Errorf("got %v", got)
	}
}

// TestConfigurationListsEveryFlag keeps the table of docs/configuration.md (flag, environment variable, Helm value)
// complete: a flag that the operator has and the table does not name fails here.
func TestConfigurationListsEveryFlag(t *testing.T) {
	data, err := os.ReadFile("../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	table := string(data)
	// The flags of the operator are read out of the source files that register them.
	for _, name := range operatorFlagNames(t) {
		if strings.HasPrefix(name, "zap-") || strings.HasPrefix(name, "test.") {
			continue
		}
		env := envName(name)
		if !strings.Contains(table, "`--"+name) && !strings.Contains(table, "`"+env) {
			t.Errorf("--%s (%s) is not in the table of docs/configuration.md", name, env)
		}
	}
}

// operatorFlagNames reads the names of the flags out of cmd/main.go and cmd/actor.go, where each is registered by
// flag.XxxVar(&v, "name", ...).
func operatorFlagNames(t *testing.T) []string {
	t.Helper()
	var names []string
	re := regexp.MustCompile(`flag\.(?:String|Bool|Int|Duration)?Var\([^,]+,\s*"([a-z0-9-]+)"`)
	for _, f := range []string{"main.go", "actor.go"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			names = append(names, m[1])
		}
	}
	if len(names) < 30 {
		t.Fatalf("found %d flags: the pattern no longer matches how they are registered", len(names))
	}
	return names
}
