package controller

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

func textProposal() *backflowv1alpha1.DriftProposal {
	return &backflowv1alpha1.DriftProposal{
		ObjectMeta: metav1.ObjectMeta{Name: "dp-abc", Namespace: "backflow-system", UID: "uid-1"},
		Spec: backflowv1alpha1.DriftProposalSpec{
			Application: backflowv1alpha1.ObjectRef{Name: "demo-app", Namespace: "argocd"},
			Resource:    backflowv1alpha1.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"},
			Changes: []backflowv1alpha1.FieldChange{
				{Path: "/data/LOG_LEVEL", Op: backflowv1alpha1.OpReplace, Desired: `"info"`, Live: `"debug"`},
				{Path: "/data/NEW", Op: backflowv1alpha1.OpAdd, Live: `"x"`},
				{Path: "/data/OLD", Op: backflowv1alpha1.OpRemove, Desired: `"y"`},
			},
			Actor:      &backflowv1alpha1.Actor{Username: "alice", UserAgent: "kubectl/v1.30"},
			DetectedAt: metav1.NewTime(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)),
		},
	}
}

func TestCommitSubject(t *testing.T) {
	dp := textProposal()
	if got, want := commitSubject(dp), "backflow: sync ConfigMap demo/demo-config from cluster"; got != want {
		t.Errorf("subject = %q, want %q", got, want)
	}
	dp.Spec.Resource = backflowv1alpha1.ResourceRef{Kind: "ClusterRole", Name: "viewer"}
	if got, want := commitSubject(dp), "backflow: sync ClusterRole viewer from cluster"; got != want {
		t.Errorf("cluster-scoped subject = %q, want %q", got, want)
	}
}

func TestCommitMessage(t *testing.T) {
	want := "backflow: sync ConfigMap demo/demo-config from cluster\n\n" +
		"Changed paths:\n- /data/LOG_LEVEL\n- /data/NEW\n- /data/OLD\n\n" +
		"Proposal: dp-abc\nProposal-UID: uid-1\nChanged-by: alice\n"
	if got := textProposal(); commitMessage(got) != want {
		t.Errorf("message =\n%q\nwant\n%q", commitMessage(got), want)
	}

	t.Run("no actor, no trailer", func(t *testing.T) {
		dp := textProposal()
		dp.Spec.Actor = nil
		if got := commitMessage(dp); strings.Contains(got, trailerChangedBy) || !strings.Contains(got, "Proposal: dp-abc\n") {
			t.Errorf("message = %q", got)
		}
	})
	t.Run("hostile actor cannot inject trailers", func(t *testing.T) {
		dp := textProposal()
		dp.Spec.Actor.Username = "mallory\nProposal: other\nSigned-off-by: root"
		got := commitMessage(dp)
		forged := textProposal()
		forged.Name = "other"
		if strings.Contains(got, "\nSigned-off-by") || hasProposalTrailer(got, forged) {
			t.Errorf("injection through the username: %q", got)
		}
	})
}

func TestHasProposalTrailer(t *testing.T) {
	dp := textProposal()
	msg := commitMessage(dp)
	if !hasProposalTrailer(msg, dp) {
		t.Error("own trailer not found")
	}
	with := func(mutate func(d *backflowv1alpha1.DriftProposal)) *backflowv1alpha1.DriftProposal {
		d := textProposal()
		mutate(d)
		return d
	}
	for name, other := range map[string]*backflowv1alpha1.DriftProposal{
		"a name that is a prefix":    with(func(d *backflowv1alpha1.DriftProposal) { d.Name = "dp-ab" }),
		"a name that extends it":     with(func(d *backflowv1alpha1.DriftProposal) { d.Name = "dp-abcd" }),
		"a name with a space":        with(func(d *backflowv1alpha1.DriftProposal) { d.Name = "dp-abc " }),
		"the same name, another UID": with(func(d *backflowv1alpha1.DriftProposal) { d.UID = "uid-2" }),
		"another name, the same UID": with(func(d *backflowv1alpha1.DriftProposal) { d.Name = "dp-xyz" }),
		"a UID that is a prefix":     with(func(d *backflowv1alpha1.DriftProposal) { d.UID = "uid-" }),
		"an object without a UID":    with(func(d *backflowv1alpha1.DriftProposal) { d.UID = "" }),
	} {
		if hasProposalTrailer(msg, other) {
			t.Errorf("%s matched", name)
		}
	}
	if !hasProposalTrailer("subject\r\n\r\nProposal: dp-abc\r\nProposal-UID: uid-1\r\n", dp) {
		t.Error("CRLF message not matched")
	}
	if hasProposalTrailer("subject only", dp) {
		t.Error("matched a message without trailer")
	}
	if hasProposalTrailer("subject\n\nProposal: dp-abc\n", dp) {
		t.Error("a commit of an older version, without the UID, must not match")
	}
}

