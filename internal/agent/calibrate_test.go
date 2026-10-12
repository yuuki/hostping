package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
)

type countingRunner struct{ runs int }

func (r *countingRunner) Run(context.Context, engine.RunRequest) (model.Report, error) {
	r.runs++
	return model.Report{Mode: model.ModeIdle, Trigger: model.Trigger(fmt.Sprint(r.runs))}, nil
}

// scriptedGate answers before/after from scripted sequences; an exhausted
// script answers idle.
type scriptedGate struct {
	pre  []error
	post []bool
}

func (g *scriptedGate) before(context.Context) (engine.RunRequest, error) {
	if len(g.pre) == 0 {
		return engine.RunRequest{Mode: model.ModeIdle}, nil
	}
	err := g.pre[0]
	g.pre = g.pre[1:]
	return engine.RunRequest{Mode: model.ModeIdle}, err
}

func (g *scriptedGate) after(context.Context) (bool, string, error) {
	if len(g.post) == 0 {
		return true, "", nil
	}
	ok := g.post[0]
	g.post = g.post[1:]
	return ok, "busy RNICs: rnic0", nil
}

func nolog(string, ...any) {}

func TestCalibrateLoopRefusesABusyHostAtStart(t *testing.T) {
	r := &countingRunner{}
	gate := &scriptedGate{pre: []error{fmt.Errorf("%w: busy RNICs rnic0", errHostBusy)}}
	_, _, err := calibrateLoop(context.Background(), r, gate, CalibrateOptions{Runs: 3, MaxDiscards: 5}, nolog)
	if err == nil || !strings.Contains(err.Error(), "refusing to calibrate") {
		t.Fatalf("err = %v", err)
	}
	if r.runs != 0 {
		t.Fatalf("ran %d times on a busy host", r.runs)
	}
}

func TestCalibrateLoopRefusesUnknownIdleState(t *testing.T) {
	r := &countingRunner{}
	unknown := errors.New("cannot classify idle vs busy")
	// Unknown is never retried, not even after a good run.
	gate := &scriptedGate{pre: []error{nil, unknown}}
	_, _, err := calibrateLoop(context.Background(), r, gate, CalibrateOptions{Runs: 3, MaxDiscards: 5}, nolog)
	if !errors.Is(err, unknown) {
		t.Fatalf("err = %v", err)
	}
}

func TestCalibrateLoopDiscardsRunsTheHostWasBusyDuring(t *testing.T) {
	r := &countingRunner{}
	gate := &scriptedGate{
		post: []bool{true, false, true},
		pre:  []error{nil, nil, nil, fmt.Errorf("%w: x", errHostBusy)},
	}
	reps, discarded, err := calibrateLoop(context.Background(), r, gate, CalibrateOptions{Runs: 3, MaxDiscards: 2}, nolog)
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 3 || discarded != 2 || r.runs != 4 {
		t.Fatalf("reps=%d discarded=%d runs=%d", len(reps), discarded, r.runs)
	}
	// The run the host was busy during (the second) is not among the results.
	for _, rep := range reps {
		if rep.Trigger == "2" {
			t.Fatal("discarded run kept")
		}
	}
}

func TestCalibrateLoopGivesUpAfterMaxDiscards(t *testing.T) {
	r := &countingRunner{}
	gate := &scriptedGate{post: []bool{false, false, false}}
	_, discarded, err := calibrateLoop(context.Background(), r, gate, CalibrateOptions{Runs: 2, MaxDiscards: 2}, nolog)
	if err == nil || !strings.Contains(err.Error(), "does not stay idle") {
		t.Fatalf("err = %v", err)
	}
	if discarded != 3 {
		t.Fatalf("discarded = %d", discarded)
	}
}

func TestCalibrateLoopRejectsBadOptions(t *testing.T) {
	for _, opt := range []CalibrateOptions{{Runs: 0}, {Runs: 1, Interval: -1}} {
		if _, _, err := calibrateLoop(context.Background(), &countingRunner{}, &scriptedGate{}, opt, nolog); err == nil {
			t.Errorf("%+v accepted", opt)
		}
	}
}

func TestCalibrateSim(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sim = true
	res, err := Calibrate(cfg, CalibrateOptions{Runs: 3, MaxDiscards: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reports) != 3 || res.Discarded != 0 {
		t.Fatalf("reports=%d discarded=%d", len(res.Reports), res.Discarded)
	}
	for _, rep := range res.Reports {
		if rep.Mode != model.ModeIdle || len(rep.Paths) == 0 {
			t.Fatalf("report mode=%s paths=%d", rep.Mode, len(rep.Paths))
		}
	}
	if len(res.Graph.RNICs) == 0 {
		t.Fatal("no graph returned")
	}
}

func TestCalibrateSimRefusesBusyHost(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sim = true
	cfg.SimFault = "busy:rnic0"
	_, err := Calibrate(cfg, CalibrateOptions{Runs: 1})
	if err == nil || !strings.Contains(err.Error(), "refusing to calibrate") {
		t.Fatalf("err = %v", err)
	}
}

func TestCalibrateRejectsBadBaselineFileAtStart(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sim = true
	cfg.BaselineFile = "/nonexistent/baselines.json"
	if _, err := Calibrate(cfg, CalibrateOptions{Runs: 1}); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("err = %v", err)
	}
}
