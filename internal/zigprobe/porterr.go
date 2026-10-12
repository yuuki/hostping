package zigprobe

import (
	"fmt"
	"strings"

	"github.com/yuuki/hostping/internal/model"
)

// portErr maps a HOSTPING_ERR_PORT failure to its sentinel by the Zig
// error name that leads msg: "NoLid: …" and "PortDown: …" are InfiniBand
// ports without a usable LID, "Port: …" a probe that could not use the port
// it found (a RoCE RNIC paired with an InfiniBand one). Anything else is the
// RoCE case, no active RoCEv2 GID.
func portErr(msg string) error {
	switch {
	case strings.HasPrefix(msg, "NoLid"):
		return fmt.Errorf("%w: %s", model.ErrNoLID, msg)
	case strings.HasPrefix(msg, "PortDown"):
		return fmt.Errorf("%w: %s", model.ErrPortNotActive, msg)
	case strings.HasPrefix(msg, "Port:"):
		return fmt.Errorf("%w: %s", model.ErrProbe, msg)
	default:
		return fmt.Errorf("%w: %s", model.ErrNoGID, msg)
	}
}
