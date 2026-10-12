# Metrics and reports

hostping writes each diagnosis as a JSON report and exports its measurements
as metrics over Prometheus and OTLP.

## Examples

```bash
# Daemon: Prometheus /metrics on :9464, optional OTLP
export OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317
./bin/hostping --sim --metrics :9464

# One-shot runs (hostpingctl diagnose, hostping --once) export nothing unless
# --metrics / --otlp (or OTEL_EXPORTER_OTLP_ENDPOINT) is given.
./bin/hostping --sim --once --metrics :9464

# Reports as one JSON line each, to stdout and/or appended to a file
./bin/hostpingctl diagnose --sim --report-format=json | jq .causes
./bin/hostping --sim --report-file reports.jsonl --report-format=none

# Daemon: keep flapping streaks and the idle cadence across a restart (Linux)
./bin/hostping --sim --state-dir /var/lib/hostping
```

## Reports

Each confirmed report goes to stdout as JSON. Diagnoses and other log lines go
to stderr.

`--report-format` picks the stdout form:

- `pretty`: indented. The default for one-shot runs.
- `json`: one compact object per line. The daemon's default, so journald and
  log shippers see one entry per report.
- `none`

The default depends on the run kind. It does not depend on whether stdout is a
terminal.

`--report-file PATH` additionally appends every report to `PATH` as one JSON
line. The file is created 0640 and reopened per report, so logrotate needs no
signal.

