package drift

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// Paths names every path of g to the tracker, in graph order, together with
// the key of each RNIC (the firmware map's keys, see RNICKey).
//
// A path is keyed by what it physically is, not by its RNIC and endpoint IDs:
// those are positions in the sorted device lists, so adding, removing or
// excluding one device renumbers every device after it, and a history keyed
// by them would silently continue under another path. The key is the RNIC's
// PCI address, the endpoint's (a GPU's PCI address; a memory node's ID and
// how many memory nodes the host has) and the distance, so a path whose
// route class changes starts a new history. Two paths that would share a key
// (a hand-written topology that repeats a PCI address) are told apart by
// their IDs.
func Paths(g topology.Graph) (paths []PathIdent, rnicKeys []string) {
	seen := map[string]bool{}
	for _, n := range g.RNICs {
		rnicKeys = append(rnicKeys, RNICKey(n))
	}
	memNodes := 0
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointMemory {
			memNodes++
		}
	}
	for _, p := range g.Paths {
		n, okN := g.RNIC(p.RNIC)
		e, okE := g.Endpoint(p.Endpoint)
		if !okN || !okE {
			continue
		}
		key := bound(RNICKey(n) + "->" + endpointKey(e, memNodes) + "/" + p.Distance.Key())
		if seen[key] {
			key = bound(key + "#" + p.RNIC + "->" + p.Endpoint)
		}
		seen[key] = true
		paths = append(paths, PathIdent{RNIC: p.RNIC, Endpoint: p.Endpoint, Key: key, EndpointSensor: e.Kind == model.EndpointGPU})
	}
	return paths, rnicKeys
}

// RNICKey identifies an RNIC in the history: its PCI address, else its
// verbs device name, else its ID.
func RNICKey(n model.RNIC) string {
	return bound(firstOf(n.PCIeBDF, n.Device, n.ID))
}

// endpointKey identifies an endpoint: a GPU by its PCI address, else by its
// ID. A memory node has no address, only its ID, which is its position among
// the host's memoryNodes memory nodes. That count is part of the key, since
// "mem1" is another node, with other memory behind it, once sub-NUMA
// clustering splits each package in two.
func endpointKey(e model.Endpoint, memoryNodes int) string {
	if e.Kind == model.EndpointGPU {
		return firstOf(e.PCIeBDF, e.ID)
	}
	return e.ID + "of" + strconv.Itoa(memoryNodes)
}

func firstOf(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return "unknown"
}

// bound makes k a key the file accepts: valid UTF-8 and at most maxKey
// bytes. A longer key is replaced by its SHA-256.
func bound(k string) string {
	k = strings.ToValidUTF8(k, "�")
	if len(k) <= maxKey {
		return k
	}
	sum := sha256.Sum256([]byte(k))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Samples picks the readings of rep that may enter the history, with the
// device temperatures read at the end of the run (by RNIC and endpoint ID; a
// device that was not read is absent).
//
// Only an idle-trigger run on an idle host qualifies, and none of its
// readings do while a CPU package is loaded: load on a package slows the
// paths through it without making them abnormal. Within the run, Clean says
// which paths count.
func Samples(rep model.Report, temps map[string]float64) []Sample {
	if rep.Trigger != model.TriggerIdle || rep.Mode != model.ModeIdle || !rep.HostIdle {
		return nil
	}
	if len(model.CPUBusySockets(rep.CPULoad)) > 0 {
		return nil
	}
	var out []Sample
	for _, p := range rep.Paths {
		if !Clean(p) {
			continue
		}
		s := Sample{RNIC: p.RNIC, Endpoint: p.Endpoint, Reading: Reading{
			BandwidthBPS: float64(p.BandwidthBPS),
			LatP50Ns:     float64(p.SmallLatNsP50),
			LatP99Ns:     float64(p.SmallLatNsP99),
		}}
		if c, ok := temps[p.RNIC]; ok && tempOK(c) {
			s.TempRNIC = &c
		}
		if c, ok := temps[p.Endpoint]; ok && tempOK(c) && p.EndpointKind == model.EndpointGPU {
			s.TempEndpoint = &c
		}
		out = append(out, s)
	}
	return out
}

// Clean reports whether a path's reading of an idle run may enter the
// history: every tracked signal was read, and the run judged the path
// neither faulty nor measured in a state that says nothing about it.
//
// Left out are a path that is abnormal on bandwidth or latency (a fault must
// not become the reference, and the existing diagnoses already report it), a
// path whose bandwidth was disputed by its own paired re-read (bw_recheck:
// the sweep reading the history would store is the one the recheck
// overruled or confirmed as faulty), a latency the recheck or the locality
// check cleared (the sweep reading was high for the time or the NUMA node it
// was taken in), and a path that is uncertain for a reason other than the
// steady states of a healthy idle host.
func Clean(p model.PathMeasurement) bool {
	if !valueOK(float64(p.BandwidthBPS)) || !valueOK(float64(p.SmallLatNsP50)) || !valueOK(float64(p.SmallLatNsP99)) {
		return false
	}
	if p.SmallLatNsP99 < p.SmallLatNsP50 {
		return false
	}
	if p.BWRecheck != nil || p.BusyRecheck != nil {
		return false
	}
	if p.LatencyStatus == model.LatencyAbnormal {
		return false
	}
	switch p.LatencyReason {
	case model.LatencyRecheckCleared, model.LatencyLocalityShift, model.LatencyCPUBusy, model.LatencyBusy:
		return false
	}
	switch p.Status {
	case model.PathNormal:
		return true
	case model.PathUncertain:
		switch p.UncertainReason {
		case model.UncertainNonAffinitive, model.UncertainBelowCeiling, model.UncertainNoCeiling:
			return true
		}
	}
	return false
}
