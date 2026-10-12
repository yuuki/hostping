package baseline

import (
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

const gbps = 1_000_000_000

func TestTableKey(t *testing.T) {
	cases := []struct {
		rate, hop uint64
		want      string
	}{
		{200 * gbps, 0, "line_rate=200Gbps"},
		{0, 0, "line_rate=200Gbps"},
		{100 * gbps, 64 * gbps, "line_rate=100Gbps,pcie_hop=64Gbps"},
		{400 * gbps, 252_060_000_000, "line_rate=400Gbps,pcie_hop=252.06Gbps"},
		{400 * gbps, 252_001_000_000, "line_rate=400Gbps,pcie_hop=252Gbps"},
		{100 * gbps, 31_500_000_000, "line_rate=100Gbps,pcie_hop=31.5Gbps"},
	}
	for _, tc := range cases {
		got := TableKey(tc.rate, tc.hop)
		if got != tc.want {
			t.Errorf("TableKey(%d, %d) = %q, want %q", tc.rate, tc.hop, got, tc.want)
		}
		if err := ValidateTableKey(got); err != nil {
			t.Errorf("ValidateTableKey(%q): %v", got, err)
		}
	}
	for _, bad := range []string{"", "200Gbps", "line_rate=200G", "line_rate=200.00Gbps", "line_rate=200Gbps,pcie_hop=64.0Gbps",
		"line_rate=200Gbps,hop=64Gbps", "pcie_hop=64Gbps,line_rate=200Gbps", "line_rate=200Gbps ,pcie_hop=64Gbps"} {
		if err := ValidateTableKey(bad); err == nil {
			t.Errorf("ValidateTableKey(%q) accepted", bad)
		}
	}
}

const tablesFile = `{
  "local_mem": {"bandwidth_bps": 197000000000},
  "remote_mem": {"bandwidth_bps": 190000000000},
  "tables": {
    "line_rate=100Gbps,pcie_hop=64Gbps": {
      "local_mem": {"bandwidth": "60Gbps", "latency_ns": 1000}
    }
  }
}`

func TestLoadFileTables(t *testing.T) {
	ov, err := LoadFile(writeFile(t, tablesFile))
	if err != nil {
		t.Fatal(err)
	}
	if ov.Distances[model.DistanceLocalMem].BandwidthBPS != 197*gbps || len(ov.Distances) != 2 {
		t.Fatalf("top level = %+v", ov.Distances)
	}
	slow := model.RNIC{ID: "dev1", LineRate: 100 * gbps, PCIeCapBPS: 64 * gbps}
	fast := model.RNIC{ID: "dev0", LineRate: 200 * gbps}
	if got := ov.ForRNIC(slow); got[model.DistanceLocalMem].BandwidthBPS != 60*gbps || got[model.DistanceLocalMem].LatencyNs != 1000 {
		t.Fatalf("slow RNIC overlay = %+v", got)
	}
	if got := ov.ForRNIC(fast); got[model.DistanceLocalMem].BandwidthBPS != 197*gbps {
		t.Fatalf("fast RNIC overlay = %+v, want the top level", got)
	}
	// A distance the section leaves out is the slow RNIC's own Fig.4
	// default, not the top-level value measured on the fast table.
	eff := ov.Effective(slow)[model.DistanceRemoteMem].BandwidthBPS
	if want := DefaultsFor(100*gbps, 64*gbps)[model.DistanceRemoteMem].BandwidthBPS; eff != want {
		t.Fatalf("slow remote_mem = %d, want its Fig.4 default %d", eff, want)
	}

	tabs := Tables([]model.RNIC{fast, slow}, ov)
	if len(tabs) != 2 || tabs[0].Section != "" || tabs[1].Section != "line_rate=100Gbps,pcie_hop=64Gbps" {
		t.Fatalf("tables = %+v", tabs)
	}
	if line := FormatTable(tabs[1]); !strings.Contains(line, "tables section line_rate=100Gbps,pcie_hop=64Gbps") || !strings.Contains(line, "local_mem=60Gbps/1us (user/user)") {
		t.Fatalf("log line: %s", line)
	}

	src := DescribeSources(ov)
	if lines := strings.Split(src, "\n"); len(lines) != 2 || !strings.HasPrefix(lines[1], "baselines: tables section line_rate=100Gbps,pcie_hop=64Gbps: bandwidth from --baseline-file/--baseline for local_mem;") {
		t.Fatalf("DescribeSources = %q", src)
	}
	desc := DescribeTables(ov, []model.RNIC{fast, slow, {ID: "dev2", LineRate: 100 * gbps, PCIeCapBPS: 32 * gbps}})
	for _, want := range []string{
		"line_rate=200Gbps (dev0) from top-level values",
		"line_rate=100Gbps,pcie_hop=64Gbps (dev1) from tables section",
		"line_rate=100Gbps,pcie_hop=32Gbps (dev2) from top-level values",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("DescribeTables lacks %q: %s", want, desc)
		}
	}
	if desc := DescribeTables(ov, []model.RNIC{fast}); !strings.Contains(desc, "matching no RNIC here, ignored: line_rate=100Gbps,pcie_hop=64Gbps") {
		t.Errorf("unused section not reported: %s", desc)
	}
	if DescribeTables(Uniform(ov.Distances), []model.RNIC{fast, slow}) != "" {
		t.Error("DescribeTables without sections should say nothing")
	}
}

