package baseline

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

func TestDescribeSources(t *testing.T) {
	if got := DescribeSources(Overlay{}); got != "" {
		t.Fatalf("nothing set: %q", got)
	}
	got := DescribeSources(Uniform(map[model.Distance]PathBaseline{
		model.DistanceLocalMem:  {BandwidthBPS: 270_000_000_000},
		model.DistanceRemoteMem: {LatencyNs: 1400},
	}))
	for _, want := range []string{
		"bandwidth from --baseline-file/--baseline for local_mem;",
		"Fig.4 default for same_switch,multi_switch,cross_root,cross_socket,remote_mem;",
		"latency from --baseline-file/--baseline for remote_mem",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
}

func TestDefaultsScalesToLineRate(t *testing.T) {
	b200 := Defaults(0)
	if b200[model.DistanceSameSwitch].BandwidthBPS < 190_000_000_000 {
		t.Fatalf("200G local %d", b200[model.DistanceSameSwitch].BandwidthBPS)
	}
	b100 := Defaults(100_000_000_000)
	if b100[model.DistanceSameSwitch].BandwidthBPS > 100_000_000_000 {
		t.Fatalf("100G local baseline exceeds line rate: %d", b100[model.DistanceSameSwitch].BandwidthBPS)
	}
	if b100[model.DistanceCrossRoot].BandwidthBPS > 100_000_000_000 {
		t.Fatalf("100G cross-root baseline exceeds line rate: %d", b100[model.DistanceCrossRoot].BandwidthBPS)
	}
}

func TestForRNICUsesLineRate(t *testing.T) {
	n := model.RNIC{ID: "rnic0", LineRate: 100_000_000_000}
	b := ForRNIC(n, nil)
	if b[model.DistanceLocalMem].BandwidthBPS > 100_000_000_000 {
		t.Fatalf("ForRNIC ignored 100G line rate: %d", b[model.DistanceLocalMem].BandwidthBPS)
	}
}

// A 400G RNIC on a ~252 Gb/s PCIe hop must get a baseline the bus can actually
// clear. On that hardware class host-DRAM loopback measures ~266–278 Gb/s,
// while 0.985x400G asks for 394 Gb/s.
func TestForRNICClampsToPCIeHopCapacity(t *testing.T) {
	const hopCap = 252_000_000_000
	n := model.RNIC{ID: "rnic0", LineRate: 400_000_000_000, PCIeCapBPS: hopCap}
	b := ForRNIC(n, nil)

	for _, d := range []model.Distance{model.DistanceLocalMem, model.DistanceRemoteMem, model.DistanceSameSwitch} {
		got := b[d].BandwidthBPS
		if got > hopCap {
			t.Fatalf("%s baseline %d exceeds the %d hop feeding the RNIC", d, got, hopCap)
		}
		// A host of this class clears the baseline with room to spare.
		if got > 266_000_000_000 {
			t.Fatalf("%s baseline %d is above the ~266Gbps floor such a host clears", d, got)
		}
	}
	if b[model.DistanceLocalMem].LatencyNs != 1000 {
		t.Fatalf("clamping must not touch Fig.4 latencies: %d", b[model.DistanceLocalMem].LatencyNs)
	}
	// Fig.4 ratios still order the distances; only the rate they scale from moved.
	if b[model.DistanceCrossRoot].BandwidthBPS >= b[model.DistanceSameSwitch].BandwidthBPS {
		t.Fatalf("cross_root %d should stay below same_switch %d",
			b[model.DistanceCrossRoot].BandwidthBPS, b[model.DistanceSameSwitch].BandwidthBPS)
	}
	if b[model.DistanceCrossSocket].BandwidthBPS >= b[model.DistanceCrossRoot].BandwidthBPS {
		t.Fatalf("cross_socket %d should stay below cross_root %d",
			b[model.DistanceCrossSocket].BandwidthBPS, b[model.DistanceCrossRoot].BandwidthBPS)
	}
}

// The paper's hosts are 200G on Gen4 x16, where the wire is under the bus.
// Those defaults must not move.
func TestForRNIC200GKeepsFig4Values(t *testing.T) {
	n := model.RNIC{ID: "rnic0", LineRate: 200_000_000_000, PCIeCapBPS: 252_000_000_000}
	got := ForRNIC(n, nil)
	want := map[model.Distance]PathBaseline{
		model.DistanceSameSwitch:  {BandwidthBPS: 197_000_000_000, LatencyNs: 1000},
		model.DistanceMultiSwitch: {BandwidthBPS: 197_000_000_000, LatencyNs: 1050},
		model.DistanceCrossRoot:   {BandwidthBPS: 125_500_000_000, LatencyNs: 2200},
		model.DistanceCrossSocket: {BandwidthBPS: 116_400_000_000, LatencyNs: 2400},
		model.DistanceLocalMem:    {BandwidthBPS: 197_000_000_000, LatencyNs: 1000},
		model.DistanceRemoteMem:   {BandwidthBPS: 197_000_000_000, LatencyNs: 1400},
	}
	for d, w := range want {
		if !reflect.DeepEqual(got[d], w) {
			t.Fatalf("200G %s: got %+v want %+v", d, got[d], w)
		}
	}
}

func TestForRNICUnknownHopCapacityKeepsLineRateScaling(t *testing.T) {
	n := model.RNIC{ID: "rnic0", LineRate: 400_000_000_000}
	got := ForRNIC(n, nil)
	want := Defaults(400_000_000_000)
	for d := range want {
		if !reflect.DeepEqual(got[d], want[d]) {
			t.Fatalf("no hop capacity must leave %s scaled to line rate: got %+v want %+v", d, got[d], want[d])
		}
	}
}

// An override is absolute: it wins over the clamped default in both directions.
func TestForRNICOverrideBeatsClamp(t *testing.T) {
	n := model.RNIC{ID: "rnic0", LineRate: 400_000_000_000, PCIeCapBPS: 252_000_000_000}
	ov := map[model.Distance]PathBaseline{
		model.DistanceLocalMem:  {BandwidthBPS: 300_000_000_000},
		model.DistanceRemoteMem: {BandwidthBPS: 100_000_000_000, LatencyNs: 3000},
	}
	b := ForRNIC(n, ov)
	if b[model.DistanceLocalMem].BandwidthBPS != 300_000_000_000 {
		t.Fatalf("override above the hop capacity must still win: %d", b[model.DistanceLocalMem].BandwidthBPS)
	}
	if b[model.DistanceRemoteMem].BandwidthBPS != 100_000_000_000 || b[model.DistanceRemoteMem].LatencyNs != 3000 {
		t.Fatalf("override below the clamp must still win: %+v", b[model.DistanceRemoteMem])
	}
	if b[model.DistanceSameSwitch].BandwidthBPS != uint64(float64(252_000_000_000)*0.985) {
		t.Fatalf("unset same_switch should stay clamped: %d", b[model.DistanceSameSwitch].BandwidthBPS)
	}
}

func TestForRNICAppliesOverrideAfterScale(t *testing.T) {
	n := model.RNIC{ID: "rnic0", LineRate: 400_000_000_000}
	ov := map[model.Distance]PathBaseline{
		model.DistanceLocalMem: {BandwidthBPS: 270_000_000_000},
	}
	b := ForRNIC(n, ov)
	if b[model.DistanceLocalMem].BandwidthBPS != 270_000_000_000 {
		t.Fatalf("local_mem override: %d", b[model.DistanceLocalMem].BandwidthBPS)
	}
	if b[model.DistanceLocalMem].LatencyNs != Defaults(400_000_000_000)[model.DistanceLocalMem].LatencyNs {
		t.Fatalf("unset latency should stay default: %d", b[model.DistanceLocalMem].LatencyNs)
	}
	scaledRemote := Defaults(400_000_000_000)[model.DistanceRemoteMem]
	if !reflect.DeepEqual(b[model.DistanceRemoteMem], scaledRemote) {
		t.Fatalf("unset remote_mem should stay Fig.4×400G: %+v vs %+v", b[model.DistanceRemoteMem], scaledRemote)
	}
	if b[model.DistanceSameSwitch].BandwidthBPS != uint64(float64(400_000_000_000)*0.985) {
		t.Fatalf("unset same_switch should stay scaled: %d", b[model.DistanceSameSwitch].BandwidthBPS)
	}
}

func TestParseSpecs(t *testing.T) {
	m, err := ParseSpecs("local_mem=270Gbps,1400ns;remote_mem=265Gbps")
	if err != nil {
		t.Fatal(err)
	}
	if m[model.DistanceLocalMem].BandwidthBPS != 270_000_000_000 || m[model.DistanceLocalMem].LatencyNs != 1400 {
		t.Fatalf("local_mem %+v", m[model.DistanceLocalMem])
	}
	if m[model.DistanceRemoteMem].BandwidthBPS != 265_000_000_000 || m[model.DistanceRemoteMem].LatencyNs != 0 {
		t.Fatalf("remote_mem %+v", m[model.DistanceRemoteMem])
	}
	m, err = ParseSpecs("local_mem=270Gbps,1400ns,remote_mem=270Gbps,1.8us")
	if err != nil {
		t.Fatal(err)
	}
	if m[model.DistanceRemoteMem].LatencyNs != 1800 {
		t.Fatalf("1.8us → %d", m[model.DistanceRemoteMem].LatencyNs)
	}
	if _, err := ParseSpecs("local_mem=270"); err == nil {
		t.Fatal("bare number without unit must fail")
	}
}

func TestLoadJSON(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "b.json")
	if err := os.WriteFile(jsonPath, []byte(`{
  "local_mem":  { "bandwidth_bps": 270000000000, "latency_ns": 2700 },
  "remote_mem": { "bandwidth": "265Gbps", "latency": "3us" }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if m.Distances[model.DistanceLocalMem].BandwidthBPS != 270_000_000_000 || m.Distances[model.DistanceLocalMem].LatencyNs != 2700 {
		t.Fatalf("json local %+v", m.Distances[model.DistanceLocalMem])
	}
	if m.Distances[model.DistanceRemoteMem].BandwidthBPS != 265_000_000_000 || m.Distances[model.DistanceRemoteMem].LatencyNs != 3000 {
		t.Fatalf("json remote %+v", m.Distances[model.DistanceRemoteMem])
	}
}

func TestLoadFileRejectsYAMLAndUnknownFields(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "b.yaml")
	if err := os.WriteFile(yamlPath, []byte("---\nlocal_mem:\n  bandwidth: 270Gbps\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(yamlPath); err == nil {
		t.Fatal("YAML must be rejected")
	}
	typo := filepath.Join(dir, "typo.json")
	if err := os.WriteFile(typo, []byte(`{"local_mem":{"bandwith":"270Gbps","latency":"1400ns"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(typo); err == nil {
		t.Fatal("unknown JSON field must fail, not drop bandwidth")
	}
}

func TestParseBandwidthRejectsNaNAndOverflow(t *testing.T) {
	if _, err := ParseBandwidth("NaNGbps"); err == nil {
		t.Fatal("NaN")
	}
	if _, err := ParseBandwidth("InfGbps"); err == nil {
		t.Fatal("Inf")
	}
	if _, err := ParseBandwidth("1e20Gbps"); err == nil {
		t.Fatal("overflow")
	}
}

func TestLoadFlagsOverrideFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "b.json")
	if err := os.WriteFile(p, []byte(`{"local_mem":{"bandwidth_bps":200000000000}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Load(p, []string{"local_mem=270Gbps"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Distances[model.DistanceLocalMem].BandwidthBPS != 270_000_000_000 {
		t.Fatalf("flag should win: %d", m.Distances[model.DistanceLocalMem].BandwidthBPS)
	}
}

func TestTableMarksUserVsDefault(t *testing.T) {
	ov := map[model.Distance]PathBaseline{model.DistanceLocalMem: {BandwidthBPS: 270_000_000_000}}
	tab := Table(400_000_000_000, 0, ov)
	if tab.LineRateBPS != 400_000_000_000 {
		t.Fatalf("line rate %d", tab.LineRateBPS)
	}
	var local, remote model.BaselineEntry
	for _, e := range tab.Entries {
		switch e.Distance {
		case "local_mem":
			local = e
		case "remote_mem":
			remote = e
		}
	}
	if local.BandwidthSource != model.BaselineSourceUser || local.LatencySource != model.BaselineSourceDefault {
		t.Fatalf("local %+v", local)
	}
	if remote.BandwidthSource != model.BaselineSourceDefault {
		t.Fatalf("remote should stay default: %+v", remote)
	}
}

func TestTableReportsDerivedRateAndKeepsClampADefault(t *testing.T) {
	ov := map[model.Distance]PathBaseline{model.DistanceLocalMem: {BandwidthBPS: 270_000_000_000}}
	tab := Table(400_000_000_000, 252_000_000_000, ov)
	if tab.LineRateBPS != 400_000_000_000 {
		t.Fatalf("line rate must stay the wire speed: %d", tab.LineRateBPS)
	}
	if tab.DerivedFromBPS != 252_000_000_000 {
		t.Fatalf("derived_from should be the hop capacity: %d", tab.DerivedFromBPS)
	}
	for _, e := range tab.Entries {
		if e.Distance == "local_mem" {
			if e.BandwidthBPS != 270_000_000_000 || e.BandwidthSource != model.BaselineSourceUser {
				t.Fatalf("overridden local_mem %+v", e)
			}
			continue
		}
		// Clamping changes the value the default derives from, not who set it.
		if e.BandwidthSource != model.BaselineSourceDefault || e.LatencySource != model.BaselineSourceDefault {
			t.Fatalf("%s must stay default-sourced: %+v", e.Distance, e)
		}
	}

	if got := FormatTable(tab); !strings.Contains(got, "derived_from=252Gbps (pcie hop)") {
		t.Fatalf("log line must say why the baseline moved: %s", got)
	}
	if got := FormatTable(Table(200_000_000_000, 252_000_000_000, nil)); strings.Contains(got, "derived_from") {
		t.Fatalf("an unclamped 200G table should keep the old one-line form: %s", got)
	}
}

func TestTablesKeyOnLineRateAndHopCapacity(t *testing.T) {
	rnics := []model.RNIC{
		{ID: "rnic0", LineRate: 400_000_000_000, PCIeCapBPS: 252_000_000_000},
		{ID: "rnic1", LineRate: 400_000_000_000, PCIeCapBPS: 252_000_000_000},
		{ID: "rnic2", LineRate: 400_000_000_000, PCIeCapBPS: 126_000_000_000},
	}
	got := Tables(rnics, Overlay{})
	if len(got) != 2 {
		t.Fatalf("same wire on two different buses needs two tables, got %d", len(got))
	}
	if got[0].DerivedFromBPS != 252_000_000_000 || got[1].DerivedFromBPS != 126_000_000_000 {
		t.Fatalf("tables %+v", got)
	}
}

// A hop wider than the Gen4 x16 bus Fig.4 was measured behind is held to it.
// A 400G RNIC in a Gen5 x16 slot (~504 Gb/s) would otherwise ask host DRAM for
// 0.985×400G = 394 Gb/s, which a healthy host of that class (about 270 Gb/s)
// never reaches.
func TestDerivedRateHoldsWideHopToFig4Bus(t *testing.T) {
	const gen5x16 = 504_000_000_000
	cases := []struct {
		line, hop, want uint64
	}{
		{400_000_000_000, gen5x16, Fig4BusBPS},
		{400_000_000_000, 252_000_000_000, 252_000_000_000},
		{400_000_000_000, 126_000_000_000, 126_000_000_000},
		{200_000_000_000, gen5x16, 200_000_000_000},
		{400_000_000_000, 0, 400_000_000_000}, // unknown hop: line rate alone
	}
	for _, tc := range cases {
		if got := DerivedRate(tc.line, tc.hop); got != tc.want {
			t.Errorf("DerivedRate(%d, %d)=%d want %d", tc.line, tc.hop, got, tc.want)
		}
	}
	local := ForRNIC(model.RNIC{LineRate: 400_000_000_000, PCIeCapBPS: gen5x16}, nil)[model.DistanceLocalMem].BandwidthBPS
	if local > 266_000_000_000 {
		t.Fatalf("Gen5 local_mem default %d is above what a healthy host of that class clears", local)
	}

	tab := Table(400_000_000_000, gen5x16, nil)
	if tab.PCIeCapBPS != gen5x16 || tab.DerivedFromBPS != Fig4BusBPS {
		t.Fatalf("table %+v", tab)
	}
	if got := FormatTable(tab); !strings.Contains(got, "derived_from=252Gbps (fig.4 bus; pcie hop 504Gbps)") {
		t.Fatalf("log line must say the hop was held to the Fig.4 bus: %s", got)
	}
}
