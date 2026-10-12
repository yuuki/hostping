package pci

import (
	"strconv"
	"strings"

	"github.com/yuuki/hostping/internal/model"
)

// GTs parses a sysfs current_link_speed / max_link_speed value such as
// "16.0 GT/s PCIe", "8GT/s" or "Unknown" into GT/s. 0 means unreadable.
func GTs(s string) float64 {
	l := strings.ToLower(s)
	i := strings.Index(l, "gt")
	if i < 0 {
		return 0
	}
	num := strings.TrimRight(l[:i], " \t")
	j := len(num)
	for j > 0 {
		c := num[j-1]
		if (c < '0' || c > '9') && c != '.' {
			break
		}
		j--
	}
	v, err := strconv.ParseFloat(num[j:], 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// EffectiveBPS is the payload bandwidth of a link: transfer rate × lanes
// minus line-code overhead (8b/10b for Gen1/2, 128b/130b for Gen3 and later).
func EffectiveBPS(gts float64, width int) uint64 {
	if gts <= 0 || width <= 0 {
		return 0
	}
	enc := 128.0 / 130.0
	if gts < 8 {
		enc = 0.8
	}
	return uint64(gts * 1e9 * float64(width) * enc)
}

// Capability is the speed and width a PCIe link can train to:
// min(device max_link_*, upstream port max_link_*). The upstream port stands
// in for the slot. A device's own max_link_* is only what the card supports —
// a Gen5 x16 card in a Gen4 or x8 slot legitimately trains below it — so a
// half whose upstream port maximum is unknown is reported as unknown (0)
// rather than guessed from the device alone.
func Capability(c model.PCIeConfig) (gts float64, width int) {
	if dev, port := GTs(c.MaxSpeed), GTs(c.PortMaxSpeed); dev > 0 && port > 0 {
		gts = min(dev, port)
	}
	if c.MaxWidth > 0 && c.PortMaxWidth > 0 {
		width = min(c.MaxWidth, c.PortMaxWidth)
	}
	return gts, width
}

// CapacityBPS is the effective bandwidth of the link at its capability, and 0
// when either half of the capability is unknown.
func CapacityBPS(c model.PCIeConfig) uint64 {
	return EffectiveBPS(Capability(c))
}

// NegotiatedBPS is the effective bandwidth of the link as currently trained,
// and 0 when speed or width is unreadable.
func NegotiatedBPS(c model.PCIeConfig) uint64 {
	return EffectiveBPS(GTs(c.Speed), c.Width)
}

// DownTrain reports which halves of the link trained below its capability.
// A half whose negotiated or capability value is unknown never reports a
// down-train: missing sysfs is not evidence of a fault.
func DownTrain(c model.PCIeConfig) (speed, width bool) {
	capGTs, capWidth := Capability(c)
	if cur := GTs(c.Speed); cur > 0 && capGTs > 0 && cur < capGTs {
		speed = true
	}
	if c.Width > 0 && capWidth > 0 && c.Width < capWidth {
		width = true
	}
	return speed, width
}
