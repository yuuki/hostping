package monitor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

// ethtoolMLX5 is ethtool -S output shaped like an mlx5 port's: a few hundred
// counters, of which the per-priority pause durations and discards matter.
func ethtoolMLX5() string {
	var b strings.Builder
	b.WriteString("NIC statistics:\n")
	for i := 0; i < 64; i++ {
		fmt.Fprintf(&b, "     rx%d_packets: %d\n     rx%d_bytes: %d\n     tx%d_packets: %d\n", i, i*1000, i, i*64000, i, i*900)
	}
	for p := 0; p < 8; p++ {
		fmt.Fprintf(&b, "     rx_prio%d_bytes: %d\n     rx_prio%d_pause_duration: %d\n     tx_prio%d_pause_duration: %d\n     rx_prio%d_discards: %d\n", p, p*1e6, p, p*7, p, p*5, p, p)
	}
	b.WriteString("     rx_discards_phy: 28\n     tx_discards_phy: 0\n     rx_out_of_buffer: 0\n")
	return b.String()
}

// BenchmarkParseEthtoolStats times reading one netdev's ethtool -S, which
// the monitor does on every poll for an RNIC without hw_counters pause
// durations.
func BenchmarkParseEthtoolStats(b *testing.B) {
	out := ethtoolMLX5()
	b.SetBytes(int64(len(out)))
	b.ReportAllocs()
	for b.Loop() {
		var c model.RNICCounters
		var acc pauseAccum
		parseEthtoolStats(out, &c, &acc)
		acc.flush(&c)
	}
}

// BenchmarkParseNvidiaGPUs times reading the utilization of eight GPUs.
func BenchmarkParseNvidiaGPUs(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&sb, "00000000:%02X:00.0, %d, Default\n", 0x17+i*0x20, i*10)
	}
	out := sb.String()
	b.ReportAllocs()
	for b.Loop() {
		parseNvidiaGPUs(out)
	}
}
