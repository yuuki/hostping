package telemetry

import (
	"strconv"

	"go.opentelemetry.io/otel/attribute"

	"github.com/yuuki/hostping/internal/model"
)

// Status and reason codes of the hostping.path.*_code and
// hostping.link.status_code gauges.
//
// Each gauge holds one series per path or link whose value is the code of its
// current state. Every report rewrites the value, so a state change replaces
// the number rather than leaving a series of the old state behind; that is
// what the one-hot gauges of v0.1.1 (one 0/1 series per possible state) were
// for, at 22 series per path instead of 4.
//
// The codes are part of the metrics contract, so they are spelled out here
// rather than derived from the model enums: reordering or inserting an enum
// value must not renumber what dashboards and alerts already compare against.
// A new value gets the next unused code; a code is never reused.
//
// Zero: a status always has a value, so the status codes start at 1 and 0 is
// never a status. A 0 cannot be mistaken for a state by a query that fills a
// missing series with 0 or by a backend that defaults an unset gauge to 0. The
// reason codes use 0 for "none", the reason of a path that needs none, so
// `!= 0` reads "has a reason". The same numbers mean the same thing across the
// status gauges: 1 is healthy and 3 abnormal.
//
// codeUnmapped is written for an enum value missing from its table. The tests
// check every table covers its enum, so it marks a hostping bug.
const codeUnmapped int64 = -1

var pathStatusCodes = map[model.PathStatus]int64{
	model.PathNormal:    1,
	model.PathUncertain: 2,
	model.PathAbnormal:  3,
}

var uncertainReasonCodes = map[model.PathUncertainReason]int64{
	model.UncertainNone:          0,
	model.UncertainNoPath:        1,
	model.UncertainRNICBusy:      2,
	model.UncertainNonAffinitive: 3,
	model.UncertainBelowCeiling:  4,
	model.UncertainNoCeiling:     5,
	model.UncertainHostShift:     6,
	model.UncertainCPUBusy:       7,
}

var latencyStatusCodes = map[model.LatencyStatus]int64{
	model.LatencyNormal:   1,
	model.LatencyUnknown:  2,
	model.LatencyAbnormal: 3,
}

var latencyReasonCodes = map[model.LatencyReason]int64{
	model.LatencyReasonNone:     0,
	model.LatencyByBaseline:     1,
	model.LatencyBySiblings:     2,
	model.LatencyBusy:           3,
	model.LatencyFewSiblings:    4,
	model.LatencyNoPath:         5,
	model.LatencyNoReading:      6,
	model.LatencyRecheckCleared: 7,
	model.LatencyLocalityShift:  8,
	model.LatencyCPUBusy:        9,
}

var linkStatusCodes = map[model.LinkStatus]int64{
	model.LinkNormal:    1,
	model.LinkUncertain: 2,
	model.LinkAbnormal:  3,
	model.LinkGray:      4,
}

func lookupCode[K comparable](table map[K]int64, k K) int64 {
	if c, ok := table[k]; ok {
		return c
	}
	return codeUnmapped
}

func pathStatusCode(s model.PathStatus) int64 { return lookupCode(pathStatusCodes, s) }

func uncertainReasonCode(r model.PathUncertainReason) int64 {
	return lookupCode(uncertainReasonCodes, r)
}

func latencyStatusCode(s model.LatencyStatus) int64 { return lookupCode(latencyStatusCodes, s) }

func latencyReasonCode(r model.LatencyReason) int64 { return lookupCode(latencyReasonCodes, r) }

func linkStatusCode(s model.LinkStatus) int64 { return lookupCode(linkStatusCodes, s) }

// statusCodeRow is one hostping.status_code.info series: code of metric means
// status (on a status gauge) or reason (on a reason gauge).
type statusCodeRow struct {
	metric string
	code   int64
	status string
	reason string
}

func (r statusCodeRow) attrs() attribute.Set {
	kv := []attribute.KeyValue{
		attribute.String("metric", r.metric),
		attribute.String("code", strconv.FormatInt(r.code, 10)),
	}
	if r.status != "" {
		kv = append(kv, attribute.String("status", r.status))
	}
	if r.reason != "" {
		kv = append(kv, attribute.String("reason", r.reason))
	}
	return attribute.NewSet(kv...)
}

func statusRows[K interface {
	comparable
	String() string
}](metric string, table map[K]int64) []statusCodeRow {
	rows := make([]statusCodeRow, 0, len(table))
	for k, c := range table {
		rows = append(rows, statusCodeRow{metric: metric, code: c, status: k.String()})
	}
	return rows
}

func reasonRows[K interface {
	comparable
	String() string
}](metric string, table map[K]int64) []statusCodeRow {
	rows := make([]statusCodeRow, 0, len(table))
	for k, c := range table {
		rows = append(rows, statusCodeRow{metric: metric, code: c, reason: k.String()})
	}
	return rows
}

// statusCodeRows is every (metric, code) pair hostping.status_code.info
// names. metric is the OpenTelemetry instrument name, which an attribute value
// keeps on every backend (Prometheus renders the series name with
// underscores, not the label).
func statusCodeRows() []statusCodeRow {
	var rows []statusCodeRow
	rows = append(rows, statusRows("hostping.path.status_code", pathStatusCodes)...)
	rows = append(rows, reasonRows("hostping.path.uncertain_reason_code", uncertainReasonCodes)...)
	rows = append(rows, statusRows("hostping.path.latency_status_code", latencyStatusCodes)...)
	rows = append(rows, reasonRows("hostping.path.latency_reason_code", latencyReasonCodes)...)
	rows = append(rows, statusRows("hostping.link.status_code", linkStatusCodes)...)
	return rows
}

// registerStatusCodeInfo exports hostping.status_code.info: a constant 1 per
// code of every code gauge, naming it. It is fixed by the tables above and
// observed on every collection, so it is on /metrics from the first scrape.
func (t *Telemetry) registerStatusCodeInfo() error {
	rows := statusCodeRows()
	sets := make([]attribute.Set, len(rows))
	for i, r := range rows {
		sets[i] = r.attrs()
	}
	return t.constGauge("hostping.status_code.info", "Always 1; names the status or reason that code means on the code gauge named by metric", sets)
}
