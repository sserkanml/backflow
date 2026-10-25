package controller

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/scm"
	"github.com/sserkanml/backflow/internal/scratch"
)

// Defaults of ScratchSweeper.
const (
	DefaultSweepInterval = 15 * time.Minute
	DefaultScratchMaxAge = time.Hour
)

// ScratchSweeper deletes the scratch branches that a verification left behind
// (the operator stopped before it could delete them): branches with the prefix
// backflow/scratch/ whose tip is older than MaxAge, in the repositories of the
// ref sources of the Applications of policies that allow scratch branches. No
// other branch is looked at.
type ScratchSweeper struct {
	Client client.Reader
	Git    scratch.Git
	// Interval and MaxAge default to DefaultSweepInterval and DefaultScratchMaxAge.
	Interval, MaxAge time.Duration
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

var _ manager.Runnable = (*ScratchSweeper)(nil)

// NeedLeaderElection implements manager.LeaderElectionRunnable: only the leader
// sweeps.
func (s *ScratchSweeper) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable: it sweeps every Interval until ctx ends.
func (s *ScratchSweeper) Start(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.Sweep(ctx)
		}
	}
}

// Sweep makes one pass over the repositories and logs what it deleted.
func (s *ScratchSweeper) Sweep(ctx context.Context) {
	log := logf.FromContext(ctx).WithName("scratch-sweeper")
	maxAge := s.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultScratchMaxAge
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	var policies backflowv1alpha1.BackflowPolicyList
	if err := s.Client.List(ctx, &policies); err != nil {
		log.Error(err, "Cannot list the policies")
		return
	}
	done := map[string]bool{}
	for i := range policies.Items {
		policy := &policies.Items[i]
		if !policy.Spec.ScratchBranchesAllowed() {
			continue
		}
		for _, app := range policy.Status.Applications {
			for _, src := range app.Sources {
				key := policy.Namespace + "\x00" + src.RepoURL
				if src.Ref == "" || src.ScmConnection == "" || done[key] {
					continue
				}
				done[key] = true
				access, err := scmAccessForRepo(ctx, s.Client, policy.Namespace, policy, app.Name, src.RepoURL)
				if err != nil || access == nil {
					continue
				}
				deleted, err := scratch.Sweep(ctx, s.Git, src.RepoURL, access.auth, maxAge, now)
				if err != nil {
					log.V(1).Info("Cannot sweep a repository", "repository", scm.RedactURL(src.RepoURL), "error", redactText(err.Error()))
					continue
				}
				if len(deleted) > 0 {
					log.Info("Deleted scratch branches that were left behind", "repository", scm.RedactURL(src.RepoURL), "branches", deleted)
				}
			}
		}
	}
}
