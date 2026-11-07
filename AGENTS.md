# AGENTS.md

Guide for coding agents (and humans) working in this repository. The
user-facing documentation is [README.md](README.md) and [docs/](docs/); this
file covers how the code is put together, how to build and check it, and the
rules for changing it.

## 1. Overview and architecture

hostping diagnoses intra-host network bottlenecks on RDMA servers. It is an
independent implementation of the NSDI'23 Hostping system and its ToN'24
extension (mutual probe). An agent per host measures RNIC→endpoint paths
(NUMA memory, GPUs) with RDMA loopback and localizes a degraded path to the
hop that explains it.

```
monitor (1 s poll: sysfs counters, ethtool, nvidia-smi, /proc/stat, uncore PMUs)
   │  triggers: idle, PFC/drops, periodic (--busy-interval), mutual-probe alarm
   ▼
engine (probe sweep, rechecks) ──► Prober: zigprobe (live, Zig C ABI) or sim (Fig. 2)
   ▼
analyzer (classification, Algorithm 1, root causes) ──► model.Report
   ▼
agent: JSON report (stdout / --report-file) and telemetry (OTel: Prometheus /metrics, OTLP)
```

- `cmd/hostping` is the daemon; `cmd/hostpingctl` is the operator CLI
  (`diagnose`, `topology`, `calibrate`, `version`). Both are wired by
  `internal/agent`.
- The Go side owns orchestration and diagnosis. The Zig library
  (`zig/`, `libhostping_probe.so`) owns the RDMA verbs (loopback and mutual
  probes) and the `--sim` Eq. 1 model, behind the C ABI in
  `include/hostping.h`, which `internal/zigprobe` calls through cgo.
- `--sim` runs the whole pipeline on the simulated NSDI'23 Fig. 2 host with
  injectable faults (`--fault`), without an RNIC, GPU or privileges.
- [docs/design.md](docs/design.md) is the reference for what each path does
  on live hardware and where it fails closed. Read it before changing
  measurement, classification or diagnosis behaviour.

## 2. Directory layout

| Path | Contents |
| --- | --- |
| `cmd/hostping`, `cmd/hostpingctl` | Entry points |
| `internal/agent` | Daemon loop, one-shot runs, calibrate, idle gate, report output (cgo through zigprobe) |
| `internal/monitor` | Counter polling, triggers, external samplers (`nvidia-smi`, `ethtool`), uncore PMUs |
| `internal/engine` | Probe scheduling, rechecks, run requests |
| `internal/analyzer` | Path and link verdicts, Algorithm 1, root causes |
| `internal/baseline`, `internal/calibrate` | Distance-class baselines; `--baseline` / `--baseline-file` parsing; calibration |
| `internal/topology`, `internal/pci`, `internal/sysfs` | Discovery from sysfs, PCI config space, the sysfs readers |
| `internal/mutual` | Scheduled UD mutual probe (`--mutual-interval`) |
| `internal/telemetry` | OTel instruments, Prometheus and OTLP exporters, `/healthz` |
| `internal/model` | Shared domain types, report JSON |
| `internal/config` | Flags of both binaries |
| `internal/state` | `--state-dir` persistence |
| `internal/sim` | Simulated host and faults |
| `internal/zigprobe` | cgo bindings to the Zig probe library |
| `zig/` | Probe library (`build.zig`, `src/*.zig`, Zig unit tests in `src/root_test.zig`) |
| `include/hostping.h` | Public C ABI between Go and Zig |
| `contrib/` | systemd unit, Prometheus alerting rules and their promtool tests |
| `packaging/nfpm/` | .deb / .rpm definition |
| `docs/` | Design, metrics, operations, runbook, building, releasing |

## 3. Build

Toolchain: Go 1.27.3 and Zig 0.17.0 (`mise.toml`, `go.mod`, the Dockerfile and
the workflows pin them; bump them together). Linux builds also need
`libibverbs-dev`.

```bash
make zig        # zig/zig-out/lib/libhostping_probe.so (needed by every cgo build)
make bin        # bin/hostping, bin/hostpingctl and the .so next to them
make docker-test / docker-bin / docker-bin-amd64 / docker-dist   # in the dev container
```

[docs/building.md](docs/building.md) and [docs/releasing.md](docs/releasing.md)
cover the container, cross-compilation, the glibc baseline and release
artifacts.

## 4. Test

