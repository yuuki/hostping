package config

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/pci"
)

// MaxPriority is the highest 802.1p priority.
const MaxPriority = 7

// DropIgnored reports whether --drop-ignore-rnic names n, by RNIC ID, IB
// device name or PCI address.
func (c Config) DropIgnored(n model.RNIC) bool {
	return slices.ContainsFunc(c.DropIgnoreRNICs, func(e string) bool { return namesRNIC(e, n) })
}

// ValidateDropIgnore rejects a --drop-ignore-rnic entry that names none of
// rnics, so a typo cannot leave a storage NIC's drops diagnosed.
func (c Config) ValidateDropIgnore(rnics []model.RNIC) error {
	for _, e := range c.DropIgnoreRNICs {
		if !slices.ContainsFunc(rnics, func(n model.RNIC) bool { return namesRNIC(e, n) }) {
			return fmt.Errorf("--drop-ignore-rnic %q matches no RNIC (want an RNIC ID, an IB device name or a PCI address)", e)
		}
	}
	return nil
}

// namesRNIC compares a PCI address in its canonical form, so 3c:00.0 and
// 00000000:3C:00.0 name the device at 0000:3c:00.0.
func namesRNIC(e string, n model.RNIC) bool {
	if e == n.ID || e == n.Device {
		return true
	}
	bdf := pci.CanonicalBDF(e)
	return bdf != "" && bdf == pci.CanonicalBDF(n.PCIeBDF)
}

// Lossless reports whether PFC protects priority p. With no
// --lossless-priorities every priority is treated as lossless.
func (c Config) Lossless(p int) bool {
	return len(c.LosslessPriorities) == 0 || slices.Contains(c.LosslessPriorities, p)
}

// priorityList is the --lossless-priorities flag: comma-separated 802.1p
// priorities, kept sorted and unique. It may be repeated.
type priorityList []int

func (l *priorityList) String() string {
	if l == nil {
		return ""
	}
	s := make([]string, len(*l))
	for i, p := range *l {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

func (l *priorityList) Set(v string) error {
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimSpace(e)
		p, err := strconv.Atoi(e)
		if err != nil || p < 0 || p > MaxPriority {
			return fmt.Errorf("priority %q is not 0 to %d", e, MaxPriority)
		}
		if !slices.Contains(*l, p) {
			*l = append(*l, p)
		}
	}
	slices.Sort(*l)
	return nil
}
