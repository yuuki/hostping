//go:build linux

package monitor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// perfFD is one uncore event opened system-wide on one CPU.
type perfFD struct{ fd int }

// openPerfCounter opens ev as a counting event (no sampling, enabled at once)
// on ev.CPU for every task, which is how uncore PMUs are read. It reads the
// enabled and running times too, so a multiplexed interval is detected
// instead of being taken at face value.
func openPerfCounter(ev uncoreEvent) (perfCounter, error) {
	attr := unix.PerfEventAttr{
		Type:        ev.Type,
		Config:      ev.Config,
		Ext1:        ev.Config1,
		Ext2:        ev.Config2,
		Read_format: unix.PERF_FORMAT_TOTAL_TIME_ENABLED | unix.PERF_FORMAT_TOTAL_TIME_RUNNING,
	}
	attr.Size = uint32(unsafe.Sizeof(attr))
	fd, err := unix.PerfEventOpen(&attr, -1, ev.CPU, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
			return nil, fmt.Errorf("perf_event_open denied (needs CAP_PERFMON or kernel.perf_event_paranoid <= 0): %w", err)
		}
		return nil, fmt.Errorf("perf_event_open: %w", err)
	}
	return &perfFD{fd: fd}, nil
}

func (p *perfFD) Read() (count, enabled, running uint64, err error) {
	var b [24]byte
	n, err := unix.Read(p.fd, b[:])
	if err != nil {
		return 0, 0, 0, err
	}
	if n != len(b) {
		return 0, 0, 0, fmt.Errorf("perf read: %d bytes", n)
	}
	e := binary.NativeEndian
	return e.Uint64(b[0:]), e.Uint64(b[8:]), e.Uint64(b[16:]), nil
}

func (p *perfFD) Close() error { return unix.Close(p.fd) }

// perfSupported reports whether this OS has perf_event_open.
const perfSupported = true

// mbind(2) mode and flags.
const (
	mpolBind     = 2
	mpolMFStrict = 1
	mpolMFMove   = 2
)

// touchNodeMemory maps sncTouchBytes bound to node before the first touch,
// writes every cache line of it and reads it back, so the node's IMC boxes
// count the traffic, then unmaps it.
func touchNodeMemory(node int) error {
	if node < 0 {
		return fmt.Errorf("NUMA node %d", node)
	}
	b, err := unix.Mmap(-1, 0, sncTouchBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return fmt.Errorf("mmap: %w", err)
	}
	defer func() { _ = unix.Munmap(b) }()
	_ = unix.Madvise(b, unix.MADV_HUGEPAGE) // fewer faults; optional
	mask := make([]uint64, node/64+2)
	mask[node/64] |= 1 << (node % 64)
	if _, _, e := unix.Syscall6(unix.SYS_MBIND, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		mpolBind, uintptr(unsafe.Pointer(&mask[0])), uintptr(len(mask)*64), mpolMFStrict|mpolMFMove); e != 0 {
		return fmt.Errorf("mbind: %w", e)
	}
	for i := 0; i < len(b); i += 64 {
		b[i] = 1
	}
	var sum byte
	for i := 0; i < len(b); i += 64 {
		sum += b[i]
	}
	runtime.KeepAlive(sum)
	return nil
}
