package sim_test

import (
	"testing"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/sim"
)

func TestEquation1Bandwidth(t *testing.T) {
	const size = 128 * 1024
	const bw = 197_000_000_000
	tproc := baseline.TprocNs
	host := uint64(600)
	small := sim.LatencyNs(tproc, host, 1, bw)
	large := sim.LatencyNs(tproc, host, size, bw)
	got := sim.BandwidthBPS(size, large, small)
	if d := diff(got, bw); d > 0.01 {
		t.Fatalf("bw got %d want %d relerr %f", got, bw, d)
	}
}

func TestTransferDominatesLargeMessage(t *testing.T) {
	const size = 128 * 1024
	const bw = 200_000_000_000
	xfer := sim.TransferNs(size, bw)
	if xfer < 4000 || xfer > 6000 {
		t.Fatalf("128KiB @ 200Gb/s = %d ns, want ~5243", xfer)
	}
}

func diff(a, b uint64) float64 {
	if a > b {
		return float64(a-b) / float64(b)
	}
	return float64(b-a) / float64(b)
}
