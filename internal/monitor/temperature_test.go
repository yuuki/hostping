package monitor

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"testing"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// tempGraph is two RNICs and three GPUs by PCI address, and a memory node.
func tempGraph() topology.Graph {
	return topology.Graph{
		RNICs: []model.RNIC{
			{ID: "rnic0", Device: "mlx5_0", PCIeBDF: "0000:17:00.0"},
			{ID: "rnic1", Device: "mlx5_1", PCIeBDF: "0000:37:00.0"},
			{ID: "rnic2", Device: "mlx5_2"}, // no PCI address: no sensor can be attributed
		},
		Endpoints: []model.Endpoint{
			{ID: "gpu0", Kind: model.EndpointGPU, PCIeBDF: "0000:18:00.0"},
			{ID: "gpu1", Kind: model.EndpointGPU, PCIeBDF: "0000:19:00.0"},
			{ID: "gpu2", Kind: model.EndpointGPU, PCIeBDF: "0001:18:00.0"},
			{ID: "mem0", Kind: model.EndpointMemory},
		},
	}
}

func hwmon(bdf, chip, input string) string {
	return filepath.Join("bus/pci/devices", bdf, "hwmon", chip, input)
}

// noSMI fails the test when nvidia-smi is forked.
func noSMI(t *testing.T) runner {
	return func(context.Context, string, ...string) ([]byte, error) {
		t.Error("nvidia-smi was run although every GPU has a hwmon sensor")
		return nil, errors.New("unexpected")
	}
}

// A device's temperature is the hottest of the sensors registered under its
// own PCI function; a device without one is absent, never 0.
func TestTempReaderHwmon(t *testing.T) {
	root := t.TempDir()
	// rnic0: the ASIC and two modules, one of them unplugged (unreadable).
	mkFile(t, root, hwmon("0000:17:00.0", "hwmon4", "temp1_input"), "58000")
	mkFile(t, root, hwmon("0000:17:00.0", "hwmon4", "temp2_input"), "63500")
	mkFile(t, root, hwmon("0000:17:00.0", "hwmon4", "temp3_input"), "")
	mkFile(t, root, hwmon("0000:17:00.0", "hwmon4", "temp1_crit"), "105000")
	mkFile(t, root, hwmon("0000:17:00.0", "hwmon4", "temp1_label"), "asic")
	mkFile(t, root, hwmon("0000:17:00.0", "hwmon4", "name"), "mlx5")
	// rnic1 has a PCI directory but no hwmon device.
	mkFile(t, root, "bus/pci/devices/0000:37:00.0/vendor", "0x15b3")
	// The three GPUs through hwmon: one above zero, one below, one faulted.
	mkFile(t, root, hwmon("0000:18:00.0", "hwmon7", "temp1_input"), "71000")
	mkFile(t, root, hwmon("0000:19:00.0", "hwmon8", "temp1_input"), "-5000")
	mkFile(t, root, hwmon("0001:18:00.0", "hwmon9", "temp1_input"), "65535000")
	mkFile(t, root, hwmon("0001:18:00.0", "hwmon9", "temp2_input"), "44250")
	// A sensor that belongs to no device of the graph is never read.
	mkFile(t, root, "class/hwmon/hwmon0/temp1_input", "99000")

	got := TempReader{Graph: tempGraph(), Sysroot: root, run: noSMI(t)}.Read(context.Background())
	want := map[string]float64{"rnic0": 63.5, "gpu0": 71, "gpu1": -5, "gpu2": 44.25}
	if !maps.Equal(got, want) {
		t.Fatalf("temperatures = %v, want %v", got, want)
	}
}

