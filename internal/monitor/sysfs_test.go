package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

func mkFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mkIBPort lays out one active InfiniBand port with the byte counters every
// mlx5 device exports.
func mkIBPort(t *testing.T, root, dev, port string) {
	t.Helper()
	base := filepath.Join("class/infiniband", dev, "ports", port)
	mkFile(t, root, filepath.Join(base, "state"), "4: ACTIVE")
	mkFile(t, root, filepath.Join(base, "counters/port_rcv_data"), "250")
	mkFile(t, root, filepath.Join(base, "counters/port_xmit_data"), "500")
}

func TestReadRNICSysfsCountsDropsAndPause(t *testing.T) {
	root := t.TempDir()
	mkIBPort(t, root, "mlx5_0", "1")
	hw := "class/infiniband/mlx5_0/ports/1/hw_counters"
	// Drops, by each of the four reasons looksLikeDrop accepts.
	mkFile(t, root, filepath.Join(hw, "out_of_buffer"), "7")
	mkFile(t, root, filepath.Join(hw, "rx_prio0_buf_discard"), "3")
	mkFile(t, root, filepath.Join(hw, "tx_discards_phy"), "11")
	// Link-integrity errors, which must not be read as drops.
	mkFile(t, root, filepath.Join(hw, "port_rcv_errors"), "1000")
	mkFile(t, root, filepath.Join(hw, "port_rcv_constraint_errors"), "1000")
	mkFile(t, root, filepath.Join(hw, "symbol_error"), "1000")
	// Pause: a global counter wins over the per-priority one.
	mkFile(t, root, filepath.Join(hw, "tx_pause_duration"), "40")
	mkFile(t, root, filepath.Join(hw, "tx_prio3_pause_duration"), "9")
	mkFile(t, root, filepath.Join(hw, "rx_prio3_pause_duration"), "5")

	c, err := ReadRNICSysfsFrom(context.Background(), root, model.RNIC{Device: "mlx5_0"})
	if err != nil {
		t.Fatal(err)
	}
	if c.RxBytes != 1000 || c.TxBytes != 2000 {
		t.Fatalf("bytes rx=%d tx=%d, want 1000/2000 (IB words are 4 bytes)", c.RxBytes, c.TxBytes)
	}
	if c.RxDrops != 10 {
		t.Fatalf("RxDrops=%d, want 10 (out_of_buffer + buf_discard, no rcv/symbol errors)", c.RxDrops)
	}
	if c.TxDrops != 11 {
		t.Fatalf("TxDrops=%d, want 11 (tx_discards_phy)", c.TxDrops)
	}
	if c.TxPauseDurationUs != 40 {
		t.Fatalf("TxPauseDurationUs=%d, want 40 (global, not global+prio)", c.TxPauseDurationUs)
	}
	if c.RxPauseDurationUs != 5 {
		t.Fatalf("RxPauseDurationUs=%d, want 5 (per-priority, no global)", c.RxPauseDurationUs)
	}
}

func TestReadRNICSysfsPortXmitDiscardsIsTx(t *testing.T) {
	root := t.TempDir()
	mkIBPort(t, root, "mlx5_0", "1")
	mkFile(t, root, "class/infiniband/mlx5_0/ports/1/counters/port_xmit_discards", "4")
	c, err := ReadRNICSysfsFrom(context.Background(), root, model.RNIC{Device: "mlx5_0"})
	if err != nil {
		t.Fatal(err)
	}
	// counters/ is not scanned as a directory, so this must be counted once.
	if c.TxDrops != 4 || c.RxDrops != 0 {
		t.Fatalf("tx=%d rx=%d, want 4/0", c.TxDrops, c.RxDrops)
	}
}

func TestReadRNICSysfsPicksFirstActivePort(t *testing.T) {
	root := t.TempDir()
	mkFile(t, root, "class/infiniband/mlx5_0/ports/1/state", "1: DOWN")
	mkIBPort(t, root, "mlx5_0", "2")
	mkFile(t, root, "class/infiniband/mlx5_0/ports/2/hw_counters/out_of_buffer", "6")
	c, err := ReadRNICSysfsFrom(context.Background(), root, model.RNIC{Device: "mlx5_0"})
	if err != nil {
		t.Fatal(err)
	}
	if c.RxDrops != 6 {
		t.Fatalf("RxDrops=%d, want 6 from the ACTIVE port 2", c.RxDrops)
	}
}