Every report carries `reported_at` (UTC), `version` and `commit` next to its
own fields. A discarded idle-mode run reaches neither output. See
[Logs and reports under journald](operations.md#logs-and-reports-under-journald).

## Metrics

Each confirmed report records the report metrics. Each monitor poll records the
`hostping.rnic.*`, `hostping.cpu.busy` and `hostping.monitor.*` ones. Every
instrument is a gauge (last value) except the `counter` rows.

The liveness metrics (`hostping.engine.last_*`,
`hostping.engine.run.failed` / `.discarded`, `hostping.monitor.*`) tell a stuck
agent from one with nothing to report. See
[operations.md](operations.md#liveness-and-staleness-alerts).

| Name | Type (unit) | Attributes | Meaning |
| --- | --- | --- | --- |
| `hostping.loopback.latency` | gauge (µs) | `rnic`, `endpoint` | Small-message loopback latency, p50 of the last run |
| `hostping.loopback.bandwidth` | gauge (bit/s) | `rnic`, `endpoint` | Bandwidth from Eq. 1 |
| `hostping.path.abnormal` | gauge (0/1) | `rnic`, `endpoint` | 1 when the path is abnormal |
| `hostping.path.status_code` | gauge (code) | `rnic`, `endpoint` | The path's current status: 1 `normal`, 2 `uncertain`, 3 `abnormal` |
| `hostping.path.uncertain_reason_code` | gauge (code) | `rnic`, `endpoint` | Why an uncertain path is uncertain: 1 `no_path`, 2 `rnic_busy`, 3 `non_affinitive`, 4 `below_ceiling`, 5 `no_ceiling`, 6 `host_shift`, 7 `cpu_busy`; 0 `none` on a normal or abnormal path |
| `hostping.path.latency_status_code` | gauge (code) | `rnic`, `endpoint` | The path's advisory small-message latency verdict: 1 `normal`, 2 `unknown`, 3 `abnormal` |
| `hostping.path.latency_reason_code` | gauge (code) | `rnic`, `endpoint` | What the latency verdict was judged against (1 `user_baseline`, 2 `siblings`; 7 `recheck_cleared` when the paired recheck cleared a sibling verdict, 8 `locality_shift` when the other RNICs on its NUMA node read as high) or why it was not judged (3 `busy`, 4 `few_siblings`, 5 `no_path`, 6 `no_reading`, 9 `cpu_busy`); 0 `none` on a path never classified |
| `hostping.link.abnormal_count` | gauge | `link` | Algorithm 1 `abnormal_cnt` |
| `hostping.link.status_code` | gauge (code) | `link` | The link's current Algorithm 1 status: 1 `normal`, 2 `uncertain`, 3 `abnormal`, 4 `gray` |
| `hostping.bus.utilization` | gauge (0–1) | `link` | Intra-host bus utilization of the hop, sampled before the run; no sample when unknown (see [Bus utilization](design.md#bus-utilization)) |
| `hostping.mutual.rtt` | gauge (µs) | `src`, `dst` | RNIC↔RNIC round trip, p50 of the last run's burst, or over the probe window when `--mutual-interval` is set (the JSON `mutual` entry marks a cross-rail pair with `cross_rail`) |
| `hostping.mutual.loss_ratio` | gauge | `src`, `dst` | Simulated drop loss; on the live probe, whose losses are timeouts, 0, or 1 on a pair that cannot be reached |
| `hostping.mutual.timeout_ratio` | gauge (0–1) | `src`, `dst` | Probes with no reply within `--mutual-timeout`, `1 − recv/sent` (over `--mutual-window` when the schedule is on); 1 on a pair that cannot be reached, absent when no probe count was reported |
| `hostping.rnic.throughput` | gauge (bit/s) | `rnic` | Peak(rx,tx) bps |
| `hostping.cpu.busy` | gauge (0–1) | `numa`, `socket` | CPU busy fraction of the NUMA node over the poll interval, from `/proc/stat`; no sample when unread (see [CPU load](design.md#cpu-load-on-an-idle-host)) |
| `hostping.rnic.pause_duration_ratio` | gauge (ratio) | `rnic` | Tx pause seconds per second; `--pfc-high` (default 0.03 = 30 ms/s) compares against it |
| `hostping.rnic.drop.delta` | gauge | `rnic` | Packet drop counter delta over the last poll |
| `hostping.verb.duration` | gauge (µs) | `rnic`, `endpoint`, `endpoint_kind`, `verb` | Execution time of the loopback probe's setup verbs (`gpu_alloc` on GPU endpoints only, `mr_reg`, `qp_setup`; `mem_warmup` on memory endpoints with `--mem-warmup`) in the last run. Not exported with `--metrics-verb-durations=false` |
| `hostping.endpoint.skipped` | gauge | `kind` | Endpoints (`gpu`/`memory`) whose loopback probes were skipped because registration failed, the GPU's compute mode is not `Default`, or an earlier loopback on the same InfiniBand RNIC failed at the port in that run; 0 when none |
| `hostping.host.idle` | gauge (0/1) | — | 1 when the last run classified the host as idle |
| `hostping.engine.run.count` | counter | `trigger`, `mode` | Engine runs whose report was confirmed (held idle-mode runs count once confirmed) |
| `hostping.engine.last_run_timestamp` | gauge (s) | — | Unix time the last engine run ended, whatever its outcome; absent until the first run |
| `hostping.engine.last_success_timestamp` | gauge (s) | — | Unix time the last run whose report was confirmed and written ended; absent until then |
| `hostping.engine.run.failed` | counter | `stage` | Runs that failed: `engine` (the run returned an error) or `report` (a confirmed report could not be written to stdout or `--report-file`). 0 from start-up |
| `hostping.engine.run.discarded` | counter | `reason` | Idle-mode runs discarded because the clean poll after them found the host `host_busy` or `idle_unknown` (idle state unreadable). 0 from start-up |
| `hostping.monitor.poll.errors` | counter | — | Monitor polls that returned an error (counters or GPU state unreadable). 0 from start-up |
| `hostping.monitor.last_poll_timestamp` | gauge (s) | — | Unix time of the last monitor poll that completed without error |
| `hostping.monitor.host_idle` | gauge (0/1) | — | 1 when the last clean poll found the host idle, 0 when busy or unclassified. Unlike `hostping.host.idle`, follows every poll rather than the last report |
| `hostping.diagnosis.count` | counter | `code`, `confidence` | Root-cause events |
| `hostping.build.info` | gauge | `version`, `commit`, `zig`, `go` | Always 1; names the running build. Exported from start-up, before any run |
| `hostping.rnic.info` | gauge | `rnic`, `device`, `pcie_bdf`, `netdev`, `link_layer`, `fabric`, `socket`, `numa` | Always 1, one series per RNIC; maps the `rnic` ID the other series carry to the IB device name (`mlx5_N`), PCI address, netdev, port link layer (`Ethernet`/`InfiniBand`), mutual-probe pairing key (`fabric`, e.g. `v4:10.0.0.0`) and NUMA placement. An identifier discovery could not read is `unknown`. Exported from start-up; RNICs dropped by `--exclude-rnic` have none |
| `hostping.endpoint.info` | gauge | `endpoint`, `kind`, `index`, `socket`, `numa`, `pcie_bdf` (GPU only), `root_port` (GPU only) | Always 1, one series per loopback endpoint; maps `gpuN`/`memN` to the GPU's PCI address and CPU root port, or to the memory node. Exported from start-up |
| `hostping.status_code.info` | gauge | `metric`, `code`, `status` or `reason` | Always 1, one series per code of the `*_code` gauges above (28 in all); names what `code` means on the gauge `metric` (the OpenTelemetry name, e.g. `hostping.path.status_code`). Status gauges' rows carry `status`, reason gauges' rows `reason`. Exported from start-up |

Loopback latency, mutual RTT and verb durations are gauges. The probe produces
one p50 per path (or RNIC pair) per run and calls each setup verb once. A
histogram would only bucket summary values, and its count would be a run
count.

Link and path status are code gauges. Each path or link has one series whose
value is the code of its current state, rewritten on every report. A link or
path that changes state changes that value and leaves no stale series behind.
The codes are fixed: a new state gets a new code, and no code is renumbered or
reused. Status codes start at 1, and reason codes use 0 for `none`.
[Status codes](operations.md#status-codes) has the full table, Grafana value
mappings and how to join `hostping.status_code.info` in PromQL.

`hostping.path.abnormal` keeps its meaning of 1 only when the path is
abnormal. It cannot express uncertain, which reads there as the same 0 as
normal.

`hostping.endpoint.skipped` follows the same rule as the status gauges. Every
kind is written on every report, so a recovered kind falls back to 0. The
failure reason is not an attribute. It is free-form probe text, so it would be
unbounded in cardinality and would strand a series behind every reason that
gets fixed. The reason is in the log line and the `skipped_endpoints` report
field instead.

The `hostping.mutual.*` gauges are absent altogether on a host where no RNIC
pair qualifies for the mutual probe. See [Diagnoses](design.md#diagnoses).

Per host the series count is roughly 10 per RNIC→endpoint path plus 2 per
link. See [Series cardinality](operations.md#series-cardinality). For what
each `hostping.diagnosis.count` code means and which ones to page on, see the
[runbook](runbook.md).

## Endpoints

`--metrics` (default `:9464`) serves `/metrics` and `/healthz`. `/healthz`
returns 503 when the daemon's poll loop has stalled (see
[Liveness and staleness alerts](operations.md#liveness-and-staleness-alerts)).

The metrics endpoint is plain, unauthenticated HTTP unless
`--metrics-web-config` names a Prometheus exporter-toolkit web config (TLS,
basic auth). In production, bind it to a management address. An address that
cannot be bound fails the start.

`--metrics-verb-durations=false` stops exporting `hostping.verb.duration` to
both `/metrics` and OTLP. It is 3 of the 10 series of a GPU path and 2 of the 9
of a memory path, so it is the largest per-path share on a host with many
RNICs (see [Series cardinality](operations.md#series-cardinality)). The probes
still time the verbs: the report's `verb_ns` and the `gpu_slow_mr_reg`
diagnosis are unaffected.

OTLP is enabled by `--otlp` or `OTEL_EXPORTER_OTLP_ENDPOINT`. The transport
security depends on the endpoint:

- A non-loopback `host:port` uses TLS with the system roots.
- A loopback `host:port` uses plaintext.
- An `http://` or `https://` URL follows its scheme.

For a `host:port`, `--otlp-insecure` or the standard
`OTEL_EXPORTER_OTLP_INSECURE` overrides that choice. A plaintext collector on
another host therefore needs one of them. Settings that contradict each other
fail the start. `OTEL_EXPORTER_OTLP_CERTIFICATE` / `_HEADERS` are honoured.
See [operations.md](operations.md#metrics-endpoint).
