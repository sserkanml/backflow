package gitrepo

import (
	"errors"
	"testing"
	"time"
)

func scratchCommits(t *testing.T, cache *Cache, url, base string, n int) map[string]string {
	t.Helper()
	out := map[string]string{}
	for i := range n {
		m := meta
		m.When = detected.Add(time.Duration(i) * time.Hour)
		sha, err := cache.CommitFile(t.Context(), url, base, nil, "README.md", []byte{byte('a' + i), '\n'}, m)
		if err != nil {
			t.Fatal(err)
		}
		out["backflow/scratch/dp-"+string(rune('a'+i))] = sha
	}
	return out
}

func TestPushBranchesCreatesManyInOnePush(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	branches := scratchCommits(t, cache, src.url(), base, 3)
	if err := cache.PushBranches(t.Context(), src.url(), branches, nil, nil); err != nil {
		t.Fatal(err)
	}
	for name, sha := range branches {
		if got := src.branchSHA(name); got != sha {
			t.Errorf("%s = %s, want %s", name, got, sha)
		}
	}
	if got := src.branchSHA("master"); got != base {
		t.Errorf("the target branch moved to %s", got)
	}

	t.Run("a branch that exists is refused and nothing is created", func(t *testing.T) {
		more := scratchCommits(t, cache, src.url(), base, 1)
		more["backflow/scratch/new"] = more["backflow/scratch/dp-a"]
		delete(more, "backflow/scratch/dp-a")
		more["backflow/scratch/dp-b"] = branches["backflow/scratch/dp-c"]
		err := cache.PushBranches(t.Context(), src.url(), more, nil, nil)
		if !errors.Is(err, ErrBranchExists) {
			t.Fatalf("err = %v", err)
		}
		if got := src.branchSHA("backflow/scratch/new"); got != "" {
			t.Errorf("a branch was created: %s", got)
		}
		if got := src.branchSHA("backflow/scratch/dp-b"); got != branches["backflow/scratch/dp-b"] {
			t.Errorf("an existing branch moved to %s", got)
		}
	})

	t.Run("a commit that is not in the cache", func(t *testing.T) {
		err := cache.PushBranches(t.Context(), src.url(), map[string]string{"backflow/scratch/x": base[:39] + "0"}, nil, nil)
		if !errors.Is(err, ErrRevisionNotFound) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestListAndDeleteBranches(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	branches := scratchCommits(t, cache, src.url(), base, 3)
	branches["backflow/dp-keep"] = branches["backflow/scratch/dp-a"] // not under the prefix
	if err := cache.PushBranches(t.Context(), src.url(), branches, nil, nil); err != nil {
		t.Fatal(err)
	}

	got, err := cache.ListBranches(t.Context(), src.url(), "backflow/scratch/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "backflow/scratch/dp-a" || !got[1].When.Equal(detected.Add(time.Hour)) {
		t.Errorf("branches = %+v", got)
	}
	if _, err := cache.ListBranches(t.Context(), src.url(), "", nil); err == nil {
		t.Error("an empty prefix was accepted")
	}

	// Deleting skips a branch that is not there, and touches nothing else.
	if err := cache.DeleteBranches(t.Context(), src.url(), []string{"backflow/scratch/dp-a", "backflow/scratch/gone", "backflow/scratch/dp-b"}, nil); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"backflow/scratch/dp-a": false, "backflow/scratch/dp-b": false, "backflow/scratch/dp-c": true,
		"backflow/dp-keep": true, "master": true,
	} {
		if (src.branchSHA(name) != "") != want {
			t.Errorf("%s exists = %v, want %v", name, !want, want)
		}
	}
	if err := cache.DeleteBranches(t.Context(), src.url(), nil, nil); err != nil {
		t.Errorf("deleting nothing: %v", err)
	}
}

func TestPushBranchesWithPushOptions(t *testing.T) {
	src, base := seed(t)
	cache := NewCache(t.TempDir())
	branches := scratchCommits(t, cache, src.url(), base, 1)
	err := cache.PushBranches(t.Context(), src.url(), branches, map[string]string{"ci.skip": ""}, nil)
	if err != nil && !errors.Is(err, ErrPushOptionsNotSupported) {
		t.Fatal(err)
	}
	// A remote that takes the option creates the branch; one that does not says
	// so and creates nothing.
	got := src.branchSHA("backflow/scratch/dp-a")
	if (err == nil) != (got != "") {
		t.Errorf("err = %v, branch = %q", err, got)
	}
}
