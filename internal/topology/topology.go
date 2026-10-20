package topology

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"

	"github.com/yuuki/hostping/internal/model"
)

// Graph is the intra-host network: RNICs, endpoints, links, and RNIC→endpoint paths.
type Graph struct {
	Name      string           `json:"name"`
	RNICs     []model.RNIC     `json:"rnics"`
	Endpoints []model.Endpoint `json:"endpoints"`
	Links     []model.Link     `json:"links"`
	Paths     []model.Path     `json:"paths"`
	// Excluded lists the RDMA devices discovery dropped on --exclude-rnic.
	// They take no part in the graph.
	Excluded []model.ExcludedRNIC `json:"excluded,omitempty"`
}

// setRNICPCIeCaps records on every RNIC the capacity of the PCIe hop that
// attaches it. Baseline derivation needs that number to bound the Fig.4 ratios
// by the bus instead of the wire, and it must reach the baseline package
// without dragging the whole graph along. An RNIC that already carries a
// capacity (a hand-written topology file) keeps it; one whose hop is missing
// keeps 0, which means "unknown" and leaves the baseline scaled to line rate.
func (g *Graph) setRNICPCIeCaps() {
	for i := range g.RNICs {
		n := &g.RNICs[i]
		if n.PCIeCapBPS > 0 {
			continue
		}
		for _, l := range g.Links {
			if l.Kind != model.LinkKindPCIeRNIC {
				continue
			}
			if l.Device == n.ID || l.ID == model.PCIeLinkID(n.ID) {
				n.PCIeCapBPS = l.CapacityBPS
				break
			}
		}
	}
}

func (g Graph) RNIC(id string) (model.RNIC, bool) {
	for _, n := range g.RNICs {
		if n.ID == id {
			return n, true
		}
	}
	return model.RNIC{}, false
}

func (g Graph) Endpoint(id string) (model.Endpoint, bool) {
	for _, e := range g.Endpoints {
		if e.ID == id {
			return e, true
		}
	}
	return model.Endpoint{}, false
}

func (g Graph) Link(id string) (model.Link, bool) {
	for _, l := range g.Links {
		if l.ID == id {
			return l, true
		}
	}
	return model.Link{}, false
}

func (g Graph) Path(rnic, endpoint string) (model.Path, bool) {
	for _, p := range g.Paths {
		if p.RNIC == rnic && p.Endpoint == endpoint {
			return p, true
		}
	}
	return model.Path{}, false
}

// PathCapacity is the narrowest hop capacity along p: the bandwidth the path
// carries while every hop is healthy. Hops whose capacity is unknown (0) are
// skipped, and 0 is returned when no hop on the path declares one.
func (g Graph) PathCapacity(p model.Path) uint64 {
	var min uint64
	for _, id := range p.LinkIDs {
		l, ok := g.Link(id)
		if !ok || l.CapacityBPS == 0 {
			continue
		}
		if min == 0 || l.CapacityBPS < min {
			min = l.CapacityBPS
		}
	}
	return min
}

// RootPortPair is the unordered pair of root-port links p crosses, the key of
// a learned baseline sub-class. A path that crosses fewer or more than two
// distinct root ports has no pair (ok false): only a path between two root
// ports can belong to a sub-class.
func (g Graph) RootPortPair(p model.Path) (model.RootPortPair, bool) {
	var rps []string
	for _, id := range p.LinkIDs {
		l, ok := g.Link(id)
		if !ok || l.Kind != model.LinkKindRootPort || slices.Contains(rps, id) {
			continue
		}
		rps = append(rps, id)
	}
	if len(rps) != 2 {
		return model.RootPortPair{}, false
	}
	return model.NewRootPortPair(rps[0], rps[1]), true
}

// RootPortPairs is the set of root-port pairs the graph's paths cross.
func (g Graph) RootPortPairs() map[model.RootPortPair]bool {
	out := map[model.RootPortPair]bool{}
	for _, p := range g.Paths {
		if pair, ok := g.RootPortPair(p); ok {
			out[pair] = true
		}
	}
	return out
}

