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
	ExitDone  = 0
	ExitError = 1 // an error, or an object that could not be migrated: run again after looking at the output
)

const usage = `usage: backflow migrate [--backup-dir DIR] [flags]

Moves the Backflow objects to backflow.io/v1 and sets storedVersions of the CRDs to [v1]. Run it BEFORE you apply the CRDs of
this release: it adds v1 to the CRDs that are installed itself. Two cases, which a cluster can also have together:

* Objects stored as v1beta1 (the release 0.18): v1 has the same schema, so each is stored again as v1 with an update that changes
  nothing. This is safe with the operator running, needs no backup, and is the whole migration. (v1beta1 stays served until 2.0.)
* Objects stored as v1alpha1 (the release 0.17 and earlier): ScmConnections and BackflowPolicies are rewritten in place as v1,
  DriftProposals deleted and created again with their status (their spec is immutable); ChangeRecords are deleted and not backed
  up. Before anything is changed a backup of every object is written to DIR (one YAML file per object, never overwritten;
  required in this case), DIR/DriftProposal/*.done says which proposals are complete, so a run that was interrupted is finished by
  the next one. The operator must be stopped (--stop-operator does it) and v1alpha1 is removed from the CRDs when none is left.

A second run does only what is left. Exit status: 0 everything is done, 1 an error or an object that could not be migrated.
The other subcommands: convert-files rewrites manifests in Git; restore creates the objects of a backup again (a rollback).

flags:
`

// Main runs the subcommand and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "convert-files" {
		return ConvertFilesMain(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "restore" {
		return restoreMain(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opt Options
	var kubeconfig, kubeContext string
	fs.StringVar(&opt.BackupDir, "backup-dir", "", "the directory for the backup; required unless --dry-run")
	fs.BoolVar(&opt.DryRun, "dry-run", false, "print what would change, change nothing")
	fs.BoolVar(&opt.StopOperator, "stop-operator", false,
		"scale the operator's Deployment to zero when there is something to migrate, and wait until it is gone")
	fs.BoolVar(&opt.RestoreOperator, "restore-operator", false,
		"only scale the operator back to the replicas --stop-operator remembered")
	fs.BoolVar(&opt.SkipFinished, "skip-finished", false,
		"do not migrate DriftProposals that are finished (Merged, Rejected, Reverted, Superseded, Failed): back them up and delete them")
	fs.StringVar(&opt.OperatorNamespace, "operator-namespace", "", "where to look for the running operator; default: every namespace")
	fs.StringVar(&opt.OperatorSelector, "operator-selector", "", "the label selector of the operator's Deployment; default app.kubernetes.io/name=backflow")
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
	if opt.RestoreOperator {
		return ExitDone
	}
	return Summarize(stdout, opt, rep)
}

// Summarize prints the last lines of a run and returns the exit code.
func Summarize(out io.Writer, opt Options, rep *Report) int {
	verb, n := "converted", rep.Converted
	if opt.DryRun {
		verb, n = "would be converted", rep.Planned
	}
	_, _ = fmt.Fprintf(out, "\nsummary: %d object(s) %s, %d already v1, %d failed, %d ChangeRecord(s) dropped",
		n, verb, rep.Skipped, rep.Failed, rep.ChangeRecordsDropped)
	if rep.Restored > 0 {
		_, _ = fmt.Fprintf(out, ", %d object(s) stored again from v1beta1 as v1", rep.Restored)
	}
	if rep.ProposalsDropped > 0 {
		_, _ = fmt.Fprintf(out, ", %d finished DriftProposal(s) dropped (--skip-finished)", rep.ProposalsDropped)
	}
	if rep.ProposalsRestored > 0 {
		_, _ = fmt.Fprintf(out, ", %d DriftProposal(s) created again from the backup", rep.ProposalsRestored)
	}
	_, _ = fmt.Fprintln(out)
	if rep.Failed > 0 {
		_, _ = fmt.Fprintln(out, "NOT FINISHED: fix the objects named above (the backup has their v1alpha1 form) and run again; what is done is not done twice")
		return ExitError
	}
	return ExitDone
}

// connect builds the client the subcommands use.
func connect(kubeconfig, kubeContext string) (client.Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot load the kubeconfig: %w", err)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, coordinationv1.AddToScheme, apiextensionsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("cannot connect to the cluster: %w", err)
	}
	return c, nil
}

// restoreMain runs `migrate restore`.
func restoreMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("backup-dir", "", "the directory of the backup `migrate` wrote")
	kubeconfig := fs.String("kubeconfig", "", "path to a kubeconfig; default: KUBECONFIG, then ~/.kube/config")
	kubeContext := fs.String("context", "", "the kubeconfig context to use")
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, restoreUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitDone
		}
		return ExitError
	}
	c, err := connect(*kubeconfig, *kubeContext)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ERROR: %v\n", err)
		return ExitError
	}
	rep, err := Restore(context.Background(), c, *dir, stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ERROR: %v\n", err)
		return ExitError
	}
	_, _ = fmt.Fprintf(stdout, "\nsummary: %d restored, %d existed, %d failed\n", rep.Created, rep.Existing, rep.Failed)
	if rep.Failed > 0 {
		return ExitError
	}
	return ExitDone
}