| Command | What | Needs |
| --- | --- | --- |
| `make test` | `zig build test`, then `go test ./...` | Zig, libibverbs-dev (no RNIC) |
| `make test-race` | The same with `-race` | Same |
| `make test-fuzz` | Every Go fuzz target for `FUZZTIME` (default 30s; `FUZZFLAGS=-parallel=2` on a shared machine) | Go only: the targets are in pure-Go packages |
| `make bench` | Benchmarks (`-count=$(BENCH_COUNT)`) into `bench.txt`, for `benchstat` | Zig, libibverbs-dev |
| `make docker-test` | `make test` in the dev container | Docker |
| `hostpingctl diagnose --sim [--fault ...]` | End-to-end on the simulated host | A build |

- No test needs an RNIC, a GPU or root. Live probe paths are covered by
  fail-closed tests and the C ABI checks in `internal/zigprobe`; diagnosis
  behaviour by `--sim` tests, including the golden reports in
  `internal/analyzer/testdata/golden`.
- `go test ./internal/<pkg>` works without the Zig library for every package
  except `internal/agent`, `internal/zigprobe` and `cmd/...`.
- Fuzz targets (`Fuzz*` in `*_test.go`) cover the parsers of external input:
  sysfs CPU lists, PCI config space, link speeds and addresses, `ethtool -S`
  and `nvidia-smi` output, PMU format strings, the `--baseline` and
  `--baseline-file` values, capacities, and the label values exported from
  sysfs names. A failing input lands in `testdata/fuzz/FuzzXxx/`; commit it
  as a regression seed with the fix.
- `goleak` checks for leaked goroutines in `agent`, `monitor`, `mutual` and
  `telemetry` (`main_test.go`).
- Golden files: `go test ./internal/analyzer -run TestGolden -update`,
  `go test ./internal/telemetry -run Instruments -update`, and the `-update`
  flags of `internal/config` and `internal/model`. Regenerate them only for
  an intentional change and review the diff.
- Real RDMA hardware is never exercised by CI. Changes to `zig/src/verbs_linux.zig`
  or the live probe paths need a run on a host (see README "Usage").
