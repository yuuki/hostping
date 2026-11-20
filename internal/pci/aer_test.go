package pci

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aerCorrectable renders aer_dev_correctable as drivers/pci/pcie/aer.c
// prints it (aer_stats_dev_attr): every named row in bit order, then the
// total. rows gives the counts of the rows that are not zero.
func aerCorrectable(rows map[string]uint64) string {
	return aerFile([]string{"RxErr", "BadTLP", "BadDLLP", "Rollover", "Timeout", "NonFatalErr", "CorrIntErr", "HeaderOF"}, "TOTAL_ERR_COR", rows)
}

// aerUncorrectable renders aer_dev_nonfatal or aer_dev_fatal the same way;
// total is TOTAL_ERR_NONFATAL or TOTAL_ERR_FATAL.
func aerUncorrectable(total string, rows map[string]uint64) string {
	return aerFile([]string{"Undefined", "DLP", "SDES", "TLP", "FCP", "CmpltTO", "CmpltAbrt", "UnxCmplt", "RxOF", "MalfTLP", "ECRC",
		"UnsupReq", "ACSViol", "UncorrIntErr", "BlockedTLP", "AtomicOpBlocked", "TLPBlockedErr", "PoisonTLPBlocked"}, total, rows)
}

func aerFile(names []string, total string, rows map[string]uint64) string {
	var b strings.Builder
	var sum uint64
	for _, n := range names {
		fmt.Fprintf(&b, "%s %d\n", n, rows[n])
		sum += rows[n]
	}
	fmt.Fprintf(&b, "%s %d\n", total, sum)
	return b.String()
}

