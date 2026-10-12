package buildinfo

import (
	"runtime"
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	vcs := func(rev, modified string) *debug.BuildInfo {
		bi := &debug.BuildInfo{GoVersion: "go1.99.0", Main: debug.Module{Version: "(devel)"}}
		if rev != "" {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: "vcs.revision", Value: rev})
		}
		if modified != "" {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: "vcs.modified", Value: modified})
		}
		return bi
	}
	tests := []struct {
		name        string
		version     string
		commit      string
		bi          *debug.BuildInfo
		wantVersion string
		wantCommit  string
		wantGo      string
	}{
		{
			name:    "ldflags win over the embedded build info",
			version: "1.2.3", commit: "abc1234",
			bi: &debug.BuildInfo{
				GoVersion: "go1.99.0",
				Main:      debug.Module{Version: "v9.9.9"},
				Settings:  []debug.BuildSetting{{Key: "vcs.revision", Value: "ffffffffffffffff"}, {Key: "vcs.modified", Value: "true"}},
			},
			wantVersion: "1.2.3", wantCommit: "abc1234", wantGo: "go1.99.0",
		},
		{
			name:        "clean VCS checkout",
			bi:          vcs("0123456789abcdef0123456789abcdef01234567", "false"),
			wantVersion: UnknownVersion, wantCommit: "0123456", wantGo: "go1.99.0",
		},
		{
			name:        "modified VCS checkout",
			bi:          vcs("0123456789abcdef0123456789abcdef01234567", "true"),
			wantVersion: UnknownVersion, wantCommit: "0123456-dirty", wantGo: "go1.99.0",
		},
		{
			name:        "module version from go install",
			bi:          &debug.BuildInfo{GoVersion: "go1.99.0", Main: debug.Module{Version: "v1.4.0"}},
			wantVersion: "v1.4.0", wantCommit: UnknownCommit, wantGo: "go1.99.0",
		},
		{
			name:        "-buildvcs=false leaves no VCS settings",
			bi:          vcs("", ""),
			wantVersion: UnknownVersion, wantCommit: UnknownCommit, wantGo: "go1.99.0",
		},
		{
			name:        "no build info at all",
			wantVersion: UnknownVersion, wantCommit: UnknownCommit, wantGo: runtime.Version(),
		},
		{
			name:    "only the version is stamped",
			version: "1.2.3",
			bi:      vcs("abcdef0123", "true"),
			// The commit still comes from VCS stamping.
			wantVersion: "1.2.3", wantCommit: "abcdef0-dirty", wantGo: "go1.99.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolve(tt.version, tt.commit, tt.bi)
			if got.Version != tt.wantVersion || got.Commit != tt.wantCommit || got.Go != tt.wantGo {
				t.Fatalf("resolve = %+v, want version=%q commit=%q go=%q", got, tt.wantVersion, tt.wantCommit, tt.wantGo)
			}
		})
	}
}

func TestLine(t *testing.T) {
	info := Info{Version: "1.2.3", Commit: "abc1234", Zig: "0.2.0", Go: "go1.27.0"}
	if got, want := info.Line("hostping"), "hostping 1.2.3 (commit abc1234, zig probe 0.2.0, go1.27.0)"; got != want {
		t.Fatalf("Line = %q, want %q", got, want)
	}
	info.Zig = ""
	if got, want := info.Line("hostpingctl"), "hostpingctl 1.2.3 (commit abc1234, zig probe unknown, go1.27.0)"; got != want {
		t.Fatalf("Line without zig = %q, want %q", got, want)
	}
}

func TestGetNeverEmpty(t *testing.T) {
	info := Get()
	if info.Version == "" || info.Commit == "" || info.Go == "" {
		t.Fatalf("Get = %+v, want every field set", info)
	}
}
