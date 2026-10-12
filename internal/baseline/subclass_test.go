package baseline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const subClassFile = `{
  "same_switch": {"bandwidth_bps": 197000000000},
  "cross_root": {
    "bandwidth_bps": 125000000000,
    "latency_ns": 2200,
    "sub_classes": [
      {"name": "fast", "bandwidth": "197Gbps", "latency": "1us",
       "root_port_pairs": [["rp.0000:bb:01.0", "rp.0000:aa:01.0"], ["rp.0000:cc:01.0", "rp.0000:dd:01.0"]]},
      {"name": "mid", "bandwidth_bps": 160000000000,
       "root_port_pairs": [["rp.0000:aa:01.0", "rp.0000:cc:01.0"]]}
    ]
  }
}`

func TestLoadFileSubClasses(t *testing.T) {
	m, err := LoadFile(writeFile(t, subClassFile))
	if err != nil {
		t.Fatal(err)
	}
	cr := m.Distances[model.DistanceCrossRoot]
	if cr.BandwidthBPS != 125_000_000_000 || cr.LatencyNs != 2200 || len(cr.SubClasses) != 2 {
		t.Fatalf("cross_root = %+v", cr)
	}
	fast := cr.SubClasses[0]
	if fast.Name != "fast" || fast.BandwidthBPS != 197_000_000_000 || fast.LatencyNs != 1000 {
		t.Fatalf("fast = %+v (unit strings must parse like the parent's)", fast)
	}
	// Pairs are order-insensitive: stored canonical and sorted.
	want := []model.RootPortPair{{"rp.0000:aa:01.0", "rp.0000:bb:01.0"}, {"rp.0000:cc:01.0", "rp.0000:dd:01.0"}}
	if len(fast.RootPortPairs) != 2 || fast.RootPortPairs[0] != want[0] || fast.RootPortPairs[1] != want[1] {
		t.Fatalf("fast pairs = %v, want %v", fast.RootPortPairs, want)
	}
	if mid := cr.SubClasses[1]; mid.LatencyNs != 0 || mid.BandwidthBPS != 160_000_000_000 {
		t.Fatalf("mid = %+v: latency is optional", mid)
	}
	if len(m.Distances[model.DistanceSameSwitch].SubClasses) != 0 {
		t.Fatal("a distance without sub_classes grew one")
	}

	// ForPair resolves a path's values; an unset sub-class value is the
	// distance's own.
	b, name := cr.ForPair(model.NewRootPortPair("rp.0000:aa:01.0", "rp.0000:bb:01.0"))
	if name != "fast" || b.BandwidthBPS != 197_000_000_000 || b.LatencyNs != 1000 || b.SubClasses != nil {
		t.Fatalf("fast pair resolves to %+v %q", b, name)
	}
	b, name = cr.ForPair(model.NewRootPortPair("rp.0000:cc:01.0", "rp.0000:aa:01.0"))
	if name != "mid" || b.BandwidthBPS != 160_000_000_000 || b.LatencyNs != 2200 {
		t.Fatalf("mid pair resolves to %+v %q", b, name)
	}
	for _, p := range []model.RootPortPair{{}, model.NewRootPortPair("rp.0000:aa:01.0", "rp.0000:dd:01.0")} {
		if b, name := cr.ForPair(p); name != "" || b.BandwidthBPS != 125_000_000_000 {
			t.Fatalf("pair %v resolves to %+v %q, want the distance", p, b, name)
		}
	}
}

