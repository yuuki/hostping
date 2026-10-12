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
)

func host(name string, local, lat uint64) MergeInput {
	return MergeInput{Name: name, Bases: baseline.Uniform(map[model.Distance]baseline.PathBaseline{
		model.DistanceLocalMem: {BandwidthBPS: local, LatencyNs: lat},
	})}
}

func TestMergeLowerQuartileBandwidthUpperQuartileLatency(t *testing.T) {
	in := []MergeInput{
		host("a", 200, 1000), host("b", 190, 1100), host("c", 180, 1200), host("d", 170, 1300),
	}
	// One host also calibrated remote memory.
	in[0].Bases.Distances[model.DistanceRemoteMem] = baseline.PathBaseline{BandwidthBPS: 150}
	r, err := Merge(in, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	b := r.Baselines().Distances
	// Sorted 170, 180, 190, 200: p25 = 170 + 0.75*10 = 177.5 -> 178.
	if got := b[model.DistanceLocalMem].BandwidthBPS; got != 178 {
		t.Fatalf("local_mem bandwidth = %d, want 178", got)
	}
	// Sorted 1000..1300: p75 = 1225.
	if got := b[model.DistanceLocalMem].LatencyNs; got != 1225 {
		t.Fatalf("local_mem latency = %d, want 1225", got)
	}
	if got := b[model.DistanceRemoteMem]; got.BandwidthBPS != 150 || got.LatencyNs != 0 {
		t.Fatalf("remote_mem = %+v, want the one host's value", got)
	}
	if _, ok := b[model.DistanceSameSwitch]; ok {
		t.Fatal("distance in no file was written")
	}
	for _, row := range r.Rows {
		if len(row.Low) > 0 {
			t.Fatalf("%s: unexpected low hosts %v", row.Distance.Key(), row.Low)
		}
	}
}

func TestMergeWarnsAboutAHostFarBelow(t *testing.T) {
	in := []MergeInput{host("a", 200, 0), host("b", 200, 0), host("c", 198, 0), host("d", 199, 0), host("slow", 100, 0)}
	r, err := Merge(in, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Baselines().Distances[model.DistanceLocalMem].BandwidthBPS; got != 198 {
		t.Fatalf("p25 = %d, want 198: one bad host must not drag the fleet floor down", got)
	}
	var buf bytes.Buffer
	r.WriteTable(&buf)
	if !strings.Contains(buf.String(), "WARNING: slow: local_mem") {
		t.Fatalf("no warning for the slow host:\n%s", buf.String())
	}
}

func TestMergeNeedsInput(t *testing.T) {
	if _, err := Merge(nil, 0.2); err == nil {
		t.Fatal("merge of nothing succeeded")
	}
}

func TestMergeOutputRoundTrips(t *testing.T) {
	r, err := Merge([]MergeInput{host("a", 197_008_172_851, 0), host("b", 196_000_000_000, 0)}, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	data, err := MarshalFile(r.Baselines())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fleet.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := baseline.LoadFile(path)
	if err != nil {
		t.Fatalf("loader rejected merge output: %v\n%s", err, data)
	}
	if !reflect.DeepEqual(got.Distances[model.DistanceLocalMem], r.Baselines().Distances[model.DistanceLocalMem]) {
		t.Fatalf("round trip = %+v", got)
	}
}
