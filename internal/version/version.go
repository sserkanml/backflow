// Package version tells which version and which commit this binary was built from.
package version

import "runtime/debug"

// Version and Commit are set at build time with
//
//	-ldflags "-X github.com/sserkanml/backflow/internal/version.Version=0.12.0 -X github.com/sserkanml/backflow/internal/version.Commit=<sha>"
//
// which is what the Dockerfile does for the release and main images (the image
// build has no .git, so the toolchain has nothing to stamp).
var (
	Version string
	Commit  string
)

// Unknown values.
const (
	// Dev is the version of a binary built without a version.
	Dev = "dev"
	// UnknownCommit is the commit of a binary built without one.
	UnknownCommit = "unknown"
)

// commitLength is how many characters of a commit are reported.
const commitLength = 12

// Get returns the version and the commit. Each comes from the -ldflags values
// when set, else from what the Go toolchain recorded in the binary (a module
// version, the VCS revision of a build inside a Git checkout, with "-dirty" when
// the tree was modified), else "dev" and "unknown".
func Get() (version, commit string) {
	return resolve(Version, Commit, debug.ReadBuildInfo)
}

func resolve(version, commit string, read func() (*debug.BuildInfo, bool)) (string, string) {
	if version != "" && commit != "" {
		return version, short(commit)
	}
	var vcsVersion, vcsCommit string
	if info, ok := read(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			vcsVersion = v
		}
		modified := false
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				vcsCommit = short(s.Value)
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		if modified && vcsCommit != "" {
			vcsCommit += "-dirty"
		}
	}
	return first(version, vcsVersion, Dev), first(short(commit), vcsCommit, UnknownCommit)
}

func short(commit string) string {
	return commit[:min(len(commit), commitLength)]
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
