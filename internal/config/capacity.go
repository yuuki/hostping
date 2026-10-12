package config

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// LinkCapacity is an operator-supplied capacity for one kind of bus link
// (memory channels, inter-socket buses) whose capacity discovery cannot read
// and otherwise fills with a nominal constant. All applies to every link of
// the kind; ByLink overrides single links by topology link ID. Zero values
// mean "not given", and the link keeps its assumed capacity.
type LinkCapacity struct {
	All    uint64
	ByLink map[string]uint64
}

// Set reports whether any capacity was given.
func (c LinkCapacity) Set() bool { return c.All > 0 || len(c.ByLink) > 0 }

// linkCapacityFlag is a repeatable flag.Value filling a LinkCapacity from
// "BW" (every link of the kind) or "LINK=BW" (one link, whose ID must carry
// prefix, e.g. "mem.1=400GB/s").
type linkCapacityFlag struct {
	c      *LinkCapacity
	prefix string
}

func (f *linkCapacityFlag) String() string {
	if f == nil || f.c == nil {
		return ""
	}
	var parts []string
	if f.c.All > 0 {
		parts = append(parts, strconv.FormatUint(f.c.All, 10)+"bps")
	}
	ids := make([]string, 0, len(f.c.ByLink))
	for id := range f.c.ByLink {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		parts = append(parts, id+"="+strconv.FormatUint(f.c.ByLink[id], 10)+"bps")
	}
	return strings.Join(parts, ",")
}

func (f *linkCapacityFlag) Set(v string) error {
	id, bw, ok := strings.Cut(v, "=")
	if !ok {
		n, err := ParseCapacity(v)
		if err != nil {
			return err
		}
		f.c.All = n
		return nil
	}
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, f.prefix) || len(id) == len(f.prefix) {
		return fmt.Errorf("link %q: want a %sN link ID from `hostpingctl topology`", id, f.prefix)
	}
	n, err := ParseCapacity(bw)
	if err != nil {
		return err
	}
	if f.c.ByLink == nil {
		f.c.ByLink = map[string]uint64{}
	}
	f.c.ByLink[id] = n
	return nil
}

// ParseCapacity parses a bus capacity into bits per second. Bits are written
// with "bps" or "b/s" (400Gbps, 400Gb/s), bytes with "B/s" or "Bps" (50GB/s,
// 46.5GiB/s): memory bandwidth is usually quoted in bytes, link rates in bits,
// and the case of the b is the only thing telling them apart, so a unit is
// required and a bare "GB" is rejected. Decimal prefixes k/K, M, G, T and
// binary Ki, Mi, Gi, Ti are accepted; like --baseline, a lower-case m, g or t
// is read as the decimal prefix, never as milli.
func ParseCapacity(s string) (uint64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), "_", "")
	var num string
	var bytes bool
	switch {
	case strings.HasSuffix(s, "B/s"), strings.HasSuffix(s, "Bps"):
		num, bytes = s[:len(s)-3], true
	case strings.HasSuffix(s, "b/s"), strings.HasSuffix(s, "bps"):
		num = s[:len(s)-3]
	default:
		return 0, fmt.Errorf("capacity %q: need a unit, bits (bps, Gbps, Gb/s) or bytes (GB/s, GiB/s)", s)
	}
	mult := 1.0
	for _, p := range []struct {
		suffix string
		mult   float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
		{"m", 1e6}, {"g", 1e9}, {"t", 1e12},
	} {
		if strings.HasSuffix(num, p.suffix) {
			num, mult = strings.TrimSuffix(num, p.suffix), p.mult
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil || n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("capacity %q: invalid number", s)
	}
	v := n * mult
	if bytes {
		v *= 8
	}
	if v < 1 || v > float64(math.MaxUint64) {
		return 0, fmt.Errorf("capacity %q: out of range", s)
	}
	return uint64(v), nil
}
