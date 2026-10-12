package analyzer

import (
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

func TestHealthyPCIeSpeed(t *testing.T) {
	const (
		g100 = 100_000_000_000
		g200 = 200_000_000_000
		g400 = 400_000_000_000
	)
	cases := []struct {
		name     string
		cfg      model.PCIeConfig
		lineRate uint64
		want     bool
	}{
		{"gen4 x16 carries 200G", model.PCIeConfig{Speed: "16.0 GT/s PCIe", Width: 16}, g200, true},
		{"gen4 x4 cannot carry 200G", model.PCIeConfig{Speed: "16.0 GT/s PCIe", Width: 4}, g200, false},
		{"gen4 x8 carries 100G", model.PCIeConfig{Speed: "16.0 GT/s PCIe", Width: 8}, g100, true},
		{"gen3 x16 carries 100G", model.PCIeConfig{Speed: "8.0 GT/s PCIe", Width: 16}, g100, true},
		{"gen3 x16 cannot carry 200G", model.PCIeConfig{Speed: "8GT/s", Width: 16}, g200, false},
		{"gen3 x8 cannot carry 100G", model.PCIeConfig{Speed: "8GT/s", Width: 8}, g100, false},
		{"gen2 x16 cannot carry 100G", model.PCIeConfig{Speed: "5.0 GT/s PCIe", Width: 16}, g100, false},
		{"gen1 x16 cannot carry 100G", model.PCIeConfig{Speed: "2.5 GT/s PCIe", Width: 16}, g100, false},
		{"gen5 x16 carries 400G", model.PCIeConfig{Speed: "32.0 GT/s PCIe", Width: 16}, g400, true},
		{"gen4 x16 cannot carry 400G", model.PCIeConfig{Speed: "16.0 GT/s PCIe", Width: 16}, g400, false},
		// Unknown line rate falls back to the 200G baseline assumption.
		{"gen4 x16 with unknown line rate", model.PCIeConfig{Speed: "16.0 GT/s PCIe", Width: 16}, 0, true},
		{"gen3 x16 with unknown line rate", model.PCIeConfig{Speed: "8GT/s", Width: 16}, 0, false},
		// Unreadable fields must not invent a down-train.
		{"unknown speed", model.PCIeConfig{Speed: "Unknown", Width: 4}, g200, true},
		{"unknown width judged at x16", model.PCIeConfig{Speed: "16.0 GT/s PCIe"}, g200, true},
		// max_link_* is the device capability, not the slot's: a card below its
		// own maximum is healthy as long as the line rate still fits.
		{"width below device max but enough bandwidth", model.PCIeConfig{
			Speed: "32.0 GT/s PCIe", Width: 8, MaxSpeed: "32.0 GT/s PCIe", MaxWidth: 16,
		}, g200, true},
		{"speed below device max but enough bandwidth", model.PCIeConfig{
			Speed: "32.0 GT/s PCIe", Width: 16, MaxSpeed: "64.0 GT/s PCIe", MaxWidth: 16,
		}, g200, true},
		{"below device max and too little bandwidth", model.PCIeConfig{
			Speed: "16.0 GT/s PCIe", Width: 4, MaxSpeed: "16.0 GT/s PCIe", MaxWidth: 16,
		}, g200, false},
		{"at device max", model.PCIeConfig{
			Speed: "16.0 GT/s PCIe", Width: 16, MaxSpeed: "16.0 GT/s PCIe", MaxWidth: 16,
		}, g200, true},
	}
	for _, tc := range cases {
		if got := healthyPCIeSpeed(tc.cfg, tc.lineRate); got != tc.want {
			t.Errorf("%s: healthyPCIeSpeed(%+v, %d)=%v want %v", tc.name, tc.cfg, tc.lineRate, got, tc.want)
		}
	}
}
