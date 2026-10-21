package mutual

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// simTransport is a SimTransport over the Fig.2 world with the given faults
// applied, every Fig.2 RNIC as an endpoint, in graph order.
func simTransport(t *testing.T, faults string) (*SimTransport, *sim.World) {
	t.Helper()
	g := topology.Fig2()
	w := sim.New(g)
	if err := w.ApplyFault(faults); err != nil {
		t.Fatal(err)
	}
	return &SimTransport{World: w, RNICs: g.RNICs}, w
}

// Send answers from the world: an endpoint index outside the RNIC list is
// unavailable (not counted), a pair the world cannot connect has no route,
// and a healthy probe queues its reply one simulated RTT after the send
// time, on the transport's own clock.
func TestSimTransportSendFollowsWorld(t *testing.T) {
	tr, w := simTransport(t, "connect_fail:rnic3")
	for _, idx := range [][2]int{{-1, 0}, {0, -1}, {4, 0}, {0, 4}} {
		if _, err := tr.Send(idx[0], idx[1], 1); !errors.Is(err, ErrUnavailable) {
			t.Errorf("Send(%d, %d) = %v, want ErrUnavailable", idx[0], idx[1], err)
		}
	}
	if _, err := tr.Send(0, 3, 1); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("Send to a connect_fail RNIC = %v, want ErrNoRoute", err)
	}
	if reps, err := tr.Wait(0, 0); err != nil || len(reps) != 0 {
		t.Fatalf("a probe without a route queued a reply: %v, %v", reps, err)
	}
	sent, err := tr.Send(0, 1, 7)
	if err != nil {
		t.Fatal(err)
	}
	reps, err := tr.Wait(0, 0)
	if err != nil || len(reps) != 1 {
		t.Fatalf("Wait = %v, %v, want one reply", reps, err)
	}
	rtt := w.Mutual("rnic0", "rnic1").RTTnsP50
	if want := (Reply{Src: 0, Seq: 7, AtNs: sent + rtt}); reps[0] != want {
		t.Fatalf("reply = %+v, want %+v (sent at %d, RTT %d)", reps[0], want, sent, rtt)
	}
	if later, _ := tr.Send(1, 0, 8); later < sent {
		t.Fatalf("the transport clock went backwards: %d then %d", sent, later)
	}
}

// A probe over an RNIC with injected drops is lost when the uniform sample
// falls below the pair's loss ratio: Send reports no error (the probe went
// out) and no reply ever arrives. Float is the sample, so a test decides.
func TestSimTransportLossDrawsFromFloat(t *testing.T) {
	tr, w := simTransport(t, "drops:rnic0")
	loss := w.Mutual("rnic0", "rnic1").LossRatio
	if loss <= 0 || loss >= 1 {
		t.Fatalf("drops fault gives loss ratio %v, want strictly between 0 and 1", loss)
	}
	tr.Float = func() float64 { return loss / 2 }
	if _, err := tr.Send(0, 1, 1); err != nil {
		t.Fatalf("a lost probe must not be an error: %v", err)
	}
	if reps, _ := tr.Wait(0, 0); len(reps) != 0 {
		t.Fatalf("lost probe was answered: %+v", reps)
	}
	tr.Float = func() float64 { return loss }
	if _, err := tr.Send(0, 1, 2); err != nil {
		t.Fatal(err)
	}
	tr.Float = func() float64 { return 0.999 }
	if _, err := tr.Send(1, 0, 3); err != nil {
		t.Fatal(err)
	}
	reps, _ := tr.Wait(0, 0)
	if len(reps) != 2 || reps[0].Seq != 2 || reps[1].Seq != 3 {
		t.Fatalf("samples at and above the loss ratio must be answered, in send order: %+v", reps)
	}
	// A pair without drops never consults the sample.
	tr.Float = func() float64 { t.Fatal("Float called for a lossless pair"); return 0 }
	if _, err := tr.Send(2, 3, 4); err != nil {
		t.Fatal(err)
	}
}

// Wait returns every queued reply at once, without sleeping, and empties
// the queue; with nothing queued it sleeps for spin+block, but Close (or
// Interrupt) from another goroutine cuts that sleep short, and every empty
// Wait after it returns at once, so a daemon shutting down in --sim mode
// does not wait out the simulated block.
func TestSimTransportWaitReturnsOnClose(t *testing.T) {
	tr, _ := simTransport(t, "")
	for seq := uint32(1); seq <= 3; seq++ {
		if _, err := tr.Send(0, 1, seq); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	reps, err := tr.Wait(time.Second, time.Second)
	if err != nil || len(reps) != 3 {
		t.Fatalf("Wait = %d replies, %v, want the 3 queued", len(reps), err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("Wait slept %v with replies queued", d)
	}
	if reps, _ := tr.Wait(0, 0); len(reps) != 0 {
		t.Fatalf("second Wait returned the same replies again: %+v", reps)
	}
	start = time.Now()
	reps, err = tr.Wait(2*time.Millisecond, 3*time.Millisecond)
	if err != nil || reps != nil {
		t.Fatalf("empty Wait = %v, %v, want nil, nil", reps, err)
	}
	if d := time.Since(start); d < 5*time.Millisecond {
		t.Fatalf("empty Wait returned after %v, want at least spin+block (5ms)", d)
	}

	// Close while Wait sleeps on a long block: Wait returns promptly.
	closed := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		tr.Close()
		tr.Close() // idempotent, from any goroutine
		close(closed)
	}()
	start = time.Now()
	reps, err = tr.Wait(0, 10*time.Second)
	if err != nil || reps != nil {
		t.Fatalf("interrupted Wait = %v, %v, want nil, nil", reps, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Wait slept %v after Close, want prompt return", d)
	}
	<-closed
	start = time.Now()
	if _, err := tr.Wait(0, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("empty Wait after Close slept %v, want immediate return", d)
	}
}

// Run on a SimTransport stops with its context: the loop interrupts the
// transport's Wait, so cancellation does not wait out the schedule interval.
func TestLoopRunOnSimTransportStopsWithContext(t *testing.T) {
	tr, _ := simTransport(t, "")
	pairs := [][2]model.RNIC{{tr.RNICs[0], tr.RNICs[1]}}
	l := New(Config{Interval: time.Hour, Timeout: 500 * time.Millisecond, Window: time.Hour, Spin: 0, LossIsDrop: true}, tr.RNICs, pairs, tr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation (Interval is 1h)")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Run returned %v after cancellation, want prompt", d)
	}
}
