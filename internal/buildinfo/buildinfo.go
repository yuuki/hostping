// Package buildinfo identifies the build that is running: its version, the
// commit it was built from, and the Go toolchain. The Makefile stamps Version
// and Commit with -ldflags "-X ..."; a plain `go build` or `go install` falls
// back to what the Go toolchain embedded (runtime/debug.ReadBuildInfo).
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set at link time:
//
//	go build -ldflags "-X github.com/yuuki/hostping/internal/buildinfo.Version=1.2.3 \
//	    -X github.com/yuuki/hostping/internal/buildinfo.Commit=abc1234"
var (
	Version string
	Commit  string
)

const (
	// UnknownVersion is reported when neither -ldflags nor the module
	// version say which release this is.
	UnknownVersion = "devel"
	// UnknownCommit is reported when neither -ldflags nor VCS stamping
	// (absent under -buildvcs=false or outside a checkout) name the commit.
	UnknownCommit = "unknown"
	// UnknownZig is printed when the caller did not fill in the probe version.
	UnknownZig = "unknown"

	shortCommit = 7
)

// Info is one build's identity.
type Info struct {
	Version string
	Commit  string
	// Zig is the Zig probe library version. buildinfo cannot ask the probe
	// itself (that needs cgo), so the caller fills it in.
	Zig string
	Go  string
}

// Get returns the running binary's build info, with Zig left empty.
func Get() Info {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		bi = nil
	}
	return resolve(Version, Commit, bi)
}

// resolve prefers the link-time values, then the toolchain-embedded ones,
// then the placeholders. bi may be nil.
func resolve(version, commit string, bi *debug.BuildInfo) Info {
	info := Info{Version: version, Commit: commit, Go: runtime.Version()}
	if bi != nil {
		if bi.GoVersion != "" {
			info.Go = bi.GoVersion
		}
		if info.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			info.Version = bi.Main.Version
		}
		if info.Commit == "" {
			info.Commit = vcsCommit(bi.Settings)
		}
	}
	if info.Version == "" {
		info.Version = UnknownVersion
	}
	if info.Commit == "" {
		info.Commit = UnknownCommit
	}
	return info
}

// vcsCommit is the short vcs.revision, with -dirty appended when the tree
// had local modifications. It is empty when the build was not VCS-stamped.
func vcsCommit(settings []debug.BuildSetting) string {
	var rev string
	var dirty bool
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > shortCommit {
		rev = rev[:shortCommit]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

// Line is the one-line version banner both binaries print, e.g.
// "hostping 1.2.3 (commit abc1234, zig probe 0.2.0, go1.27.0)".
func (i Info) Line(program string) string {
	zig := i.Zig
	if zig == "" {
		zig = UnknownZig
	}
	return fmt.Sprintf("%s %s (commit %s, zig probe %s, %s)", program, i.Version, i.Commit, zig, i.Go)
}
