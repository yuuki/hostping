package monitor_test

import (
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
)

// A mutual-probe alarm starts a run on a loaded host that raised neither PFC
// nor drops, in busy mode, at most once per --mutual-window.
func TestMutualAlarmStartsRunOncePerWindow(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	cfg.MutualWindow = 20 * time.Second
	cfg.BusyCooldown = 5 * time.Second
	h := newLoadedHost(t, cfg, "rnic0")
	alarm := false
	h.m.MutualAlarm = func() bool { return alarm }
	for i := 1; i <= 10; i++ {
		if d := h.step(); d.Run {
			t.Fatalf("step %d: no alarm, no run: %+v", i, d)
		}
	}
	alarm = true
	var runs []int
	for i := 11; i <= 60; i++ {
		d := h.step()
		if !d.Run {
			continue
		}
		if d.Trigger != model.TriggerMutualTimeout || d.Mode != model.ModeBusy || !d.BusyRNICs["rnic0"] {
			t.Fatalf("step %d decision %+v", i, d)
		}
		runs = append(runs, i)
	}
	if len(runs) != 3 || runs[0] != 11 || runs[1] != 31 || runs[2] != 51 {
		t.Fatalf("alarm runs at steps %v, want [11 31 51]", runs)
	}
}

// On an idle host the alarm run is an idle-mode run.
func TestMutualAlarmIdleHostRunsIdleMode(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	h := newLoadedHost(t, cfg)
	h.s.GPU[0].Idle = true
	if d := h.step(); !d.Run || d.Trigger != model.TriggerIdle {
		t.Fatalf("first idle run: %+v", d)
	}
	h.m.MutualAlarm = func() bool { return true }
	d := h.step()
	if !d.Run || d.Trigger != model.TriggerMutualTimeout || d.Mode != model.ModeIdle {
		t.Fatalf("alarm on an idle host: %+v", d)
	}
}
