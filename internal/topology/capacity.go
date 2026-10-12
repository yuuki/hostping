package topology

import (
	"fmt"
	"sort"
)

// SetCapacity replaces the capacity of the links of kind with an
// operator-supplied one: byLink for the links it names, all for every other
// link of the kind (0 leaves them as they are). A replaced capacity is no
// longer assumed, so bus utilization may be computed against it. A named link
// that is missing or of another kind is an error rather than a silent no-op,
// since a typo would otherwise leave the link on its nominal constant.
func (g *Graph) SetCapacity(kind string, all uint64, byLink map[string]uint64) error {
	ids := make([]string, 0, len(byLink))
	for id := range byLink {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		l, ok := g.Link(id)
		if !ok {
			return fmt.Errorf("capacity for %s: no such link in topology %s", id, g.Name)
		}
		if l.Kind != kind {
			return fmt.Errorf("capacity for %s: link is %s, not %s", id, l.Kind, kind)
		}
	}
	for i := range g.Links {
		l := &g.Links[i]
		if l.Kind != kind {
			continue
		}
		c, ok := byLink[l.ID]
		if !ok {
			c = all
		}
		if c == 0 {
			continue
		}
		l.CapacityBPS = c
		l.CapacityAssumed = false
	}
	return nil
}
