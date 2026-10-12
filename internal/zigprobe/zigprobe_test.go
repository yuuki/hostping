package zigprobe_test

import (
	"testing"

	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/zigprobe"
)

func TestZigEquationMatchesGo(t *testing.T) {
	if zigprobe.Version() == "" {
		t.Fatal("empty version")
	}
	const size, bw uint64 = 128 * 1024, 200_000_000_000
	got := zigprobe.SimLatency(400, 600, size, bw)
	want := sim.LatencyNs(400, 600, size, bw)
	if got != want {
		t.Fatalf("zig %d go %d", got, want)
	}
}
