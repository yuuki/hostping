package calibrate

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

const gb = 1_000_000_000

const slowKey = "line_rate=100Gbps,pcie_hop=64Gbps"

// fig2SlowRNIC is Fig.2 with rnic3 a lower-line-rate RNIC in a narrow slot
// next to faster RNICs: it derives a Fig.4 table of its own.
func fig2SlowRNIC(t *testing.T) topology.Graph {
	t.Helper()
	g := fig2(t)
	for i := range g.RNICs {
		if g.RNICs[i].ID == "rnic3" {
			g.RNICs[i].LineRate = 100 * gb
			g.RNICs[i].PCIeCapBPS = 64 * gb
		}
	}
	return g
}

// Three idle runs: the fast RNICs read their memory at the fast level, rnic3
// at its own. In Fig.2 rnic3's local memory is mem1 and its remote mem0.
func twoTableRuns() []model.Report {
	var reps []model.Report
	for i := range uint64(3) {
		reps = append(reps, idleRun(
			pm("rnic0", "mem0", (196+i)*gb, 0), pm("rnic1", "mem0", (197+i)*gb, 0), pm("rnic2", "mem1", (198+i)*gb, 0),
			pm("rnic0", "mem1", (190+i)*gb, 0),
			pm("rnic3", "mem1", (60+i)*gb, 0), pm("rnic3", "mem0", (58+i)*gb, 0),
		))
	}
	return reps
}

func writeLoad(t *testing.T, data []byte) baseline.Overlay {
	t.Helper()
	path := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	ov, err := baseline.LoadFile(path)
	if err != nil {
		t.Fatalf("loader rejected %v\n%s", err, data)
	}
	return ov
}

func TestSummarizeGroupsByTable(t *testing.T) {
	g := fig2SlowRNIC(t)
	r := Summarize(g, twoTableRuns(), Options{AbnormalTh: 0.2})
	fastRNIC, _ := g.RNIC("rnic0")
	if r.TopTable != baseline.RNICTable(fastRNIC) || !reflect.DeepEqual(r.TopRNICs, []string{"rnic0", "rnic1", "rnic2"}) {
		t.Fatalf("top = %s %v", r.TopTable, r.TopRNICs)
	}
	if got := distResult(t, r, model.DistanceLocalMem).BandwidthBPS; got != 198*gb {
		t.Fatalf("top local_mem = %d, want the fast RNICs' level only", got)
	}
	if len(r.Sections) != 1 || r.Sections[0].Key != slowKey || !reflect.DeepEqual(r.Sections[0].RNICs, []string{"rnic3"}) {
		t.Fatalf("sections = %+v", r.Sections)
	}
	if len(r.Outliers) != 0 {
		t.Fatalf("the slow RNIC must not be an outlier of the fast table: %+v", r.Outliers)
	}

	data, err := MarshalFile(r.Baselines())
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "local_mem": {"bandwidth_bps":198000000000},
  "remote_mem": {"bandwidth_bps":191000000000},
  "tables": {
    "line_rate=100Gbps,pcie_hop=64Gbps": {
      "local_mem": {"bandwidth_bps":61000000000},
      "remote_mem": {"bandwidth_bps":59000000000}
    }
  }
}
`
	if string(data) != want {
		t.Fatalf("file:\n%s\nwant:\n%s", data, want)
	}
	if ov := writeLoad(t, data); !reflect.DeepEqual(ov, r.Baselines()) {
		t.Fatalf("round trip %+v, want %+v", ov, r.Baselines())
	}

	var tab bytes.Buffer
	r.RateTables = 2
	r.WriteTable(&tab)
	for _, w := range []string{
		"top level: Fig.4 table " + r.TopTable + " (RNICs rnic0, rnic1, rnic2)",
		"tables section " + slowKey + " (RNICs rnic3):",
		"NOTE: this host's RNICs derive 2 different Fig.4 tables",
		"the \"tables\" sections those of " + slowKey,
	} {
		if !strings.Contains(tab.String(), w) {
			t.Errorf("table lacks %q:\n%s", w, tab.String())
		}
	}
	if strings.Contains(tab.String(), "one value per distance for all of them") {
		t.Errorf("stale multi-table note:\n%s", tab.String())
	}
}

// A host whose RNICs derive one table writes exactly the file it wrote
// before sections existed, and no table note.
func TestSingleTableFileUnchanged(t *testing.T) {
	g := fig2(t)
	var reps []model.Report
	for i := range uint64(3) {
		reps = append(reps, idleRun(
			pm("rnic0", "mem0", (196+i)*gb, 1000), pm("rnic1", "mem0", (197+i)*gb, 1100), pm("rnic2", "mem1", (198+i)*gb, 1000),
			pm("rnic0", "mem1", (190+i)*gb, 1400),
		))
	}
	r := Summarize(g, reps, Options{Latency: true, AbnormalTh: 0.2})
	if r.TopTable != "" || len(r.Sections) != 0 {
		t.Fatalf("single-table host grew sections: %q %+v", r.TopTable, r.Sections)
	}
	data, err := MarshalFile(r.Baselines())
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "local_mem": {"bandwidth_bps":198000000000,"latency_ns":1000},
  "remote_mem": {"bandwidth_bps":191000000000,"latency_ns":1400}
}
`
	if string(data) != want {
		t.Fatalf("file:\n%s\nwant:\n%s", data, want)
	}
	var tab bytes.Buffer
	r.WriteTable(&tab)
	if strings.Contains(tab.String(), "tables") || strings.Contains(tab.String(), "Fig.4 table") {
		t.Fatalf("single-table summary mentions tables:\n%s", tab.String())
	}
}

