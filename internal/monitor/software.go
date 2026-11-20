package monitor

import (
	"path/filepath"

	"github.com/yuuki/hostping/internal/topology"
)

// KernelRelease is the running kernel's release (uname -r), read from
// <procroot>/sys/kernel/osrelease; procroot is normally /proc. It is "" when
// it cannot be read.
func KernelRelease(procroot string) string {
	return readTrim(filepath.Join(procroot, "sys/kernel/osrelease"))
}

// RNICFirmware is the firmware version of each RNIC of g, from the verbs
// device's fw_ver attribute under sysroot (normally /sys), by RNIC ID. An
// RNIC whose version cannot be read is absent.
func RNICFirmware(sysroot string, g topology.Graph) map[string]string {
	out := map[string]string{}
	for _, n := range g.RNICs {
		// The device name is a directory name; one that is not (a
		// hand-written topology file) names nothing under sysfs.
		if n.Device == "" || filepath.Base(n.Device) != n.Device || n.Device == "." || n.Device == ".." {
			continue
		}
		if v := readTrim(filepath.Join(sysroot, "class/infiniband", n.Device, "fw_ver")); v != "" {
			out[n.ID] = v
		}
	}
	return out
}
