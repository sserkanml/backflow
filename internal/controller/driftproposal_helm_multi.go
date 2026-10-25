package controller

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/argocd"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/helmmap"
	"github.com/sserkanml/backflow/internal/rendermap"
	"github.com/sserkanml/backflow/internal/scm"
	"github.com/sserkanml/backflow/internal/scratch"
)

// Reasons of the Mapped condition for the proposals of multi-source Applications.
const (
	reasonMultipleValueRepositories = "MultipleValueRepositories"
	reasonValueFileNotInGit         = "ValueFileNotInGit"
	reasonScratchBranchesDisabled   = "ScratchBranchesDisabled"
	reasonScratchBranchRejected     = "ScratchBranchRejected"
	reasonArgoCDUnsupported         = "ArgoCDUnsupported"
	reasonChartDefaultsUnavailable  = "ChartDefaultsUnavailable"
)

// minArgoCDVersion is the oldest Argo CD whose manifests can be rendered by
// revision per source (revisions and sourcePositions).
const minArgoCDVersion = "2.6"

// ChartArgo is the part of the Argo CD client that mapping a multi-source
// Application uses: the values of a chart, and a render by revision.
// *argocd.Client implements it.
type ChartArgo interface {
	GetHelmDetails(ctx context.Context, appName, appNamespace, appProject string, source map[string]any, index int) (*argocd.HelmDetails, error)
	GetManifestsByRevisions(ctx context.Context, q argocd.RevisionsQuery) (*argocd.ManifestsResponse, error)
}

// chartArgo returns the policy's Argo CD client for charts and renders by
// revision, cached per policy like the renderer.
func (r *DriftProposalReconciler) chartArgo(ctx context.Context, policy *backflowv1alpha1.BackflowPolicy) (ChartArgo, error) {
	if policy.Spec.ArgoCD == nil {
		return nil, &heldError{reason: reasonArgoCDNotConfigured, cause: fmt.Errorf("BackflowPolicy %q has no spec.argoCD", policy.Name)}
	}
	cfg, err := argoCDConfig(ctx, r.Client, policy)
	if err != nil {
		return nil, &heldError{reason: reasonArgoCDCredentials, cause: err}
	}
	build := r.NewChartArgo
	if build == nil {
		build = func(c argocd.Config) (ChartArgo, error) { return argocd.New(c) }
	}
	return r.chartClients.get(policy.Namespace+"/"+policy.Name, cfg, build)
}

