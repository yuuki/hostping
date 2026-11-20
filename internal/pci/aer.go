package pci

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// PCIe Advanced Error Reporting statistics, as the kernel exports them per
// PCI function in sysfs since Linux 4.19: aer_dev_correctable,
// aer_dev_nonfatal and aer_dev_fatal (Documentation/ABI/testing/
// sysfs-bus-pci-devices-aer, named sysfs-bus-pci-devices-aer_stats before
// Linux 6.16). Each file is one "<row> <count>" line per error type the
// kernel names, then a TOTAL_ERR_* line.
//
// The counts are what the kernel's AER handler recorded for the function
// (pci_dev_aer_stats_incr in drivers/pci/pcie/aer.c): one per handled error
// report and status bit, not one per error. Several errors behind one report
// count once, a masked status bit counts nothing, and a platform whose
// firmware keeps AER to itself (no _OSC grant) counts nothing at all. A
// count is therefore a lower bound, and a count that does not move is no
// proof of a clean link.

// The rows that report on the link itself: the ones the kernel classes as
// Physical Layer or Data Link Layer errors (AER_PHYSICAL_LAYER_ERROR_MASK and
// AER_DATA_LINK_LAYER_ERROR_MASK in drivers/pci/pcie/aer.c). Every other row
// is a Transaction Layer or device-internal error, which says something about
// a request or a component, not about the link the function sits on. The
// names are the kernel's own row names.
const (
	// AERRxErr is Receiver Error (Correctable Error Status bit 0): the
	// physical layer of this end received something it could not decode.
	AERRxErr = "RxErr"
	// AERBadTLP is Bad TLP (bit 6): this end received a TLP whose link CRC
	// or sequence number was wrong. It answers with a NAK and the link
	// partner replays, so this is where an LCRC error and the NAK it causes
	// are visible; AER has no counter for the NAK or the replay itself.
	AERBadTLP = "BadTLP"
	// AERBadDLLP is Bad DLLP (bit 7): this end received a data link layer
	// packet (an ACK, a NAK, a flow-control update) with a bad CRC.
	AERBadDLLP = "BadDLLP"
	// AERRollover is REPLAY_NUM Rollover (bit 8): this end replayed the same
	// TLPs until its replay counter rolled over, and the link retrained.
	AERRollover = "Rollover"
	// AERTimeout is Replay Timer Timeout (bit 12): this end got neither an
	// ACK nor a NAK for a TLP in time and replayed it.
	AERTimeout = "Timeout"
	// AERDLP is Data Link Protocol Error (Uncorrectable Error Status bit 4).
	AERDLP = "DLP"
	// AERSDES is Surprise Down Error (bit 5): the link went down without
	// being told to.
	AERSDES = "SDES"
)

// AERCorrectableLinkRows are the link rows of aer_dev_correctable, in the
// order the kernel prints them. RxErr, BadTLP and BadDLLP are counted by the
// receiving end, Rollover and Timeout by the transmitting end
// (AER_AGENT_TRANSMITTER_MASK).
var AERCorrectableLinkRows = []string{AERRxErr, AERBadTLP, AERBadDLLP, AERRollover, AERTimeout}

// AERUncorrectableLinkRows are the link rows of aer_dev_nonfatal and
// aer_dev_fatal.
var AERUncorrectableLinkRows = []string{AERDLP, AERSDES}

// aerRowAliases maps every spelling of a link row to its name: the row names
// drivers/pci/pcie/aer.c has printed since the files exist, and the long
// names the sample output of the ABI document shows ("RELAY_NUM" is the
// document's spelling).
var aerRowAliases = map[string]string{
	"RxErr":                AERRxErr,
	"Receiver Error":       AERRxErr,
	"BadTLP":               AERBadTLP,
	"Bad TLP":              AERBadTLP,
	"BadDLLP":              AERBadDLLP,
	"Bad DLLP":             AERBadDLLP,
	"Rollover":             AERRollover,
	"RELAY_NUM Rollover":   AERRollover,
	"Timeout":              AERTimeout,
	"Replay Timer Timeout": AERTimeout,
	"DLP":                  AERDLP,
	"Data Link Protocol":   AERDLP,
	"SDES":                 AERSDES,
	"Surprise Down Error":  AERSDES,
}

// Why a function's AER statistics could not be taken. Each is wrapped with
// the file and the cause, so errors.Is tells the cases apart.
var (
	// ErrAERAbsent: the function has no AER statistics files. It has no AER
	// capability, or the kernel is older than 4.19 or built without
	// CONFIG_PCIEAER.
	ErrAERAbsent = errors.New("no AER statistics")
	// ErrAERUnreadable: a file exists but could not be read.
	ErrAERUnreadable = errors.New("AER statistics unreadable")
	// ErrAERFormat: a file is not the "<row> <count>" list the kernel
	// writes, or lacks a row the reader needs.
	ErrAERFormat = errors.New("AER statistics malformed")
)

