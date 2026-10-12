package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// CalibrateOptions controls the runs behind hostpingctl calibrate.
type CalibrateOptions struct {
	// Runs is how many idle-mode runs the result is drawn from.
	Runs int
	// Interval is the pause between two runs, so they sample the host at
	// different moments rather than back to back.
	Interval time.Duration
	// MaxDiscards bounds how many runs may be thrown away because the host
	// was busy before or after them, once calibration has started, before
	// calibration gives up.
	MaxDiscards int
}

// CalibrateResult is what the runs measured.
type CalibrateResult struct {
	Graph     topology.Graph
	Reports   []model.Report
	Discarded int
}

// errHostBusy is an idle gate's answer when the host is loaded right now.
var errHostBusy = errors.New("host is not idle")

// idleGate decides whether calibration may run, and whether a finished run
// may be kept.
type idleGate interface {
	// before returns the request for the next idle-mode run, an error
	// wrapping errHostBusy when the host is loaded, or another error when
	// idle state cannot be read.
	before(ctx context.Context) (engine.RunRequest, error)
	// after reports whether the host stayed idle through the run that just
	// ended, and if not, what was busy.
	after(ctx context.Context) (bool, string, error)
}

type runner interface {
	Run(ctx context.Context, req engine.RunRequest) (model.Report, error)
}

// Calibrate performs opt.Runs idle-mode runs of the one-shot path, on the
// simulator with --sim or on the live host, for hostpingctl calibrate.
//
// It refuses to start on a host that is busy or whose idle state cannot be
// read: a baseline measured under load would be a floor every healthy idle
// run clears trivially. A run whose post-run poll finds the host busy is
// discarded, as the daemon discards such a run, and retried after the
// interval; so is a later run the host was busy before. More than
// opt.MaxDiscards of those fail calibration rather than settle for a result
// drawn from a host that does not stay idle.
func Calibrate(cfg config.Config, opt CalibrateOptions) (CalibrateResult, error) {
	cfg.Once = true
	rt, err := newRig(cfg)
	if err != nil {
		return CalibrateResult{}, err
	}
	defer rt.close()
	// Calibration reads each run's sweep readings (bandwidth and, with
	// --latency, small-message p50), never the latency verdict, and the
	// recheck changes nothing it reads: skip its probes. The same holds for
	// the bandwidth recheck, which changes only verdicts.
	rt.eng.SkipLatencyRecheck = true
	rt.eng.SkipBWRecheck = true
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var gate idleGate
	if rt.world != nil {
		gate = simGate{world: rt.world, g: rt.g}
	} else {
		gate = liveGate{mon: rt.mon, sampler: rt.sampler, g: rt.g, cfg: cfg}
	}
	reps, discarded, err := calibrateLoop(ctx, rt.eng, gate, opt, log.Printf)
	if err != nil {
		return CalibrateResult{}, err
	}
	return CalibrateResult{Graph: rt.g, Reports: reps, Discarded: discarded}, nil
}