func sectionHost(name string, top, slow uint64) MergeInput {
	ov := baseline.Overlay{Distances: map[model.Distance]baseline.PathBaseline{model.DistanceLocalMem: {BandwidthBPS: top}}}
	if slow > 0 {
		ov.Tables = map[string]map[model.Distance]baseline.PathBaseline{
			slowKey: {model.DistanceLocalMem: {BandwidthBPS: slow}},
		}
	}
	return MergeInput{Name: name, Bases: ov}
}

func TestMergeSections(t *testing.T) {
	in := []MergeInput{
		sectionHost("a", 200*gb, 60*gb),
		sectionHost("b", 196*gb, 64*gb),
		sectionHost("c", 198*gb, 0),
	}
	r, err := Merge(in, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sections) != 1 {
		t.Fatalf("sections = %+v", r.Sections)
	}
	s := r.Sections[0]
	if s.Key != slowKey || !reflect.DeepEqual(s.Files, []string{"a", "b"}) || !reflect.DeepEqual(s.Missing, []string{"c"}) {
		t.Fatalf("section = %+v", s)
	}
	b := r.Baselines()
	// Lower quartile of 60 and 64 over the files with the section only.
	if got := b.Tables[slowKey][model.DistanceLocalMem].BandwidthBPS; got != 61*gb {
		t.Fatalf("merged section local_mem = %d", got)
	}
	if got := b.Distances[model.DistanceLocalMem].BandwidthBPS; got != 197*gb {
		t.Fatalf("merged top local_mem = %d", got)
	}
	data, err := MarshalFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if ov := writeLoad(t, data); !reflect.DeepEqual(ov, b) {
		t.Fatalf("round trip %+v, want %+v", ov, b)
	}
	var tab bytes.Buffer
	r.WriteTable(&tab)
	if !strings.Contains(tab.String(), "WARNING: tables section "+slowKey+" is in a, b but not in c") {
		t.Fatalf("no missing-section warning:\n%s", tab.String())
	}

	// Files without sections merge as before and write no section.
	r, err = Merge([]MergeInput{sectionHost("a", 200*gb, 0), sectionHost("b", 196*gb, 0)}, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sections) != 0 || r.Baselines().Tables != nil {
		t.Fatalf("sections from section-less files: %+v", r.Sections)
	}
}
