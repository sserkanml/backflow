package chart

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A fake opm: init writes the package, render writes a bundle named after what it was given (and fails for an
// image that is "missing"), validate succeeds. It records what it rendered.
const fakeOpm = `#!/usr/bin/env bash
case "$1" in
  init) printf -- '---\nschema: olm.package\nname: backflow\n' ;;
  validate) exit 0 ;;
  render)
    shift
    while [[ "$1" == --* && "$1" != --output ]]; do shift; done
    arg="$1"
    echo "$arg" >> "$RENDERED"
    case "$arg" in *missing*) echo "no such image" >&2; exit 1 ;; esac
    if [[ -d "$arg" ]]; then image='""'; name="this"; else image="$arg"; name="${arg##*:}"; fi
    printf -- '---\nschema: olm.bundle\nname: backflow.v%s\nimage: %s\n' "$name" "$image" ;;
esac
`

// catalogRepo is a git repository with the script, a bundle/ and tags, and a fake opm.
func catalogRepo(t *testing.T, tags ...string) (dir, opm string, env []string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	script, err := os.ReadFile("../../hack/olm-catalog.sh")
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		"hack/olm-catalog.sh": string(script), "bundle/manifests/x.yaml": "x", "docs/olm/icon.svg": "<svg/>",
		"fake-opm": fakeOpm,
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	run("add", ".")
	run("commit", "-q", "-m", "x")
	for _, tag := range tags {
		run("tag", tag)
	}
	return dir, filepath.Join(dir, "fake-opm"), []string{"RENDERED=" + filepath.Join(dir, "rendered")}
}

func runCatalog(t *testing.T, dir, opm string, env []string, version string, extra ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(filepath.Join(dir, "hack/olm-catalog.sh"))
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), env...), "OPM="+opm, "BUNDLE_VERSION="+version,
		"BUNDLE_IMG=ghcr.io/sserkanml/backflow-bundle:"+version)
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCatalogHasTheReleasedVersionsFromTheTags(t *testing.T) {
	// 0.18.1 is before the first bundle, the release candidate is not a release, 2.0.0 is after the one being made.
	dir, opm, env := catalogRepo(t, "v0.18.1", "v1.0.0", "v1.0.1", "v1.1.0-rc.1", "v2.0.0", "not-a-version")
	if out, err := runCatalog(t, dir, opm, env, "1.0.2"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	catalog, _ := os.ReadFile(filepath.Join(dir, "catalog/out/backflow/catalog.yaml"))
	for _, want := range []string{
		"name: backflow.v1.0.0\nimage: ghcr.io/sserkanml/backflow-bundle:1.0.0",
		"name: backflow.v1.0.1\nimage: ghcr.io/sserkanml/backflow-bundle:1.0.1",
		"image: ghcr.io/sserkanml/backflow-bundle:1.0.2",
		"  - name: backflow.v1.0.0\n  - name: backflow.v1.0.1\n    replaces: backflow.v1.0.0\n" +
			"  - name: backflow.v1.0.2\n    replaces: backflow.v1.0.1\n",
	} {
		if !strings.Contains(string(catalog), want) {
			t.Errorf("the catalog lacks %q:\n%s", want, catalog)
		}
	}
	for _, not := range []string{"0.18.1", "rc.1", "2.0.0"} {
		if strings.Contains(string(catalog), "backflow.v"+not) {
			t.Errorf("the catalog has %s", not)
		}
	}
}

func TestCatalogOfTheFirstReleaseIsAloneInItsChannel(t *testing.T) {
	dir, opm, env := catalogRepo(t, "v0.18.1")
	if out, err := runCatalog(t, dir, opm, env, "1.0.0"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	catalog, _ := os.ReadFile(filepath.Join(dir, "catalog/out/backflow/catalog.yaml"))
	got := string(catalog)
	if !strings.Contains(got, "entries:\n  - name: backflow.v1.0.0\n") || strings.Contains(got, "replaces") {
		t.Errorf("the catalog:\n%s", catalog)
	}
}

// A release whose bundle was not published makes the release that is being made fail: its catalog would upgrade
// nobody to the new version.
func TestCatalogFailsWhenAReleasedBundleIsMissing(t *testing.T) {
	dir, opm, env := catalogRepo(t, "v1.0.0", "v1.0.1")
	// The image of 1.0.0 does not exist in the registry (the fake opm fails for it).
	out, err := runCatalog(t, dir, opm, env, "1.0.2", "IMAGE_REPO=ghcr.io/missing")
	if err == nil || !strings.Contains(out, "release 1.0.0 has no published bundle") {
		t.Errorf("err = %v\n%s", err, out)
	}
}

func TestCatalogRefusesAShallowCloneBecauseItWouldLeaveVersionsOut(t *testing.T) {
	dir, _, env := catalogRepo(t, "v1.0.0")
	// A clone with depth 1 is shallow.
	shallow := filepath.Join(t.TempDir(), "shallow")
	clone := exec.Command("git", "clone", "-q", "--depth", "1", "file://"+dir, shallow)
	if out, err := clone.CombinedOutput(); err != nil {
		t.Skipf("cannot make a shallow clone: %v\n%s", err, out)
	}
	for _, f := range []string{"bundle/manifests/x.yaml", "docs/olm/icon.svg", "fake-opm"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(shallow, f)), 0o755)
		data, _ := os.ReadFile(filepath.Join(dir, f))
		_ = os.WriteFile(filepath.Join(shallow, f), data, 0o755)
	}
	out, err := runCatalog(t, shallow, filepath.Join(shallow, "fake-opm"), env, "1.0.1")
	if err == nil || !strings.Contains(out, "shallow") {
		t.Errorf("err = %v\n%s", err, out)
	}
}

func TestCatalogUsesTheListOfATestInsteadOfTheTags(t *testing.T) {
	dir, opm, env := catalogRepo(t, "v1.0.0")
	if out, err := runCatalog(t, dir, opm, env, "1.0.2", "PREVIOUS_BUNDLES=1.0.1=localhost:5001/b:one"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	catalog, _ := os.ReadFile(filepath.Join(dir, "catalog/out/backflow/catalog.yaml"))
	got := string(catalog)
	if strings.Contains(got, "backflow-bundle:1.0.0") || !strings.Contains(got, "image: localhost:5001/b:one") {
		t.Errorf("the catalog:\n%s", catalog)
	}
	// Empty is a list too: no earlier version.
	if out, err := runCatalog(t, dir, opm, env, "1.0.2", "PREVIOUS_BUNDLES="); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
