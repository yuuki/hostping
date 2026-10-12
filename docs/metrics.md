# Metrics and reports

## Reports

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

# Daemon: keep flapping streaks and the idle cadence across restarts (Linux)
./bin/hostping --sim --state-dir /var/lib/hostping
```

Each confirmed report goes to stdout as JSON, and diagnoses and other log lines go to stderr. `--report-format` picks the stdout form: `pretty` (indented, the default for one-shot runs), `json` (one compact object per line, the daemon's default, so journald and log shippers see one entry per report) or `none`. The default depends on the run kind, never on whether stdout is a terminal. `--report-file PATH` additionally appends every report to `PATH` as one JSON line (created 0640, reopened per report so logrotate needs no signal). Every report carries `reported_at` (UTC), `version` and `commit` next to its own fields. A discarded idle-mode run reaches neither output. See [Logs and reports under journald](operations.md#logs-and-reports-under-journald).

## Metrics

Each engine run records the report metrics; each monitor poll records the
`hostping.rnic.*` ones. Every instrument is a gauge (last value) except the
`counter` rows. The liveness metrics (`hostping.engine.last_*`,
`hostping.engine.run.failed` / `.discarded`, `hostping.monitor.*`) tell a stuck
agent from one with nothing to report; see
[operations.md](operations.md#liveness-and-staleness-alerts).

| Name | Type (unit) | Attributes | Meaning |
| --- | --- | --- | --- |
| `hostping.loopback.latency` | gauge (µs) | `rnic`, `endpoint` | Small-message loopback latency, p50 of the last run |
| `hostping.loopback.bandwidth` | gauge (bit/s) | `rnic`, `endpoint` | Bandwidth from Eq. 1 |
| `hostping.path.abnormal` | gauge (0/1) | `rnic`, `endpoint` | 1 when the path is abnormal |
| `hostping.path.status` | gauge (0/1) | `rnic`, `endpoint`, `status`, `reason` | 1 on the path's current status (`uncertain`/`normal`/`abnormal`) and, when uncertain, why (`no_path`/`rnic_busy`/`non_affinitive`/`below_ceiling`/`no_ceiling`/`host_shift`/`cpu_busy`), 0 on the others |
| `hostping.path.latency_status` | gauge (0/1) | `rnic`, `endpoint`, `status`, `reason` | 1 on the path's advisory small-message latency verdict (`unknown`/`normal`/`abnormal`) and its reason (`user_baseline`/`siblings` when judged, `recheck_cleared` when the paired recheck cleared a sibling verdict, `locality_shift` when the other RNICs on its NUMA node read as high; `busy`/`few_siblings`/`no_path`/`no_reading`/`cpu_busy` when not), 0 on the others |
| `hostping.link.abnormal_count` | gauge | `link` | Algorithm 1 `abnormal_cnt` |
| `hostping.link.status` | gauge (0/1) | `link`, `status` | 1 on the link's current status (`uncertain`/`normal`/`abnormal`/`gray`), 0 on the others |
| `hostping.bus.utilization` | gauge (0–1) | `link` | Intra-host bus utilization of the hop, sampled before the run; no sample when unknown (see [Bus utilization](design.md#bus-utilization)) |
| `hostping.mutual.rtt` | gauge (µs) | `src`, `dst` | RNIC↔RNIC round trip, p50 of the last run's burst, or over the probe window when `--mutual-interval` is set (the JSON `mutual` entry marks a cross-rail pair with `cross_rail`) |
| `hostping.mutual.loss_ratio` | gauge | `src`, `dst` | Simulated drop loss; 0 on the live probe, whose losses are timeouts |
| `hostping.mutual.timeout_ratio` | gauge (0–1) | `src`, `dst` | Probes with no reply within `--mutual-timeout`, `1 − recv/sent` (over `--mutual-window` when the schedule is on); 1 on a pair that cannot be reached, absent when no probe count was reported |
| `hostping.rnic.throughput` | gauge (bit/s) | `rnic` | Peak(rx,tx) bps |
| `hostping.cpu.busy` | gauge (0–1) | `numa`, `socket` | CPU busy fraction of the NUMA node over the poll interval, from `/proc/stat`; no sample when unread (see [CPU load](design.md#cpu-load-on-an-idle-host)) |
| `hostping.rnic.pause_duration_ratio` | gauge (ratio) | `rnic` | Tx pause seconds per second; `--pfc-high` (default 0.03 = 30 ms/s) compares against it |
| `hostping.rnic.drop.delta` | gauge | `rnic` | Packet drop counter delta over the last poll |
| `hostping.verb.duration` | gauge (µs) | `rnic`, `endpoint`, `endpoint_kind`, `verb` | Execution time of the loopback probe's setup verbs (`gpu_alloc` on GPU endpoints only, `mr_reg`, `qp_setup`; `mem_warmup` on memory endpoints with `--mem-warmup`) in the last run |
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

Loopback latency, mutual RTT and verb durations are gauges, not histograms: the
probe produces one p50 per path (or RNIC pair) per run, and calls each setup
verb once, so a histogram would only bucket summary values and its count would
be a run count. Link and path status are
enum-style gauges rather than a `status` attribute on `abnormal_count`, so a
link or path that changes state leaves no stale series behind; that is also why
`hostping.path.status` writes every uncertain reason as its own series (8 per
path) and not just the one that currently applies, and `hostping.path.latency_status`
likewise writes 9 per path. `hostping.path.abnormal`
keeps its meaning — 1 only when the path is abnormal — but it cannot express
uncertain, which reads there as the same 0 as normal. `hostping.endpoint.skipped`
follows the same rule: every kind is written on every report so a recovered kind
falls back to 0, and the failure reason is deliberately *not* an attribute —
it is free-form probe text, so it would be unbounded in cardinality and would
strand a series behind every reason that gets fixed. The reason belongs to the
log line and the `skipped_endpoints` report field. The `hostping.mutual.*`
gauges are absent altogether on a host where no RNIC pair qualifies for the
mutual probe; see [Diagnoses](design.md#diagnoses).

Per host the series count is roughly 23 per RNIC→endpoint path plus 5 per
link; see [Series cardinality](operations.md#series-cardinality). For
what each `hostping.diagnosis.count` code means and which ones to page on, see
the [runbook](runbook.md).

## Endpoints

`--metrics` (default `:9464`) serves `/metrics` and `/healthz`, which is 503
when the daemon's poll loop has stalled (see
[operations.md](operations.md#metrics-endpoint)). It is plain,
unauthenticated HTTP unless `--metrics-web-config` names a Prometheus
exporter-toolkit web config (TLS, basic auth); bind it to a management address
in production. An address that cannot be bound fails the start. OTLP
(`--otlp` or `OTEL_EXPORTER_OTLP_ENDPOINT`) is TLS with the system roots for a
non-loopback `host:port`, plaintext for loopback, and follows the scheme of an
`http://` / `https://` URL; `--otlp-insecure` or the standard
`OTEL_EXPORTER_OTLP_INSECURE` overrides it, and `OTEL_EXPORTER_OTLP_CERTIFICATE`
/ `_HEADERS` are honoured. A plaintext collector on another host, which earlier
releases reached without any setting, now needs `--otlp-insecure`. See
[operations.md](operations.md#metrics-endpoint).
