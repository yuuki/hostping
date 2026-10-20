package analyzer

import (
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// run is the fixed input of one analyzer pass: what every entry point —
// Analyze, the Classify* functions and the Plan* functions — takes besides
// the measurements. The helpers are methods on it so they do not each take
// the same five arguments.
type run struct {
	g     topology.Graph
	cfg   config.Config
	mode  model.RunMode
	busy  map[string]bool
	bases baseline.Overlay
}

func newRun(g topology.Graph, mode model.RunMode, busy map[string]bool, cfg config.Config, bases baseline.Overlay) run {
	return run{g: g, cfg: cfg, mode: mode, busy: busy, bases: bases}
}

// run is the pass in's fixed input.
func (in Input) run() run {
	return newRun(in.Graph, in.Mode, in.BusyRNICs, in.Cfg, in.Bases)
}

// idleJudged reports whether RNIC rnic's paths are judged against the idle
// baseline in this run: every RNIC of an idle run, and the unloaded ones of
// a busy run.
func (r run) idleJudged(rnic string) bool {
	return r.mode != model.ModeBusy || !r.busy[rnic]
}

// pathClass is what one routed path is judged as: the path and its RNIC, the
// idle baseline it is held to, the learned sub-class it falls in, the
// baseline-file section its RNIC takes its values from, and the latency the
// operator set for it. The bandwidth verdict, the busy sibling comparison,
// the latency class and the bandwidth recheck grouping each key on a part of
// it (busyKey, bwKey, latKey).
type pathClass struct {
	path model.Path
	rnic model.RNIC
	// base is the idle baseline the path is judged against: the Fig.4
	// default scaled to its RNIC with the operator's overrides applied,
	// narrowed to the learned sub-class that lists the path's root-port
	// pair.
	base baseline.PathBaseline
	// subClass is the full name of that sub-class ("cross_root/fast"), or
	// "" when the distance's own value applies. It is the path's
	// BaselineClass in the report.
	subClass string
	// section is the baseline-file "tables" section the RNIC takes its
	// values from, "" for the top level. A section exists because its
	// table reads differently, so its paths are no latency siblings of the
	// rest.
	section string
	// userLatencyNs is the latency the operator set for the path's class in
	// the overlay — the sub-class's, else the distance's — and 0 when none:
	// the Fig.4 latencies are one server's numbers and are never judged
	// against (latency.go).
	userLatencyNs uint64
	// busyProbed reports whether the path is in the reduced busy probe set,
	// the paths a loaded RNIC measures at all.
	busyProbed bool
}

// resolvePathClass is m's path class, and false when the graph has no path
// for it.
//
// The bandwidth baseline comes from the RNIC's effective table
// (Overlay.Effective: Fig.4 scaled to the RNIC, overlaid), the user latency
// from the overlay alone (Overlay.Of), since a latency is judged only against
// a value the operator supplied. Both narrow to the sub-class by the same
// root-port pair; Apply keeps each distance's sub-classes as the overlay
// lists them and the defaults carry none, so the two routes name the same
// sub-class for every distance a path can have.
func (r run) resolvePathClass(m model.PathMeasurement) (pathClass, bool) {
	p, ok := r.g.Path(m.RNIC, m.Endpoint)
	if !ok {
		return pathClass{}, false
	}
	n := mustRNIC(r.g, p.RNIC)
	pair, _ := r.g.RootPortPair(p)
	base, sub := baseline.ForPath(r.bases.Effective(n), p.Distance).ForPair(pair)
	ov, section := r.bases.Of(baseline.RNICTable(n))
	user, _ := ov[p.Distance].ForPair(pair)
	c := pathClass{
		path:          p,
		rnic:          n,
		base:          base,
		section:       section,
		userLatencyNs: user.LatencyNs,
		busyProbed:    r.g.BusyProbed(p),
	}
	if sub != "" {
		c.subClass = baseline.ClassName(p.Distance, sub)
	}
	return c, true
}

// pathClasses resolves the class of every routed measurement in ms once,
// keyed by pathKey. A class depends on the graph and the baselines only, so
// one map serves every pass over the measurements and over any copy of them
// with the same paths.
func (r run) pathClasses(ms []model.PathMeasurement) map[string]pathClass {
	out := make(map[string]pathClass, len(ms))
	for _, m := range ms {
		k := pathKey(m.RNIC, m.Endpoint)
		if _, done := out[k]; done {
			continue
		}
		if c, ok := r.resolvePathClass(m); ok {
			out[k] = c
		}
	}
	return out
}

// pathClassOf is the class of m in classes, and false when m's path is not
// routed.
func pathClassOf(classes map[string]pathClass, m model.PathMeasurement) (pathClass, bool) {
	c, ok := classes[pathKey(m.RNIC, m.Endpoint)]
	return c, ok
}

// busyKey is the busy sibling class of the path (busyRefKey).
func (c pathClass) busyKey() busyRefKey {
	return busyRefKey{rnic: c.path.RNIC, dist: c.path.Distance, subClass: c.subClass}
}

// bwKey is the distance class of the path for the bandwidth recheck
// (bwClass).
func (c pathClass) bwKey() bwClass {
	return bwClass{dist: c.path.Distance, subClass: c.subClass, base: c.base.BandwidthBPS}
}

// bwGroup is the bandwidth recheck group the path belongs to (bwGroupKey).
func (c pathClass) bwGroup() string {
	return bwGroupKey(c.path.RNIC, c.path.Distance, c.subClass)
}

// latKey is the latency class of the path (latClass).
func (c pathClass) latKey() latClass {
	return latClass{dist: c.path.Distance, subClass: c.subClass, section: c.section}
}
