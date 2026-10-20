package zigprobe

import (
	"fmt"
	"strings"

	"github.com/yuuki/hostping/internal/model"
)

// The Zig error names that lead a probe's error text ("Name: detail"; see
// errclass.zig for which HOSTPING_ERR_* code each maps to). One code covers
// several Zig errors, so the Go side sub-classifies by the leading name;
// include/hostping.h says so at the codes concerned. The names match as
// prefixes of the text, which is why Port carries its colon: PortDown would
// match it otherwise.
const (
	zigErrNoLid    = "NoLid"    // HOSTPING_ERR_PORT: InfiniBand port without a LID
	zigErrPortDown = "PortDown" // HOSTPING_ERR_PORT: InfiniBand port not ACTIVE
	zigErrPort     = "Port:"    // HOSTPING_ERR_PORT: the port found cannot be used
	zigErrConnect  = "Connect"  // HOSTPING_ERR_CONNECT: no address handle to the peer
)

// zigErrIs reports whether msg is the Zig error name's text.
func zigErrIs(msg, name string) bool {
	return strings.HasPrefix(msg, name)
}

// portErr maps a HOSTPING_ERR_PORT failure to its sentinel by the Zig
// error name that leads msg: "NoLid: …" and "PortDown: …" are InfiniBand
// ports without a usable LID, "Port: …" a probe that could not use the port
// it found (a RoCE RNIC paired with an InfiniBand one). Anything else is the
// RoCE case, no active RoCEv2 GID.
func portErr(msg string) error {
	switch {
	case zigErrIs(msg, zigErrNoLid):
		return fmt.Errorf("%w: %s", model.ErrNoLID, msg)
	case zigErrIs(msg, zigErrPortDown):
		return fmt.Errorf("%w: %s", model.ErrPortNotActive, msg)
	case zigErrIs(msg, zigErrPort):
		return fmt.Errorf("%w: %s", model.ErrProbe, msg)
	default:
		return fmt.Errorf("%w: %s", model.ErrNoGID, msg)
	}
}