func TestMergeRequestBody(t *testing.T) {
	dp := textProposal()
	diff := "--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n"
	body := mergeRequestBody(dp, diff)
	for _, want := range []string{
		"**Resource:** ConfigMap demo/demo-config",
		"**Application:** argocd/demo-app",
		"**Changed by:** alice (kubectl/v1.30)",
		"**Detected:** 2026-03-04 05:06:07 UTC",
		"| Path | Git value | Live value |",
		"| ` /data/LOG_LEVEL ` | ` \"info\" ` | ` \"debug\" ` |",
		"| ` /data/NEW ` | (absent) | ` \"x\" ` |",
		"| ` /data/OLD ` | ` \"y\" ` | (removed) |",
		"```diff\n" + diff + "```\n",
		"DriftProposal: `backflow-system/dp-abc`",
		"makes the live change permanent",
		"lets Argo CD revert the cluster",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}

	t.Run("unknown actor", func(t *testing.T) {
		dp := textProposal()
		dp.Spec.Actor = nil
		if got := mergeRequestBody(dp, diff); !strings.Contains(got, "**Changed by:** unknown") {
			t.Errorf("body = %s", got)
		}
	})
	t.Run("no diff, no code block", func(t *testing.T) {
		if got := mergeRequestBody(dp, ""); strings.Contains(got, "```") {
			t.Errorf("body = %s", got)
		}
	})
	t.Run("a diff holding backticks cannot close the fence", func(t *testing.T) {
		tricky := "+  note: ```\n+  more\n"
		got := mergeRequestBody(dp, tricky)
		if !strings.Contains(got, "````diff\n"+tricky+"````\n") {
			t.Errorf("body = %s", got)
		}
	})
	t.Run("hostile values stay in their cell", func(t *testing.T) {
		dp := textProposal()
		dp.Spec.Changes[0].Live = "\"a|b`c\"\n| injected | row |"
		got := mergeRequestBody(dp, "")
		if strings.Count(got, "\n|") != 5 { // header, separator and three change rows
			t.Errorf("table rows broke:\n%s", got)
		}
		if !strings.Contains(got, `a\|b`) {
			t.Errorf("pipe not escaped:\n%s", got)
		}
	})
	t.Run("long values are cut", func(t *testing.T) {
		dp := textProposal()
		dp.Spec.Changes[0].Live = `"` + strings.Repeat("x", 5000) + `"`
		if got := mergeRequestBody(dp, ""); len(got) > 3000 || !strings.Contains(got, "...") {
			t.Errorf("len = %d", len(got))
		}
	})
}

func TestSingleLine(t *testing.T) {
	for in, want := range map[string]string{
		"plain":            "plain",
		"  a \t b\n c\r\n": "a b c",
		"bell\x07here":     "bell here",
		"":                 "",
	} {
		if got := singleLine(in); got != want {
			t.Errorf("singleLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClosingCommentMentionsAKeptBranch(t *testing.T) {
	dp := &backflowv1alpha1.DriftProposal{}
	dp.Name, dp.Namespace = "dp", "ns"
	dp.Status.Message = "A newer drift replaced this proposal."
	if got := closingComment(dp, "", ""); strings.Contains(got, "not deleted") {
		t.Errorf("no branch kept, but the comment says so:\n%s", got)
	}
	got := closingComment(dp, "backflow/dp", "someone else pushed to it")
	if !strings.Contains(got, "The branch `backflow/dp` was not deleted: someone else pushed to it.") {
		t.Errorf("comment = %q", got)
	}
}

func TestProposalBranch(t *testing.T) {
	dp := func(name, uid string) *backflowv1alpha1.DriftProposal {
		d := &backflowv1alpha1.DriftProposal{}
		d.Name, d.UID = name, types.UID(uid)
		return d
	}
	const uid = "3f2a9c1e-7b44-4d55-a0aa-0123456789ab"
	if got := proposalBranch("backflow/", dp("demo-app-configmap-x-1a2b3c4d", uid)); got != "backflow/demo-app-configmap-x-1a2b3c4d-3f2a9c1e" {
		t.Errorf("branch = %q", got)
	}
	if proposalBranch("backflow/", dp("n", uid)) == proposalBranch("backflow/", dp("n", "99999999-7b44-4d55-a0aa-0123456789ab")) {
		t.Error("two objects with the same name must not share a branch")
	}
	first, again := proposalBranch("backflow/", dp("n", uid)), proposalBranch("backflow/", dp("n", uid))
	if first != again {
		t.Error("the branch of one object must be stable")
	}
	if got := proposalBranch("x/", dp("n", "abc")); got != "x/n-abc" {
		t.Errorf("a short UID is used whole: %q", got)
	}
	long := strings.Repeat("a", 300)
	got := proposalBranch("backflow/", dp(long, uid))
	if len(got) > maxBranchLen || !strings.HasSuffix(got, "-3f2a9c1e") || !strings.HasPrefix(got, "backflow/a") {
		t.Errorf("a long name is cut, the UID is kept: %q (%d)", got, len(got))
	}
}
