package drift

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
	"unicode/utf8"
)

// Version is the schema version of the drift history. Any change to the
// file, an added field included, takes a new version: a build that meets a
// newer version than it knows leaves the file untouched (see Store.Load), and
// that only protects the history if a newer build never writes a file this
// one would accept and then rewrite without the new fields.
const Version = 1

// FileName is the drift history inside --state-dir.
const FileName = "drift.json"

// ErrInvalid marks content that is not a drift history this build can use:
// not JSON, another shape, or values outside what the tracker itself writes.
var ErrInvalid = errors.New("not a usable drift history")

// File is the content of the drift history.
type File struct {
	Version int `json:"version"`
	// SavedAt is when the file was written.
	SavedAt time.Time `json:"saved_at,omitzero"`
	// LastDay is the day of the last run the history saw; 0 before the
	// first.
	LastDay int `json:"last_day"`
	// BehindSince is the day the clock was first seen behind the newest
	// day of the history, while it is (see Tracker.clockBehind); nil
	// otherwise. It is kept here so that a restart does not start the wait
	// over.
	BehindSince *int `json:"clock_behind_since,omitempty"`
	// Software is the software the last observed run saw.
	Software Software `json:"software,omitzero"`
	// Events are the software changes seen, oldest first.
	Events []Event `json:"events,omitempty"`
	// Paths holds one bounded summary per path, by path key.
	Paths map[string]*PathHistory `json:"paths"`
}

