package zigprobe

import (
	"testing"

	"github.com/yuuki/hostping/internal/engine"
)

var _ engine.GPUReleaser = Prober{}

// With no GPU runtime loaded (no probe has allocated), releasing is a no-op
// that must not load or call into a CUDA/HIP library. Twice, because the
// engine calls it after every run.
func TestReleaseGPUWithoutRuntimeIsNoop(t *testing.T) {
	Prober{}.ReleaseGPU()
	Prober{}.ReleaseGPU()
}