func (g Graph) LinksByKind(kind string) []model.Link {
	var out []model.Link
	for _, l := range g.Links {
		if l.Kind == kind {
			out = append(out, l)
		}
	}
	return out
}

func (g Graph) AffinitiveEndpoints(rnicID string) []model.Endpoint {
	n, ok := g.RNIC(rnicID)
	if !ok {
		return nil
	}
	var out []model.Endpoint
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointMemory && e.Socket == n.Socket {
			// Under sub-NUMA clustering another NUMA node of the package is
			// remote memory across the package mesh, not affinitive.
			if p, ok := g.Path(n.ID, e.ID); ok && p.Distance == model.DistanceRemoteMem {
				continue
			}
			out = append(out, e)
			continue
		}
		if e.Kind == model.EndpointGPU && e.RootPort == n.RootPort {
			out = append(out, e)
		}
	}
	return out
}

func (g Graph) PathsFrom(rnicID string) []model.Path {
	var out []model.Path
	for _, p := range g.Paths {
		if p.RNIC == rnicID {
			out = append(out, p)
		}
	}
	return out
}

// BusyProbed is the reduced probe set used when PFC/drops trigger the engine:
// every memory node plus GPUs under the same root port (NSDI'23 §4.3, Fig.10).
func (g Graph) BusyProbed(p model.Path) bool {
	e, ok := g.Endpoint(p.Endpoint)
	if !ok {
		return false
	}
	if e.Kind == model.EndpointMemory {
		return true
	}
	return p.Affinitive
}

