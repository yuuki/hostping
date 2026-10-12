package agent

import (
	"fmt"
	"maps"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/state"
	"github.com/yuuki/hostping/internal/topology"
)

// persister carries the daemon's streaks (gray links, slow GPU memory
// registration, path latency) and idle cadence across a restart through --state-dir (internal/state). It is opt-in and daemon-only:
// without --state-dir, for a one-shot run, or when the kernel boot ID cannot
// be read, every method is a no-op (a nil persister too) and the daemon
// behaves as it always has.
//
// The file is written after each idle-mode run is confirmed or discarded
// (after a discard has rolled the streaks back), after a busy-mode run that
// changed the streaks (busy runs test links too, so they extend or break gray
// streaks), and on shutdown.
type persister struct {
	store *state.Store // nil: persistence off
	cur   state.File
	dirty bool // cur holds a restored or recorded run, worth writing on shutdown
	cfg   config.Config
	now   func() time.Time
	logf  func(format string, args ...any)
}

// newPersister opens --state-dir for the daemon. A directory that cannot be
// created or written is an error, so a wrong path fails at start-up like
// --report-file; an unreadable boot ID only switches persistence off, since
// without it a state file from before a reboot could not be told apart.
func newPersister(cfg config.Config, g topology.Graph, bootID func() (string, error), now func() time.Time, logf func(string, ...any)) (*persister, error) {
	p := &persister{cfg: cfg, now: now, logf: logf}
	if cfg.StateDir == "" || cfg.Once {
		return p, nil
	}
	id, idErr := bootID()
	st, err := state.Open(cfg.StateDir, id, state.Fingerprint(g))
	if err != nil {
		return nil, fmt.Errorf("--state-dir: %w", err)
	}
	if idErr != nil {
		logf("hostping: state persistence disabled: kernel boot ID unreadable (%v), so saved flapping streaks could not be told apart from a previous boot's", idErr)
		return p, nil
	}
	p.store = st
	return p, nil
}

// maxAge is how old the last recorded run may be for its streaks to count:
// one flap window of idle runs. A streak older than that cannot be
// consecutive with the next test.
func (p *persister) maxAge() time.Duration {
	return time.Duration(p.cfg.FlapWindow) * p.cfg.IdleInterval
}

// restore loads the state file into the engine and the monitor, or says in
// one line why it does not.
func (p *persister) restore(eng *engine.Engine, mon *monitor.Monitor) {
	if p == nil || p.store == nil {
		return
	}
	now := p.now()
	f, ok, why := p.store.Load(now, p.maxAge())
	if !ok {
		p.logf("hostping: state %s not restored: %s", p.store.Path(), why)
		return
	}
	eng.RestoreStreaks(engine.Streaks{Gray: f.Gray, SlowMR: f.SlowMR, Latency: f.Latency})
	next := "on the first idle poll"
	if !f.LastIdleEnd.IsZero() {
		mon.ResumeIdle(f.LastIdleEnd)
		if due := f.LastIdleEnd.Add(p.cfg.IdleInterval); due.After(now) {
			next = "in " + due.Sub(now).Round(time.Second).String()
		}
	}
	p.cur, p.dirty = f, true
	p.logf("hostping: state %s restored: gray streaks on %d links, slow-registration streaks on %d GPUs, latency streaks on %d paths, last run %v ago; next idle run %s",
		p.store.Path(), len(f.Gray), len(f.SlowMR), len(eng.Latency), now.Sub(f.LastTest()).Round(time.Second), next)
}

// idleResolved records an idle-mode run whose held report was just confirmed
// or discarded. A discard has already rolled the engine's streaks back, so
// its run adds nothing to the streaks, but it still counts for the cadence.
func (p *persister) idleResolved(eng *engine.Engine, rep model.Report, confirmed bool) {
	if p == nil || p.store == nil {
		return
	}
	end := runEnd(rep)
	p.setStreaks(eng.SnapshotStreaks())
	if confirmed {
		p.cur.GrayUpdatedAt = end
	}
	if rep.Trigger == model.TriggerIdle {
		p.cur.LastIdleEnd = end
	}
	p.save()
}

// otherRun records a run that is not held (busy mode) when it changed the
// streaks; one that did not leaves nothing new to keep.
func (p *persister) otherRun(eng *engine.Engine, before engine.Streaks, rep model.Report) {
	if p == nil || p.store == nil {
		return
	}
	after := eng.SnapshotStreaks()
	if maps.Equal(before.Gray, after.Gray) && maps.Equal(before.SlowMR, after.SlowMR) && maps.Equal(before.Latency, after.Latency) {
		return
	}
	p.setStreaks(after)
	p.cur.GrayUpdatedAt = runEnd(rep)
	p.save()
}

// shutdown writes the state as the daemon stops. An idle-mode report still
// held is unconfirmed: its streaks are left out (the counters from before it
// are kept, as a discard would), but it did run, so it counts for the cadence.
func (p *persister) shutdown(eng *engine.Engine, pending *idleHold) {
	if p == nil || p.store == nil {
		return
	}
	p.setStreaks(eng.SnapshotStreaks())
	if pending != nil && pending.held {
		p.setStreaks(pending.streaks)
		if pending.rep.Trigger == model.TriggerIdle {
			p.cur.LastIdleEnd = runEnd(pending.rep)
		}
		p.dirty = true
	}
	if !p.dirty {
		return
	}
	p.save()
}

// setStreaks copies s into the file content.
func (p *persister) setStreaks(s engine.Streaks) {
	p.cur.Gray = maps.Clone(map[string]int(s.Gray))
	p.cur.SlowMR = maps.Clone(s.SlowMR)
	p.cur.Latency = maps.Clone(s.Latency)
}

func (p *persister) save() {
	p.dirty = true
	if err := p.store.Save(p.cur, p.now()); err != nil {
		p.logf("hostping: state %s not saved: %v", p.store.Path(), err)
	}
}

// runEnd is when the engine run behind rep ended.
func runEnd(rep model.Report) time.Time {
	return rep.Started.Add(rep.Elapsed)
}
