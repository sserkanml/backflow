package version

import (
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	vcs := func(revision, modified string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: modified},
			}}, true
		}
	}
	none := func() (*debug.BuildInfo, bool) { return nil, false }
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for _, tt := range []struct {
		name                    string
		version, commit         string
		read                    func() (*debug.BuildInfo, bool)
		wantVersion, wantCommit string
	}{
		{"ldflags win", "0.12.0", sha, vcs("ffffffffffffffff", "false"), "0.12.0", "0123456789ab"},
		{"vcs fills the commit, dev the version", "", "", vcs(sha, "false"), "dev", "0123456789ab"},
		{"a modified tree is dirty", "", "", vcs(sha, "true"), "dev", "0123456789ab-dirty"},
		{"the version from ldflags, the commit from vcs", "1.2.3", "", vcs(sha, "false"), "1.2.3", "0123456789ab"},
		{"nothing known", "", "", none, "dev", "unknown"},
		{"a module version", "", "", func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: "v0.3.0"}}, true
		}, "v0.3.0", "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v, c := resolve(tt.version, tt.commit, tt.read)
			if v != tt.wantVersion || c != tt.wantCommit {
				t.Errorf("= %q, %q; want %q, %q", v, c, tt.wantVersion, tt.wantCommit)
			}
		})
	}
}

// TestLdflagsReachTheBinary builds a program with the -ldflags the Dockerfile
// uses and checks what it reports, so a renamed variable or package does not
// silently leave every release image saying "dev".
func TestLdflagsReachTheBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a program")
	}
	const pkg = "github.com/sserkanml/backflow/internal/version"
	for _, tt := range []struct{ name, flags, want string }{
		{"both set", "-X " + pkg + ".Version=0.12.0 -X " + pkg + ".Commit=0123456789abcdef0123", "0.12.0 0123456789ab"},
		{"empty values, as an image built without arguments", "-X " + pkg + ".Version= -X " + pkg + ".Commit=", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, err := exec.Command("go", "run", "-buildvcs=false", "-ldflags", tt.flags, "./printversion").CombinedOutput()
			if err != nil {
				t.Fatalf("go run: %v\n%s", err, out)
			}
			got := strings.TrimSpace(string(out))
			if tt.want == "" {
				tt.want = "dev unknown" // -buildvcs=false: nothing to fall back to
			}
			if got != tt.want {
				t.Errorf("reports %q, want %q", got, tt.want)
			}
		})
	}
}
