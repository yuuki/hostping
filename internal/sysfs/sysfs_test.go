package sysfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadString(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "f"), " 4: ACTIVE\n")
	if got := ReadString(filepath.Join(root, "f")); got != " 4: ACTIVE\n" {
		t.Fatalf("ReadString keeps the contents as they are, got %q", got)
	}
	if got := ReadString(filepath.Join(root, "missing")); got != "" {
		t.Fatalf("missing file = %q, want \"\"", got)
	}
}

// The two integer readers agree on everything but the sign: ReadInt passes a
// negative value through (pci's and the uncore's reader always did) and
// ReadIntNonNegative turns it into the default (topology's reader always
// did, which is how an RNIC with numa_node -1 lands on NUMA 0).
func TestReadIntSignedness(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name, body     string
		signed, nonNeg int
	}{
		{"plain", "3", 3, 3},
		{"trailing newline", "3\n", 3, 3},
		{"whitespace", "  7 \n\n", 7, 7},
		{"negative", "-1\n", -1, 9},
		{"negative two", "-2", -2, 9},
		{"zero", "0\n", 0, 0},
		{"non-numeric", "abc\n", 9, 9},
		{"hex", "0x10\n", 9, 9},
		{"float", "1.5", 9, 9},
		{"empty", "", 9, 9},
		{"blank", "\n", 9, 9},
		{"two fields", "1 2", 9, 9},
		{"missing", "", 9, 9},
	}
	for _, c := range cases {
		p := filepath.Join(root, c.name)
		if c.name != "missing" {
			write(t, p, c.body)
		}
		if got := ReadInt(p, 9); got != c.signed {
			t.Errorf("%s: ReadInt(%q) = %d, want %d", c.name, c.body, got, c.signed)
		}
		if got := ReadIntNonNegative(p, 9); got != c.nonNeg {
			t.Errorf("%s: ReadIntNonNegative(%q) = %d, want %d", c.name, c.body, got, c.nonNeg)
		}
	}
	// The default is returned as given, sign included: the NUMA default of
	// pci.ReadLink is -1 and topology's EDAC node default is -1.
	if got := ReadInt(filepath.Join(root, "missing"), -1); got != -1 {
		t.Fatalf("ReadInt default = %d, want -1", got)
	}
	if got := ReadIntNonNegative(filepath.Join(root, "negative"), -1); got != -1 {
		t.Fatalf("ReadIntNonNegative default = %d, want -1", got)
	}
}

func TestReadUint64OK(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name, body string
		want       uint64
		ok         bool
	}{
		{"plain", "12345\n", 12345, true},
		{"zero", "0", 0, true},
		{"whitespace", " 8 \n", 8, true},
		{"max", "18446744073709551615\n", 1<<64 - 1, true},
		{"negative", "-1\n", 0, false},
		{"non-numeric", "N/A\n", 0, false},
		{"empty", "", 0, false},
		{"overflow", "18446744073709551616", 0, false},
	}
	for _, c := range cases {
		p := filepath.Join(root, c.name)
		write(t, p, c.body)
		got, ok := ReadUint64OK(p)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: ReadUint64OK(%q) = %d,%v, want %d,%v", c.name, c.body, got, ok, c.want, c.ok)
		}
	}
	if got, ok := ReadUint64OK(filepath.Join(root, "missing")); got != 0 || ok {
		t.Fatalf("missing = %d,%v, want 0,false", got, ok)
	}
}

