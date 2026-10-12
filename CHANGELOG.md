# Changelog

All notable changes to this project are documented in this file. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
project uses [Semantic Versioning](https://semver.org/). A release's section
becomes the notes of its GitHub Release (see
[docs/releasing.md](docs/releasing.md)).

## [Unreleased]

## [0.1.0] - 2026-10-10

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

