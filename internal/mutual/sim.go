package mutual

import (
	"fmt"
	"math/rand/v2"
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

func (t *SimTransport) Wait(spin, block time.Duration) ([]Reply, error) {
	if len(t.queued) == 0 {
		time.Sleep(spin + block)
		return nil, nil
	}
	out := t.queued
	t.queued = nil
	return out, nil
}

func (t *SimTransport) Close() {}
