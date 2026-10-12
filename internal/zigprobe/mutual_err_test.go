package zigprobe

import (
	"context"
	"errors"
	"testing"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/mutual"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// A missing RDMA device is a setup failure, not evidence that the RoCE path
// between two RNICs is down. It must surface as ErrNoDevice so the engine can
// skip the pair instead of diagnosing rnic_connectivity_failure.
func TestMutualMissingDeviceIsNoDevice(t *testing.T) {
	p := Prober{Iters: 1, TimeoutMS: 200}
	m, err := p.Mutual(context.Background(),
		model.RNIC{ID: "rnic0", Device: "hostping_absent0"},
		model.RNIC{ID: "rnic1", Device: "hostping_absent1"})
	if err == nil {
		t.Fatalf("missing device must be an error, got measurement %+v", m)
	}
	if !errors.Is(err, model.ErrNoDevice) {
		t.Fatalf("want ErrNoDevice, got %v", err)
	}
}

// A connectivity-class failure is still a measurement, not an error: the pair
// must stay in the report so the analyzer can diagnose it.
func TestMutualConnectFailureIsAMeasurement(t *testing.T) {
	g := topology.Fig2()
	w := sim.New(g)
	if err := w.ApplyFault("connect_fail:rnic0"); err != nil {
		t.Fatal(err)
	}
	p := Prober{Sim: w, Graph: g, Iters: 1, TimeoutMS: 200}
	m, err := p.Mutual(context.Background(), g.RNICs[0], g.RNICs[1])
	if err != nil {
		t.Fatalf("connect failure must be a measurement, got error %v", err)
	}
	if m.Connected {
		t.Fatalf("want Connected=false, got %+v", m)
	}
	if m.LossRatio != 1 {
		t.Fatalf("want LossRatio=1, got %+v", m)
	}
}

// The mutual buffers are 4 KiB. A larger payload is a bad argument, reported as
// an error, never as a measured "not connected" pair; the 1-byte default and the
// 4 KiB maximum both measure normally.
func TestMutualMsgSizeBound(t *testing.T) {
	g := topology.Fig2()
	w := sim.New(g)
	for _, size := range []uint32{0, 1, 4096} {
		p := Prober{Sim: w, Graph: g, Iters: 4, TimeoutMS: 200, MutualMsg: size}
		m, err := p.Mutual(context.Background(), g.RNICs[0], g.RNICs[1])
		if err != nil || !m.Connected || m.RTTnsP50 == 0 {
			t.Fatalf("MutualMsg=%d: m=%+v err=%v", size, m, err)
		}
	}
	p := Prober{Sim: w, Graph: g, Iters: 4, TimeoutMS: 200, MutualMsg: 4097}
	m, err := p.Mutual(context.Background(), g.RNICs[0], g.RNICs[1])
	if !errors.Is(err, model.ErrProbe) {
		t.Fatalf("MutualMsg=4097 must be ErrProbe, got m=%+v err=%v", m, err)
	}
}

// The mutual probe waits --mutual-timeout for each reply (ToN'24: 500 ms),
// not the loopback --timeout.
func TestMutualTimeoutIsSeparate(t *testing.T) {
	a, b := model.RNIC{ID: "rnic0", Device: "dev0"}, model.RNIC{ID: "rnic1", Device: "dev1"}
	if got := mutualReqTimeoutMS(Prober{TimeoutMS: 5000}, a, b); got != 500 {
		t.Fatalf("default mutual timeout: want 500, got %d", got)
	}
	if got := mutualReqTimeoutMS(Prober{TimeoutMS: 5000, MutualTimeoutMS: 250}, a, b); got != 250 {
		t.Fatalf("want 250, got %d", got)
	}
}

// The default request takes the RC path, which is what ran before the UD probe
// existed; ud must be asked for.
func TestMutualTransportDefaultsToRC(t *testing.T) {
	a, b := model.RNIC{ID: "rnic0", Device: "dev0"}, model.RNIC{ID: "rnic1", Device: "dev1"}
	if got := mutualReqTransport(Prober{}, a, b); got != "HOSTPING_MUTUAL_RC" {
		t.Fatalf("default transport: %s", got)
	}
	if got := mutualReqTransport(Prober{MutualTransport: model.MutualTransportRC}, a, b); got != "HOSTPING_MUTUAL_RC" {
		t.Fatalf("rc: %s", got)
	}
	if got := mutualReqTransport(Prober{MutualTransport: model.MutualTransportUD}, a, b); got != "HOSTPING_MUTUAL_UD" {
		t.Fatalf("ud: %s", got)
	}
}

// An endpoint that cannot be opened is setup, not a path failure: with nothing
// opened the transport is an error, and the reason names the missing device.
func TestOpenUDMissingDevice(t *testing.T) {
	_, errs, err := OpenUD([]model.RNIC{{ID: "rnic0", Device: "hostping_absent0"}, {ID: "rnic1", Device: "hostping_absent1"}}, 1)
	if err == nil {
		t.Fatal("no endpoint opened: want an error")
	}
	if len(errs) != 2 || !errors.Is(errs[0], model.ErrNoDevice) {
		t.Fatalf("want ErrNoDevice per endpoint, got %v", errs)
	}
}

// A probe to or from an endpoint that is not open measures nothing, so the
// loop leaves it out instead of counting a timeout.
func TestUDSendUnopenedIsUnavailable(t *testing.T) {
	tr := closedUDTransport([]model.RNIC{{ID: "rnic0", Device: "dev0"}, {ID: "rnic1", Device: "dev1"}}, model.ErrNoDevice)
	if _, err := tr.Send(0, 1, 1); !errors.Is(err, mutual.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	tr.Close()
}
