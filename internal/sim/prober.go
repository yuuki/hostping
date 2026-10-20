package sim

import (
	"context"

	"github.com/yuuki/hostping/internal/model"
)

// Prober uses the paper Eq.1 world. It is the engine's prober (engine.Prober)
// over a World, for tests and the simulated agent.
type Prober struct{ World *World }

func (p Prober) Loopback(_ context.Context, rnic model.RNIC, ep model.Endpoint, small, large uint32) (model.PathMeasurement, error) {
	return p.World.Loopback(rnic.ID, ep.ID, small, large)
}

func (p Prober) Mutual(_ context.Context, src, dst model.RNIC) (model.MutualMeasurement, error) {
	return p.World.Mutual(src.ID, dst.ID), nil
}
