package pci

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sysfs"
)

// CanonicalBDF returns the sysfs spelling of a PCI address: lower case with a
// domain of at least four hex digits. sysfs names its directories with %04x,
// but NVML/nvidia-smi print the domain with %08x, so a BDF that arrives from a
// GPU tool has to be narrowed before it can be used as a path.
// It returns "" when s is not a BDF.
func CanonicalBDF(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	dom, rest, ok := strings.Cut(s, ":")
	if !ok {
		return ""
	}
	// A bare bus:dev.func has no domain; sysfs always has one.
	if !strings.Contains(rest, ":") {
		rest = s
		dom = "0"
	}
	d, err := strconv.ParseUint(dom, 16, 32)
	if err != nil {
		return ""
	}
	out := fmt.Sprintf("%04x:%s", d, rest)
	if !IsBDF(out) {
		return ""
	}
	return out
}

// ReadLink reports the negotiated and advertised PCIe link of bdf, the
// advertised maximum of the upstream port it hangs off, its NUMA node, and its
// ACS/ATS state, all from sysfs under sysroot (normally /sys).
// A device whose current_link_speed cannot be read is an error: without it
// there is nothing to diagnose. Everything else is optional and left at its
// "unknown" value (empty MaxSpeed/PortMaxSpeed, zero MaxWidth/PortMaxWidth,
// NUMA -1, ACSKnown/ATSKnown false) when the file is missing.
func ReadLink(sysroot, bdf string) (model.PCIeConfig, error) {
	canon := CanonicalBDF(bdf)
	if canon == "" {
		return model.PCIeConfig{}, fmt.Errorf("pci %q: not a BDF", bdf)
	}
	dir := filepath.Join(sysroot, "bus/pci/devices", canon)
	cfg := model.PCIeConfig{BDF: canon, NUMA: -1}
	speed := strings.TrimSpace(sysfs.ReadString(filepath.Join(dir, "current_link_speed")))
	if speed == "" {
		return model.PCIeConfig{}, fmt.Errorf("pci %s: sysfs missing", canon)
	}
	cfg.Speed = speed
	cfg.MaxSpeed = strings.TrimSpace(sysfs.ReadString(filepath.Join(dir, "max_link_speed")))
	cfg.Width = sysfs.ReadInt(filepath.Join(dir, "current_link_width"), 0)
	cfg.MaxWidth = sysfs.ReadInt(filepath.Join(dir, "max_link_width"), 0)
	cfg.NUMA = sysfs.ReadInt(filepath.Join(dir, "numa_node"), -1)
	if up := upstreamPort(sysroot, canon); up != "" {
		cfg.PortMaxSpeed = strings.TrimSpace(sysfs.ReadString(filepath.Join(up, "max_link_speed")))
		cfg.PortMaxWidth = sysfs.ReadInt(filepath.Join(up, "max_link_width"), 0)
	}
	if caps, err := ReadConfig(sysroot, canon); err == nil {
		cfg.ACSKnown, cfg.ACSEnabled = caps.ACSPresent, caps.ACS
		cfg.ATSKnown, cfg.ATSEnabled = caps.ATSPresent, caps.ATS
	}
	return cfg, nil
}

// upstreamPort returns the sysfs directory of the bridge port bdf hangs off:
// the parent of its resolved /sys/devices path. It is "" when the parent is not
// a PCI function (a host bridge "pci0000:00" above a root port, or a tree that
// cannot be resolved).
func upstreamPort(sysroot, bdf string) string {
	resolved, err := Resolve(sysroot, bdf)
	if err != nil {
		return ""
	}
	parent := filepath.Dir(resolved)
	if !IsBDF(filepath.Base(parent)) {
		return ""
	}
	return parent
}
