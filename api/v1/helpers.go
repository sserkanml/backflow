/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// First returns the first source of the proposal's Application: the one a single-source
// Application has, and the one Argo CD names first otherwise. The zero value when there is none.
func (s DriftProposalSpec) First() ProposalSource {
	if len(s.Sources) == 0 {
		return ProposalSource{}
	}
	return s.Sources[0]
}

// MultiSource reports whether the Application of the proposal has more than one source.
func (s DriftProposalSpec) MultiSource() bool { return len(s.Sources) > 1 }

// EnsureGit returns status.git, created when absent.
func (s *DriftProposalStatus) EnsureGit() *GitStatus {
	if s.Git == nil {
		s.Git = &GitStatus{}
	}
	return s.Git
}

// EnsurePush returns the push bookkeeping, created when absent.
func (g *GitStatus) EnsurePush() *PushStatus {
	if g.Push == nil {
		g.Push = &PushStatus{}
	}
	return g.Push
}

// GitBranch is status.git.branch, or empty.
func (s DriftProposalStatus) GitBranch() string {
	if s.Git == nil {
		return ""
	}
	return s.Git.Branch
}

// GitCommit is status.git.commitSHA, or empty.
func (s DriftProposalStatus) GitCommit() string {
	if s.Git == nil {
		return ""
	}
	return s.Git.CommitSHA
}

// GitMergeRequest is status.git.mergeRequest, or nil.
func (s DriftProposalStatus) GitMergeRequest() *MergeRequestRef {
	if s.Git == nil {
		return nil
	}
	return s.Git.MergeRequest
}

// PushRejectionCount is status.git.push.rejections, or zero.
func (s DriftProposalStatus) PushRejectionCount() int32 {
	if s.Git == nil || s.Git.Push == nil {
		return 0
	}
	return s.Git.Push.Rejections
}

// PushLastRejectedAt is status.git.push.lastRejectedAt, or nil.
func (s DriftProposalStatus) PushLastRejectedAt() *metav1.Time {
	if s.Git == nil || s.Git.Push == nil {
		return nil
	}
	return s.Git.Push.LastRejectedAt
}

// EnsureSignals returns status.signals, created when absent.
func (s *DriftProposalStatus) EnsureSignals() *Signals {
	if s.Signals == nil {
		s.Signals = &Signals{}
	}
	return s.Signals
}

// Signal returns status.signals, or the zero value.
func (s DriftProposalStatus) Signal() Signals {
	if s.Signals == nil {
		return Signals{}
	}
	return *s.Signals
}

// PathOrChart is the path inside the repository of a Git source, or the chart name of a source
// that comes from a Helm repository or a registry.
func (s SourceSummary) PathOrChart() string {
	if s.Path != "" {
		return s.Path
	}
	return s.Chart
}

// HiddenCount is how many selected Applications are hidden from the policy, of all reasons.
func (s *ArgoCDStatus) HiddenCount() int32 {
	if s == nil {
		return 0
	}
	var n int32
	for _, h := range s.HiddenApplications {
		n += h.Count
	}
	return n
}
