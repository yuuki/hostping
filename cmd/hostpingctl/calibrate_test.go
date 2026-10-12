package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/model"
)

// hostpingctl calibrate --sim --runs 3 --interval 0, end to end: the file it
// prints is accepted by the --baseline-file loader and carries the Fig.2
// world's healthy readings, and merge accepts what calibrate wrote.
func TestCalibrateSimEndToEnd(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := runCalibrate([]string{"--sim", "--runs", "3", "--interval", "0"}, &stdout, &stderr); err != nil {
		t.Fatalf("calibrate: %v\n%s", err, stderr.String())
	}
	a := filepath.Join(dir, "a.json")
	if err := os.WriteFile(a, stdout.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := baseline.LoadFile(a)
	if err != nil {
		t.Fatalf("loader rejected calibrate output: %v\n%s", err, stdout.String())
	}
	def := baseline.Defaults(baseline.DefaultRateBPS)
	for _, d := range []model.Distance{model.DistanceSameSwitch, model.DistanceCrossRoot, model.DistanceCrossSocket, model.DistanceLocalMem, model.DistanceRemoteMem} {
		bw := got.Distances[d].BandwidthBPS
		want := def[d].BandwidthBPS
		// The healthy simulator reads its Fig.4 numbers back to within a percent.
		if bw < want*99/100 || bw > want*101/100 {
			t.Errorf("%s = %d, want about %d", d.Key(), bw, want)
		}
		if got.Distances[d].LatencyNs != 0 {
			t.Errorf("%s: latency written without --latency", d.Key())
		}
	}
	if _, ok := got.Distances[model.DistanceMultiSwitch]; ok {
		t.Error("Fig.2 has no multi_switch path, yet one was written")
	}
	if !strings.Contains(stderr.String(), "no measured path") || !strings.Contains(stderr.String(), "3 idle-mode runs used") {
		t.Errorf("table:\n%s", stderr.String())
	}

	b := filepath.Join(dir, "b.json")
	stderr.Reset()
	if err := runCalibrate([]string{"--sim", "--runs", "1", "--interval", "0", "--latency", "--out", b}, &bytes.Buffer{}, &stderr); err != nil {
		t.Fatalf("calibrate --out: %v\n%s", err, stderr.String())
	}
	if lb, err := baseline.LoadFile(b); err != nil || lb.Distances[model.DistanceLocalMem].LatencyNs == 0 {
		t.Fatalf("--latency file = %+v, %v", lb, err)
	}

	var merged bytes.Buffer
	stderr.Reset()
	if err := runCalibrate([]string{"merge", a, b}, &merged, &stderr); err != nil {
		t.Fatalf("merge: %v\n%s", err, stderr.String())
	}
	m := filepath.Join(dir, "fleet.json")
	if err := os.WriteFile(m, merged.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := baseline.LoadFile(m); err != nil {
		t.Fatalf("loader rejected merge output: %v\n%s", err, merged.String())
	}
}

func TestCalibrateRejectsStrayArguments(t *testing.T) {
	err := runCalibrate([]string{"--sim", "a.json"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "calibrate merge") {
		t.Fatalf("err = %v", err)
	}
	if err := runCalibrate([]string{"merge"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("merge without files accepted")
	}
}
