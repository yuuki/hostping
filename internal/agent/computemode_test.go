package agent

import (
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/topology"
)

func TestRequestFromDecisionCarriesComputeModes(t *testing.T) {
	req, err := requestFromDecision(monitor.Decision{
		Ready: true, IdleKnown: true, GPUComputeMode: map[string]string{"gpu0": model.ComputeModeProhibited},
	})
	if err != nil || req.GPUComputeMode["gpu0"] != model.ComputeModeProhibited {
		t.Fatalf("req=%+v err=%v", req, err)
	}
}

// --sim --once --fault compute_mode:<gpu> reports the GPU as skipped, end to
// end through the one-shot path.
func TestRunOnceSimComputeModeFault(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	cfg := config.Defaults()
	cfg.Sim = true
	cfg.Once = true
	cfg.SimFault = "compute_mode:gpu3"

	out, err := os.CreateTemp(t.TempDir(), "report")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	defer quiet(t)()
	os.Stdout = out
	if err := Run(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	// Only the skips: the report's status fields marshal one way.
	var rep struct {
		SkippedEndpoints []model.EndpointSkip `json:"skipped_endpoints"`
	}
	if err := json.NewDecoder(out).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.SkippedEndpoints) != 1 {
		t.Fatalf("skips = %+v, want one", rep.SkippedEndpoints)
	}
	s := rep.SkippedEndpoints[0]
	if s.Kind != model.EndpointGPU || len(s.Endpoints) != 1 || s.Endpoints[0] != "gpu3" || s.Reason != "compute_mode: Exclusive_Process" {
		t.Fatalf("skip = %+v, want gpu3 compute_mode: Exclusive_Process", s)
	}
}

// The simulated monitor reads the fault's compute modes like the live one
// reads nvidia-smi, so the daemon loop carries them to its runs too.
func TestIdleGPUsCarriesSimComputeMode(t *testing.T) {
	got := idleGPUs(topology.Fig2(), map[string]string{"gpu1": model.ComputeModeProhibited})
	if len(got) != 8 {
		t.Fatalf("got %d GPUs, want 8", len(got))
	}
	for _, st := range got {
		want := ""
		if st.ID == "gpu1" {
			want = model.ComputeModeProhibited
		}
		if !st.Idle || st.ComputeMode != want {
			t.Fatalf("%+v, want idle with mode %q", st, want)
		}
	}
}
