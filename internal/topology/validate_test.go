package topology

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validGraph is the smallest topology the engine can actually probe.
const validGraph = `{
  "name": "one-rail",
  "rnics": [{"id": "rnic0", "device": "mlx5_0", "socket": 0, "root_port": "rp.0", "line_rate_bps": 200000000000}],
  "endpoints": [{"id": "mem0", "kind": "memory", "socket": 0, "numa": 0}],
  "links": [
    {"id": "pcie.rnic0", "kind": "pcie_rnic", "socket": 0, "capacity_bps": 200000000000},
    {"id": "rp.0", "kind": "root_port", "socket": 0, "capacity_bps": 200000000000},
    {"id": "mem.0", "kind": "memory", "socket": 0, "capacity_bps": 200000000000}
  ],
  "paths": [
    {"rnic": "rnic0", "endpoint": "mem0", "link_ids": ["pcie.rnic0", "rp.0", "mem.0"],
     "distance": 10, "affinitive": true}
  ]
}`

func writeGraph(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "topology.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A valid file must keep loading exactly as before.
func TestLoadFileAcceptsValidGraph(t *testing.T) {
	g, err := LoadFile(writeGraph(t, validGraph))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if g.Name != "one-rail" || len(g.Paths) != 1 || len(g.Paths[0].LinkIDs) != 3 {
		t.Fatalf("graph did not round-trip: %+v", g)
	}
	if _, ok := g.Path("rnic0", "mem0"); !ok {
		t.Error("path rnic0→mem0 missing")
	}
}

// Fig.2 is the reference topology and must validate.
func TestFig2Validates(t *testing.T) {
	if err := Fig2().Validate(); err != nil {
		t.Fatalf("Fig2: %v", err)
	}
}

func TestLoadFileRejectsMalformed(t *testing.T) {
	// drop removes a top-level key from the valid fixture.
	drop := func(key string) string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(validGraph), &m); err != nil {
			t.Fatal(err)
		}
		delete(m, key)
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// with replaces one top-level key of the valid fixture.
	with := func(key, raw string) string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(validGraph), &m); err != nil {
			t.Fatal(err)
		}
		m[key] = json.RawMessage(raw)
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty object", `{}`, "no rnics"},
		{"no rnics", drop("rnics"), "no rnics"},
		{"no endpoints", drop("endpoints"), "no endpoints"},
		{"no links", drop("links"), "no links"},
		{"no paths", drop("paths"), "no paths"},
		{"empty paths", with("paths", `[]`), "no paths"},
		{"blank rnic id", with("rnics", `[{"id": "", "device": "mlx5_0"}]`), "empty id"},
		{
			"duplicate endpoint id",
			with("endpoints", `[{"id": "mem0", "kind": "memory"}, {"id": "mem0", "kind": "memory"}]`),
			`duplicate id "mem0"`,
		},
		{
			"unknown rnic in path",
			with("paths", `[{"rnic": "rnic9", "endpoint": "mem0", "link_ids": ["pcie.rnic0"]}]`),
			`unknown rnic "rnic9"`,
		},
		{
			"unknown endpoint in path",
			with("paths", `[{"rnic": "rnic0", "endpoint": "gpu9", "link_ids": ["pcie.rnic0"]}]`),
			`unknown endpoint "gpu9"`,
		},
		{
			"unknown link in path",
			with("paths", `[{"rnic": "rnic0", "endpoint": "mem0", "link_ids": ["pcie.rnic0", "upi.7"]}]`),
			`unknown link "upi.7"`,
		},
		{
			"path without links",
			with("paths", `[{"rnic": "rnic0", "endpoint": "mem0"}]`),
			"no link_ids",
		},
		{
			"duplicate path",
			with("paths", `[{"rnic": "rnic0", "endpoint": "mem0", "link_ids": ["pcie.rnic0"]},
			                {"rnic": "rnic0", "endpoint": "mem0", "link_ids": ["pcie.rnic0"]}]`),
			"duplicate path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadFile(writeGraph(t, tc.body))
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "topology") {
				t.Errorf("error = %q, want it to name the topology file", err)
			}
		})
	}
}

func TestLoadFileRejectsBrokenJSON(t *testing.T) {
	if _, err := LoadFile(writeGraph(t, `{"name":`)); err == nil {
		t.Fatal("truncated JSON was accepted")
	}
}

// Baseline derivation reads the PCIe capacity off the RNIC, so every graph
// source must fill it in from the pcie_rnic hop.
func TestRNICsCarryPCIeHopCapacity(t *testing.T) {
	for _, n := range Fig2().RNICs {
		if n.PCIeCapBPS != pcieGen4x16 {
			t.Fatalf("Fig.2 %s: pcie capacity %d, want %d", n.ID, n.PCIeCapBPS, uint64(pcieGen4x16))
		}
	}

	g, err := LoadFile(writeGraph(t, validGraph))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if g.RNICs[0].PCIeCapBPS != 200_000_000_000 {
		t.Fatalf("loaded file: pcie capacity %d, want 200000000000", g.RNICs[0].PCIeCapBPS)
	}
}

// A file that states the capacity itself wins over the hop lookup, and one with
// no pcie_rnic hop stays at 0 ("unknown"), which leaves baselines on line rate.
func TestRNICPCIeCapacityExplicitAndMissing(t *testing.T) {
	explicit := strings.Replace(validGraph,
		`"line_rate_bps": 200000000000}`,
		`"line_rate_bps": 200000000000, "pcie_capacity_bps": 126000000000}`, 1)
	g, err := LoadFile(writeGraph(t, explicit))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if g.RNICs[0].PCIeCapBPS != 126_000_000_000 {
		t.Fatalf("explicit pcie_capacity_bps must win: %d", g.RNICs[0].PCIeCapBPS)
	}

	noHop := strings.Replace(validGraph, `"kind": "pcie_rnic"`, `"kind": "root_port"`, 1)
	g, err = LoadFile(writeGraph(t, noHop))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if g.RNICs[0].PCIeCapBPS != 0 {
		t.Fatalf("no pcie_rnic hop must stay unknown: %d", g.RNICs[0].PCIeCapBPS)
	}
}
