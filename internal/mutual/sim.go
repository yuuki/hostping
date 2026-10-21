package mutual

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
)

// SimTransport is the probe path of the --sim world: a pair that the world
// cannot connect has no route, and every other probe is lost with the pair's
// simulated loss ratio or answered after the simulated RTT.
type SimTransport struct {
	World *sim.World
	RNICs []model.RNIC
	// Float returns a uniform [0,1) sample; nil uses math/rand.
	Float func() float64

	start  time.Time
	queued []Reply

	// done is closed by Interrupt (and Close) so that a Wait asleep on an
	// empty queue returns at once; the Loop interrupts it when its context
	// is cancelled, instead of waiting out the simulated block.
	initOnce  sync.Once
	closeOnce sync.Once
	done      chan struct{}
}

func (t *SimTransport) doneCh() chan struct{} {
	t.initOnce.Do(func() { t.done = make(chan struct{}) })
	return t.done
}

func (t *SimTransport) clock() uint64 {
	if t.start.IsZero() {
		t.start = time.Now()
	}
	return uint64(time.Since(t.start))
}

func (t *SimTransport) Send(src, dst int, seq uint32) (uint64, error) {
	if src < 0 || dst < 0 || src >= len(t.RNICs) || dst >= len(t.RNICs) {
		return 0, fmt.Errorf("%w: index out of range", ErrUnavailable)
	}
	now := t.clock()
	m := t.World.Mutual(t.RNICs[src].ID, t.RNICs[dst].ID)
	if !m.Connected {
		return now, fmt.Errorf("%w: simulated connect failure", ErrNoRoute)
	}
	f := t.Float
	if f == nil {
		f = rand.Float64
	}
	if m.LossRatio > 0 && f() < m.LossRatio {
		return now, nil
	}
	t.queued = append(t.queued, Reply{Src: src, Seq: seq, AtNs: now + m.RTTnsP50})
	return now, nil
}

// Wait returns every queued reply at once; with nothing queued it sleeps for
// spin+block, or until Interrupt or Close cuts the sleep short.
func (t *SimTransport) Wait(spin, block time.Duration) ([]Reply, error) {
	if len(t.queued) == 0 {
		if d := spin + block; d > 0 {
			tm := time.NewTimer(d)
			defer tm.Stop()
			select {
			case <-tm.C:
			case <-t.doneCh():
			}
		}
		return nil, nil
	}
	out := t.queued
	t.queued = nil
	return out, nil
}

// Interrupt wakes a Wait that is asleep, and makes every later empty Wait
// return at once. It may be called from any goroutine, any number of times.
func (t *SimTransport) Interrupt() {
	t.closeOnce.Do(func() { close(t.doneCh()) })
}

// Close releases nothing (the world is in-process); it interrupts a Wait.
func (t *SimTransport) Close() { t.Interrupt() }