// writeAER writes the three AER statistics files of the device at bdf.
func writeAER(t *testing.T, root, bdf string, cor, nonfatal, fatal map[string]uint64) {
	t.Helper()
	dir := filepath.Join(root, "bus/pci/devices", bdf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"aer_dev_correctable": aerCorrectable(cor),
		"aer_dev_nonfatal":    aerUncorrectable("TOTAL_ERR_NONFATAL", nonfatal),
		"aer_dev_fatal":       aerUncorrectable("TOTAL_ERR_FATAL", fatal),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestParseAERStats(t *testing.T) {
	// The sample output of Documentation/ABI/testing/sysfs-bus-pci-devices-aer:
	// long row names, with spaces.
	const abiSample = "Receiver Error 2\nBad TLP 0\nBad DLLP 0\nRELAY_NUM Rollover 0\nReplay Timer Timeout 0\n" +
		"Advisory Non-Fatal 0\nCorrected Internal Error 0\nHeader Log Overflow 0\nTOTAL_ERR_COR 2\n"
	tests := []struct {
		name string
		in   string
		want map[string]uint64
		err  string
	}{
		{name: "kernel", in: aerCorrectable(map[string]uint64{"BadTLP": 412, "Timeout": 3}),
			want: map[string]uint64{"RxErr": 0, "BadTLP": 412, "BadDLLP": 0, "Rollover": 0, "Timeout": 3, "NonFatalErr": 0, "CorrIntErr": 0, "HeaderOF": 0, "TOTAL_ERR_COR": 415}},
		{name: "abi document sample", in: abiSample,
			want: map[string]uint64{"Receiver Error": 2, "Bad TLP": 0, "Bad DLLP": 0, "RELAY_NUM Rollover": 0, "Replay Timer Timeout": 0,
				"Advisory Non-Fatal": 0, "Corrected Internal Error": 0, "Header Log Overflow": 0, "TOTAL_ERR_COR": 2}},
		{name: "unnamed bit", in: "RxErr 1\ndev_cor_errs_bit[3] 7\nTOTAL_ERR_COR 8\n",
			want: map[string]uint64{"RxErr": 1, "dev_cor_errs_bit[3]": 7, "TOTAL_ERR_COR": 8}},
		{name: "largest count", in: "RxErr 18446744073709551615\n", want: map[string]uint64{"RxErr": 18446744073709551615}},
		{name: "blank lines and CRLF", in: "\nRxErr 1\r\n\n  BadTLP\t2  \n", want: map[string]uint64{"RxErr": 1, "BadTLP": 2}},
		{name: "empty", in: "", want: map[string]uint64{}},
		{name: "no count", in: "RxErr\n", err: `line 1: "RxErr" is not "<row> <count>"`},
		{name: "count out of range", in: "RxErr 18446744073709551616\n", err: "line 1"},
		{name: "negative count", in: "RxErr -1\n", err: "line 1"},
		{name: "count only", in: " 5\n", err: "line 1"},
		{name: "hex count", in: "RxErr 0x10\n", err: "line 1"},
		{name: "row twice", in: "RxErr 1\nRxErr 2\n", err: `line 2: row "RxErr" given twice`},
		{name: "second line bad", in: "RxErr 1\nBadTLP x\n", err: "line 2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAERStats(tc.in)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want one containing %q", err, tc.err)
				}
				if got != nil {
					t.Fatalf("rows %v returned with an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(got, tc.want) {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseAERStatsRowLimit(t *testing.T) {
	var b strings.Builder
	for i := 0; i < aerMaxRows; i++ {
		fmt.Fprintf(&b, "row%d %d\n", i, i)
	}
	if rows, err := ParseAERStats(b.String()); err != nil || len(rows) != aerMaxRows {
		t.Fatalf("%d rows: len=%d err=%v, want all of them", aerMaxRows, len(rows), err)
	}
	b.WriteString("one_more 1\n")
	if _, err := ParseAERStats(b.String()); err == nil {
		t.Fatalf("%d rows accepted, want an error", aerMaxRows+1)
	}
}

func TestReadLinkErrors(t *testing.T) {
	root := t.TempDir()
	const bdf = "0000:3c:00.0"
	writeAER(t, root, bdf,
		map[string]uint64{"RxErr": 95, "BadTLP": 412, "BadDLLP": 7, "Rollover": 1, "Timeout": 3, "NonFatalErr": 40, "CorrIntErr": 9},
		map[string]uint64{"DLP": 2, "CmpltTO": 5, "UnsupReq": 11},
		map[string]uint64{"DLP": 1, "SDES": 4, "MalfTLP": 6})

	got, err := ReadLinkErrors(root, bdf)
	if err != nil {
		t.Fatal(err)
	}
	wantCor := map[string]uint64{AERRxErr: 95, AERBadTLP: 412, AERBadDLLP: 7, AERRollover: 1, AERTimeout: 3}
	if !maps.Equal(got.Correctable, wantCor) {
		t.Fatalf("correctable = %v, want the five link rows %v (no transaction-layer row)", got.Correctable, wantCor)
	}
	// DLP is the sum of the nonfatal and the fatal file.
	wantUnc := map[string]uint64{AERDLP: 3, AERSDES: 4}
	if !maps.Equal(got.Uncorrectable, wantUnc) {
		t.Fatalf("uncorrectable = %v, want %v", got.Uncorrectable, wantUnc)
	}

	// A bare bus:dev.fn and an NVML-style domain name the same function.
	for _, alias := range []string{"3C:00.0", "00000000:3c:00.0"} {
		again, err := ReadLinkErrors(root, alias)
		if err != nil || !maps.Equal(again.Correctable, wantCor) {
			t.Fatalf("ReadLinkErrors(%q) = %+v, %v", alias, again, err)
		}
	}
}

// The ABI document's sample spells the rows out; both spellings read alike.
func TestReadLinkErrorsLongRowNames(t *testing.T) {
	root := t.TempDir()
	const bdf = "0000:00:1c.0"
	dir := filepath.Join(root, "bus/pci/devices", bdf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("aer_dev_correctable", "Receiver Error 2\nBad TLP 1\nBad DLLP 0\nRELAY_NUM Rollover 0\nReplay Timer Timeout 4\nAdvisory Non-Fatal 0\nCorrected Internal Error 0\nHeader Log Overflow 0\nTOTAL_ERR_COR 7\n")
	unc := "Undefined 0\nData Link Protocol 1\nSurprise Down Error 0\nPoisoned TLP 0\nFlow Control Protocol 0\nCompletion Timeout 0\nCompleter Abort 0\nUnexpected Completion 0\nReceiver Overflow 0\nMalformed TLP 0\nECRC 0\nUnsupported Request 0\nACS Violation 0\nUncorrectable Internal Error 0\nMC Blocked TLP 0\nAtomicOp Egress Blocked 0\nTLP Prefix Blocked Error 0\n"
	write("aer_dev_nonfatal", unc+"TOTAL_ERR_NONFATAL 1\n")
	write("aer_dev_fatal", unc+"TOTAL_ERR_FATAL 1\n")

	got, err := ReadLinkErrors(root, bdf)
	if err != nil {
		t.Fatal(err)
	}
	if want := (map[string]uint64{AERRxErr: 2, AERBadTLP: 1, AERBadDLLP: 0, AERRollover: 0, AERTimeout: 4}); !maps.Equal(got.Correctable, want) {
		t.Fatalf("correctable = %v, want %v", got.Correctable, want)
	}
	if want := (map[string]uint64{AERDLP: 2, AERSDES: 0}); !maps.Equal(got.Uncorrectable, want) {
		t.Fatalf("uncorrectable = %v, want %v", got.Uncorrectable, want)
	}
}

// Every way the statistics cannot be taken is an error with its own
// sentinel, and never a set of zero counts.
func TestReadLinkErrorsFailures(t *testing.T) {
	const bdf = "0000:3c:00.0"
	healthy := func(t *testing.T) (root, dir string) {
		t.Helper()
		root = t.TempDir()
		writeAER(t, root, bdf, nil, nil, nil)
		return root, filepath.Join(root, "bus/pci/devices", bdf)
	}
	tests := []struct {
		name   string
		mutate func(t *testing.T, dir string)
		want   error
	}{
		{name: "no AER capability", want: ErrAERAbsent, mutate: func(t *testing.T, dir string) {
			for _, f := range []string{"aer_dev_correctable", "aer_dev_nonfatal", "aer_dev_fatal"} {
				if err := os.Remove(filepath.Join(dir, f)); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{name: "device gone", want: ErrAERAbsent, mutate: func(t *testing.T, dir string) {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "fatal file missing", want: ErrAERUnreadable, mutate: func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "aer_dev_fatal")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "correctable is a directory", want: ErrAERUnreadable, mutate: func(t *testing.T, dir string) {
			p := filepath.Join(dir, "aer_dev_correctable")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "empty file", want: ErrAERFormat, mutate: func(t *testing.T, dir string) {
			overwrite(t, filepath.Join(dir, "aer_dev_correctable"), "")
		}},
		{name: "link row missing", want: ErrAERFormat, mutate: func(t *testing.T, dir string) {
			overwrite(t, filepath.Join(dir, "aer_dev_correctable"), "RxErr 0\nBadTLP 0\nBadDLLP 0\nRollover 0\nTOTAL_ERR_COR 0\n")
		}},
		{name: "row under both names", want: ErrAERFormat, mutate: func(t *testing.T, dir string) {
			overwrite(t, filepath.Join(dir, "aer_dev_correctable"), aerCorrectable(nil)+"Receiver Error 4\n")
		}},
		{name: "garbage", want: ErrAERFormat, mutate: func(t *testing.T, dir string) {
			overwrite(t, filepath.Join(dir, "aer_dev_nonfatal"), "\x00\x01 not a counter\n")
		}},
		{name: "uncorrectable link row missing", want: ErrAERFormat, mutate: func(t *testing.T, dir string) {
			overwrite(t, filepath.Join(dir, "aer_dev_fatal"), "Undefined 0\nDLP 0\nTOTAL_ERR_FATAL 0\n")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, dir := healthy(t)
			tc.mutate(t, dir)
			got, err := ReadLinkErrors(root, bdf)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			for _, other := range []error{ErrAERAbsent, ErrAERUnreadable, ErrAERFormat} {
				if !errors.Is(other, tc.want) && errors.Is(err, other) {
					t.Fatalf("err = %v also matches %v", err, other)
				}
			}
			if got.Correctable != nil || got.Uncorrectable != nil {
				t.Fatalf("counts %+v returned with an error", got)
			}
		})
	}

	if _, err := ReadLinkErrors(t.TempDir(), "not-a-bdf"); err == nil {
		t.Fatal("a name that is no BDF read without an error")
	}
}

// The cause is formatted, not wrapped: errors.Is sees the classification
// only, so a missing statistics file is never mistaken for a missing device
// by a caller that checks for os.ErrNotExist.
func TestReadLinkErrorsWrapsOnlyTheSentinel(t *testing.T) {
	_, err := ReadLinkErrors(t.TempDir(), "0000:3c:00.0")
	if !errors.Is(err, ErrAERAbsent) {
		t.Fatalf("err = %v, want ErrAERAbsent", err)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v wraps the cause", err)
	}
	if !strings.Contains(err.Error(), "aer_dev_correctable") {
		t.Fatalf("err = %v does not name the file", err)
	}
}

func overwrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mkTree creates the sysfs device tree rel (under devices/) with a
// bus/pci/devices symlink for every PCI function on it; types gives the
// pcie_type of the functions that have one.
func mkTree(t *testing.T, root, rel string, types map[string]string) {
	t.Helper()
	dir := filepath.Join(root, "devices", rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bus/pci/devices"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "devices")
	for _, part := range strings.Split(rel, "/") {
		path = filepath.Join(path, part)
		if !IsBDF(part) {
			continue
		}
		link := filepath.Join(root, "bus/pci/devices", part)
		if _, err := os.Lstat(link); err != nil {
			if err := os.Symlink(path, link); err != nil {
				t.Fatal(err)
			}
		}
		if ty, ok := types[part]; ok {
			overwrite(t, filepath.Join(path, "pcie_type"), ty+"\n")
		}
	}
}

func TestAncestryLinks(t *testing.T) {
	const (
		rp  = "0000:3a:00.0"
		up  = "0000:3b:00.0"
		dn  = "0000:3c:00.0"
		up2 = "0000:3d:00.0"
		dn2 = "0000:3e:00.0"
		dev = "0000:3f:00.0"
	)
	tests := []struct {
		name  string
		rel   string
		types map[string]string
		want  []AncestryLink
		err   bool
	}{
		{name: "device under the root port", rel: "pci0000:3a/" + rp + "/" + dev,
			want: []AncestryLink{{rp, dev}}},
		{name: "one switch", rel: "pci0000:3a/" + rp + "/" + up + "/" + dn + "/" + dev,
			want: []AncestryLink{{rp, up}, {dn, dev}}},
		{name: "cascaded switches", rel: "pci0000:3a/" + rp + "/" + up + "/" + dn + "/" + up2 + "/" + dn2 + "/" + dev,
			want: []AncestryLink{{rp, up}, {dn, up2}, {dn2, dev}}},
		{name: "types agree", rel: "pci0000:3a/" + rp + "/" + up + "/" + dn + "/" + dev,
			types: map[string]string{rp: "4", up: "5", dn: "6", dev: "0"},
			want:  []AncestryLink{{rp, up}, {dn, dev}}},
		{name: "no root port", rel: "pci0000:3a/" + dev, err: true},
		{name: "odd ancestry", rel: "pci0000:3a/" + rp + "/" + dn + "/" + dev, err: true},
		{name: "downstream port at the top", rel: "pci0000:3a/" + rp + "/" + dev,
			types: map[string]string{rp: "6"}, err: true},
		{name: "two upstream ports in a row", rel: "pci0000:3a/" + rp + "/" + up + "/" + dn + "/" + dev,
			types: map[string]string{up: "5", dn: "5"}, err: true},
		{name: "endpoint above the device", rel: "pci0000:3a/" + rp + "/" + up + "/" + dn + "/" + dev,
			types: map[string]string{up: "0"}, err: true},
		{name: "device is a switch port", rel: "pci0000:3a/" + rp + "/" + dev,
			types: map[string]string{dev: "5"}, err: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mkTree(t, root, tc.rel, tc.types)
			got, err := AncestryLinks(root, dev)
			if tc.err {
				if err == nil {
					t.Fatalf("links = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("links = %v, want %v", got, tc.want)
			}
		})
	}

	if _, err := AncestryLinks(t.TempDir(), dev); err == nil {
		t.Fatal("a device that is not in sysfs has links")
	}
	if _, err := AncestryLinks(t.TempDir(), "rp.0"); err == nil {
		t.Fatal("a name that is no BDF has links")
	}
}

func TestLinkPortAndDeviceFunctions(t *testing.T) {
	root := t.TempDir()
	const rp = "pci0000:3a/0000:3a:00.0"
	mkTree(t, root, rp+"/0000:3b:00.0", nil)
	mkTree(t, root, rp+"/0000:3b:00.1", nil)
	// Another device number on the same bus (ARI, or a fixture) is not a
	// function of the same device.
	mkTree(t, root, rp+"/0000:3b:01.0", nil)
	if err := os.MkdirAll(filepath.Join(root, "devices", rp, "pci_bus"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := LinkPort(root, "0000:3b:00.1"); got != "0000:3a:00.0" {
		t.Fatalf("LinkPort = %q, want the root port", got)
	}
	// A root port's sysfs parent is the host bridge, which is no PCI function.
	if got := LinkPort(root, "0000:3a:00.0"); got != "" {
		t.Fatalf("LinkPort of a root port = %q, want none", got)
	}
	if got := LinkPort(root, "0000:99:00.0"); got != "" {
		t.Fatalf("LinkPort of a missing device = %q, want none", got)
	}

	want := []string{"0000:3b:00.0", "0000:3b:00.1"}
	for _, fn := range want {
		if got := DeviceFunctions(root, fn); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("DeviceFunctions(%s) = %v, want %v", fn, got, want)
		}
	}
	if got := DeviceFunctions(root, "0000:99:00.0"); fmt.Sprint(got) != "[0000:99:00.0]" {
		t.Fatalf("DeviceFunctions of a missing device = %v, want itself", got)
	}
	if got := DeviceFunctions(root, "bogus"); got != nil {
		t.Fatalf("DeviceFunctions of a non-BDF = %v, want none", got)
	}
}