func calibrateLoop(ctx context.Context, eng runner, gate idleGate, opt CalibrateOptions, logf func(string, ...any)) ([]model.Report, int, error) {
	if opt.Runs < 1 {
		return nil, 0, fmt.Errorf("--runs must be at least 1, got %d", opt.Runs)
	}
	if opt.Interval < 0 {
		return nil, 0, fmt.Errorf("--interval must not be negative, got %v", opt.Interval)
	}
	var reps []model.Report
	discarded := 0
	discard := func(why string) error {
		discarded++
		logf("hostping calibrate: discarded run (%d of at most %d): %s", discarded, opt.MaxDiscards, why)
		if discarded > opt.MaxDiscards {
			return fmt.Errorf("calibration abandoned after %d discarded runs: the host does not stay idle (last: %s)", discarded, why)
		}
		return nil
	}
	for attempt := 0; len(reps) < opt.Runs; attempt++ {
		if attempt > 0 && opt.Interval > 0 {
			select {
			case <-ctx.Done():
				return nil, discarded, ctx.Err()
			case <-time.After(opt.Interval):
			}
		}
		req, err := gate.before(ctx)
		if err != nil {
			if !errors.Is(err, errHostBusy) {
				return nil, discarded, err
			}
			if attempt == 0 {
				return nil, discarded, fmt.Errorf("refusing to calibrate: %w; calibrate a healthy host while it is idle", err)
			}
			if err := discard(err.Error()); err != nil {
				return nil, discarded, err
			}
			continue
		}
		rep, err := eng.Run(ctx, req)
		if err != nil {
			return nil, discarded, err
		}
		ok, why, err := gate.after(ctx)
		if err != nil {
			return nil, discarded, fmt.Errorf("could not confirm the host stayed idle during the calibration run: %w", err)
		}
		if !ok {
			if err := discard("the host became busy during the run: " + why); err != nil {
				return nil, discarded, err
			}
			continue
		}
		reps = append(reps, rep)
		logf("hostping calibrate: run %d/%d done (paths=%d skipped_endpoints=%d)", len(reps), opt.Runs, len(rep.Paths), rep.SkippedEndpointCount(""))
	}
	return reps, discarded, nil
}

// simGate is the simulator: idle unless a --fault loaded an RNIC, and with no
// load that can come back during a run.
type simGate struct {
	world *sim.World
	g     topology.Graph
}

func (s simGate) before(context.Context) (engine.RunRequest, error) {
	if busy := sortedKeys(s.world.Busy); len(busy) > 0 {
		return engine.RunRequest{}, fmt.Errorf("%w: busy RNICs %s", errHostBusy, strings.Join(busy, ","))
	}
	return engine.RunRequest{
		Trigger:        model.TriggerManual,
		Mode:           model.ModeIdle,
		HostIdle:       true,
		Util:           s.world.LinkUtil,
		PCIe:           pcieFrom(s.world, s.g),
		Hints:          hintsFrom(s.world),
		GPUComputeMode: s.world.ComputeMode,
	}, nil
}

func (simGate) after(context.Context) (bool, string, error) { return true, "", nil }

// liveGate reads the hardware monitor the way a live one-shot diagnose does.
type liveGate struct {
	mon     *monitor.Monitor
	sampler monitor.Sampler
	g       topology.Graph
	cfg     config.Config
}

func (l liveGate) before(ctx context.Context) (engine.RunRequest, error) {
	if _, err := l.mon.Poll(ctx); err != nil {
		return engine.RunRequest{}, fmt.Errorf("monitor warmup: %w", err)
	}
	dec, err := pollAfter(ctx, l.mon, l.cfg.MetricInterval)
	if err != nil {
		return engine.RunRequest{}, err
	}
	req, err := requestFromDecision(dec)
	if err != nil {
		// Unready or unknown idle state: refused, never retried as busy.
		return engine.RunRequest{}, err
	}
	if ok, why := idleVerdict(dec); !ok || req.Mode != model.ModeIdle {
		if why == "" {
			why = "monitor chose " + string(req.Mode) + " mode"
		}
		return engine.RunRequest{}, fmt.Errorf("%w: %s", errHostBusy, why)
	}
	// A baseline measured under CPU load would hold the loaded package's
	// paths to a level it reaches only under that load.
	if why := cpuLoadRose(nil, dec); why != "" {
		return engine.RunRequest{}, fmt.Errorf("%w: %s", errHostBusy, why)
	}
	req.Trigger = model.TriggerManual
	req.PCIe = pcieFrom(nil, l.g)
	req.Util = utilFrom(ctx, nil, l.sampler, req.Util)
	return req, nil
}

func (l liveGate) after(ctx context.Context) (bool, string, error) {
	// No CPU package may be loaded after a calibration run either.
	return confirmIdleAfterRun(ctx, l.mon, l.cfg, nil)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
