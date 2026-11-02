package migrate

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Exit codes of `backflow migrate`.
const (
	ExitDone       = 0
	ExitError      = 1
	ExitIncomplete = 2 // everything that this version can do is done, and DriftProposals are left
)

const usage = `usage: backflow migrate --backup-dir DIR [flags]

Rewrites the ScmConnections and BackflowPolicies of backflow.io/v1alpha1 to v1beta1 in place, and sets storedVersions of their CRDs
to [v1beta1] when every object is done. Run it with the operator stopped. It refuses to run while the operator is running.

Before anything is changed a backup of every object that is still v1alpha1 is written to DIR (one YAML file per object, never
overwritten). ChangeRecords are deleted and not backed up. DriftProposals are backed up and left as they are; while one is left
the storedVersions of its CRD is not changed and the exit status is 2. A second run converts what is left.

flags:
`

// Main runs the subcommand and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opt Options
	var kubeconfig, kubeContext string
	fs.StringVar(&opt.BackupDir, "backup-dir", "", "the directory for the backup; required unless --dry-run")
	fs.BoolVar(&opt.DryRun, "dry-run", false, "print what would change, change nothing")
	fs.BoolVar(&opt.ServeV1alpha1, "serve-v1alpha1", false,
		"serve v1alpha1 in the CRDs while the objects are migrated and take it out again; without it the CRDs must serve v1alpha1")
	fs.StringVar(&opt.OperatorNamespace, "operator-namespace", "", "where to look for the running operator; default: every namespace")
	fs.StringVar(&kubeconfig, "kubeconfig", "", "path to a kubeconfig; default: KUBECONFIG, then ~/.kube/config")
	fs.StringVar(&kubeContext, "context", "", "the kubeconfig context to use")
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitDone
		}
		return ExitError
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return ExitError
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cannot load the kubeconfig: %v\n", err)
		return ExitError
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, coordinationv1.AddToScheme, apiextensionsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return ExitError
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cannot connect to the cluster: %v\n", err)
		return ExitError
	}

	opt.Out = stdout
	rep, err := Run(context.Background(), c, opt)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ERROR: %v\n", err)
		return ExitError
	}
	return Summarize(stdout, opt, rep)
}

// Summarize prints the last lines of a run and returns the exit code.
func Summarize(out io.Writer, opt Options, rep *Report) int {
	verb, n := "converted", rep.Converted
	if opt.DryRun {
		verb, n = "would be converted", rep.Planned
	}
	_, _ = fmt.Fprintf(out, "\nsummary: %d object(s) %s, %d already v1beta1, %d failed, %d ChangeRecord(s) dropped, %d DriftProposal(s) left as v1alpha1\n",
		n, verb, rep.Skipped, rep.Failed, rep.ChangeRecordsDropped, len(rep.Proposals))
	switch {
	case rep.Failed > 0:
		_, _ = fmt.Fprintln(out, "NOT FINISHED: fix the objects named above (the backup has their v1alpha1 form) and run again; what is done is not done twice")
		return ExitError
	case len(rep.Proposals) > 0:
		_, _ = fmt.Fprintln(out, "NOT FINISHED: DriftProposals are left; storedVersions of their CRD is unchanged, and the operator will not start yet")
		return ExitIncomplete
	}
	return ExitDone
}