- Timing-sensitive tests (the idle gate's process-group tests) can flake on a
  heavily loaded machine; re-run them in isolation before concluding.

## 5. Static analysis

```bash
make lint          # golangci-lint (.golangci.yml, incl. gofmt and staticcheck), go vet, go mod tidy -diff, zig fmt --check
make fmt           # apply gofmt (golangci-lint fmt) and zig fmt
make lint-actions  # actionlint and zizmor on .github/ (offline without GH_TOKEN)
make vuln          # govulncheck ./...
make complexity    # cccc ranking (cccc.toml); CI tracks it with octocov, nothing gates on it
```

Tool versions are pinned in the Makefile and in `.github/workflows/`; bump
both together. `make lint` builds the Zig library first, because cgo links
`internal/zigprobe` and the commands even to type-check them.

## 6. Coding conventions

- Go: gofmt; errors wrap their sentinel with `%w` and format the cause with
  `%v` (`fmt.Errorf("%w: %s: %v", mutual.ErrUnavailable, id, err)`), so
  `errors.Is` sees the classification and not the cause's own sentinels.
  The `errorlint` errorf check is off for that reason; keep the pattern.
- Logging: zerolog through `internal/logging`; the engine and agent log
  through injectable `logf` functions, and a state that persists is logged
  once per change, not per poll.
- "Unknown is not zero": a reading that could not be taken is absent (no
  series, `unknown` label, a skip reason in the report), never 0 or idle.
  Live paths fail closed with a reason rather than fall back to the Fig. 2
  topology or a stand-in measurement.
- External commands: fixed argument vectors, no shell, a timeout and a
  `WaitDelay` on every one (`internal/monitor/extcmd.go`,
  `internal/agent/idlegate.go`).
- Comments say why; match the existing density. English only.
- Zig: `zig fmt`; pure logic goes in files `zig build test` covers without
  ibverbs (see `src/root_test.zig`).
- Commits: short scoped subjects (`fix:`, `feat(telemetry):`,
  `refactor(analyzer):`, `test(monitor):`, `ci:`, `docs:`,
  `chore(release):`), with the reason in the body. User-visible changes go
  under `[Unreleased]` in [CHANGELOG.md](CHANGELOG.md).

## 7. Compatibility contracts

These must not change silently. hostping is pre-1.0, so they may change
between minor versions, but every change is called out in the CHANGELOG.

| Contract | Defined in | Pinned by |
| --- | --- | --- |
| Metric names, units, labels, status codes | `internal/telemetry`, [docs/metrics.md](docs/metrics.md) | `internal/telemetry/testdata/*.golden`, `TestInstrumentsMatchMetricsDoc` |
| Report JSON (fields, cause codes, evidence strings) | `internal/model` | `internal/model/testdata/*.golden`, `internal/analyzer/testdata/golden/` |
| CLI flags of both binaries | `internal/config/config.go` | `internal/config/testdata/flags*.golden` |
| Baseline file format | `internal/baseline/parse.go` | `internal/baseline` tests |
| `--state-dir` file | `internal/state` | `internal/state` tests |
| C ABI between Go and Zig (structs, error codes, error-name prefixes) | `include/hostping.h`, `zig/src/abi.zig`, `zig/src/capi.zig` | compile-time checks in `abi.zig`, `internal/zigprobe/*abi_test.go` |
| UD mutual-probe tag (immediate data) | `zig/src/udtag.zig`, `internal/mutual` (`seqMask`) | Zig tests, `internal/mutual` tests |
| Alerting rules | `contrib/prometheus/hostping-rules.yml` | `promtool test rules contrib/prometheus/hostping-rules.test.yml` |
| Packaging layout, systemd unit | `packaging/nfpm/`, `contrib/systemd/` | release workflow (`verify` job) |

Exported label values from sysfs (device and netdev names) are made valid
UTF-8 in `internal/telemetry` (`orUnknown`); keep any new label built from
discovered strings going through it.

## 8. Concurrency and resource management

- The daemon is one poll loop (`internal/agent`, ticking every
  `--metric-interval`) that samples the monitor and runs the engine
  serially; probes run one at a time. The scheduled mutual probe
  (`internal/mutual.Loop`) runs in its own goroutine and is the only user of
  its UD endpoints. The metrics server runs in its own goroutine.
- Every goroutine stops on context cancellation (SIGINT/SIGTERM) and is
  waited for; goleak enforces it in tests.
- The C ABI has no cancellation hook: a probe is bounded up front by its
  timeout, clamped to the context deadline (`effectiveTimeoutMS`).
- External samplers are bounded (2 s, 5 s for `nvidia-smi dmon`) and
  rate-limited or backed off; the idle gate kills its process group.
- A run ends by releasing the GPU contexts its probes created; an idle daemon
  holds none.
- Telemetry instruments are recorded at run and poll time. A scrape computes
  nothing: the info gauges observe attribute sets built at start-up.

## 9. Performance constraints

- Probes cost running workloads bandwidth and GPU memory: the probe sets,
  `--busy-samples`, rechecks and their caps are deliberate
  ([operations.md, Cost to running workloads](docs/operations.md#cost-to-running-workloads)).
- The 1 s poll path (sysfs counters, `ethtool -S`) must stay cheap and fork
  rarely; `nvidia-smi` is rate-limited to `--gpu-util-interval`.
- Series cardinality is bounded by the topology
  ([operations.md, Series cardinality](docs/operations.md#series-cardinality)).
  Do not add labels that grow with time or traffic.
- `make bench` covers a simulated run, report recording and the poll-path
  parsers; compare before and after with `benchstat`.

## 10. Before opening a PR

1. `make check` (lint, lint-actions, test-race). Run it as
   `env -u GH_TOKEN make check` where the GitHub API is unreachable.
2. `make test-fuzz FUZZTIME=30s` when touching a parser or a fuzz target.
3. Golden files regenerated and reviewed when a contract changes on purpose,
   with docs ([docs/metrics.md](docs/metrics.md), [docs/design.md](docs/design.md),
   [docs/operations.md](docs/operations.md), [docs/runbook.md](docs/runbook.md))
   and the CHANGELOG updated.
4. Zig changes: `make test` runs `zig build test`; live verbs changes also
   need a run on RDMA hardware, stated in the PR.
5. Workflow changes: `make lint-actions` clean (no zizmor findings).
6. Packaging, the glibc baseline or the Dockerfile: `make docker-dist
   VERSION=X.Y.Z` (see [docs/releasing.md](docs/releasing.md)).

## 11. Rules for AI agents

Forbidden:

- Disabling, skipping, deleting or weakening tests, golden files, fuzz
  properties or lint rules to get green; hiding failures (`|| true`,
  `continue-on-error`, blanket excludes). A `//nolint` needs the linter name
  and a reason.
- Editing generated or recorded outputs by hand (golden files: use
  `-update`; `zig-out/`).
- Adding dependencies without a justification in the PR (runtime ones need
  a strong one).
- Changing a contract from section 7 without calling it out.
- Loosening workflow permissions, unpinning actions, adding
  `pull_request_target`, or adding caches to the release workflow.
- Committing secrets, rewriting history, force-pushing, tagging or
  publishing releases.

## 12. Changes that need human review

Even with green CI: `.github/workflows/` and `packaging/`, `contrib/systemd/`
(privileges, hardening), anything that runs as root or forks (external
commands, the idle gate), parsers of sysfs, PCI config space and tool
output, the C ABI and the UD mutual-probe wire format, the Zig verbs code,
public contracts (section 7), dependency additions and large refactors.
