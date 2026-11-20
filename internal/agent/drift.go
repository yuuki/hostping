package agent

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/drift"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/topology"
)

// driftSaveEvery bounds how often the drift history is written while
// nothing in it changed but the readings of the current day: a crash loses
// at most this much of them. A day that rolled over, a verdict that changed
// or a software change is written at once.
const driftSaveEvery = 30 * time.Minute

// driftTracker feeds the daemon's confirmed idle-trigger runs to the drift
// history (--drift-tracking, internal/drift) and keeps it in --state-dir. It
// is opt-in and daemon-only: a nil driftTracker does nothing, which is what a
// daemon without the flag and every one-shot run get.
//
// Only an idle-trigger run that the idle hold confirmed reaches it. A
// discarded run never does, a busy-mode run has no idle readings, and a
// mutual-alarm run in idle mode is left out because --idle-gate-command does
// not gate it: it can land in a job's pause, the case the gate exists for.
type driftTracker struct {
	// tr is nil while the history on disk is blocked (drift.LoadBlocked):
	// every path is then unknown with history_blocked and nothing is written.
	tr    *drift.Tracker
	store *drift.Store
	paths []drift.PathIdent
	// rnicKey maps an RNIC ID to its key in the history.
	rnicKey map[string]string
	now     func() time.Time
	logf    func(format string, args ...any)
	// temps reads the device temperatures and software the versions of the
	// live host. Both are nil under --sim, which has neither: the
	// temperature is then unknown and no software change is recorded.
	temps    func(context.Context) map[string]float64
	software func() (kernel string, firmware map[string]string)
	// held is the temperatures read at the end of the idle run whose report
	// is being held, the closest reading to the probes themselves.
	held map[string]float64
	// lastSave and savedDay are when, and on which UTC day, the history was
	// last written; dirty is set while it holds something unwritten.
	lastSave time.Time
	savedDay int
	dirty    bool
}

// newDriftTracker opens the drift history for the daemon. A history that
// cannot be used is not an error: an unusable file is set aside and tracking
// starts over, and one that must not be replaced leaves tracking off, with
// one log line either way.
func newDriftTracker(cfg config.Config, g topology.Graph, now func() time.Time, logf func(string, ...any)) (*driftTracker, error) {
	if !cfg.DriftTracking || cfg.Once {
		return nil, nil
	}
	if err := cfg.ValidateDrift(); err != nil {
		return nil, err
	}
	// newPersister has created and probed the directory already; this keeps
	// the tracker usable on its own.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("--state-dir: %w", err)
	}
	d := &driftTracker{store: drift.NewStore(cfg.StateDir), now: now, logf: logf, rnicKey: map[string]string{}}
	var rnicKeys []string
	d.paths, rnicKeys = drift.Paths(g)
	for i, n := range g.RNICs {
		d.rnicKey[n.ID] = rnicKeys[i]
	}
	f, status, why := d.store.Load()
	switch status {
	case drift.LoadBlocked:
		logf("hostping: drift tracking is off: the history %s can be neither used nor replaced (%s); every path reports history_blocked until it is removed or this build is upgraded", d.store.Path(), why)
		return d, nil
	case drift.LoadSetAside:
		logf("hostping: drift history %s was not usable (%s): kept as %s, and tracking starts over", d.store.Path(), why, d.store.RejectedPath())
	case drift.LoadFresh:
		logf("hostping: drift tracking starts a new history in %s: %d paths, each unknown until it has been read on enough idle days", d.store.Path(), len(d.paths))
	case drift.LoadRestored:
		known, refs, drifted := 0, 0, 0
		for _, p := range d.paths {
			if h := f.Paths[p.Key]; h != nil {
				known++
				if h.Ref != nil {
					refs++
				}
				if len(h.Drifted) > 0 {
					drifted++
				}
			}
		}
		logf("hostping: drift history %s restored: %d of this topology's %d paths (%d with a reference, %d judged drifted), %d paths of earlier topologies",
			d.store.Path(), known, len(d.paths), refs, drifted, len(f.Paths)-known)
	}
	d.tr = drift.New(cfg.DriftTh, d.paths, rnicKeys, f)
	return d, nil
}

// live gives the tracker the readers of a live host.
func (d *driftTracker) live(g topology.Graph) {
	if d == nil {
		return
	}
	d.temps = monitor.TempReader{Graph: g}.Read
	d.software = func() (string, map[string]string) {
		return monitor.KernelRelease("/proc"), monitor.RNICFirmware("/sys", g)
	}
}

// runEnded is called when an idle-trigger run has just ended, before its
// report is held: the device temperatures are read now, while the devices
// are as warm as the probes left them.
func (d *driftTracker) runEnded(ctx context.Context, rep model.Report) {
	if d == nil || d.tr == nil || rep.Trigger != model.TriggerIdle || d.temps == nil {
		return
	}
	d.held = d.temps(ctx)
}

// discarded drops what was read for a run whose report was discarded.
func (d *driftTracker) discarded() {
	if d != nil {
		d.held = nil
	}
}

// observe applies a confirmed report to the history. Only an idle-trigger
// run is applied; its report gets the drift section, and every path's state
// is returned for the metrics. Any other report is left as it is.
func (d *driftTracker) observe(rep *model.Report) []model.PathDrift {
	if d == nil || rep.Trigger != model.TriggerIdle || rep.Mode != model.ModeIdle {
		return nil
	}
	held := d.held
	d.held = nil
	if d.tr == nil {
		paths := make([]model.PathDrift, len(d.paths))
		for i, p := range d.paths {
			paths[i] = model.PathDrift{RNIC: p.RNIC, Endpoint: p.Endpoint, Reason: model.DriftHistoryBlocked}
		}
		rep.Drift = &model.DriftSummary{Unknown: len(paths)}
		if len(paths) > 0 {
			rep.Drift.UnknownReasons = []model.DriftUnknownCount{{Reason: model.DriftHistoryBlocked, Paths: len(paths)}}
		}
		return paths
	}
	now := d.now()
	res := d.tr.Observe(now, drift.Samples(*rep, held), d.readSoftware())
	for _, line := range res.Logs {
		d.logf("hostping: drift: %s", line)
	}
	rep.Drift = &res.Summary
	d.dirty = true
	if day := int(now.Unix() / 86400); len(res.Logs) > 0 || day != d.savedDay || now.Sub(d.lastSave) >= driftSaveEvery {
		d.save(now)
	}
	return res.Paths
}

// readSoftware is the software of the live host, keyed as the history keys
// it; the zero value where it cannot be read (--sim).
func (d *driftTracker) readSoftware() drift.Software {
	if d.software == nil {
		return drift.Software{}
	}
	kernel, firmware := d.software()
	sw := drift.Software{Kernel: kernel}
	for id, v := range firmware {
		if key, ok := d.rnicKey[id]; ok {
			if sw.Firmware == nil {
				sw.Firmware = map[string]string{}
			}
			sw.Firmware[key] = v
		}
	}
	return sw
}

// shutdown writes what the history holds beyond its last save.
func (d *driftTracker) shutdown() {
	if d == nil || d.tr == nil || !d.dirty {
		return
	}
	d.save(d.now())
}

func (d *driftTracker) save(now time.Time) {
	if err := d.store.Save(d.tr.File(), now); err != nil {
		// Still dirty: the next run, or the shutdown, tries again.
		d.logf("hostping: drift history %s not saved: %v", d.store.Path(), err)
		return
	}
	d.lastSave, d.savedDay, d.dirty = now, int(now.Unix()/86400), false
}
