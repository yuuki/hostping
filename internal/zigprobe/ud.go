package zigprobe

/*
#include <stdlib.h>
#include "hostping.h"
*/
import "C"
import (
	"fmt"
	"time"
	"unsafe"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/mutual"
)

// udReopenEvery is how often an endpoint that failed to open is tried again,
// so an RNIC that was down when the loop started joins once it is back.
const udReopenEvery = 30 * time.Second

// UDTransport is the live mutual.Transport: one hostping_ud endpoint per
// RNIC, driven from the probe loop's goroutine only.
type UDTransport struct {
	rnics   []model.RNIC
	msg     uint32
	eps     []*C.hostping_ud_ep
	errs    []error
	tried   []time.Time
	replies [64]C.hostping_ud_reply
}

// OpenUD opens an endpoint on every RNIC's device. An RNIC that cannot be
// opened is left closed (its probes are ErrUnavailable) and retried later;
// errs holds each RNIC's open error, nil when it opened. It is an error only
// when nothing opened.
func OpenUD(rnics []model.RNIC, msg uint32) (*UDTransport, []error, error) {
	if len(rnics) > int(C.HOSTPING_UD_MAX_EPS) {
		return nil, nil, fmt.Errorf("%w: %d RNICs exceed the %d-endpoint mutual probe", model.ErrProbe, len(rnics), int(C.HOSTPING_UD_MAX_EPS))
	}
	if msg == 0 {
		msg = 1
	}
	t := &UDTransport{
		rnics: rnics,
		msg:   msg,
		eps:   make([]*C.hostping_ud_ep, len(rnics)),
		errs:  make([]error, len(rnics)),
		tried: make([]time.Time, len(rnics)),
	}
	opened := 0
	for i := range rnics {
		if t.open(i) == nil {
			opened++
		}
	}
	errs := append([]error(nil), t.errs...)
	if opened == 0 && len(rnics) > 0 {
		t.Close()
		return nil, errs, fmt.Errorf("%w: no mutual probe endpoint opened: %v", model.ErrProbe, errs[0])
	}
	return t, errs, nil
}

// closedUDTransport is a transport whose endpoints all failed to open with
// err just now. Kept out of *_test.go because Go does not allow cgo there.
func closedUDTransport(rnics []model.RNIC, err error) *UDTransport {
	t := &UDTransport{
		rnics: rnics,
		msg:   1,
		eps:   make([]*C.hostping_ud_ep, len(rnics)),
		errs:  make([]error, len(rnics)),
		tried: make([]time.Time, len(rnics)),
	}
	for i := range rnics {
		t.errs[i], t.tried[i] = err, time.Now()
	}
	return t
}

func (t *UDTransport) open(i int) error {
	t.tried[i] = time.Now()
	dev := C.CString(t.rnics[i].Device)
	defer C.free(unsafe.Pointer(dev))
	var ep *C.hostping_ud_ep
	var errBuf [C.HOSTPING_ERR_LEN]C.char
	rc := C.hostping_ud_open(dev, C.uint32_t(t.msg), &ep, &errBuf[0])
	if rc != 0 {
		t.errs[i] = mapProbeErr(int(rc), C.GoString(&errBuf[0]))
		return t.errs[i]
	}
	t.eps[i], t.errs[i] = ep, nil
	return nil
}

func (t *UDTransport) ready(i int) bool {
	if t.eps[i] != nil {
		return true
	}
	if time.Since(t.tried[i]) >= udReopenEvery {
		return t.open(i) == nil
	}
	return false
}

func (t *UDTransport) Send(src, dst int, seq uint32) (uint64, error) {
	if src < 0 || dst < 0 || src >= len(t.eps) || dst >= len(t.eps) {
		return 0, fmt.Errorf("%w: endpoint index out of range", mutual.ErrUnavailable)
	}
	if !t.ready(src) {
		return 0, fmt.Errorf("%w: %s: %v", mutual.ErrUnavailable, t.rnics[src].ID, t.errs[src])
	}
	if !t.ready(dst) {
		return 0, fmt.Errorf("%w: %s: %v", mutual.ErrUnavailable, t.rnics[dst].ID, t.errs[dst])
	}
	var sent C.uint64_t
	var errBuf [C.HOSTPING_ERR_LEN]C.char
	rc := C.hostping_ud_send(&t.eps[0], C.uint32_t(len(t.eps)), C.uint32_t(src), C.uint32_t(dst), C.uint32_t(seq), &sent, &errBuf[0])
	if rc == 0 {
		return uint64(sent), nil
	}
	msg := C.GoString(&errBuf[0])
	if rc == C.HOSTPING_ERR_CONNECT && len(msg) >= 7 && msg[:7] == "Connect" {
		// No address handle: no route to the peer's GID.
		return 0, fmt.Errorf("%w: %s", mutual.ErrNoRoute, msg)
	}
	if rc == C.HOSTPING_ERR_PORT {
		// The two endpoints cannot address each other at all (a RoCE and an
		// InfiniBand port): no probe was possible, so none is counted lost.
		return 0, fmt.Errorf("%w: %v", mutual.ErrUnavailable, portErr(msg))
	}
	// Anything else (a full or failed send queue) is a probe that did not go
	// out: it counts as lost.
	return 0, mapProbeErr(int(rc), msg)
}

func (t *UDTransport) Wait(spin, block time.Duration) ([]mutual.Reply, error) {
	if len(t.eps) == 0 {
		time.Sleep(spin + block)
		return nil, nil
	}
	if spin < 0 {
		spin = 0
	}
	if block < 0 {
		block = 0
	}
	blockMillis := uint32((block + time.Millisecond - 1) / time.Millisecond)
	var n C.uint32_t
	var errBuf [C.HOSTPING_ERR_LEN]C.char
	rc := C.hostping_ud_wait(&t.eps[0], C.uint32_t(len(t.eps)), C.uint64_t(spin.Nanoseconds()), C.uint32_t(blockMillis),
		&t.replies[0], C.uint32_t(len(t.replies)), &n, &errBuf[0])
	if rc != 0 {
		return nil, mapProbeErr(int(rc), C.GoString(&errBuf[0]))
	}
	out := make([]mutual.Reply, 0, int(n))
	for _, r := range t.replies[:int(n)] {
		out = append(out, mutual.Reply{Src: int(r.ep), Seq: uint32(r.seq), AtNs: uint64(r.t_ns)})
	}
	return out, nil
}

func (t *UDTransport) Close() {
	for i, ep := range t.eps {
		if ep != nil {
			C.hostping_ud_close(ep)
			t.eps[i] = nil
		}
	}
}
