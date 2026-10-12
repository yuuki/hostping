// Package pci walks sysfs PCI topology and parses ACS/ATS from config space.
package pci

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var bdfRe = regexp.MustCompile(`(?i)^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]$`)

func IsBDF(s string) bool { return bdfRe.MatchString(s) }

// Resolve returns the real /sys/devices/... path for a BDF under sysroot.
func Resolve(sysroot, bdf string) (string, error) {
	p := filepath.Join(sysroot, "bus/pci/devices", bdf)
	return filepath.EvalSymlinks(p)
}

// AncestryBDFs returns PCI BDF hops from the host-near end toward the device,
// including the device itself.
func AncestryBDFs(resolved string) []string {
	var hops []string
	for _, part := range strings.Split(resolved, string(os.PathSeparator)) {
		if IsBDF(part) {
			hops = append(hops, part)
		}
	}
	return hops
}

// PCIeType is the PCI Express device/port type, or -1 if unknown.
func PCIeType(sysroot, bdf string) int {
	s := strings.ToLower(strings.TrimSpace(readFile(filepath.Join(sysroot, "bus/pci/devices", bdf, "pcie_type"))))
	if s == "" {
		return -1
	}
	switch {
	case strings.Contains(s, "root_port") || strings.HasPrefix(s, "4"):
		return 4
	case strings.Contains(s, "upstream") || strings.HasPrefix(s, "5"):
		return 5
	case strings.Contains(s, "downstream") || strings.HasPrefix(s, "6"):
		return 6
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimRight(fields[0], ":"))
	if err != nil {
		return -1
	}
	return n
}

func Class(sysroot, bdf string) string {
	return strings.ToLower(strings.TrimSpace(readFile(filepath.Join(sysroot, "bus/pci/devices", bdf, "class"))))
}

func IsPCIBridge(class string) bool {
	return class == "0x060400" || class == "0x060401"
}

// Role classifies a BDF on a device's ancestry (index 0 = closest to the host).
// "root_port", "switch", or "" (endpoint / unknown).
func Role(sysroot, bdf string, hostNearIndex int) string {
	t := PCIeType(sysroot, bdf)
	class := Class(sysroot, bdf)
	switch t {
	case 4:
		return "root_port"
	case 5, 6:
		return "switch"
	}
	if IsPCIBridge(class) {
		if hostNearIndex == 0 {
			return "root_port"
		}
		return "switch"
	}
	return ""
}

// Caps holds ACS/ATS enablement parsed from PCI config space.
type Caps struct {
	ACS, ATS               bool
	ACSPresent, ATSPresent bool
}

// ParseConfig extracts ACS (ext cap 0x000D) and ATS (0x000F) control bits.
func ParseConfig(cfg []byte) Caps {
	var c Caps
	if len(cfg) < 0x108 {
		return c
	}
	off := 0x100
	for guard := 0; guard < 64 && off+4 <= len(cfg); guard++ {
		id := binary.LittleEndian.Uint16(cfg[off:])
		next := int(binary.LittleEndian.Uint16(cfg[off+2:]) >> 4)
		if id == 0x000D && off+8 <= len(cfg) {
			c.ACSPresent = true
			ctrl := binary.LittleEndian.Uint16(cfg[off+6:])
			// ACS is a Hostping problem only when P2P redirect/forwarding is on
			// (bits 2–5). Source Validation (bit 0) is common on Intel root ports.
			c.ACS = ctrl&0x3c != 0
		}
		if id == 0x000F && off+8 <= len(cfg) {
			c.ATSPresent = true
			ctrl := binary.LittleEndian.Uint16(cfg[off+6:])
			c.ATS = ctrl&0x8000 != 0
		}
		if next == 0 || next == off {
			break
		}
		off = next
	}
	return c
}

func ReadConfig(sysroot, bdf string) (Caps, error) {
	b, err := os.ReadFile(filepath.Join(sysroot, "bus/pci/devices", bdf, "config"))
	if err != nil {
		return Caps{}, err
	}
	return ParseConfig(b), nil
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