// Parse reads a drift history and checks every bound the tracker relies on.
// Unknown fields are rejected: a file with one was written by something
// other than this schema version.
func Parse(b []byte) (File, error) {
	var f File
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("%w: %s: %v", ErrInvalid, "json", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return File{}, fmt.Errorf("%w: %s", ErrInvalid, "data after the JSON object")
	}
	if err := f.validate(); err != nil {
		return File{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return f, nil
}

// Marshal encodes f as the file's content: one compact JSON object and a
// newline. The "->" of the path keys is written as it is, not as the
// \u003e that encoding/json escapes for HTML by default: operators read
// this file.
func Marshal(f File) ([]byte, error) {
	if f.Paths == nil {
		f.Paths = map[string]*PathHistory{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (f *File) validate() error {
	if f.Version != Version {
		return fmt.Errorf("schema version %d, this build reads %d", f.Version, Version)
	}
	if len(f.Paths) > MaxPaths {
		return fmt.Errorf("%d paths, more than the %d a history holds", len(f.Paths), MaxPaths)
	}
	if !dayOK(f.LastDay) {
		return fmt.Errorf("last_day %d out of range", f.LastDay)
	}
	if f.BehindSince != nil && !dayOK(*f.BehindSince) {
		return fmt.Errorf("clock_behind_since %d out of range", *f.BehindSince)
	}
	if err := textOK("software.kernel", f.Software.Kernel, maxText); err != nil {
		return err
	}
	if err := textOK("software.hostping", f.Software.Hostping, maxText); err != nil {
		return err
	}
	if len(f.Software.Firmware) > MaxPaths {
		return fmt.Errorf("%d firmware versions", len(f.Software.Firmware))
	}
	for k, v := range f.Software.Firmware {
		if err := keyOK("software.firmware", k); err != nil {
			return err
		}
		if err := textOK("software.firmware "+k, v, maxText); err != nil {
			return err
		}
	}
	// At most maxEventsPer of the kernel, of hostping and of every RNIC.
	if len(f.Events) > maxEventsPer*(MaxPaths+2) {
		return fmt.Errorf("%d events", len(f.Events))
	}
	for i, e := range f.Events {
		if !dayOK(e.Day) {
			return fmt.Errorf("events[%d]: day %d out of range", i, e.Day)
		}
		for what, v := range map[string]string{"component": e.Component, "from": e.From, "to": e.To} {
			if err := textOK("events "+what, v, maxText); err != nil {
				return err
			}
		}
		if e.Device != "" {
			if err := keyOK("events device", e.Device); err != nil {
				return err
			}
		}
	}
	for k, h := range f.Paths {
		if err := keyOK("paths", k); err != nil {
			return err
		}
		if h == nil {
			return fmt.Errorf("paths[%q] is null", k)
		}
		if err := h.validate(); err != nil {
			return fmt.Errorf("paths[%q]: %v", k, err)
		}
	}
	return nil
}

func (h *PathHistory) validate() error {
	if err := textOK("probe", h.Probe, maxKey); err != nil {
		return err
	}
	if h.Absent < 0 || h.Absent > maxDay {
		return fmt.Errorf("absent %d out of range", h.Absent)
	}
	if len(h.Days) > windowDays {
		return fmt.Errorf("%d days, more than %d", len(h.Days), windowDays)
	}
	last := -1
	for i, d := range h.Days {
		if !dayOK(d.Day) || d.Day <= last {
			return fmt.Errorf("days[%d]: day %d out of range or not after day %d", i, d.Day, last)
		}
		last = d.Day
		if d.N < 1 || d.N > maxSeen {
			return fmt.Errorf("days[%d]: n %d", i, d.N)
		}
		if err := checkReading(d.Reading); err != nil {
			return fmt.Errorf("days[%d]: %v", i, err)
		}
	}
	if r := h.Ref; r != nil {
		if !dayOK(r.From) || !dayOK(r.To) || r.From > r.To {
			return fmt.Errorf("reference: from %d, to %d", r.From, r.To)
		}
		if err := checkReading(r.Reading); err != nil {
			return fmt.Errorf("reference: %v", err)
		}
	}
	if o := h.Open; o != nil {
		if !dayOK(o.Day) || o.Day <= last {
			return fmt.Errorf("open: day %d out of range or not after day %d", o.Day, last)
		}
		if o.Seen < 1 || o.Seen > maxSeen {
			return fmt.Errorf("open: seen %d", o.Seen)
		}
		if o.Stride < 1 || o.Stride > maxStride || o.Stride&(o.Stride-1) != 0 {
			return fmt.Errorf("open: stride %d is not a power of two up to %d", o.Stride, maxStride)
		}
		if len(o.Samples) < 1 || len(o.Samples) > openCap {
			return fmt.Errorf("open: %d samples", len(o.Samples))
		}
		for i, s := range o.Samples {
			if err := checkReading(s); err != nil {
				return fmt.Errorf("open: samples[%d]: %v", i, err)
			}
		}
	}
	for s, v := range h.Drifted {
		if !knownSignal(string(s)) {
			return fmt.Errorf("drifted: unknown signal %q", s)
		}
		if !dayOK(v.Since) {
			return fmt.Errorf("drifted[%s]: day %d out of range", s, v.Since)
		}
		// A NaN fails both comparisons.
		if !(v.Threshold > 0 && v.Threshold <= maxThreshold) {
			return fmt.Errorf("drifted[%s]: threshold %v", s, v.Threshold)
		}
		if h.Ref == nil {
			return fmt.Errorf("drifted[%s] without a reference", s)
		}
	}
	return nil
}

// checkReading reports a reading the history cannot hold. It is a function,
// not a method: Day and Reference embed Reading, and would inherit it as
// their own check.
func checkReading(r Reading) error {
	for _, v := range []float64{r.BandwidthBPS, r.LatP50Ns, r.LatP99Ns} {
		if !valueOK(v) {
			return fmt.Errorf("value %v is not a reading between %v and %v", v, float64(minValue), float64(maxValue))
		}
	}
	for _, p := range []*float64{r.TempRNIC, r.TempEndpoint} {
		if p != nil && !tempOK(*p) {
			return fmt.Errorf("temperature %v out of range", *p)
		}
	}
	return nil
}

// valueOK reports a bandwidth or latency the history may hold: from
// minValue to maxValue. A NaN fails both comparisons. The lower bound is
// what keeps a change against a reference, a ratio of two such values, a
// finite number that a report can carry.
func valueOK(v float64) bool { return v >= minValue && v <= maxValue }

// tempOK reports a temperature the history may hold.
func tempOK(c float64) bool { return c >= minTempC && c <= maxTempC && !math.IsNaN(c) }

func dayOK(d int) bool { return d >= 0 && d <= maxDay }

func knownSignal(name string) bool {
	for _, s := range signals {
		if string(s.name) == name {
			return true
		}
	}
	return false
}

func keyOK(what, k string) error {
	if k == "" || len(k) > maxKey || !utf8.ValidString(k) {
		return fmt.Errorf("%s: key %q is empty, longer than %d bytes or not UTF-8", what, k, maxKey)
	}
	return nil
}

func textOK(what, s string, limit int) error {
	if len(s) > limit || !utf8.ValidString(s) {
		return fmt.Errorf("%s: %q is longer than %d bytes or not UTF-8", what, s, limit)
	}
	return nil
}