func TestParseCPUList(t *testing.T) {
	got, err := ParseCPUList("0-2,8,10-11\n")
	if err != nil || !reflect.DeepEqual(got, []int{0, 1, 2, 8, 10, 11}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := ParseCPUList(" \n"); err != nil || got != nil {
		t.Fatalf("blank list = %v, %v; want nil, nil", got, err)
	}
	for _, bad := range []string{"3-1", "a", "0,", "1-", "-1"} {
		if _, err := ParseCPUList(bad); err == nil {
			t.Errorf("ParseCPUList(%q): want an error", bad)
		}
	}
}

// Nodes reports the three cpulist outcomes apart, in directory order, and
// skips entries that are not nodeN.
func TestNodes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "devices/system/node")
	write(t, filepath.Join(dir, "node0/cpulist"), "0-1\n")
	write(t, filepath.Join(dir, "node1/cpulist"), "\n")
	if err := os.MkdirAll(filepath.Join(dir, "node2"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "node3/cpulist"), "abc\n")
	write(t, filepath.Join(dir, "node10/cpulist"), "4\n")
	write(t, filepath.Join(dir, "nodefoo/cpulist"), "0\n")
	write(t, filepath.Join(dir, "cpu0/cpulist"), "0\n")
	write(t, filepath.Join(dir, "has_cpu"), "0-1\n")
	nodes, err := Nodes(root)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	if want := []int{0, 1, 10, 2, 3}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("node IDs %v, want %v (directory order)", ids, want)
	}
	byID := map[int]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	if n := byID[0]; n.Name != "node0" || !reflect.DeepEqual(n.CPUs, []int{0, 1}) || n.ReadErr != nil || n.ParseErr != nil {
		t.Fatalf("node0 = %+v", n)
	}
	if n := byID[1]; n.CPUs != nil || n.ReadErr != nil || n.ParseErr != nil {
		t.Fatalf("empty cpulist: node1 = %+v, want no CPUs and no error", n)
	}
	if n := byID[2]; n.CPUs != nil || !errors.Is(n.ReadErr, fs.ErrNotExist) || n.ParseErr != nil {
		t.Fatalf("missing cpulist: node2 = %+v, want ReadErr not-exist", n)
	}
	if n := byID[3]; n.CPUs != nil || n.ReadErr != nil || n.ParseErr == nil {
		t.Fatalf("unparsable cpulist: node3 = %+v, want ParseErr", n)
	}
	if n := byID[10]; !reflect.DeepEqual(n.CPUs, []int{4}) {
		t.Fatalf("node10 = %+v", n)
	}
}

func TestNodesMissingDirectory(t *testing.T) {
	root := t.TempDir()
	nodes, err := Nodes(root)
	if !os.IsNotExist(err) || nodes != nil {
		t.Fatalf("got %v, %v; want os.IsNotExist", nodes, err)
	}
	// A directory with no nodeN entries is an empty, non-nil list.
	if err := os.MkdirAll(filepath.Join(root, "devices/system/node"), 0o755); err != nil {
		t.Fatal(err)
	}
	nodes, err = Nodes(root)
	if err != nil || len(nodes) != 0 {
		t.Fatalf("empty dir: %v, %v", nodes, err)
	}
}

func TestPackageID(t *testing.T) {
	root := t.TempDir()
	cpu := func(c int) string {
		return filepath.Join(root, "devices/system/cpu", "cpu"+itoa(c), "topology/physical_package_id")
	}
	write(t, cpu(0), "1\n")
	write(t, cpu(1), " 0 \n")
	write(t, cpu(2), "-3\n")
	write(t, cpu(3), "\n")
	if got := PackageID(root, 0); got != 1 {
		t.Errorf("cpu0 = %d, want 1", got)
	}
	if got := PackageID(root, 1); got != 0 {
		t.Errorf("cpu1 = %d, want 0", got)
	}
	// Signed: the callers' "< 0" checks catch it, whatever the number.
	if got := PackageID(root, 2); got != -3 {
		t.Errorf("cpu2 = %d, want -3 (passed through)", got)
	}
	if got := PackageID(root, 3); got != -1 {
		t.Errorf("cpu3 (blank) = %d, want -1", got)
	}
	if got := PackageID(root, 4); got != -1 {
		t.Errorf("cpu4 (missing) = %d, want -1", got)
	}
}

func itoa(n int) string { return string(rune('0' + n)) }

func TestPortActive(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		body string
		want bool
	}{
		{"4: ACTIVE\n", true},
		{"4: ACTIVE", true},
		{"active\n", true},
		{"ACTIVE", true},
		{"  4: whatever\n", true}, // numeric state 4, leading whitespace trimmed
		{"4:", true},
		{"5: ACTIVE_DEFER\n", true}, // substring match, as the three former checks did
		{"1: DOWN\n", false},
		{"2: INIT\n", false},
		{"3: ARMED\n", false},
		{"14: X\n", false}, // "4:" must be the prefix, not merely present
		{"", false},
		{"\n", false},
	}
	for i, c := range cases {
		dir := filepath.Join(root, "ports", itoa(i))
		write(t, filepath.Join(dir, "state"), c.body)
		if got := PortActive(dir); got != c.want {
			t.Errorf("state %q: PortActive = %v, want %v", c.body, got, c.want)
		}
	}
	if PortActive(filepath.Join(root, "ports", "missing")) {
		t.Fatal("a port without a state file is not active")
	}
}