// aerMaxRows bounds the rows ParseAERStats accepts. The kernel prints at most
// one row per status bit and a total, 33 lines; a file with far more is not
// one of its files.
const aerMaxRows = 128

// ParseAERStats parses the contents of one aer_dev_* file into its rows, by
// the name the file gives them. A row is a name, which may hold spaces, then
// a decimal count; blank lines are skipped. Anything else, a row given twice
// or more than aerMaxRows rows is an error: a file that is not understood is
// unknown, never a set of zero counts.
func ParseAERStats(s string) (map[string]uint64, error) {
	rows := map[string]uint64{}
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cut := strings.LastIndexAny(line, " \t")
		if cut < 0 {
			return nil, fmt.Errorf("line %d: %q is not \"<row> <count>\"", i+1, line)
		}
		name := strings.TrimSpace(line[:cut])
		v, err := strconv.ParseUint(line[cut+1:], 10, 64)
		if err != nil || name == "" {
			return nil, fmt.Errorf("line %d: %q is not \"<row> <count>\"", i+1, line)
		}
		if _, dup := rows[name]; dup {
			return nil, fmt.Errorf("line %d: row %q given twice", i+1, name)
		}
		if len(rows) == aerMaxRows {
			return nil, fmt.Errorf("more than %d rows", aerMaxRows)
		}
		rows[name] = v
	}
	return rows, nil
}

// aerLinkRows picks the link rows named in want out of the parsed rows of one
// file, under their kernel names. Every wanted row must be there exactly
// once, whichever spelling it uses: the kernel prints every row it names even
// at zero, so a missing one means the file is not what the reader expects.
func aerLinkRows(rows map[string]uint64, want []string) (map[string]uint64, error) {
	out := make(map[string]uint64, len(want))
	for name, v := range rows {
		row, ok := aerRowAliases[name]
		if !ok {
			continue
		}
		if _, dup := out[row]; dup {
			return nil, fmt.Errorf("row %s given under two names", row)
		}
		out[row] = v
	}
	for _, row := range want {
		if _, ok := out[row]; !ok {
			return nil, fmt.Errorf("no %s row", row)
		}
	}
	for row := range out {
		if !slices.Contains(want, row) {
			delete(out, row)
		}
	}
	return out, nil
}

// LinkErrors is one PCI function's cumulative AER counts of the errors that
// report on its PCIe link, by kernel row name. The function counts what it
// saw itself: an endpoint or a switch upstream port for the link above it, a
// root port or a switch downstream port for the link below it.
type LinkErrors struct {
	// Correctable holds the AERCorrectableLinkRows of aer_dev_correctable.
	Correctable map[string]uint64
	// Uncorrectable holds the AERUncorrectableLinkRows, each the sum of its
	// rows in aer_dev_nonfatal and aer_dev_fatal.
	Uncorrectable map[string]uint64
}

// ReadLinkErrors reads the AER link-error counts of the function at bdf
// under sysroot (normally /sys). The three statistics files are created
// together, so a function that lacks the first has none (ErrAERAbsent); any
// file that is missing after that, unreadable or malformed is an error too.
// Nothing is ever read as zero that was not read.
func ReadLinkErrors(sysroot, bdf string) (LinkErrors, error) {
	canon := CanonicalBDF(bdf)
	if canon == "" {
		return LinkErrors{}, fmt.Errorf("pci %q: not a BDF", bdf)
	}
	dir := filepath.Join(sysroot, "bus/pci/devices", canon)
	cor, err := readAERFile(filepath.Join(dir, "aer_dev_correctable"), AERCorrectableLinkRows, true)
	if err != nil {
		return LinkErrors{}, err
	}
	out := LinkErrors{Correctable: cor, Uncorrectable: map[string]uint64{}}
	for _, name := range []string{"aer_dev_nonfatal", "aer_dev_fatal"} {
		rows, err := readAERFile(filepath.Join(dir, name), AERUncorrectableLinkRows, false)
		if err != nil {
			return LinkErrors{}, err
		}
		for row, v := range rows {
			out.Uncorrectable[row] = saturatingAdd(out.Uncorrectable[row], v)
		}
	}
	return out, nil
}

// readAERFile reads one statistics file and returns its want rows. A missing
// file is ErrAERAbsent when first is set, and otherwise unreadable: the
// function has statistics, so the file should be there.
func readAERFile(path string, want []string, first bool) (map[string]uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if first && errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s: %v", ErrAERAbsent, path, err)
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrAERUnreadable, path, err)
	}
	rows, err := ParseAERStats(string(b))
	if err == nil {
		rows, err = aerLinkRows(rows, want)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrAERFormat, path, err)
	}
	return rows, nil
}

