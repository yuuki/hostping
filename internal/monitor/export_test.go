package monitor

import "time"

// SetClock replaces the poll clock so tests can step through intervals.
func (m *Monitor) SetClock(now func() time.Time) { m.now = now }