func TestLoadFileSubClassOnly(t *testing.T) {
	m, err := LoadFile(writeFile(t, `{"cross_root": {"sub_classes": [{"name": "fast", "bandwidth_bps": 197000000000, "root_port_pairs": [["rp.a", "rp.b"]]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	// The distance keeps its Fig.4 default; only the sub-class is set.
	eff := ForRNIC(model.RNIC{LineRate: DefaultRateBPS}, m.Distances)[model.DistanceCrossRoot]
	if eff.BandwidthBPS != 125_500_000_000 || len(eff.SubClasses) != 1 {
		t.Fatalf("effective cross_root = %+v", eff)
	}
}

func TestLoadFileRejectsBadSubClasses(t *testing.T) {
	cases := map[string]string{
		"unknown field":         `{"cross_root": {"bandwidth_bps": 1, "sub_classes": [{"name": "fast", "bandwidth_bps": 2, "pairs": [["rp.a", "rp.b"]]}]}}`,
		"unknown parent field":  `{"cross_root": {"bandwidth_bps": 1, "subclasses": []}}`,
		"pair in two classes":   `{"cross_root": {"bandwidth_bps": 1, "sub_classes": [{"name": "fast", "bandwidth_bps": 2, "root_port_pairs": [["rp.a", "rp.b"]]}, {"name": "mid", "bandwidth_bps": 3, "root_port_pairs": [["rp.b", "rp.a"]]}]}}`,
		"pair twice in a class": `{"cross_root": {"bandwidth_bps": 1, "sub_classes": [{"name": "fast", "bandwidth_bps": 2, "root_port_pairs": [["rp.a", "rp.b"], ["rp.b", "rp.a"]]}]}}`,
		"identical members":     `{"cross_root": {"bandwidth_bps": 1, "sub_classes": [{"name": "fast", "bandwidth_bps": 2, "root_port_pairs": [["rp.a", "rp.a"]]}]}}`,
		"three members":         `{"cross_root": {"bandwidth_bps": 1, "sub_classes": [{"name": "fast", "bandwidth_bps": 2, "root_port_pairs": [["rp.a", "rp.b", "rp.c"]]}]}}`,
		"empty member":          `{"cross_root": {"bandwidth_bps": 1, "sub_classes": [{"name": "fast", "bandwidth_bps": 2, "root_port_pairs": [["", "rp.b"]]}]}}`,
		"no pairs":              `{"cross_root": {"bandwidth_bps": 1, "sub_classes": [{"name": "fast", "bandwidth_bps": 2, "root_port_pairs": []}]}}`,
		"no values":             `{"cross_root": {"bandwidth_bps": 1, "sub_classes": [{"name": "fast", "root_port_pairs": [["rp.a", "rp.b"]]}]}}`,
		"no name":               `{"cross_root": {"bandwidth_bps": 1, "sub_classes": [{"bandwidth_bps": 2, "root_port_pairs": [["rp.a", "rp.b"]]}]}}`,
		"bad name":              `{"cross_root": {"bandwidth_bps": 1, "sub_classes": [{"name": "a/b", "bandwidth_bps": 2, "root_port_pairs": [["rp.a", "rp.b"]]}]}}`,
		"duplicate name":        `{"cross_root": {"bandwidth_bps": 1, "sub_classes": [{"name": "fast", "bandwidth_bps": 2, "root_port_pairs": [["rp.a", "rp.b"]]}, {"name": "fast", "bandwidth_bps": 2, "root_port_pairs": [["rp.c", "rp.d"]]}]}}`,
		"bad unit":              `{"cross_root": {"bandwidth_bps": 1, "sub_classes": [{"name": "fast", "bandwidth": "197", "root_port_pairs": [["rp.a", "rp.b"]]}]}}`,
		"both bandwidth forms":  `{"cross_root": {"bandwidth_bps": 1, "sub_classes": [{"name": "fast", "bandwidth_bps": 2, "bandwidth": "2bps", "root_port_pairs": [["rp.a", "rp.b"]]}]}}`,
		"empty entry":           `{"cross_root": {"sub_classes": []}}`,
	}
	for name, body := range cases {
		if _, err := LoadFile(writeFile(t, body)); err == nil {
			t.Errorf("%s: accepted %s", name, body)
		}
	}
}

// --baseline flags replace a distance's own values and keep the file's
// sub-classes of that distance.
func TestFlagsKeepFileSubClasses(t *testing.T) {
	m, err := Load(writeFile(t, subClassFile), []string{"cross_root=100Gbps"})
	if err != nil {
		t.Fatal(err)
	}
	cr := m.Distances[model.DistanceCrossRoot]
	if cr.BandwidthBPS != 100_000_000_000 || cr.LatencyNs != 2200 || len(cr.SubClasses) != 2 {
		t.Fatalf("cross_root = %+v", cr)
	}
}

func TestTableAndLogLinesShowSubClasses(t *testing.T) {
	m, err := LoadFile(writeFile(t, subClassFile))
	if err != nil {
		t.Fatal(err)
	}
	tab := Table(DefaultRateBPS, 0, m.Distances)
	var cr model.BaselineEntry
	for _, e := range tab.Entries {
		if e.Distance == "cross_root" {
			cr = e
		}
	}
	if len(cr.SubClasses) != 2 || cr.SubClasses[0].Name != "fast" || cr.SubClasses[0].BandwidthBPS != 197_000_000_000 ||
		cr.SubClasses[1].LatencyNs != 2200 || len(cr.SubClasses[0].RootPortPairs) != 2 {
		t.Fatalf("table cross_root sub-classes = %+v", cr.SubClasses)
	}
	line := FormatTable(tab)
	for _, want := range []string{"cross_root=125Gbps/2200ns (user/user)", "cross_root/fast=197Gbps/1us (sub-class, 2 root-port pair(s))", "cross_root/mid=160Gbps/2200ns"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %q: %s", want, line)
		}
	}
	src := DescribeSources(m)
	if !strings.Contains(src, "sub-classes from --baseline-file: cross_root/fast (2 root-port pair(s)), cross_root/mid (1 root-port pair(s))") {
		t.Errorf("DescribeSources = %s", src)
	}

	present := map[model.RootPortPair]bool{model.NewRootPortPair("rp.0000:aa:01.0", "rp.0000:bb:01.0"): true}
	un := DescribeUnmatched(m, present)
	for _, want := range []string{"2 sub-class root-port pair(s) not on this host", "cross_root/fast rp.0000:cc:01.0+rp.0000:dd:01.0", "cross_root/mid rp.0000:aa:01.0+rp.0000:cc:01.0"} {
		if !strings.Contains(un, want) {
			t.Errorf("DescribeUnmatched lacks %q: %s", want, un)
		}
	}
	present[model.NewRootPortPair("rp.0000:cc:01.0", "rp.0000:dd:01.0")] = true
	present[model.NewRootPortPair("rp.0000:aa:01.0", "rp.0000:cc:01.0")] = true
	if un := DescribeUnmatched(m, present); un != "" {
		t.Errorf("every pair present: %q", un)
	}
}