// A --baseline flag overrides the file everywhere, sections included.
func TestLoadFlagsOverrideSections(t *testing.T) {
	ov, err := Load(writeFile(t, tablesFile), []string{"local_mem=50Gbps"})
	if err != nil {
		t.Fatal(err)
	}
	if ov.Distances[model.DistanceLocalMem].BandwidthBPS != 50*gbps {
		t.Fatalf("top level = %+v", ov.Distances)
	}
	sec := ov.Tables["line_rate=100Gbps,pcie_hop=64Gbps"][model.DistanceLocalMem]
	if sec.BandwidthBPS != 50*gbps || sec.LatencyNs != 1000 {
		t.Fatalf("section local_mem = %+v, want the flag's bandwidth and the file's latency", sec)
	}
}

func TestLoadFileTablesOnly(t *testing.T) {
	ov, err := LoadFile(writeFile(t, `{"tables": {"line_rate=100Gbps": {"local_mem": {"bandwidth_bps": 90000000000}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Distances) != 0 || len(ov.Tables) != 1 {
		t.Fatalf("overlay = %+v", ov)
	}
}

func TestLoadFileRejectsBadTables(t *testing.T) {
	cases := map[string]string{
		"not an object":       `{"local_mem": {"bandwidth_bps": 1}, "tables": []}`,
		"no sections":         `{"local_mem": {"bandwidth_bps": 1}, "tables": {}}`,
		"empty section":       `{"tables": {"line_rate=100Gbps": {}}}`,
		"bad key":             `{"tables": {"100G": {"local_mem": {"bandwidth_bps": 1}}}}`,
		"non-canonical key":   `{"tables": {"line_rate=100.0Gbps": {"local_mem": {"bandwidth_bps": 1}}}}`,
		"unknown distance":    `{"tables": {"line_rate=100Gbps": {"nope": {"bandwidth_bps": 1}}}}`,
		"unknown field":       `{"tables": {"line_rate=100Gbps": {"local_mem": {"bandwidth_bps": 1, "bw": 2}}}}`,
		"nested tables":       `{"tables": {"line_rate=100Gbps": {"tables": {}}}}`,
		"bad sub-class":       `{"tables": {"line_rate=100Gbps": {"cross_root": {"sub_classes": [{"name": "fast", "bandwidth_bps": 1}]}}}}`,
		"empty entry":         `{"tables": {"line_rate=100Gbps": {"local_mem": {}}}}`,
		"unknown top field":   `{"local_mem": {"bandwidth_bps": 1}, "table": {}}`,
		"unknown entry field": `{"local_mem": {"bandwidth_bps": 1, "x": 1}}`,
	}
	for name, body := range cases {
		if _, err := LoadFile(writeFile(t, body)); err == nil {
			t.Errorf("%s: accepted %s", name, body)
		}
	}
}
