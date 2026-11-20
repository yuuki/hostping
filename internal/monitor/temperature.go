package monitor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/pci"
	"github.com/yuuki/hostping/internal/sysfs"
	"github.com/yuuki/hostping/internal/topology"
)

// Device temperatures, for the drift tracking (internal/drift): a path that
// has slowed over weeks while its RNIC or GPU runs hotter than it did is
// throttling, not wearing out.
//
// A temperature is only read from a sensor that belongs to the device, by
// the device's own PCI address:
//
//   - the hwmon devices registered under the PCI function
//     (bus/pci/devices/<BDF>/hwmon/hwmonN/tempM_input, millidegrees
//     Celsius), which is where an RNIC driver that exposes its sensors and
//     the DRM driver of an AMD GPU put them;
//   - nvidia-smi's temperature.gpu, keyed by pci.bus_id, for a GPU without
//     one (the NVIDIA driver registers no hwmon device).
//
// Board, CPU and DIMM sensors are not read: nothing in sysfs says which
// path's hop they sit on, so a memory endpoint has no temperature. A device
// without a readable sensor is absent from the result: unknown, never 0.

// Plausible device temperatures. A reading outside them is a sensor that is
// absent or faulted (0xFFFF, -128), not a temperature.
const (
	minDeviceTempC = -50
	maxDeviceTempC = 200
)

// TempReader reads the temperature of the RNICs and GPUs of Graph.
type TempReader struct {
	Graph topology.Graph
	// Sysroot is the sysfs mount; empty means /sys. Tests point it at a
	// fixture tree.
	Sysroot string
	// run is nil outside tests.
	run runner
}

// Read returns the temperature in degrees Celsius of every RNIC and GPU that
// has a readable sensor, by RNIC and endpoint ID. It forks nvidia-smi at most
// once, bounded like the other sampler commands, and only when a GPU has no
// hwmon sensor. It is called once per idle run, never from the poll loop's
// every tick.
func (r TempReader) Read(ctx context.Context) map[string]float64 {
	sysroot := r.Sysroot
	if sysroot == "" {
		sysroot = "/sys"
	}
	out := map[string]float64{}
	for _, n := range r.Graph.RNICs {
		if c, ok := hwmonTemp(sysroot, n.PCIeBDF); ok {
			out[n.ID] = c
		}
	}
	var viaSMI []model.Endpoint
	for _, e := range r.Graph.Endpoints {
		if e.Kind != model.EndpointGPU {
			continue
		}
		if c, ok := hwmonTemp(sysroot, e.PCIeBDF); ok {
			out[e.ID] = c
		} else if normalizeBDF(e.PCIeBDF) != "" {
			viaSMI = append(viaSMI, e)
		}
	}
	if len(viaSMI) > 0 {
		run := r.run
		if run == nil {
			run = runBounded
		}
		smi := nvidiaTempsByBDF(ctx, run)
		for _, e := range viaSMI {
			if c, ok := smi[normalizeBDF(e.PCIeBDF)]; ok {
				out[e.ID] = c
			}
		}
	}
	return out
}

// hwmonTemp is the hottest of the temperature sensors registered under the
// PCI function bdf, in degrees Celsius. A device exposes several (an RNIC
// its ASIC and each plugged module), and throttling follows whichever is
// closest to its limit, which a single number can only stand for by the
// hottest. ok is false when the function has no hwmon device, no readable
// temperature input, or bdf is not a PCI address.
func hwmonTemp(sysroot, bdf string) (float64, bool) {
	bdf = pci.CanonicalBDF(bdf)
	if bdf == "" {
		return 0, false
	}
	base := filepath.Join(sysroot, "bus/pci/devices", bdf, "hwmon")
	chips, err := os.ReadDir(base)
	if err != nil {
		return 0, false
	}
	hottest, found := 0.0, false
	for _, chip := range chips {
		inputs, err := os.ReadDir(filepath.Join(base, chip.Name()))
		if err != nil {
			continue
		}
		for _, in := range inputs {
			if !isTempInput(in.Name()) {
				continue
			}
			c, ok := parseMilliCelsius(sysfs.ReadString(filepath.Join(base, chip.Name(), in.Name())))
			if !ok {
				continue
			}
			if !found || c > hottest {
				hottest, found = c, true
			}
		}
	}
	return hottest, found
}

// isTempInput matches the hwmon sysfs name of a temperature reading,
// temp<N>_input.
func isTempInput(name string) bool {
	n, ok := strings.CutPrefix(name, "temp")
	if !ok {
		return false
	}
	n, ok = strings.CutSuffix(n, "_input")
	if !ok || n == "" {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseMilliCelsius reads a hwmon temperature (millidegrees Celsius, signed)
// as degrees. ok is false for anything that is not a plausible temperature.
func parseMilliCelsius(s string) (float64, bool) {
	m, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return plausibleTemp(float64(m) / 1000)
}

func plausibleTemp(c float64) (float64, bool) {
	if !(c >= minDeviceTempC && c <= maxDeviceTempC) {
		return 0, false
	}
	return c, true
}

// nvidiaTempsByBDF reads the core temperature of every NVIDIA GPU, keyed by
// normalized BDF. Any failure yields nil: unknown, never 0.
func nvidiaTempsByBDF(ctx context.Context, run runner) map[string]float64 {
	b, err := run(ctx, "nvidia-smi", "--query-gpu=pci.bus_id,temperature.gpu", "--format=csv,noheader,nounits")
	if err != nil {
		return nil
	}
	return parseNvidiaTemps(string(b))
}

// parseNvidiaTemps reads "bus_id, temperature" lines. A GPU whose
// temperature is not a plausible number ([N/A], [Not Supported]) is absent.
func parseNvidiaTemps(out string) map[string]float64 {
	temps := map[string]float64{}
	for _, line := range strings.Split(out, "\n") {
		id, cell, ok := strings.Cut(strings.TrimSpace(line), ",")
		if !ok {
			continue
		}
		bdf := normalizeBDF(id)
		if bdf == "" {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(cell), 64)
		if err != nil {
			continue
		}
		if c, ok := plausibleTemp(v); ok {
			temps[bdf] = c
		}
	}
	return temps
}
