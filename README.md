# hostping

[![CI](https://github.com/yuuki/hostping/actions/workflows/ci.yml/badge.svg)](https://github.com/yuuki/hostping/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/yuuki/hostping)](https://github.com/yuuki/hostping/releases/latest)
[![License](https://img.shields.io/github/license/yuuki/hostping)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/yuuki/hostping)](go.mod)
[![Go Report Card](https://goreportcard.com/badge/github.com/yuuki/hostping)](https://goreportcard.com/report/github.com/yuuki/hostping)

hostping diagnoses intra-host network bottlenecks on RDMA servers. It is an
independent implementation of the Hostping system described in:

- Kefei Liu et al., *Hostping: Diagnosing Intra-host Network Bottlenecks in RDMA Servers*, USENIX NSDI 2023
- Kefei Liu et al., *Diagnosing End-Host Network Bottlenecks in RDMA Servers*, IEEE/ACM Transactions on Networking 32(5), 2024

hostping is not affiliated with the authors of these papers. See
[PATENTS.md](PATENTS.md) for related patent information.

## Overview

An agent runs on each host. It uses RDMA loopback to measure the latency and
bandwidth of every path between an RNIC and an endpoint, which is either a
NUMA node's memory or a GPU. When a path degrades, the agent localizes it to
the hop that explains it: the RNIC or GPU PCIe link, a CPU root port, a memory
channel or the inter-socket bus. A separate mutual probe checks RNIC-to-RNIC
reachability. Results are exported as OpenTelemetry metrics (Prometheus
scrape, optionally OTLP) and as JSON reports.

The loopback probe issues RDMA WRITEs through the RNIC from host DRAM back to
host DRAM, and from a GPU back to the same GPU over GPUDirect RDMA. The agent
runs a full mesh of probes when the host is idle and a reduced set when PFC
pauses or drops rise. It can also run periodically under load.

Paths measured on an idle host are judged against distance-class baselines.
These come from the paper's Fig. 4 scaled to the hardware, or from a per-host
calibration. Paths measured under load are compared with their siblings.
Suspect verdicts are re-read before they are reported. The paper's
Algorithm 1 then turns path verdicts into link verdicts and diagnosis codes.

In the mutual probe, RNIC pairs on the same rail exchange probes, or every
pair does with `--mutual-scope=host`. Timeouts identify unreachable or
flapping RNICs and ToR switch anomalies (ToN'24).

The agent and analyzer are written in Go. The probes and sysfs counters are
written in Zig and called through a C ABI.

```
hardware monitor ──► Hostping engine ──► data analyzer ──► OTel / JSON
      │                    │
      │              Zig loopback (DRAM → RNIC → DRAM; GPU → RNIC → GPU via GDR)
      │              Zig mutual probe (RNIC ↔ RNIC, same rail or --mutual-scope=host)
      └── RNIC pause/drops/throughput, GPU util, RNIC/GPU PCIe and memory bus util when readable
```

## Status

hostping is pre-1.0. Flags, metric names and report fields may change between
minor versions; [CHANGELOG.md](CHANGELOG.md) records notable changes.

Known limitations:

- The UD mutual-probe transport (`--mutual-transport=ud`) and the scheduled
  mutual probe (`--mutual-interval`) have not been exercised on RDMA hardware.
  The default is the RC transport.
- RNIC slow start and Tx window misconfiguration are diagnosed under `--sim`
  only. On a live host they cannot be told apart from an RNIC PCIe failure.
- Memory and inter-socket bus utilization are read only on Intel processors
  with uncore PMUs, and only when the operator supplies the bus capacity.
- InfiniBand RNICs take part in the mutual probe only with
  `--mutual-infiniband`.

[docs/design.md](docs/design.md) describes what each path does on live
hardware and where it fails closed.

## Requirements

| | |
| --- | --- |
| OS | Linux x86_64, glibc 2.28 or newer |
| RDMA | `libibverbs.so.1` from rdma-core 32 or newer; RoCEv2 or InfiniBand RNICs |
| GPU endpoints (optional) | NVIDIA or AMD GPU with GPUDirect RDMA (dma-buf, or `nvidia-peermem`); CUDA/HIP libraries are loaded with `dlopen` when present |
| Bus utilization (optional) | `CAP_PERFMON` or `kernel.perf_event_paranoid <= 0` |

The privileges and devices the daemon needs are listed in
[Privileges and devices](docs/operations.md#privileges-and-devices).
`--sim` needs no RNIC, GPU or privileges, but the Linux binaries still load
`libibverbs.so.1`. A source build can also run `--sim` on macOS.

## Install

Each release on GitHub carries `.deb` and `.rpm` packages, a tarball and
`SHA256SUMS`:

```bash
gh release download v0.1.5 --repo yuuki/hostping \
    --pattern 'hostping*' --pattern SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
sudo apt install ./hostping_*_amd64.deb      # or: sudo dnf install ./hostping-*.x86_64.rpm
```

The package installs `hostping` (the daemon), `hostpingctl` (the operator
CLI) and a systemd unit, and does not start the service. Read
[Install](docs/operations.md#install) before `systemctl enable --now hostping`.

To build from source, see [docs/building.md](docs/building.md).

## Usage

Run against the simulated host of NSDI'23 Fig. 2, with and without an
injected fault:

```bash
hostpingctl diagnose --sim
hostpingctl diagnose --sim --fault rnic_pcie:rnic0
```

On an RDMA host:

```bash
hostpingctl topology                 # discovered RNICs, endpoints and links
hostpingctl diagnose                 # one idle or busy run, report to stdout
hostping --metrics :9464             # daemon: /metrics and /healthz on :9464
```

A live one-shot run refuses to start when the host's idle state cannot be
read. To replace the default baselines with values measured on a healthy
host:

```bash
hostpingctl calibrate --out baselines.json
hostping --baseline-file baselines.json
```

`hostping -h` lists every flag. `hostpingctl` with no arguments lists its
subcommands.

## Documentation

| Document | Contents |
| --- | --- |
| [docs/design.md](docs/design.md) | Measurement, classification, baselines, diagnoses, mutual probe, bus utilization, simulated faults |
| [docs/metrics.md](docs/metrics.md) | Exported metrics, report format, metrics endpoint |
| [docs/operations.md](docs/operations.md) | Install layout, privileges, systemd, unknown data sources, alerting, upgrades, cost to workloads, baseline calibration |
| [docs/runbook.md](docs/runbook.md) | What each diagnosis code and alert means and what to check first |
| [docs/building.md](docs/building.md) | Toolchain and build targets |
| [docs/releasing.md](docs/releasing.md) | Release artifacts and how to cut a release |
| [contrib/](contrib) | systemd unit, Prometheus alerting rules |

## License

Licensed under the [Apache License 2.0](LICENSE). See [PATENTS.md](PATENTS.md)
for reported third-party patent information and the limits of the license's
patent grant.
