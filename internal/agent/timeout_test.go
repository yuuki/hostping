package agent

import (
	"math"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/topology"
	"github.com/yuuki/hostping/internal/zigprobe"
)

// --timeout and --mutual-timeout reach the probe library as uint32
// milliseconds. A value outside that range saturates instead of wrapping:
// -1s used to become a 49.7-day probe timeout.
func TestProbeTimeoutMSSaturates(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want uint32
	}{
		{5 * time.Second, 5000},
		{500 * time.Millisecond, 500},
		{1500 * time.Microsecond, 1},
		{500 * time.Microsecond, 0},
		{0, 0},
		{-time.Second, 0},
		{time.Duration(math.MinInt64), 0},
		{time.Duration(math.MaxUint32) * time.Millisecond, math.MaxUint32},
		{time.Duration(math.MaxUint32+1) * time.Millisecond, math.MaxUint32},
		{time.Duration(math.MaxInt64), math.MaxUint32},
	} {
		if got := probeTimeoutMS(c.d); got != c.want {
			t.Errorf("probeTimeoutMS(%v) = %d, want %d", c.d, got, c.want)
		}
	}
}

func TestNewProberSaturatesTimeouts(t *testing.T) {
	cfg := config.Defaults()
	cfg.Timeout = -time.Second
	cfg.MutualTimeout = 60 * 24 * time.Hour
	p, _, err := NewProber(cfg, topology.Fig2())
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}
	zp, ok := p.(zigprobe.Prober)
	if !ok {
		t.Fatalf("NewProber returned %T, want zigprobe.Prober", p)
	}
	if zp.TimeoutMS != 0 || zp.MutualTimeoutMS != math.MaxUint32 {
		t.Fatalf("TimeoutMS=%d MutualTimeoutMS=%d, want 0 and %d", zp.TimeoutMS, zp.MutualTimeoutMS, uint32(math.MaxUint32))
	}
}