// valuesSource is the one source whose files hold the values and receive the
// edits.
func valuesSource(dp *backflowv1alpha1.DriftProposal, helm map[string]any, render backflowv1alpha1.ApplicationSource) (backflowv1alpha1.ApplicationSource, error) {
	var found []backflowv1alpha1.ApplicationSource
	for _, s := range dp.Spec.Sources {
		if s.Role == backflowv1alpha1.RoleValues {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		for _, f := range valueFileRefs(helm) {
			if f.Ref == "" && render.Chart != "" {
				return backflowv1alpha1.ApplicationSource{}, &verdictError{reason: reasonValueFileNotInGit, err: fmt.Errorf(
					"the value file %s of source %d is inside the chart %s from %s, which is not in a Git repository, so Backflow cannot edit it; "+
						"only value files in a Git repository ($ref/...) are edited",
					f.Raw, render.Position, render.Chart, scm.RedactURL(render.RepoURL))}
			}
		}
		return backflowv1alpha1.ApplicationSource{}, &verdictError{reason: rendermap.ReasonNoVerifiedCandidate,
			err: errors.New("no value file of the Helm source is in a ref source ($ref/path), so there is no file of a Git repository to edit")}
	}
	names := make([]string, len(found))
	for i, s := range found {
		names[i] = "$" + s.Ref + " (" + scm.RedactURL(s.RepoURL) + ")"
	}
	return backflowv1alpha1.ApplicationSource{}, &verdictError{reason: reasonMultipleValueRepositories,
		err: fmt.Errorf("the value files come from %d repositories (%s); a proposal is written to one repository, so only one is supported",
			len(found), strings.Join(names, ", "))}
}

// translateValueFiles turns the value files and file parameters of the Helm
// source into paths of the repository of the ref source they all must come from
// (a path with a leading slash is relative to the repository root). A value file
// inside a chart that is not in Git (from a Helm repository or a registry) cannot
// be edited at all (ValueFileNotInGit); one inside a chart in Git, or in another
// ref source, is in a second repository (MultipleValueRepositories).
func translateValueFiles(helm map[string]any, ref string, render backflowv1alpha1.ApplicationSource) (valueFiles []string, params []helmmap.FileParameter, err error) {
	for _, f := range valueFileRefs(helm) {
		switch {
		case f.Ref == ref:
			continue
		case f.Ref == "" && render.Chart != "":
			return nil, nil, &verdictError{reason: reasonValueFileNotInGit, err: fmt.Errorf(
				"the value file %s of source %d is inside the chart %s from %s, which is not in a Git repository, so Backflow cannot edit it; "+
					"only value files in a Git repository ($%s/...) are edited",
				f.Raw, render.Position, render.Chart, scm.RedactURL(render.RepoURL), ref)}
		case f.Ref == "":
			return nil, nil, &verdictError{reason: reasonMultipleValueRepositories, err: fmt.Errorf(
				"the value file %s is inside the chart in %s, a second Git repository besides the one of $%s that receives the edits",
				f.Raw, scm.RedactURL(render.RepoURL), ref)}
		}
		return nil, nil, &verdictError{reason: reasonMultipleValueRepositories, err: fmt.Errorf(
			"the value file %s is in the repository of $%s, a second Git repository besides the one of $%s that receives the edits", f.Raw, f.Ref, ref)}
	}
	files, _, _ := unstructured.NestedStringSlice(helm, "valueFiles")
	for _, raw := range files {
		valueFiles = append(valueFiles, "/"+splitRefPath(raw).Path)
	}
	items, _, _ := unstructured.NestedSlice(helm, "fileParameters")
	for _, it := range items {
		m, _ := it.(map[string]any)
		params = append(params, helmmap.FileParameter{Name: strField(m, "name"), Path: "/" + splitRefPath(strField(m, "path")).Path})
	}
	return valueFiles, params, nil
}

// chartDefaults reads the chart's own values without its files: from Argo CD for
// a chart from a Helm repository or a registry, from the chart's repository for
// a chart in Git. Subcharts' defaults are not available, and are reported as
// unread.
func (r *DriftProposalReconciler) chartDefaults(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	policy *backflowv1alpha1.BackflowPolicy, app *unstructured.Unstructured, argo ChartArgo,
	render backflowv1alpha1.ApplicationSource) (*helmmap.ExternalChart, error) {
	ext := &helmmap.ExternalChart{Name: render.Chart, Version: render.Revision}
	sources := specSources(app)
	if render.Chart != "" {
		details, err := argo.GetHelmDetails(ctx, app.GetName(), app.GetNamespace(), strField(mapOf(app, "spec"), "project"),
			sources[render.Position-1], int(render.Position-1))
		switch {
		case errors.Is(err, argocd.ErrUnauthorized):
			return nil, &heldError{reason: reasonChartDefaultsUnavailable, cause: fmt.Errorf(
				"argo CD does not let the account read the chart %s: it needs applications get and repositories get (p, role:backflow, repositories, get, *, allow): %w", render.Chart, err)}
		case errors.Is(err, argocd.ErrAppDetailsFailed):
			return nil, &heldError{reason: reasonChartDefaultsUnavailable, cause: err}
		case err != nil:
			return nil, argoFailure(err)
		}
		ext.Values = []byte(details.Values)
		ext.UnreadDependencies = helmmap.SubchartNames(details.ValueFiles)
		return ext, nil
	}

	// A chart in Git: its values.yaml at the revision the Application compares against.
	ext.Name = strings.Trim(render.Path, "/")
	auth, err := r.authForRepo(ctx, dp, policy, render.RepoURL)
	if err != nil {
		return nil, err
	}
	tree, err := r.Repos.Open(ctx, render.RepoURL, render.Revision, auth)
	if err != nil {
		if errors.Is(err, gitrepo.ErrAuth) || errors.Is(err, gitrepo.ErrRepositoryNotFound) {
			return nil, &heldError{reason: reasonRepositoryAuthFailed, cause: fmt.Errorf("the chart repository %s: %w", scm.RedactURL(render.RepoURL), err)}
		}
		return nil, &heldError{reason: reasonRepositoryUnavailable, cause: err}
	}
	defer func() { _ = tree.Close() }()
	dir := strings.Trim(strings.TrimPrefix(render.Path, "./"), "/")
	if dir == "." {
		dir = ""
	}
	read := func(name string) []byte {
		b, _ := fs.ReadFile(tree, strings.TrimPrefix(dir+"/"+name, "/"))
		return b
	}
	ext.Values = read("values.yaml")
	ext.UnreadDependencies = helmmap.DependencyNames(slices.Concat(read("Chart.yaml"), []byte("\n"), read("requirements.yaml")))
	return ext, nil
}

// mapOf reads a map of the object, empty when there is none.
func mapOf(app *unstructured.Unstructured, path ...string) map[string]any {
	m, _, _ := unstructured.NestedMap(app.Object, path...)
	return m
}

// authForRepo returns the credentials of the ScmConnection matched to a
// repository of the proposal's Application, or nil.
func (r *DriftProposalReconciler) authForRepo(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	policy *backflowv1alpha1.BackflowPolicy, repoURL string) (*gitrepo.Auth, error) {
	access, err := scmAccessForRepo(ctx, r.Client, dp.Namespace, policy, dp.Spec.Application.Name, repoURL)
	if err != nil {
		return nil, &heldError{reason: reasonRepositoryAuthFailed, cause: err}
	}
	if access == nil {
		return nil, nil
	}
	return access.auth, nil
}

// scratchPushOptions are the push options of the provider that keep a scratch
// branch from running a pipeline. GitHub has none; its commit message carries
// [skip ci].
func scratchPushOptions(provider backflowv1alpha1.ScmProvider) map[string]string {
	if provider == backflowv1alpha1.ScmProviderGitLab {
		return map[string]string{"ci.skip": ""}
	}
	return nil
}

// scratchPrefix is the prefix of the scratch branches of one proposal.
func scratchPrefix(dp *backflowv1alpha1.DriftProposal) string {
	uid := strings.ReplaceAll(string(dp.UID), "-", "")
	if len(uid) < 8 {
		uid = fmt.Sprintf("%-8s", uid)
	}
	return scratch.BranchPrefix + strings.ReplaceAll(uid[:8], " ", "0") + "-"
}

// mapHelmMulti traces a proposal of a multi-source Application back to the value
// files of its ref source. The Application is rendered by Argo CD from commits:
// each candidate edit is committed to a scratch branch of the repository of the
// value files, rendered by its SHA with every other source pinned to the
// revision recorded in the proposal, and the branches are deleted again. The
// edit is accepted only when the render shows it changes exactly the drifted
// fields (see helmmap.Verify). tree is the repository of the value files at sha.
// Errors are as for mapHelm.
func (r *DriftProposalReconciler) mapHelmMulti(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	app *unstructured.Unstructured, tree fs.FS, sha string) (*mappedChange, error) {
	log := logf.FromContext(ctx)
	ms, why := helmMultiSource(app)
	if ms == nil {
		return nil, &verdictError{reason: reasonUnsupportedSourceType, err: errors.New(unsupportedMultiSource(why))}
	}
	render := dp.Spec.Sources[ms.Render]
	helm, _ := specSources(app)[ms.Render]["helm"].(map[string]any)
	values, err := valuesSource(dp, helm, render)
	if err != nil {
		return nil, err
	}
	valueFiles, fileParams, err := translateValueFiles(helm, values.Ref, render)
	if err != nil {
		return nil, err
	}

	policy, err := r.policyOf(ctx, dp)
	if err != nil {
		return nil, err
	}
	if !policy.Spec.ScratchBranchesAllowed() {
		return nil, &verdictError{reason: reasonScratchBranchesDisabled, err: fmt.Errorf(
			"an Application with several sources is rendered from commits, so verifying an edit needs scratch branches in %s, and spec.mapping.scratchBranches of the policy does not allow them (it is %s, and the policy mode is %s); set it to Always to allow them",
			scm.RedactURL(values.RepoURL), scratchMode(policy), policy.Spec.Mode)}
	}

	argo, err := r.chartArgo(ctx, policy)
	if err != nil {
		return nil, err
	}
	ext, err := r.chartDefaults(ctx, dp, policy, app, argo, render)
	if err != nil {
		return nil, err
	}

	spec := withInlineHelm(helmSpecFrom(app, helm, values.RepoURL, "", sha), helm)
	spec.ValueFiles, spec.FileParameters, spec.External = valueFiles, fileParams, ext
	src, err := helmmap.NewSource(tree, spec)
	if err != nil {
		if reason := helmmap.ReasonOf(err); reason != "" {
			return nil, &verdictError{reason: reason, err: err}
		}
		return nil, err
	}
	src.OverrideTarget = ""
	if policy.Spec.Helm != nil && policy.Spec.Helm.TargetValuesFile != "" {
		src.OverrideTarget = "/" + strings.TrimPrefix(splitRefPath(policy.Spec.Helm.TargetValuesFile).Path, "/")
	}
	files, err := helmmap.CollectFiles(tree, src)
	if err != nil {
		if reason := helmmap.ReasonOf(err); reason != "" {
			return nil, &verdictError{reason: reason, err: err}
		}
		return nil, err
	}
	fields, err := helmmap.FieldsFor(src, files, dp.Spec.Changes)
	if err != nil {
		return nil, &verdictError{reason: helmmap.ReasonInvalidInlineValues, err: err}
	}

	access, err := r.accessFor(ctx, dp, policy)
	if err != nil {
		return nil, &heldError{reason: reasonRepositoryAuthFailed, cause: err}
	}
	if access == nil {
		return nil, &heldError{reason: reasonMissingScmConnection, cause: fmt.Errorf(
			"no ScmConnection matches the repository %s that holds the value files, and scratch branches need to push to it", scm.RedactURL(values.RepoURL))}
	}
	revisions := make([]string, len(dp.Spec.Sources))
	for i, s := range dp.Spec.Sources {
		revisions[i] = s.Revision
	}
	prefix := scratchPrefix(dp)
	rend := scratch.New(scratch.Config{
		Git: r.Writer, Argo: argo, Repo: values.RepoURL, Auth: access.auth, Parent: sha, Base: files,
		AppName: app.GetName(), AppNamespace: app.GetNamespace(), Revisions: revisions, ValuesPosition: int(values.Position),
		BranchPrefix: prefix, PushOptions: scratchPushOptions(access.conn.Spec.Provider), MessageSuffix: "[skip ci]",
		Committer: committerOf(access), Now: r.now,
	})
	// Branches a crashed attempt of this proposal left behind would collide with
	// the names of this one.
	if _, err := scratch.SweepPrefix(ctx, r.Writer, values.RepoURL, access.auth, prefix); err != nil {
		return nil, scratchFailure(err)
	}
	defer func() {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := rend.Finish(fctx); err != nil {
			log.Error(err, "The scratch branches were not deleted; the sweeper deletes them within the hour",
				"proposal", dp.Name, "repository", scm.RedactURL(values.RepoURL))
		}
	}()

	res := dp.Spec.Resource
	out, err := r.verify(ctx, dp, rend, helmmap.Input{
		App:      helmAppOf(app),
		Resource: rendermap.ResourceKey{Group: res.Group, Kind: res.Kind, Namespace: res.Namespace, Name: res.Name},
		Fields:   fields, Source: src, Files: files,
	}, rendermap.Options{
		AllowCollateral: allowedCollateral(policy), AllowPartial: policy.Spec.PartialAllowed(),
		AddOverrides: policy.Spec.OverridesAllowed(),
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, scratchFailure(err)
	}
	if !out.Mapped() {
		msg := out.Unmapped.Message
		if strings.Contains(msg, "in the Application, not by a value file") {
			msg += fmt.Sprintf(" (it is set inline in the Application, in spec.sources[%d].helm; Backflow does not map values set in the Application yet)", ms.Render)
		}
		return nil, &verdictError{reason: out.Unmapped.Reason, err: errors.New(msg)}
	}
	change := &mappedChange{strategy: backflowv1alpha1.StrategyHelmValues, results: out.Results, collateral: out.Collateral,
		overrides: out.Overrides, renders: out.Renders, repoURL: values.RepoURL}
	for _, l := range out.LeftOut {
		change.leftOut = append(change.leftOut, leftOutField{path: l.Pointer, reason: l.Reason, message: l.Detail})
	}
	return change, nil
}

func scratchMode(policy *backflowv1alpha1.BackflowPolicy) backflowv1alpha1.ScratchBranchesMode {
	if policy.Spec.Mapping != nil && policy.Spec.Mapping.ScratchBranches != "" {
		return policy.Spec.Mapping.ScratchBranches
	}
	return backflowv1alpha1.ScratchBranchesAuto
}

// committerOf is the identity of the commits Backflow makes with a connection.
func committerOf(access *scmAccess) gitrepo.Signature {
	if c := access.conn.Spec.Committer; c != nil && c.Name != "" && c.Email != "" {
		return gitrepo.Signature{Name: c.Name, Email: c.Email}
	}
	return gitrepo.Signature{Name: defaultCommitterName, Email: defaultCommitterEmail}
}

// scratchFailure turns an error of a verification with scratch branches into a
// verdict about the source or a reason to try again.
func scratchFailure(err error) error {
	switch {
	case errors.Is(err, argocd.ErrUnsupported):
		return &verdictError{reason: reasonArgoCDUnsupported, err: fmt.Errorf(
			"this Argo CD cannot render a multi-source Application by revision (revisions and sourcePositions need Argo CD %s or later): %w", minArgoCDVersion, err)}
	case errors.Is(err, gitrepo.ErrPushRejected):
		return &verdictError{reason: reasonScratchBranchRejected, err: fmt.Errorf(
			"the repository refused the scratch branches (%s*): a protected branch rule or a push hook; allow Backflow to push them, or set spec.mapping.scratchBranches to Never: %w", scratch.BranchPrefix, err)}
	case errors.Is(err, scratch.ErrTooManyBranches):
		return &verdictError{reason: rendermap.ReasonTooManyCandidates, err: err}
	case errors.Is(err, gitrepo.ErrAuth):
		return &heldError{reason: reasonRepositoryAuthFailed, cause: err}
	}
	return argoFailure(err)
}

// verify runs helmmap.Verify with the renderer, within the render budget of the
// operator (Options.Budget unless the options set one), and says at V(0) how it
// went: the renders, and the wall time.
func (r *DriftProposalReconciler) verify(ctx context.Context, dp *backflowv1alpha1.DriftProposal, rend rendermap.Renderer,
	in helmmap.Input, opts rendermap.Options) (*rendermap.Outcome, error) {
	if opts.Budget == 0 {
		opts.Budget = r.RenderBudget
	}
	start := time.Now()
	out, err := helmmap.Verify(ctx, rend, in, opts)
	args := []any{"proposal", dp.Name, "application", dp.Spec.Application.Name, "elapsed", time.Since(start).Round(time.Millisecond).String()}
	if out != nil {
		args = append(args, "renders", out.Renders, "mapped", out.Mapped())
		if !out.Mapped() {
			args = append(args, "reason", out.Unmapped.Reason)
		}
	}
	if s, ok := rend.(interface{ Stats() (int, int, int) }); ok {
		branches, pushes, _ := s.Stats()
		args = append(args, "scratchBranches", branches, "pushes", pushes)
	}
	if err != nil {
		args = append(args, "error", redactText(err.Error()))
	}
	logf.FromContext(ctx).Info("Verification finished", args...)
	return out, err
}
