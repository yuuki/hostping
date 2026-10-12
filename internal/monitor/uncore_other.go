//go:build !linux

package monitor

import (
	"errors"
	"fmt"
)

// openPerfCounter is Linux-only: perf_event_open has no counterpart here.
func openPerfCounter(uncoreEvent) (perfCounter, error) {
	return nil, fmt.Errorf("%w: perf_event_open is Linux-only", errUncoreUnsupported)
}

// perfSupported reports whether this OS has perf_event_open.
const perfSupported = false

// touchNodeMemory is Linux-only: it needs mbind.
func touchNodeMemory(int) error {
	return errors.New("binding memory to a NUMA node is Linux-only")
}
