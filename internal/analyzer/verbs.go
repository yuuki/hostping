package analyzer

import (
	"fmt"
	"maps"
	"slices"

	"github.com/yuuki/hostping/internal/model"
)

// ToN'24 §VI-C: timing the verbs a probe calls exposes bottlenecks in calling
// them, such as GPU memory registration that a failing GPU makes slow. Such a
// GPU can still move data at full rate once registered, so neither latency nor
// bandwidth — and therefore neither path status nor Algorithm 1 — sees it.
const (
	// slowMRFactor is how many times its siblings' median a GPU's
	// registration time must reach to be reported.
	slowMRFactor = 4
	// slowMRFloorNs is the least absolute excess over that median worth
	// reporting. Healthy registrations take tens to hundreds of µs, where a
	// ratio alone would turn scheduler jitter into a finding.
	slowMRFloorNs = 1_000_000
	// slowMRMinSiblings is how many other GPUs must have a registration
	// time before any one of them is judged against their median.
	slowMRMinSiblings = 3
	// slowMRRuns is how many consecutive idle runs the same GPU must cross
	// the threshold in before it is reported.
	slowMRRuns = 2
)

// diagnoseSlowMR reports GPUs whose memory registration is far slower than
// that of the host's other GPUs, run after run. It is advisory: it reads only
// the verb timings, never the path statuses, and changes nothing Algorithm 1
// decides. Besides the causes it returns the per-GPU streak of consecutive
// idle runs that crossed the threshold, which the caller keeps for the next
// run (Input.SlowMRHits).
//
// Each GPU is summarised by the lower median of its readings over the RNICs
// that probed it (for an even count, the lower of the two middle values),
// since registration time belongs to the GPU's memory and one slow RNIC
// should not single out whichever GPU it happened to be paired with. Healthy
// GPUs register slowly now and then: single readings tens of times the usual
// value, on random GPU/RNIC pairs. A GPU probed from few RNICs has few
// readings, and with a plain median two such spikes out of four readings
// average into a slow GPU. The lower median moves only when more than half of
// the readings are slow, so a GPU slow on most probes still crosses.
//
// The comparison is host-wide rather than per RNIC for the same reason:
// every GPU is registered the same way, whichever RNIC it sits behind. The
// reference is the median of the other GPUs' lower medians, so one slow GPU
// cannot raise its own bar.
//
// A crossing is reported only once the same GPU has crossed in slowMRRuns
// consecutive idle runs: spikes that still line up on one GPU in one run do
// not line up on it again in the next. An idle run that judges the GPU
// without it crossing resets its streak. A run that does not judge it — busy
// mode, too few siblings, or no reading for that GPU — keeps the streak
// without adding to it, as the gray streaks of untested links are kept.
//
// It judges idle runs only. On a loaded host a registration competes with the
// workload for that GPU and its driver, so its time reflects the load rather
// than the GPU's health: under an all-to-all collective with every GPU
// saturated, single registrations take several times the idle median on
// healthy hardware. Busy mode also probes only the reduced set, which leaves
// each GPU one reading, so the per-GPU statistic cannot absorb that noise.
// The verb timings themselves are still reported and exported in every mode.
//
// An idle GPU with a single reading (e.g. a host with one RNIC) is still
// judged: uncontended registration is stable, a slow reading there is worth
// surfacing, and the confidence stays low because it was not confirmed from a
// second RNIC. Requiring two readings would leave such hosts unchecked.
func diagnoseSlowMR(in Input) ([]model.RootCause, map[string]int) {
	hits := maps.Clone(in.SlowMRHits)
	if hits == nil {
		hits = map[string]int{}
	}
	if in.Mode != model.ModeIdle {
		return nil, hits
	}
	readings := map[string][]uint64{}
	var gpus []string
	for _, e := range in.Graph.Endpoints {
		if e.Kind != model.EndpointGPU {
			continue
		}
		for _, m := range in.Measurements {
			if m.Endpoint == e.ID && m.Verbs.MRReg > 0 {
				readings[e.ID] = append(readings[e.ID], m.Verbs.MRReg)
			}
		}
		if len(readings[e.ID]) > 0 {
			gpus = append(gpus, e.ID)
		}
	}
	if len(gpus) < slowMRMinSiblings+1 {
		return nil, hits
	}
	rep := map[string]uint64{}
	for _, id := range gpus {
		rep[id] = lowerMedianU64(readings[id])
	}

	var out []model.RootCause
	for _, id := range gpus {
		var sib []uint64
		for _, other := range gpus {
			if other != id {
				sib = append(sib, rep[other])
			}
		}
		ref := medianU64(sib)
		if !slowMR(rep[id], ref) {
			delete(hits, id)
			continue
		}
		hits[id]++
		if hits[id] < slowMRRuns {
			continue
		}
		// One slow reading could be a stall on that probe; a GPU that is slow
		// from every RNIC that reached it is slow itself.
		conf := "low"
		if len(readings[id]) > 1 && allSlowMR(readings[id], ref) {
			conf = "medium"
		}
		out = append(out, model.RootCause{
			Code:       model.CauseGPUMRRegistrationSlow,
			Summary:    "GPU memory registration far slower than sibling GPUs",
			Endpoints:  []string{id},
			Confidence: conf,
			Evidence: fmt.Sprintf("mr_reg %.2f ms (lower median of %d probes) vs %.2f ms median of %d sibling GPUs; crossed in %d consecutive idle runs",
				float64(rep[id])/1e6, len(readings[id]), float64(ref)/1e6, len(sib), hits[id]),
		})
	}
	return out, hits
}

func slowMR(v, ref uint64) bool {
	return v >= slowMRFactor*ref && v-ref >= slowMRFloorNs
}

func allSlowMR(vs []uint64, ref uint64) bool {
	for _, v := range vs {
		if !slowMR(v, ref) {
			return false
		}
	}
	return true
}

// lowerMedianU64 is the middle value of an odd-length input and the lower of
// the two middle values of an even-length one, so it never moves unless more
// than half of the values do.
func lowerMedianU64(in []uint64) uint64 {
	v := slices.Clone(in)
	slices.Sort(v)
	return v[(len(v)-1)/2]
}

// medianU64 averages the two middle values of an even-length input.
func medianU64(in []uint64) uint64 {
	v := slices.Clone(in)
	slices.Sort(v)
	if len(v)%2 == 1 {
		return v[len(v)/2]
	}
	return (v[len(v)/2-1] + v[len(v)/2]) / 2
}