func (g Graph) ProbePaths(rnicID string, mode model.RunMode) []model.Path {
	var out []model.Path
	for _, p := range g.Paths {
		if p.RNIC != rnicID {
			continue
		}
		if mode == model.ModeBusy && !g.BusyProbed(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// MutualPair reports whether two RNICs should be probed. Empty fabric
// (Fig.2) pairs every distinct device. "unknown" means no RoCEv2 GID (or,
// on InfiniBand, no usable LID) was found and must not pair (avoids a
// full-mesh of fe80 RoCEv1 GIDs). A RoCE RNIC never pairs with an
// InfiniBand one ("ib:" key). Under MutualScopeRail (the default, and what
// an empty scope means) discovered fabrics pair only the same RoCEv2 rail
// or InfiniBand subnet prefix; MutualScopeHost pairs
// across rails too, which only measures something when the rails route to
// each other (ToN'24 §IV-A-2 probes RNIC to RNIC through the ToR).
func MutualPair(a, b model.RNIC, scope model.MutualScope) bool {
	if a.ID == b.ID {
		return false
	}
	if a.Device != "" && a.Device == b.Device {
		return false
	}
	if a.Fabric == "unknown" || b.Fabric == "unknown" {
		return false
	}
	// RoCE and InfiniBand RNICs cannot reach each other in any scope.
	if isIBFabric(a.Fabric) != isIBFabric(b.Fabric) {
		return false
	}
	if scope == model.MutualScopeHost {
		return true
	}
	if a.Fabric == "" && b.Fabric == "" {
		return true
	}
	return a.Fabric != "" && a.Fabric == b.Fabric
}

// CrossRail reports whether a mutual pair crosses rails: its RNICs carry
// different fabric keys, so its traffic leaves the rail. Only
// MutualScopeHost pairs such RNICs.
func CrossRail(a, b model.RNIC) bool {
	return a.Fabric != b.Fabric
}

// MutualPairs returns the RNIC pairs the mutual probe measures under scope,
// in graph order and once per unordered pair. InfiniBand RNICs are left out;
// see MutualPairsFor.
func (g Graph) MutualPairs(scope model.MutualScope) [][2]model.RNIC {
	return g.MutualPairsFor(scope, false)
}

// MutualPairsFor is MutualPairs with InfiniBand RNICs included when
// infiniband is set (--mutual-infiniband). They are left out by default in
// every scope: the subnet prefix is the only rail key InfiniBand offers, and
// separate fabrics that all keep the default prefix share it, so their RNICs
// would pair and time out as false connectivity failures.
func (g Graph) MutualPairsFor(scope model.MutualScope, infiniband bool) [][2]model.RNIC {
	var out [][2]model.RNIC
	for i, a := range g.RNICs {
		for j, b := range g.RNICs {
			if i >= j || !MutualPair(a, b, scope) {
				continue
			}
			if !infiniband && (IsInfiniBand(a) || IsInfiniBand(b)) {
				continue
			}
			out = append(out, [2]model.RNIC{a, b})
		}
	}
	return out
}

// FabricKeys returns the distinct RNIC fabric keys of the graph, sorted. An
// empty pair set is explained by this set: one key per rail on a rail-optimized
// host (nothing to pair with), or "unknown" where no RoCEv2 GID was found.
func (g Graph) FabricKeys() []string {
	seen := make(map[string]struct{}, len(g.RNICs))
	out := make([]string, 0, len(g.RNICs))
	for _, n := range g.RNICs {
		if _, ok := seen[n.Fabric]; ok {
			continue
		}
		seen[n.Fabric] = struct{}{}
		out = append(out, n.Fabric)
	}
	sort.Strings(out)
	return out
}

func LoadFile(path string) (Graph, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Graph{}, err
	}
	var g Graph
	if err := json.Unmarshal(b, &g); err != nil {
		return Graph{}, fmt.Errorf("topology json: %w", err)
	}
	if err := g.Validate(); err != nil {
		return Graph{}, fmt.Errorf("topology %s: %w", path, err)
	}
	g.setRNICPCIeCaps()
	return g, nil
}

// Validate rejects a graph the engine cannot probe. Without it a file that
// merely forgot "paths" is accepted here and fails much later as "no loopback
// measurements", which says nothing about the file.
func (g Graph) Validate() error {
	rnics, err := uniqueIDs("rnic", len(g.RNICs), func(i int) string { return g.RNICs[i].ID })
	if err != nil {
		return err
	}
	eps, err := uniqueIDs("endpoint", len(g.Endpoints), func(i int) string { return g.Endpoints[i].ID })
	if err != nil {
		return err
	}
	links, err := uniqueIDs("link", len(g.Links), func(i int) string { return g.Links[i].ID })
	if err != nil {
		return err
	}
	if len(g.Paths) == 0 {
		return fmt.Errorf("no paths: every RNIC→endpoint path must be listed")
	}

	seen := map[string]struct{}{}
	for i, p := range g.Paths {
		where := fmt.Sprintf("path %d (%s→%s)", i, p.RNIC, p.Endpoint)
		if _, ok := rnics[p.RNIC]; !ok {
			return fmt.Errorf("%s: unknown rnic %q", where, p.RNIC)
		}
		if _, ok := eps[p.Endpoint]; !ok {
			return fmt.Errorf("%s: unknown endpoint %q", where, p.Endpoint)
		}
		if len(p.LinkIDs) == 0 {
			return fmt.Errorf("%s: no link_ids", where)
		}
		for _, id := range p.LinkIDs {
			if _, ok := links[id]; !ok {
				return fmt.Errorf("%s: unknown link %q", where, id)
			}
		}
		key := p.RNIC + "->" + p.Endpoint
		if _, ok := seen[key]; ok {
			return fmt.Errorf("%s: duplicate path", where)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// uniqueIDs collects the ids of one collection, rejecting an empty collection,
// a blank id and a duplicate id.
func uniqueIDs(kind string, n int, id func(int) string) (map[string]struct{}, error) {
	if n == 0 {
		return nil, fmt.Errorf("no %ss", kind)
	}
	out := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		v := id(i)
		if v == "" {
			return nil, fmt.Errorf("%s %d: empty id", kind, i)
		}
		if _, ok := out[v]; ok {
			return nil, fmt.Errorf("%s %d: duplicate id %q", kind, i, v)
		}
		out[v] = struct{}{}
	}
	return out, nil
}

func Named(name string) (Graph, error) {
	switch name {
	case "", "fig2":
		return Fig2(), nil
	case "fig2-rails":
		return Fig2Rails(), nil
	case "fig2-snc":
		return Fig2SNC(), nil
	default:
		return Graph{}, fmt.Errorf("unknown topology %q", name)
	}
}