// stubEthtool points the package-global ethtool gate at a fake for one test,
// so a sysfs fixture never forks the real binary against the host's netdevs.
func stubEthtool(t *testing.T, run runner) {
	t.Helper()
	oldRun, oldBackoff, oldRetry, oldWhy := ethtool.run, ethtool.backoff, ethtool.retryAt, ethtool.why
	ethtool.run, ethtool.backoff, ethtool.retryAt, ethtool.why = run, nil, nil, nil
	t.Cleanup(func() {
		ethtool.run, ethtool.backoff, ethtool.retryAt, ethtool.why = oldRun, oldBackoff, oldRetry, oldWhy
	})
}

func TestReadRNICSysfsNetdevFallback(t *testing.T) {
	root := t.TempDir()
	stubEthtool(t, func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exec: ethtool: not found")
	})
	// No InfiniBand byte counters: bytes and drops come from the netdev.
	mkFile(t, root, "class/infiniband/mlx5_0/ports/1/state", "4: ACTIVE")
	st := "class/net/eth0/statistics"
	mkFile(t, root, filepath.Join(st, "rx_bytes"), "123")
	mkFile(t, root, filepath.Join(st, "tx_bytes"), "456")
	mkFile(t, root, filepath.Join(st, "rx_dropped"), "2")
	mkFile(t, root, filepath.Join(st, "rx_missed_errors"), "3")
	mkFile(t, root, filepath.Join(st, "tx_dropped"), "4")
	mkFile(t, root, filepath.Join(st, "rx_errors"), "9999")

	c, err := ReadRNICSysfsFrom(context.Background(), root, model.RNIC{Device: "mlx5_0", Netdev: "eth0"})
	if err != nil {
		t.Fatal(err)
	}
	if c.RxBytes != 123 || c.TxBytes != 456 {
		t.Fatalf("bytes rx=%d tx=%d, want 123/456", c.RxBytes, c.TxBytes)
	}
	if c.RxDrops != 5 {
		t.Fatalf("RxDrops=%d, want 5 (rx_dropped + rx_missed_errors, not rx_errors)", c.RxDrops)
	}
	if c.TxDrops != 4 {
		t.Fatalf("TxDrops=%d, want 4", c.TxDrops)
	}
}

func TestReadRNICSysfsFailsClosedWithoutCounters(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadRNICSysfsFrom(context.Background(), root, model.RNIC{Device: "mlx5_9"}); err == nil {
		t.Fatal("a device with no readable counters must be an error")
	}
}

func TestLooksLikeDrop(t *testing.T) {
	drops := []string{
		"out_of_buffer", "rx_missed_errors", "port_xmit_discards",
		"rx_discards_phy", "tx_discards_phy", "rx_prio0_buf_discard",
		"rx_dropped", "tx_dropped",
	}
	for _, n := range drops {
		if !looksLikeDrop(n) {
			t.Errorf("looksLikeDrop(%q) = false, want true", n)
		}
	}
	notDrops := []string{
		"port_rcv_errors", "port_rcv_constraint_errors", "symbol_error",
		"rx_errors", "rx_packets", "link_downed", "out_of_sequence",
		"tx_prio0_pause_duration",
	}
	for _, n := range notDrops {
		if looksLikeDrop(n) {
			t.Errorf("looksLikeDrop(%q) = true, want false", n)
		}
	}
}

func TestIsTxCounter(t *testing.T) {
	for _, n := range []string{"tx_dropped", "port_xmit_discards", "rx_prio0_tx_pause_duration"} {
		if !isTxCounter(n) {
			t.Errorf("isTxCounter(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"rx_dropped", "out_of_buffer", "port_rcv_data"} {
		if isTxCounter(n) {
			t.Errorf("isTxCounter(%q) = true, want false", n)
		}
	}
}

func TestEthtoolScanCountsDrops(t *testing.T) {
	g := &ethtoolGate{run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("NIC statistics:\n" +
			"     rx_discards_phy: 6\n" +
			"     tx_discards_phy: 2\n" +
			"     rx_packets: 100\n"), nil
	}}
	var c model.RNICCounters
	g.scan(context.Background(), "eth0", &c, true)
	if c.RxDrops != 6 || c.TxDrops != 2 {
		t.Fatalf("rx=%d tx=%d, want 6/2 (ethtool is the only source of *_discards_phy)", c.RxDrops, c.TxDrops)
	}
	// Drop counters alone are an answer, so the device must not be backed off.
	if !g.allow("eth0") {
		t.Fatal("a device reporting only drop counters must keep being polled")
	}
}