func saturatingAdd(a, b uint64) uint64 {
	if s := a + b; s >= a {
		return s
	}
	return ^uint64(0)
}

// AncestryLink is one PCIe link on a device's ancestry: Port is the
// downstream-facing port above it (a root port or a switch downstream port)
// and Lower the function below it on the ancestry (the device itself, or the
// upstream port of a switch).
type AncestryLink struct {
	Port  string
	Lower string
}

// AncestryLinks returns the PCIe links between the root port above bdf and
// bdf, host-near first; the last is bdf's own link.
//
// sysfs lists the functions of the ancestry but not which pairs of them a
// link joins, and mainline kernels have no attribute for a function's PCI
// Express port type. The structure of a PCIe hierarchy gives it: below a root
// port, links and switch-internal hops alternate, so the ancestry is a root
// port, then (switch upstream port, switch downstream port) pairs, then the
// device, and a link joins the functions at positions 2k and 2k+1. An
// ancestry of odd length, or one whose pcie_type (where a kernel has it)
// contradicts that, is not such a hierarchy, and the error says so: the
// caller then knows only the device's own link (LinkPort).
func AncestryLinks(sysroot, bdf string) ([]AncestryLink, error) {
	canon := CanonicalBDF(bdf)
	if canon == "" {
		return nil, fmt.Errorf("pci %q: not a BDF", bdf)
	}
	resolved, err := Resolve(sysroot, canon)
	if err != nil {
		return nil, fmt.Errorf("pci %s: %v", canon, err)
	}
	hops := AncestryBDFs(resolved)
	if len(hops) < 2 || len(hops)%2 != 0 {
		return nil, fmt.Errorf("pci %s: ancestry %v is not a root port, switch port pairs and the device", canon, hops)
	}
	links := make([]AncestryLink, 0, len(hops)/2)
	for i := 0; i < len(hops); i += 2 {
		if !portTypeIs(sysroot, hops[i], i == 0) || !lowerTypeIs(sysroot, hops[i+1], i+2 == len(hops)) {
			return nil, fmt.Errorf("pci %s: %s and %s are not the two ends of a link", canon, hops[i], hops[i+1])
		}
		links = append(links, AncestryLink{Port: hops[i], Lower: hops[i+1]})
	}
	return links, nil
}

// portTypeIs reports whether the port type of bdf, when the kernel states
// one, allows a downstream-facing port: a root port at the top of the
// ancestry, a switch downstream port below it.
func portTypeIs(sysroot, bdf string, top bool) bool {
	switch PCIeType(sysroot, bdf) {
	case PCIeTypeUnknown:
		return true
	case PCIeTypeRootPort:
		return top
	case PCIeTypeDownstream:
		return !top
	}
	return false
}

// lowerTypeIs reports whether the port type of bdf, when the kernel states
// one, allows the lower end of a link: the device itself at the bottom of
// the ancestry, a switch upstream port above it.
func lowerTypeIs(sysroot, bdf string, bottom bool) bool {
	switch PCIeType(sysroot, bdf) {
	case PCIeTypeUnknown:
		return true
	case PCIeTypeUpstream:
		return !bottom
	case PCIeTypeRootPort, PCIeTypeDownstream:
		return false
	}
	return bottom
}

// LinkPort returns the BDF of the port the function at bdf hangs off, the
// upper end of its PCIe link, or "" when its sysfs parent is no PCI function.
func LinkPort(sysroot, bdf string) string {
	up := upstreamPort(sysroot, CanonicalBDF(bdf))
	if up == "" {
		return ""
	}
	return filepath.Base(up)
}

// DeviceFunctions returns the functions of the PCI device bdf is a function
// of: every function with bdf's domain, bus and device number under the same
// sysfs parent, sorted, bdf included. They share one PCIe link, and an error
// on it may be recorded at any of them. It is just bdf when the tree cannot
// be read.
func DeviceFunctions(sysroot, bdf string) []string {
	canon := CanonicalBDF(bdf)
	if canon == "" {
		return nil
	}
	resolved, err := Resolve(sysroot, canon)
	if err != nil {
		return []string{canon}
	}
	ents, err := os.ReadDir(filepath.Dir(resolved))
	if err != nil {
		return []string{canon}
	}
	device := canon[:strings.LastIndexByte(canon, '.')+1]
	out := []string{canon}
	for _, e := range ents {
		name := strings.ToLower(e.Name())
		if name != canon && IsBDF(name) && strings.HasPrefix(name, device) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