// A GPU without a hwmon sensor is read through nvidia-smi, by its PCI
// address with the domain: two GPUs at the same bus:dev.fn in different
// domains are told apart.
func TestTempReaderNvidiaSMI(t *testing.T) {
	root := t.TempDir()
	mkFile(t, root, hwmon("0000:19:00.0", "hwmon8", "temp1_input"), "50000")
	calls := 0
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls++
		if name != "nvidia-smi" || len(args) != 2 || args[0] != "--query-gpu=pci.bus_id,temperature.gpu" || args[1] != "--format=csv,noheader,nounits" {
			t.Errorf("ran %s %v", name, args)
		}
		return []byte("00000000:18:00.0, 67\n00000000:19:00.0, 99\n00000001:18:00.0, 41\n"), nil
	}
	got := TempReader{Graph: tempGraph(), Sysroot: root, run: run}.Read(context.Background())
	// gpu1 keeps its hwmon reading: nvidia-smi only fills what hwmon lacks.
	want := map[string]float64{"gpu0": 67, "gpu1": 50, "gpu2": 41}
	if !maps.Equal(got, want) || calls != 1 {
		t.Fatalf("temperatures = %v after %d nvidia-smi runs, want %v after 1", got, calls, want)
	}
}

// Without nvidia-smi, and without any sensor, nothing is read and nothing
// is invented.
func TestTempReaderUnknown(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exec: nvidia-smi: not found")
	}
	got := TempReader{Graph: tempGraph(), Sysroot: t.TempDir(), run: run}.Read(context.Background())
	if len(got) != 0 {
		t.Fatalf("temperatures = %v, want none", got)
	}
	// A host with no GPU never forks.
	g := tempGraph()
	g.Endpoints = g.Endpoints[3:]
	if got := (TempReader{Graph: g, Sysroot: t.TempDir(), run: noSMI(t)}).Read(context.Background()); len(got) != 0 {
		t.Fatalf("temperatures = %v, want none", got)
	}
}

func TestParseMilliCelsius(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"58000\n", 58, true},
		{"  63500 ", 63.5, true},
		{"0", 0, true},
		{"-12000", -12, true},
		{"200000", 200, true},
		{"200001", 0, false},
		{"-50001", 0, false},
		{"65535000", 0, false},
		{"", 0, false},
		{"58.0", 0, false},
		{"N/A", 0, false},
		{"99999999999999999999", 0, false},
	}
	for _, c := range cases {
		got, ok := parseMilliCelsius(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseMilliCelsius(%q) = %v, %v, want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestIsTempInput(t *testing.T) {
	for name, want := range map[string]bool{
		"temp1_input": true, "temp12_input": true,
		"temp_input": false, "temp1_crit": false, "temp1_label": false, "tempx_input": false,
		"in1_input": false, "fan1_input": false, "temp1_input_extra": false, "xtemp1_input": false,
	} {
		if got := isTempInput(name); got != want {
			t.Errorf("isTempInput(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestParseNvidiaTemps(t *testing.T) {
	out := "00000000:3C:00.0, 45\n" +
		"00000000:5E:00.0, [N/A]\n" +
		"0001:3c:00.0,72.5\n" +
		"garbage\n" +
		"not-a-bdf, 50\n" +
		"00000000:86:00.0, 4096\n" +
		"00000000:AF:00.0, NaN\n" +
		"\n"
	want := map[string]float64{"0000:3c:00.0": 45, "0001:3c:00.0": 72.5}
	if got := parseNvidiaTemps(out); !maps.Equal(got, want) {
		t.Fatalf("parseNvidiaTemps = %v, want %v", got, want)
	}
}

func TestKernelReleaseAndFirmware(t *testing.T) {
	root := t.TempDir()
	mkFile(t, root, "proc/sys/kernel/osrelease", "6.8.0-45-generic")
	mkFile(t, root, "sys/class/infiniband/mlx5_0/fw_ver", "28.39.1002")
	mkFile(t, root, "sys/class/infiniband/mlx5_1/fw_ver", "")
	if got := KernelRelease(filepath.Join(root, "proc")); got != "6.8.0-45-generic" {
		t.Fatalf("kernel release %q", got)
	}
	if got := KernelRelease(filepath.Join(root, "nowhere")); got != "" {
		t.Fatalf("unreadable kernel release %q, want empty", got)
	}
	g := tempGraph()
	g.RNICs = append(g.RNICs, model.RNIC{ID: "rnic3", Device: "../../../proc/sys/kernel"}, model.RNIC{ID: "rnic4"})
	got := RNICFirmware(filepath.Join(root, "sys"), g)
	if want := map[string]string{"rnic0": "28.39.1002"}; !maps.Equal(got, want) {
		t.Fatalf("firmware = %v, want %v", got, want)
	}
}
