# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
project uses [Semantic Versioning](https://semver.org/). A release's section
becomes the notes of its GitHub Release (see
[docs/releasing.md](docs/releasing.md)).

## [Unreleased]

### Changed

- `--idle-gate-command` now tells a denial from a gate that could not decide.
  Exit 1 still skips the idle run as `reason="denied"`. Any other non-zero
  exit (2 or higher, including a shell's 126/127) also skips it but counts as
  `reason="error"`, so `HostpingIdleGateFailing` fires. Before, every non-zero
  exit counted as `denied`. A gate whose scheduler query failed fast, such as
  the Slurm example with slurmctld unreachable, then read as a host held by a
  job: no alert fired, `HostpingNoSuccessfulRun` was suppressed, and idle
  diagnosis stopped silently. The Slurm example in
  [Keeping idle runs out of scheduler jobs](docs/operations.md#keeping-idle-runs-out-of-scheduler-jobs)
  now exits 2 when `hostname` or `squeue` fails. A gate that denies with a
  status other than 1 must switch to exit 1, or it alerts.

### Fixed

- An `--idle-gate-command` that exits in time but leaves a background child
  holding its stderr open no longer counts as `reason="timeout"` when
  `--idle-gate-timeout` passes while the daemon waits up to 1s for that
  stderr to close. Its own exit status decides: 1 is `denied` and any other
  non-zero status is `error`. Before, such a gate that denied or failed after
  running for more than `--idle-gate-timeout` minus 1s (4s with the default)
  read as a hung gate. A gate the timeout killed still counts as `timeout`.

## [0.1.6] - 2026-10-19

### Added

- `--idle-gate-command PATH` and `--idle-gate-timeout` (default `5s`, at most
  `20s`): the daemon runs `PATH` right before each idle-trigger run and skips
  the run unless it exits 0, so a site can keep the idle full mesh out of a
  scheduler job's allocation. The utilization-based idle verdict cannot tell a
  job's quiet stretch from a free host. A non-zero exit, a timeout or an exec
  failure skips the attempt until the next `--idle-interval`. Busy-mode and
  mutual-alarm runs, `hostpingctl diagnose` and `calibrate` are not gated.
  Skips count in `hostping_engine_idle_gate_skipped_total{reason}` (`denied`,
  `timeout`, `error`). `HostpingNoSuccessfulRun` now leaves out gate-skipped
  hosts, and the new `HostpingIdleGateFailing` alert fires on `timeout` and
  `error`. See
  [Keeping idle runs out of scheduler jobs](docs/operations.md#keeping-idle-runs-out-of-scheduler-jobs).

## [0.1.5] - 2026-10-16

### Added

- `--latency-floor` (default `500ns`, at most `10us`): the least excess over
  the sibling median a path's small-message latency must show, on top of
  `--abnormal-th`, to be latency-abnormal against its siblings. The sweep, the
  paired recheck and the locality check all apply it. Raise it on a platform
  whose healthy paths move by more than 500 ns between runs, where
  `latency_status` otherwise flags a different healthy path every few runs.
- `hostping.path.latency_streak` (`hostping_path_latency_streak{rnic,endpoint}`):
  each path's `latency_streak`, the consecutive idle runs that read its latency
  `abnormal` against its siblings. `hostping_path_latency_status_code` is per
  run, so one healthy path reading high in a single run flips it; a dashboard
  or alert can require `hostping_path_latency_streak >= 3`, the persistence
  the daemon needs before it names a cause. One series per path: a Fig.2 sim
  host gains 40, from 571 to 611. See
  [Latency streak](docs/operations.md#latency-streak).

## [0.1.4] - 2026-10-13

### Added

- `hostping.topology.node.info` and `hostping.topology.edge.info`
  (`hostping_topology_node_info`, `hostping_topology_edge_info`): the host's
  intra-host topology as a graph, for Grafana's Node Graph panel. A node is a
  CPU package, a NUMA node under sub-NUMA clustering, a memory node, a root
  port, a PCIe switch, an RNIC or a GPU; an edge is one link, directed from
  the CPU side to the device side, whose `link` attribute joins
  `hostping_link_status_code` and `hostping_bus_utilization`. Cascaded
  switches hang off the switch above them, following the sysfs parent chain.
  Both are fixed at start-up and exported from the first scrape, with `--sim`
  and `--topology-file` too. A Fig.2 sim host gains 47 series (24 nodes, 23
  edges), from 524 to 571. See
  [Topology graph](docs/operations.md#topology-graph).
- Topology links carry `upstream`, the switch or root port a PCIe device or
  switch hop hangs off (`hostpingctl topology`, the topology file).

## [0.1.3] - 2026-10-13

### Changed

- The README is reduced to an overview, install and usage. The reference
  material it held moved to `docs/design.md` (measurement, classification,
  diagnoses, mutual probe, bus utilization, simulated faults),
  `docs/metrics.md` (metrics and reports) and `docs/building.md`. The
  tarball and packages ship `design.md`, `metrics.md` and `runbook.md` next to
  `operations.md`.

### Added

- `--metrics-verb-durations` (default `true`): `false` stops exporting
  `hostping.verb.duration` (`hostping_verb_duration_microseconds`), which is
  3 of the 10 series of a GPU path and 2 of the 9 of a memory path, the
  largest per-path share on a host with many RNICs. A Fig.2 sim host drops
  from 524 to 412 `hostping_*` series. The probes still time the verbs; the
  report's `verb_ns` and the `gpu_slow_mr_reg` diagnosis are unaffected.

## [0.1.2] - 2026-10-13

### Changed

- **Breaking:** the one-hot state gauges are replaced by code gauges, one
  series per path or link whose value is the code of its current state. A
  path exported 22 state series and a link 4; now 4 and 1. A Fig.2 sim host
  drops from 1285 to 524 `hostping_*` series, and a 10-RNIC host with 120
  paths and 67 links by about 2,300.

  | Removed (0.1.1) | Replacement (0.1.2) |
  | --- | --- |
  | `hostping.path.status{rnic,endpoint,status,reason}` (`hostping_path_status`) | `hostping.path.status_code{rnic,endpoint}` (1 normal, 2 uncertain, 3 abnormal) and `hostping.path.uncertain_reason_code{rnic,endpoint}` (0 none, 1 no_path, 2 rnic_busy, 3 non_affinitive, 4 below_ceiling, 5 no_ceiling, 6 host_shift, 7 cpu_busy) |
  | `hostping.path.latency_status{rnic,endpoint,status,reason}` (`hostping_path_latency_status`) | `hostping.path.latency_status_code{rnic,endpoint}` (1 normal, 2 unknown, 3 abnormal) and `hostping.path.latency_reason_code{rnic,endpoint}` (0 none, 1 user_baseline, 2 siblings, 3 busy, 4 few_siblings, 5 no_path, 6 no_reading, 7 recheck_cleared, 8 locality_shift, 9 cpu_busy) |
  | `hostping.link.status{link,status}` (`hostping_link_status`) | `hostping.link.status_code{link}` (1 normal, 2 uncertain, 3 abnormal, 4 gray) |

  There is no option to keep the old series. To migrate, rewrite
  `hostping_path_status{status="abnormal"} == 1` as
  `hostping_path_status_code == 3`, and a `status`/`reason` selector as an
  `==` on the matching code gauge (combine a status and a reason with `and`).
  A `status` or `reason` label for legends and alerts comes from joining
  `hostping_status_code_info` (below) with `count_values`.
  docs/operations.md, "Status codes", has the code table, the full migration
  table, PromQL and Grafana value mapping examples. The codes are fixed: a new
  state gets a new code and none is renumbered. The report JSON and
  `hostpingctl` output are unchanged.

### Added

- `hostping.status_code.info{metric,code,status|reason}`
  (`hostping_status_code_info`): always 1, 28 series exported from start-up,
  naming every code of the code gauges.
- Recording rules `hostping:path_status:count`,
  `hostping:path_uncertain_reason:count` and `hostping:link_status:count` in
  `contrib/prometheus/hostping-rules.yml`: paths and links per state on each
  host, labelled by name.

## [0.1.1] - 2026-10-12

### Added

- `hostping.rnic.info` (`hostping_rnic_info`) and `hostping.endpoint.info`
  (`hostping_endpoint_info`): identity gauges, always 1 and exported from
  start-up, that map the `rnicN` and `gpuN`/`memN` IDs on every other series
  to the physical device. RNICs carry `device` (IB device name), `pcie_bdf`,
  `netdev`, `link_layer`, `fabric`, `socket` and `numa`; endpoints carry
  `kind`, `index`, `socket`, `numa` and, for GPUs, `pcie_bdf` and
  `root_port`. Dashboards and alerts can now key on the device with
  `* on (instance, rnic) group_left (device, pcie_bdf) hostping_rnic_info`.
  Existing series keep their labels. RNICs dropped by `--exclude-rnic` get no
  series.

### Fixed

- The series count formula in docs/operations.md: a path to a GPU exports 28
  series and one to memory 27, not 26 and 25.

## [0.1.0] - 2026-10-12

First release.

### Added

- `hostping` daemon and `hostpingctl` CLI: an independent implementation of
  Hostping (NSDI '23, ToN '24) that measures intra-host latency and bandwidth
  with RNIC loopback probes over host memory and, through GPUDirect RDMA, GPU
  memory, and RNIC-to-RNIC connectivity with mutual probes (same rail by
  default, across rails with `--mutual-scope=host`).
- Diagnoses from the papers (RNIC/GPU PCIe, memory channel, UPI and CPU root
  port failures, memory flapping, ACS, ATS, RNIC slow start, Tx window,
  overloaded UPI/memory), localized with Algorithm 1, plus idle and busy run
  modes triggered by PFC pauses and drops, and sub-NUMA clustering support.
- `--sim` mode that runs the NSDI Fig.2 topology and injected faults without
  RDMA hardware.
- OpenTelemetry metrics through a Prometheus `/metrics` endpoint (optional TLS
  and basic auth) and OTLP push, JSON reports, Prometheus alerting rules with
  unit tests, and a runbook.
- Release artifacts for linux-amd64: a tarball, a `.deb` and an `.rpm`, each
  with the systemd unit, built against glibc 2.28 and requiring
  `libibverbs` from rdma-core 32 or newer.

