# Operating hostping

How to run the `hostping` daemon as a service: what to install, what the host
must grant it, what each data source reports when it is not available, and what
a run costs the workload.

The systemd unit in [`contrib/systemd`](../contrib/systemd) has **not yet been
run under systemd on RDMA hardware**. The privilege statements below come from
the code unless they say otherwise; "not verified" means exactly that.

## Install

Each release has a `.deb`, an `.rpm` and a tarball for linux-amd64 (see
[releasing.md](releasing.md)). The target host needs glibc 2.28 or newer and
`libibverbs.so.1` from rdma-core 32 or newer, from its own distribution.

### From the .deb or .rpm

```bash
gh release download v1.2.3 --repo yuuki/hostping \
    --pattern 'hostping*' --pattern SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
sudo apt install ./hostping_1.2.3_amd64.deb      # Debian, Ubuntu
sudo dnf install ./hostping-1.2.3-1.x86_64.rpm   # RHEL, Rocky, Alma
```

The package pulls in `libibverbs` and installs:

```
/usr/lib/hostping/{hostping,hostpingctl,libhostping_probe.so}
/usr/bin/hostping, /usr/bin/hostpingctl  -> /usr/lib/hostping/
/usr/lib/systemd/system/hostping.service  # ExecStart=/usr/lib/hostping/hostping
/etc/default/hostping                     # a config file: kept across upgrades once edited
/usr/share/doc/hostping/                  # README.md, operations.md, design.md, metrics.md, runbook.md, licenses
```

It neither enables nor starts the service. Check discovery and the options
first (see the end of this section), then:

```bash
systemctl enable --now hostping
```

An upgrade (`apt install` / `dnf install` of the newer package) replaces the
files and reloads systemd but leaves a running daemon on the old build until
`systemctl restart hostping`. Removing the package stops and disables the
service. Roll back by installing the previous package (`dnf downgrade`, or
`apt install ./hostping_<previous>_amd64.deb`).

### From the tarball

Take the release tarball `hostping-<version>-linux-amd64.tar.gz`, or build the
same files with `make docker-bin-amd64`, which writes them to
`bin/linux-amd64/` (see [building.md](building.md)).

```bash
sha256sum -c --ignore-missing SHA256SUMS
tar xzf hostping-1.2.3-linux-amd64.tar.gz    # hostping-1.2.3/ holds the files below
```

The commands below use the `bin/linux-amd64/` paths; from a tarball, take the
same files from `hostping-<version>/` instead.

`hostping` and `hostpingctl` load `libhostping_probe.so` through an `$ORIGIN`
rpath, so the three files must stay in one directory. The unit assumes one
directory per release and a `current` symlink:

```
/opt/hostping/<version>/hostping
/opt/hostping/<version>/hostpingctl
/opt/hostping/<version>/libhostping_probe.so
/opt/hostping/current -> <version>
/etc/default/hostping                     # from contrib/systemd/hostping.default
/etc/systemd/system/hostping.service      # from contrib/systemd/hostping.service
```

```bash
install -d /opt/hostping/1.2.3
install -m 0755 bin/linux-amd64/hostping bin/linux-amd64/hostpingctl /opt/hostping/1.2.3/
install -m 0644 bin/linux-amd64/libhostping_probe.so /opt/hostping/1.2.3/
ln -sfn 1.2.3 /opt/hostping/current
install -m 0644 contrib/systemd/hostping.default /etc/default/hostping
install -m 0644 contrib/systemd/hostping.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now hostping
```

Call `hostpingctl` by its full path (`/opt/hostping/current/hostpingctl`) or put
`/opt/hostping/current` on `PATH`; a copy elsewhere would not find the library.

Discovery reads `/sys/class/infiniband` and the PCI tree **once, at start**. A
daemon started before the RDMA driver has registered its devices exits with a
`sysfs discovery` error, and `Restart=on-failure` retries it every 30 s. An RNIC
or GPU that appears later, or a driver reload, needs a restart.

Before enabling the daemon, check what discovery takes as RNICs:
`hostpingctl topology` lists them. Every device under `/sys/class/infiniband`
except bonding devices is probed, including an HCA that is not a data-path NIC,
such as the one a host manages its GPU fabric's switches through over an
InfiniBand subnet of its own. Probing that HCA sends RDMA traffic into the
fabric's control plane. Discovery logs a warning naming `--exclude-rnic` for an
InfiniBand RNIC that looks like one (its subnet manager runs on the host, or
its IPoIB netdev is administratively down while the port is ACTIVE); exclude
such a device in `/etc/default/hostping` before `systemctl enable`, and see
[Excluding RNICs](design.md#excluding-rnics---exclude-rnic). RNIC IDs
(`rnicN`) are positions in the sorted device list and shift when a device is
excluded, added or removed: key dashboards and alerts on `device` or
`pcie_bdf` rather than on the ID, by joining `hostping_rnic_info` (see
[Joining physical identifiers](#joining-physical-identifiers)).

## Privileges and devices

The unit runs `hostping` as root. What it uses, and why:

| Resource | Used for | Notes |
| --- | --- | --- |
| `/dev/infiniband/uverbs*` | Every loopback and mutual probe (`ibv_open_device`, QPs, MRs) | Often mode 0666 already; check with `ls -l`. |
| `RLIMIT_MEMLOCK` | Host-memory MRs (`ibv_reg_mr`) | See below. |
| `/dev/nvidia*`, `libcuda.so.1` (or `libcudart.so.*`), and `nvidia-peermem` or dma-buf support | GPU loopback (GPUDirect RDMA) | AMD: `libamdhip64.so`. Missing pieces skip the GPU endpoints with a reason; they never stop the daemon. |
| sysfs: `/sys/class/infiniband`, `/sys/class/net`, `/sys/bus/pci/devices`, `/sys/class/drm`; `/proc/stat` | Discovery, line rate, counters, PCIe link speed and width, AMD GPU busy; CPU load per NUMA node | Read only. |
| PCI config space (`/sys/bus/pci/devices/*/config`) beyond the first 64 bytes | ACS / ATS | Root only; see below. |
| `nvidia-smi` on `PATH` | GPU utilization, compute mode, GPU PCIe throughput | Each call bounded (2 s; 5 s for `nvidia-smi dmon`). |
| `ethtool` on `PATH` | PFC pause duration and drops not exposed in sysfs (RoCE only) | Each call bounded to 2 s, backed off per netdev when it answers nothing. Without it a RoCE RNIC with no sysfs pause counter has no PFC trigger; the start-up log says so. Unprivileged `ethtool -S` is not verified. |
| `perf_event_open` on the uncore IMC and UPI PMUs | `--uncore-util` only | `CAP_PERFMON` (or `CAP_SYS_ADMIN`) or `kernel.perf_event_paranoid <= 0`. |
| A TCP port | `--metrics` (default `:9464`) | See [Metrics endpoint](#metrics-endpoint). |

**Memlock.** Neither the Go nor the Zig side raises `RLIMIT_MEMLOCK`. A memory
loopback probe registers two host buffers of `max(--large-msg, 4 KiB)` each —
512 KiB at the default, 32 MiB at the 16 MiB cap — and probes run one at a
time; the mutual probe registers a small buffer per RNIC (about 0.5 MiB at
the 4 KiB `--mutual-msg` cap). The kernel charges host
MRs (and the queue buffers the verbs provider registers) against the limit
unless the process holds `CAP_IPC_LOCK`, which root does. A refused
registration fails that probe as a setup error, so the endpoint shows up in
`skipped_endpoints` with the probe's reason rather than as a measurement. The
unit sets `LimitMEMLOCK=infinity` so a non-root deployment does not hit this;
whether GPU MRs are charged depends on the driver path and is not verified.

### Running as a dedicated user

Verified: a user with access to the RDMA and NVIDIA device nodes can run memory
and GPUDirect RDMA loopback probes. To run that way, create the user and
uncomment `User=`/`Group=` in the unit, plus `SupplementaryGroups=` if your
distribution restricts the device nodes to a group. What is lost:

- **ACS / ATS become unknown.** For an unprivileged reader the kernel returns
  only the first 64 bytes of a device's `config` file, and ACS and ATS are
  PCIe extended capabilities (offset 0x100 and up). hostping then has
  `ACSKnown`/`ATSKnown` false for every device, so `acs_enabled` and
  `ats_disabled` are never reported, and **nothing is logged about it**. Slow
  GPUDirect paths are still classified and localized by Algorithm 1; an
  abnormally slow GPU path whose cause would have been ACS or ATS may surface
  as the advisory `path_latency_abnormal` instead.
- **Uncore memory and inter-socket utilization** (`--uncore-util`) needs
  `CAP_PERFMON` or `kernel.perf_event_paranoid <= 0`. Without either, the
  daemon logs
  `perf_event_open denied (needs CAP_PERFMON or kernel.perf_event_paranoid <= 0)`
  at start and every `mem.N` and `upi.A-B` link stays unknown (verified). The
  unit's commented `AmbientCapabilities=CAP_PERFMON` grants it. With it, the
  `mem.N` and `upi.A-B` readings were checked against a known memory and
  cross-socket load. A file capability (`setcap cap_perfmon+ep`) works too,
  but the binary then runs in secure-execution mode, where the dynamic loader
  ignores the `$ORIGIN` rpath: register the directory holding
  `libhostping_probe.so` with `ldconfig` (`/etc/ld.so.conf.d/`), and set the
  capability again after replacing the binary.
- **Memlock** applies, since the user has no `CAP_IPC_LOCK`; keep
  `LimitMEMLOCK=infinity`.
- **Missing `/dev/nvidia*` nodes.** The CUDA driver creates missing nodes and
  loads `nvidia_uvm` through the setuid `nvidia-modprobe`, which
  `NoNewPrivileges=yes` blocks for a non-root user. Make sure the nodes exist
  at boot (for example through `nvidia-persistenced` or udev).

### Hardening in the unit

Enabled: `NoNewPrivileges`, `ProtectSystem=full`, `ProtectHome=read-only`,
`ProtectControlGroups`, `RestrictRealtime`, `RestrictSUIDSGID`,
`RestrictNamespaces`, `LockPersonality`. None of them touches `/dev`, `/sys`,
`/proc` or the network, and hostping itself writes no files other than the
opt-in `--report-file` and `--state-dir` ones, for which the unit's
`LogsDirectory=` and `StateDirectory=` provide directories under `/var`.

Left out, with the reason next to each in the unit: `PrivateDevices` and
`DevicePolicy=closed` (hide the verbs and GPU device nodes),
`ProtectKernelTunables` (read-only `/sys`), `ProtectKernelModules` (CUDA loads
`nvidia_uvm` on demand), `PrivateTmp` (hides the CUDA MPS pipe directory),
`PrivateNetwork`, `RestrictAddressFamilies`, `SystemCallFilter`,
`MemoryDenyWriteExecute`, `ProtectSystem=strict`, `ProtectProc`,
`ProtectKernelLogs`. Enable any of
them only after checking on your hardware that memory loopback, GPU loopback,
`nvidia-smi` and `ethtool -S` still work.

## Configuration

Everything is a flag, plus the standard `OTEL_EXPORTER_OTLP_*` variables (see
[Metrics endpoint](#metrics-endpoint)). The unit reads
`/etc/default/hostping` and passes `$HOSTPING_OPTS` on the command line; see
the commented example in
[`contrib/systemd/hostping.default`](../contrib/systemd/hostping.default).
systemd splits the variable on whitespace and does not parse quotes inside it,
so no value may contain a space; to disable a listener write `--metrics=`, not
`--metrics ""`.

A flag the binary rejects exits non-zero, and `Restart=on-failure` retries it
every 30 s with the same flags. Check a new option set before restarting:

```bash
set -a; . /etc/default/hostping; set +a
/opt/hostping/current/hostpingctl topology $HOSTPING_OPTS >/dev/null
```

That parses every flag and runs discovery (including `--mem-channel-capacity`
link IDs) without probing; it does not read `--baseline-file` or check the
`--mutual-interval` / `--mutual-transport` combination.

`hostpingctl diagnose` and the daemon do not coordinate. Running a live
`diagnose` while the daemon is up puts both runs' probe traffic on the same
RNICs, and each reads the other's as host load.

## What "unknown" means per data source

A source that cannot be read is unknown, and unknown is never read as healthy
or idle. Where a source has no unknown state, the table says so.

| Source | Unavailable when | What hostping does |
| --- | --- | --- |
| RNIC discovery | No RDMA driver, no `/sys/class/infiniband` | Exits at start with a `sysfs discovery` error; the unit retries every 30 s. |
| RNIC line rate (`ports/N/rate`) | Unreadable | No idle run and no `--busy-interval` run (the loaded set cannot be classified). GPU paths of that RNIC are `uncertain` with `no_ceiling`. |
| RNIC byte counters | Neither IB port counters nor netdev statistics readable | Every poll fails with `monitor: no RNIC counters readable for <dev>` and no run starts. |
| PFC pause duration, drops: never found | InfiniBand link layer (no PFC; `ethtool` is not run), or no vendor `*pause*duration*` sysfs counter and no `ethtool -S` answer since start | **Not applicable**, logged once per RNIC: `hostping: rnic3 has no PFC pause-duration source (…); the PFC trigger cannot fire for it`. The reason says `ethtool not on PATH` when that is the fix. The trigger cannot fire for that RNIC, it does not block the idle verdict, and `hostping.rnic.pause_duration_ratio` / `hostping.rnic.drop.delta` are absent for it, not 0. A source that appears later is logged and used from then on. |
| PFC pause duration, drops: lost | A source read before is gone: the file vanished or is unreadable, or `ethtool -S` fails, times out or is backed off | **Unknown**, logged once per change: the host is not idle (no idle run; a live `hostpingctl diagnose` refuses), no ratio or delta is computed and the gauge is not written (the exporter keeps its last measured value; the log line is the signal). PFC/drop runs from other RNICs and `--busy-interval` still happen. When it reads again the first sample only re-baselines, so the jump does not trigger a run. A counter that is present and reads 0 is known. |
| GPU utilization | No `nvidia-smi` on `PATH` and no DRM `gpu_busy_percent` | The GPU is unknown, so the host is never idle: no idle runs. PFC/drop busy runs still happen. A live `hostpingctl diagnose` refuses to run. |
| CPU load of a NUMA node | `/proc/stat` unreadable | The host is never idle while it is unread: no idle runs, a held idle-mode run is discarded (`idle_unknown`), and a live `hostpingctl diagnose` refuses to run. Logged once when it fails and once when it reads again, which only re-baselines. No `hostping.cpu.busy` sample is written, never a 0 (the exporter keeps its last measured value). |
| GPU compute mode | No `nvidia-smi`, AMD GPU, `[N/A]` | The GPU is probed as if `Default`. |
| GPU loopback (GDR) | No GPU device nodes, CUDA/HIP library, or `nvidia-peermem`/dma-buf | GPU endpoints skipped with the probe's reason: one log line per change, `skipped_endpoints`, `hostping.endpoint.skipped`. Memory paths still diagnose. |
| Memory loopback | Device node, MR registration (memlock) or NUMA binding fails | Same skip channels, kind `memory`. |
| Loopback port address | RoCE: no active-port RoCEv2 GID. InfiniBand (`ports/N/link_layer`): no ACTIVE port, or an ACTIVE port with LID 0 (no subnet manager) | Every endpoint of that RNIC is skipped through the same channels, with reason `no active rocev2 gid`, `infiniband port not active` or `infiniband port has no lid`. The RNIC's fabric is `unknown`. The addressing a probe did use is logged once per RNIC and on change: `hostping: loopback addressing rnic=rnic0 device=… link_layer=InfiniBand mode=ib_lid port=1 lid=…` (`mode=ib_lid_grh` when the LID-only connect was refused or its WRITEs completed with a remote error, `mode=rocev2_gid` on RoCE). The mode that worked is remembered per device and port. A WRITE that fails in every mode is skipped as `probe failed: Wc: <ibv_wc_status_str> (<status>)`, e.g. `transport retry counter exceeded (12)`; the same text names RoCE completion errors. When that failure is a port-level one (retry counter exceeded, a remote error, a bad response or response timeout) on an InfiniBand RNIC none of whose probes has answered yet in the run, its remaining endpoints are not probed in that run, because each would wait out the hardware retry budget once per mode; they are skipped through the same channels with reason `not probed: an earlier loopback on this RNIC failed: <first failure>`, so `skipped_endpoints` and `hostping.endpoint.skipped` still count every endpoint. The next run probes the port again. An RNIC that answered earlier in the run, a local completion error, a setup failure and a RoCE port are probed endpoint by endpoint as before. |
| Mutual probe | No active-port RoCEv2 GID or usable InfiniBand LID, or no RNIC pair in `--mutual-scope` | No pairs; logged once per fabric set; no `hostping.mutual.*` series. InfiniBand RNICs are left out in every scope unless `--mutual-infiniband` (logged once: `hostping: mutual probe leaves out N InfiniBand RNIC(s) […]`). With it they pair under `rail` by subnet prefix (`ib:` key), so separate fabrics left at the default prefix `fe80::/64` look like one rail and time out as false connectivity failures: opt in only when the host's InfiniBand ports share one fabric. RoCE and InfiniBand RNICs never pair. |
| PCIe link speed / width | Unreadable `current_link_speed` | No `rnic_pcie_failure` / `gpu_pcie_failure` from the negotiated link of that device; a hop whose maxima are unreadable is assumed Gen4 x16 and marked `capacity_assumed`. |
| ACS / ATS | Not root (see above), or the device has no such capability | `acs_enabled` / `ats_disabled` not reported. Not logged. |
| GPU PCIe utilization | No `nvidia-smi` | `pcie.<gpu>` utilization unknown. |
| Memory utilization | No `--uncore-util`, no `--mem-channel-capacity`, no perf access, AMD, sub-NUMA clustering whose start-up IMC mapping was not clear (a host loading memory hard while the agent starts) | `mem.N` unknown; the reason is logged at start. |
| Inter-socket utilization | No `--uncore-util`, no `--upi-capacity`, no perf access, AMD, a processor whose UPI flit size is not measured (without `--upi-bytes-per-flit`), more than two packages and a UPI link whose `die<N>` file is missing | `upi.A-B` unknown; the reason is logged at start. |
| CPU package of a NUMA node | A node's `cpulist` or a CPU's `topology/physical_package_id` unreadable | Every NUMA node is modeled as its own socket, as before packages were read (no `mesh.A-B` links, `upi.A-B` between every two nodes); logged once at start. On a sub-NUMA-clustered host that puts the NUMA nodes of one package behind UPI links again. |
| Package mesh utilization and capacity (`mesh.A-B`) | Always | Unknown; an abnormal mesh link is `snc_mesh_failure`. |

An unknown utilization is absent from the report's `utilization`, writes no
`hostping.bus.utilization` sample and is never taken as overloaded.

## Metrics endpoint

`--metrics` (default `:9464`, every interface) serves `/metrics` (Prometheus)
and `/healthz` (see [Liveness and staleness alerts](#liveness-and-staleness-alerts)).
By default it is plain
HTTP with no authentication, so bind it to the management address
(`--metrics 10.0.0.10:9464`) or firewall the port: on a host with data-plane
and management networks the default listens on both.

`--metrics-web-config FILE` adds TLS and/or basic authentication, using the
[Prometheus exporter-toolkit web configuration](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md)
(the same file format as `node_exporter --web.config.file`). The file is
checked at start; certificates are re-read on each new TLS connection, so a
renewed certificate needs no restart. `/healthz` sits behind the same TLS and
basic auth as `/metrics` (exporter-toolkit wraps the whole listener), so a
health checker must present the same credentials; one that cannot should
check the process (`systemctl is-active`) instead. For example:

```yaml
# /etc/hostping/web.yml
tls_server_config:
  cert_file: /etc/hostping/tls.crt
  key_file: /etc/hostping/tls.key
basic_auth_users:
  # bcrypt hash, e.g. from `htpasswd -nBC 10 "" | tr -d ':\n'`
  prometheus: $2y$10$...
```

The listener is bound before the daemon starts working: an address that cannot
be bound (in use, not configured on the host) or a web config that does not
load is a start-up error, and `Restart=on-failure` retries it every 30 s. A
slow client cannot hold a connection open: the server times out reading
headers (10 s), the request (30 s), writing the response (60 s) and idle
keep-alive connections (2 min).

`OTEL_EXPORTER_OTLP_ENDPOINT` (or `--otlp`) pushes the same metrics over OTLP
gRPC every 10 s. The endpoint is `host:port`, or a URL whose scheme decides the
transport: `http://` is plaintext, `https://` is TLS. For `host:port` the
transport is, in order of precedence:

1. `--otlp-insecure` (`--otlp-insecure=false` forces TLS);
2. `OTEL_EXPORTER_OTLP_METRICS_INSECURE`, then `OTEL_EXPORTER_OTLP_INSECURE`;
3. TLS when a CA or client certificate is configured (below);
4. plaintext when the host is loopback (`localhost`, `127.0.0.0/8`, `::1`),
   TLS with the system roots otherwise.

The exporter also reads the standard `OTEL_EXPORTER_OTLP_CERTIFICATE` (CA
bundle), `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` / `_CLIENT_KEY` (mutual TLS),
`OTEL_EXPORTER_OTLP_HEADERS` (e.g. an `authorization` header for a hosted
collector), `OTEL_EXPORTER_OTLP_TIMEOUT` and `OTEL_EXPORTER_OTLP_COMPRESSION`,
and their `_METRICS_` variants. Settings that contradict each other (a
plaintext setting with a certificate, `--otlp-insecure` against the env var or
the URL scheme) are a start-up error rather than one silently winning. The
start-up log names the transport and what chose it:
`hostping otlp exporter endpoint=10.0.0.20:4317 tls=true (non-loopback endpoint)`.

A collector on a non-loopback address that only speaks plaintext needs
`--otlp-insecure`, `OTEL_EXPORTER_OTLP_INSECURE=true`, or an `http://`
endpoint; without one the exporter attempts TLS, every push fails, and the
error is logged.

## Liveness and staleness alerts

`/healthz` answers whether the daemon's poll loop is still turning, in plain
text:

- **200 `ok`** while the loop has completed a monitor poll within the last
  max(10 × `--metric-interval`, 30 s) — 30 s at the default 1 s interval. A
  poll that returned an error still counts: the loop is alive, and the error
  is counted in `hostping_monitor_poll_errors_total`.
- **200** while an engine run is in progress, for up to 30 minutes. The loop
  runs the engine synchronously, so no poll completes during a run, and a full
  mesh whose probes time out one by one can legitimately take minutes. The end
  of a run counts as a tick.
- **503** with the reason otherwise, e.g. `no monitor poll completed for 41s
  (limit 30s)` or `engine run in progress for 31m0s (limit 30m0s)`.

Start-up counts as the first tick, which covers the warm-up before the first
poll. One-shot runs have no poll loop; their `/healthz` (only served when
`--metrics` is given) is always 200.

`/healthz` deliberately does not depend on runs succeeding: a host that is
never idle legitimately never starts an idle run, and restarting the daemon
does not fix a failing probe. Whether hostping is still producing diagnoses is
an alerting question, answered by these series (names as Prometheus renders
them):

| Series | Meaning |
| --- | --- |
| `hostping_engine_last_run_timestamp_seconds` | End of the last engine run, whatever its outcome |
| `hostping_engine_last_success_timestamp_seconds` | End of the last run whose report was confirmed and written |
| `hostping_engine_run_failed_total{stage}` | Runs that returned an error (`stage="engine"`) or whose report could not be written (`stage="report"`) |
| `hostping_engine_run_discarded_total{reason}` | Idle-mode runs discarded because the clean poll after them found the host busy (`reason="host_busy"`) or could not classify it (`reason="idle_unknown"`) |
| `hostping_monitor_poll_errors_total` | Monitor polls that returned an error |
| `hostping_monitor_last_poll_timestamp_seconds` | Last monitor poll that completed without error |
| `hostping_monitor_host_idle` | 1 when the last clean poll found the host idle, 0 when busy or unclassified |

The counters are exported as 0 from the first scrape; the timestamps are
absent until their first event (and again after a restart), so an alert on
them should also cover `absent()` if a host that never succeeds once matters.

Example alert expressions, for the default `--idle-interval` of 5 minutes
(adjust the windows to yours):

```promql
# The host stayed idle for three idle intervals, yet no run succeeded:
# idle runs fire every --idle-interval while the host is idle.
time() - hostping_engine_last_success_timestamp_seconds > 3 * 300
  and min_over_time(hostping_monitor_host_idle[15m]) == 1

# Runs fail (engine error or report not written).
rate(hostping_engine_run_failed_total[15m]) > 0

# The monitor cannot read the host's counters or GPU state.
rate(hostping_monitor_poll_errors_total[5m]) > 0
```

Use `hostping_monitor_host_idle` rather than `hostping_host_idle` for the
first one: `hostping_host_idle` is the classification of the last *report*,
so it keeps reading 1 on a host that turned busy after an idle run and has
not been diagnosed since. A steady `hostping_engine_run_discarded_total`
increase means a workload keeps coming back during idle runs; that is
expected on a host that pauses between iterations, but if it never stops,
no idle diagnosis is being made.

## Joining physical identifiers

Every measurement series names an RNIC by its ID (`rnic="rnic3"`) and an
endpoint by `gpuN` or `memN`. Those IDs are positions in discovery's sorted
lists, so they move when a device is excluded, added or removed. Two info
gauges, always 1 and exported from the first scrape, carry the physical
identifiers:

| Series | Labels |
| --- | --- |
| `hostping_rnic_info` | `rnic`, `device` (IB device, `mlx5_N`), `pcie_bdf`, `netdev`, `link_layer` (`Ethernet`/`InfiniBand`), `fabric` (the mutual-probe pairing key, e.g. `v4:10.0.0.0`), `socket`, `numa` |
| `hostping_endpoint_info` | `endpoint`, `kind` (`gpu`/`memory`), `index`, `socket`, `numa`; GPUs also `pcie_bdf` and `root_port` |

A label discovery could not fill reads `unknown` (`fabric` for an RNIC with no
RoCEv2 GID, `link_layer` on a `--topology-file` that omits it). A memory
endpoint has no `pcie_bdf` or `root_port`. An RNIC dropped by `--exclude-rnic`
has no `hostping_rnic_info` series. The values are fixed at start-up: a
renumbering after a device change shows up after the daemon restarts, together
with every other series.

Join them onto any series with `group_left`:

```promql
# Abnormal paths by IB device and PCI address of the RNIC.
hostping_path_abnormal
  * on (instance, rnic) group_left (device, pcie_bdf)
    hostping_rnic_info

# PFC pause ratio per IB device.
hostping_rnic_pause_duration_ratio
  * on (instance, rnic) group_left (device)
    hostping_rnic_info

# Loopback bandwidth to GPUs, by the GPU's PCI address.
hostping_loopback_bandwidth_bit_per_second
  * on (instance, endpoint) group_left (pcie_bdf)
    hostping_endpoint_info{kind="gpu"}
```

The info gauges carry the same constant `otel_scope_*` labels as every other
hostping series, so `on (instance, rnic)` matches one series on each side. The
mutual series name RNICs as `src` and `dst`; join each side with
`label_replace` (for example `label_replace(hostping_rnic_info, "src", "$1",
"rnic", "(.*)")`). The diagnosis counter has no RNIC label, so the bundled
alerting rules do not join: the report names the devices of each cause.

## Status codes

Path and link states are exported as codes: one series per path or link
whose value is the code of its current state. Every report rewrites the value
of every path and link it carries, so a state change replaces the number and
leaves no series of the old state behind.

| Series (Prometheus name) | Labels | Codes |
| --- | --- | --- |
| `hostping_path_status_code` | `rnic`, `endpoint` | 1 `normal`, 2 `uncertain`, 3 `abnormal` |
| `hostping_path_uncertain_reason_code` | `rnic`, `endpoint` | 0 `none` (the path is normal or abnormal), 1 `no_path`, 2 `rnic_busy`, 3 `non_affinitive`, 4 `below_ceiling`, 5 `no_ceiling`, 6 `host_shift`, 7 `cpu_busy` |
| `hostping_path_latency_status_code` | `rnic`, `endpoint` | 1 `normal`, 2 `unknown`, 3 `abnormal` |
| `hostping_path_latency_reason_code` | `rnic`, `endpoint` | 0 `none` (never classified), 1 `user_baseline`, 2 `siblings`, 3 `busy`, 4 `few_siblings`, 5 `no_path`, 6 `no_reading`, 7 `recheck_cleared`, 8 `locality_shift`, 9 `cpu_busy` |
| `hostping_link_status_code` | `link` | 1 `normal`, 2 `uncertain`, 3 `abnormal`, 4 `gray` |

The names are those of the report JSON (`status`, `uncertain_reason`,
`latency_status`, `latency_reason` of a path; `status` of a link), whose
format does not change. The reasons are documented with the classification in
[design.md](design.md#baselines) and the [runbook](runbook.md).

**The codes are fixed.** They are a table in `internal/telemetry/statuscode.go`,
not the position of a value in its Go enum, and a test pins them. A state added
later gets the next unused code of its gauge; an existing code is never
renumbered, and the code of a removed state is not reused. Codes are names, not
a scale: compare with `==`, not `<` or `>`, even though the three status
gauges happen to share 1 for healthy and 3 for abnormal.

**Zero.** A status always has a value, so status codes start at 1 and 0 is not
a status: a query that fills a missing series with 0 (`or vector(0)`, a
dashboard's null-as-zero) or a backend that defaults an unwritten gauge cannot
make it read as a state. Reason codes use 0 for `none`, the reason of a path
that needs none, so `!= 0` reads "has a reason". A value of -1 means hostping
wrote a state its table has no code for; the tests rule that out, so it is a
bug to report.

`hostping_status_code_info` names every code: one series per `(metric, code)`
(28 in all), always 1 and exported from the first scrape. `metric` is the
OpenTelemetry instrument name, with dots (`hostping.path.status_code`), the
same on every backend; rows of a status gauge carry `status`, rows of a reason
gauge `reason`:

```
hostping_status_code_info{metric="hostping.path.status_code",code="3",status="abnormal"} 1
hostping_status_code_info{metric="hostping.path.uncertain_reason_code",code="4",reason="below_ceiling"} 1
```

### Querying

```promql
# Abnormal paths, by bandwidth.
hostping_path_status_code == 3

# Uncertain paths that are uncertain for a reason other than the steady state
# of a healthy multi-GPU host (non_affinitive) or load (rnic_busy, cpu_busy).
hostping_path_uncertain_reason_code == 4 or hostping_path_uncertain_reason_code == 5
  or hostping_path_uncertain_reason_code == 6

# An uncertain path with no reason recorded: an uncertain branch forgot to say
# why. Should never match.
hostping_path_status_code == 2 and hostping_path_uncertain_reason_code == 0

# Paths whose latency is abnormal against the operator's baseline.
hostping_path_latency_status_code == 3 and hostping_path_latency_reason_code == 1

# Gray links.
hostping_link_status_code == 4
```

A filter like `== 3` keeps the code as the value, so count the result
(`count by (instance) (...)`) rather than summing it.

To get names instead of numbers, turn the code into a label with
`count_values` and join it on the info gauge. `count_values` keeps the labels
it groups by, so list the ones that identify a path or link:

```promql
# Every path with its status as a label: {instance, rnic, endpoint, code, status} 1.
count_values by (job, instance, rnic, endpoint) ("code", hostping_path_status_code)
  * on (job, instance, code) group_left (status)
    hostping_status_code_info{metric="hostping.path.status_code"}

# Paths per uncertain reason on each host.
count_values by (job, instance) ("code", hostping_path_uncertain_reason_code != 0)
  * on (job, instance, code) group_left (reason)
    hostping_status_code_info{metric="hostping.path.uncertain_reason_code"}
```

The bundled rules record the per-host counts as `hostping:path_status:count`,
`hostping:path_uncertain_reason:count` and `hostping:link_status:count`
([Alerting rules](#alerting-rules)).

The join also lets an alert select states by name and carry the name as a
label. For example, an affinitive path that has been held below its ceiling,
or had no ceiling to hold it to, for two hours (not a bundled rule):

```yaml
- alert: HostpingPathBelowCeiling
  expr: |
    count_values by (job, instance, rnic, endpoint) ("code", hostping_path_uncertain_reason_code)
      * on (job, instance, code) group_left (reason)
        hostping_status_code_info{metric="hostping.path.uncertain_reason_code", reason=~"below_ceiling|no_ceiling"}
  for: 2h
  labels:
    severity: info
  annotations:
    summary: "hostping: {{ $labels.rnic }} → {{ $labels.endpoint }} is uncertain ({{ $labels.reason }}) on {{ $labels.instance }}"
```

The status gauges hold the verdict of the last report until the next one, so
they are for locating a hop or path, not for paging; see
[Paging policy](runbook.md#paging-policy).

### Grafana

A State timeline or Table panel on a code gauge shows names with value
mappings. In the panel's JSON (`fieldConfig.defaults.mappings`), for
`hostping_path_status_code`:

```json
[{
  "type": "value",
  "options": {
    "1": {"text": "normal", "color": "green", "index": 0},
    "2": {"text": "uncertain", "color": "yellow", "index": 1},
    "3": {"text": "abnormal", "color": "red", "index": 2}
  }
}]
```

`hostping_link_status_code` adds `"4": {"text": "gray", "color": "orange"}`;
the reason gauges map their codes from the table above the same way. A
mapping is copied into each dashboard and does not follow a new code, so a
panel that should name every state as it is added can query the
`count_values` join above instead and use `{{status}}` (or `{{reason}}`) as
the legend.

### Migrating from 0.1.1

0.1.1 exported one 0/1 series per possible state (`hostping_path_status`,
`hostping_path_latency_status`, `hostping_link_status`). 0.1.2 removes them
for the code gauges; there is no option to keep the old series. Rewrite
dashboards and rules before upgrading, or run both expressions for a release
cycle while hosts are upgraded:

| 0.1.1 | 0.1.2 |
| --- | --- |
| `hostping_path_status{status="abnormal"} == 1` | `hostping_path_status_code == 3` |
| `hostping_path_status{status="normal"} == 1` | `hostping_path_status_code == 1` |
| `hostping_path_status{status="uncertain"} == 1` (any reason) | `hostping_path_status_code == 2` |
| `hostping_path_status{status="uncertain", reason="R"} == 1` | `hostping_path_uncertain_reason_code == <code of R>` |
| `hostping_path_status{status="uncertain", reason="none"} == 1` | `hostping_path_status_code == 2 and hostping_path_uncertain_reason_code == 0` |
| `hostping_path_latency_status{status="S"} == 1` | `hostping_path_latency_status_code == <code of S>` |
| `hostping_path_latency_status{status="S", reason="R"} == 1` | `hostping_path_latency_status_code == <code of S> and hostping_path_latency_reason_code == <code of R>` |
| `hostping_link_status{status="S"} == 1` | `hostping_link_status_code == <code of S>` |
| `sum by (instance, status) (hostping_path_status)` | `hostping:path_status:count` (bundled rule) |
| `sum by (instance, status) (hostping_link_status)` | `hostping:link_status:count` (bundled rule) |
| a `status` / `reason` label in a legend or alert | the `count_values` join above |

The old expressions return 1 per matching path; the new filters return the
code, which matters only to `sum()`. 0.1.1 had no latency series for a few
(status, reason) pairs the classifier does not produce; with separate status
and reason gauges every pair is representable. After the upgrade the old
series stop being scraped and go stale after Prometheus's 5-minute lookback.

## Alerting rules

[`contrib/prometheus/hostping-rules.yml`](../contrib/prometheus/hostping-rules.yml)
packages the expressions above, plus alerts on diagnoses and on skipped
endpoints and per-host counts of path and link states by name
([Status codes](#status-codes)), with unit tests in `hostping-rules.test.yml`:

```bash
promtool check rules contrib/prometheus/hostping-rules.yml
promtool test rules contrib/prometheus/hostping-rules.test.yml
```

The rules assume the scrape job is named `hostping`. A diagnosis alerts only
after it appears in two engine runs within 30 minutes; high-confidence
hardware faults page, other findings ticket, advisory codes are `info`, and
`rnic_cross_rail_unreachable` at `low` does not alert. The
[runbook](runbook.md) has the reasoning ([Paging policy](runbook.md#paging-policy))
and one section per code and alert.

## Series cardinality

What one host exports, from the instruments in `internal/telemetry` and checked
against a scrape of the `--sim` Fig.2 host. Notation:

| Symbol | Meaning |
| --- | --- |
| R | RNICs |
| E | Loopback endpoints: GPUs (E_gpu) plus NUMA memory nodes (E_mem) |
| P | Measured RNIC→endpoint paths, at most R × E: P_gpu to GPUs, P_mem to memory |
| L | Topology links: one `pcie.<rnic>` per RNIC, one `pcie.<gpu>` per GPU, plus root ports, switches, `mem.N`, inter-socket links and, under sub-NUMA clustering, `mesh.A-B` links between NUMA nodes of one package (`hostpingctl topology` lists them) |
| M | Mutual pairs: unordered RNIC pairs on a shared rail under `--mutual-scope=rail` (0 on a rail-optimized host), R × (R − 1) / 2 under `--mutual-scope=host` |

| Series | Count | Labels that multiply |
| --- | --- | --- |
| `hostping_loopback_latency_microseconds`, `hostping_loopback_bandwidth_bit_per_second`, `hostping_path_abnormal` | 3 P | `rnic` × `endpoint` |
| `hostping_path_status_code`, `hostping_path_uncertain_reason_code`, `hostping_path_latency_status_code`, `hostping_path_latency_reason_code` | 4 P | `rnic` × `endpoint`; the state is the value, not a label |
| `hostping_verb_duration_microseconds` | 3 P_gpu + 2 P_mem | `verb`: `gpu_alloc` on GPU paths only, `mr_reg`, `qp_setup`; `mem_warmup` on memory paths with `--mem-warmup` |
| `hostping_link_abnormal_count` | L | `link` |
| `hostping_link_status_code` | L | `link`; the state is the value |
| `hostping_bus_utilization` | ≤ L | `link`; only hops a live source read (RNIC and GPU PCIe, `mem.N` and `upi.A-B` with `--uncore-util`) |
| `hostping_mutual_rtt_microseconds`, `hostping_mutual_loss_ratio`, `hostping_mutual_timeout_ratio` | 3 M | `src` × `dst` |
| `hostping_rnic_throughput_bit_per_second`, `hostping_rnic_pause_duration_ratio`, `hostping_rnic_drop_delta` | ≤ 3 R | `rnic` |
| `hostping_cpu_busy` | ≤ N | `numa` × `socket`: one per NUMA node with an RNIC or memory endpoint (N) |
| `hostping_endpoint_skipped` | 2 | `kind` |
| `hostping_diagnosis_count_total` | D ≤ 29 | `code` × `confidence`: only the pairs the analyzer emits (23 codes, most at one confidence) |
| `hostping_engine_run_count_total` | T ≤ 6 for the daemon | `trigger` × `mode`: idle × {`idle`, `mutual_timeout`}, busy × {`pfc`, `packet_drops`, `periodic`, `mutual_timeout`} |
| `hostping_rnic_info` | R | `rnic` (the other labels are fixed per RNIC) |
| `hostping_endpoint_info` | E | `endpoint` (the other labels are fixed per endpoint) |
| `hostping_status_code_info` | 28 | `metric` × `code`: fixed by the code tables, whatever the host |
| Liveness: `hostping_host_idle`, `hostping_monitor_host_idle`, three `*_timestamp_seconds`, `hostping_engine_run_failed_total` (2 `stage`), `hostping_engine_run_discarded_total` (2 `reason`), `hostping_monitor_poll_errors_total`, `hostping_build_info` | 11 | |

Per host:

```
series ≈ 10 P_gpu + 9 P_mem + 2 L + U + 3 M + 4 R + E + N + 41 + D + T
```

where U ≤ L is the number of links with a utilization source, plus about 50
Go runtime, process and `target_info` series the Prometheus client adds. The
constant 41 is the 11 liveness series, the 2 `hostping_endpoint_skipped` and
the 28 `hostping_status_code_info`. The path terms dominate: about 10 series
per RNIC→endpoint pair, so a host grows with R × E. Prometheus adds its own `job` and `instance` labels and the
exporter the constant `otel_scope_*` labels; none of those multiply. The
endpoint skip reason (free-form probe text), a path's baseline sub-class and a
cause's subjects (which GPU, which link) are not labels.

Worked example, the Fig.2 host of `--sim` (R = 4, E = 10 with 8 GPUs and
2 memory nodes, so P = 40 with P_gpu = 32 and P_mem = 8; L = 23: 4 RNIC
PCIe, 8 GPU PCIe, 4 switch, 4 root port, 2 memory, 1 inter-socket; M = 6
under rail scope, no live bus utilization so U = 0, no CPU load read so
N = 0):

```
10 × 32 + 9 × 8 + 2 × 23 + 0 + 3 × 6 + 4 × 4 + 10 + 0 + 41 + D + T
= 320 + 72 + 46 + 18 + 16 + 10 + 0 + 41 + D + T = 523 + D + T
```

A healthy idle sim run scrapes 524 `hostping_*` series (D = 0, T = 1);
0.1.1, with one-hot status gauges at 22 series per path and 4 per link,
scraped 1285. The rail-optimized `--topology fig2-rails` host has M = 0 and
scrapes 506.
Each further diagnosis code and confidence, and each further trigger/mode
combination, adds one series for the life of the process. The same host
under `--mutual-scope=host` has the same M = 6; an 8-RNIC host would have
M = 28 there.

When series appear and disappear:

- **Present from the first scrape:** `hostping_build_info`,
  `hostping_rnic_info`, `hostping_endpoint_info` and
  `hostping_status_code_info` (fixed at start-up), and
  the liveness counters (written as 0).
- **Absent until first written:** path, link and verb series until the first
  run reports them; `hostping_diagnosis_count_total` per `(code, confidence)`
  until the first run that emits it; `hostping_engine_run_count_total` per
  `(trigger, mode)` until the first such run; the `*_timestamp_seconds`
  gauges until their first event. Alerts that need a series must account for
  that (see [Liveness and staleness alerts](#liveness-and-staleness-alerts)).
- **Absent when unknown, never 0:** `hostping_bus_utilization` for a hop no
  source read; `hostping_rnic_pause_duration_ratio` and
  `hostping_rnic_drop_delta` for an RNIC with no source (InfiniBand, or no
  counter found); `hostping_cpu_busy` for a NUMA node whose CPU load was not
  read; `hostping_mutual_timeout_ratio` for a pair that reported no
  probe count; every `hostping_mutual_*` series on a host with no qualifying
  pair; no path or verb series for a skipped endpoint (it is counted in
  `hostping_endpoint_skipped` instead), and no verb series for a probe that
  reported no timings.
- **Kept until restart:** gauges keep their last value. A path measured once
  and skipped afterwards, a busy run that probes only the reduced set, or a
  bus whose source stopped answering leaves the earlier series in place with
  its old value. The report of the current run is authoritative.
- **State gauges never strand a series:** the `*_status_code` and
  `*_reason_code` gauges hold the state as their value, one series per path or
  link, so a state change rewrites that value rather than creating a series.
  `hostping_endpoint_skipped` writes every `kind` on every report, so a
  recovered kind falls back to 0. A path or link missing from a later report
  keeps its last code, like every other path and link gauge (previous
  item).
- **Restart:** every series restarts. Counters begin again from 0 (`rate()`
  and `changes()` handle the reset), and the series that are absent until
  first written are absent again.

## Logs and reports under journald

The daemon writes log lines (start-up, skipped endpoints, PFC pause and drop
counter sources, discarded idle runs, diagnoses) to stderr and each confirmed
report to stdout as one compact JSON object per line (`--report-format=json`,
the daemon's default). Under the unit both go to the journal, one entry per
line, so every report is a single journal entry:

```bash
journalctl -u hostping -f
journalctl -u hostping --grep 'hostping diagnosis'
# reports only: the log lines are not JSON and are dropped by fromjson?
journalctl -u hostping -o cat | jq -cR 'fromjson? | select(.causes | length > 0)'
```

Each report line carries the report's own fields plus `reported_at` (UTC, when
it was written; for a held idle-mode run up to one poll after `started`),
`version` and `commit`. A discarded idle-mode run is written nowhere; only its
log line says it happened.

To keep reports out of the journal, or alongside it, `--report-file PATH`
appends every confirmed report to `PATH` as one JSON line, whatever
`--report-format` says about stdout; `--report-format=none` stops stdout
copies. The file is created mode 0640 and opened for each report, so
logrotate can move it away without signalling the daemon (no
`copytruncate` needed); a path that cannot be opened fails the start. The
unit's `LogsDirectory=hostping` provides `/var/log/hostping`:

```bash
# /etc/default/hostping
HOSTPING_OPTS="--report-file /var/log/hostping/reports.jsonl --report-format=none"
```

One-shot runs (`hostpingctl diagnose`, `hostping --once`) default to
`--report-format=pretty`, the indented JSON an operator reads in a terminal.
The default follows the run kind, not whether stdout is a terminal, so a
command behaves the same under ssh, a pipe or systemd; pass
`--report-format=json` to pipe a one-shot report into `jq -c` or a collector.

## Stopping, upgrading and rolling back

`systemctl stop` sends SIGTERM. The daemon stops between probes, never inside
one: a loopback probe ends within `--timeout` (5 s) of a stalled transfer, a
mutual burst against a pair that does not answer can take `--mutual-iters` ×
`--mutual-timeout` (32 s at the defaults), and the last OTLP push is bounded by
the exporter's timeout. The unit's `TimeoutStopSec=90s` covers that; if systemd
has to SIGKILL, the kernel releases the verbs resources and pinned memory with
the process. The daemon exits 0 on SIGTERM, so `Restart=on-failure` does not
bring it back.

Without `--state-dir`, a restart loses in-memory state: flapping streaks
(`--flap-window`), a held idle-mode report awaiting confirmation, and the
mutual probe window. The idle timer also restarts, so on an idle host the first
idle full-mesh run follows within seconds of the restart instead of
`--idle-interval` later. A link that flaps slowly relative to `--idle-interval`
× `--flap-window` may then never be reported on a fleet whose agents restart
regularly. The
liveness counters restart from 0 and the `*_timestamp_seconds` series are
absent until their first event after the restart; `rate()` handles the
counter reset.

`--state-dir DIR` (opt-in; the unit's `StateDirectory=hostping` provides
`/var/lib/hostping`, mode 0700) keeps four things across a daemon restart in
`DIR/state.json` (mode 0600, replaced atomically):

- the per-link consecutive-gray counters behind flapping, with the end of the
  last run that updated them, so a streak started before the restart is
  completed by the runs after it;
- the per-GPU count of consecutive idle runs whose memory registration crossed
  the `gpu_mr_registration_slow` threshold (`slow_mr`, optional: a file
  without it restores with no such streaks);
- the per-path count of consecutive idle runs that read the path's latency
  `abnormal` against its siblings, behind `path_latency_abnormal`
  (`latency`, keyed `rnic->endpoint`, optional like `slow_mr`; a path the
  graph no longer has is dropped on restore);
- the end of the last idle-trigger run (confirmed or discarded), so the next
  idle full mesh stays `--idle-interval` after it, or runs on the first idle
  poll when that is already past.

The file is written after each idle-mode run is confirmed or discarded (a
discarded run's streaks are rolled back first), after a busy-mode run that
changed a streak, and on shutdown (an idle-mode report still held at shutdown
is left out of the streaks). At start the daemon restores it only if the schema
version, the topology fingerprint and the kernel boot ID all match and its last
run ended no more than `--flap-window` × `--idle-interval` ago; otherwise it
logs one line saying why and starts from empty state. The fingerprint covers
RNIC IDs and verbs devices, endpoint IDs and kinds, link IDs and kinds, and
each path's link list and distance; it ignores capacities, line rates and rail
keys, which are re-read on every start. A corrupt or unreadable file is
ignored the same way. A directory that cannot be created or written fails the
start, like `--report-file`; a host whose boot ID
(`/proc/sys/kernel/random/boot_id`) cannot be read runs without persistence
and says so once. One-shot runs (`hostpingctl diagnose`, `hostping --once`,
`hostpingctl calibrate`) never read or write it.

Nothing survives a reboot: the boot ID changes, and links retrain on boot, so
streaks from before it are no evidence about the links now. A changed topology
(an RNIC or GPU added, removed or renamed) also starts from zero. The held
idle-mode report and the mutual probe window are never persisted.

Upgrade a tarball install (for a package, see [Install](#from-the-deb-or-rpm)):

```bash
install -d /opt/hostping/1.2.4
install -m 0755 bin/linux-amd64/hostping bin/linux-amd64/hostpingctl /opt/hostping/1.2.4/
install -m 0644 bin/linux-amd64/libhostping_probe.so /opt/hostping/1.2.4/
/opt/hostping/1.2.4/hostping --version               # the build you meant to install
/opt/hostping/1.2.4/hostpingctl diagnose --sim       # links and runs, no hardware touched
set -a; . /etc/default/hostping; set +a
/opt/hostping/1.2.4/hostpingctl topology $HOSTPING_OPTS >/dev/null   # flags still accepted
ln -sfn 1.2.4 /opt/hostping/current
systemctl restart hostping
```

After the restart, the start-up line in the journal (`hostping agent
version=... commit=...`) and the `hostping_build_info` series on `/metrics`
name the build that is now running.

Roll back by pointing `current` at the previous directory and restarting. Keep
the previous release directory until the new one has run through at least one
idle run. The environment file is shared between releases, so a rollback past a
release that added a flag you now use fails at start until the flag is removed.

## Cost to running workloads

Every run puts real traffic on the RNICs and PCIe links the service uses, and a GPU endpoint costs GPU memory while it is probed. Measured with a two-node all-to-all collective between two multi-GPU RDMA hosts:

| Situation | What the workload sees |
| --- | --- |
| Busy-mode run during service traffic (PFC/drop trigger, `--busy-interval`, or a manual `diagnose` on a loaded host) | Bus bandwidth 10–15% lower for 1–2 s per run, in every repetition. The loopback probes share the loaded RNICs and their PCIe hops with the service. |
| Idle-mode run already in flight when a job starts | Job launch about 0.4 s slower and the first ~2 s a few percent slower, then back to normal. hostping detects that the host became busy and discards the report (see the idle-mode hold in [Triggers](design.md#triggers)). |
| GPU endpoint being probed | One GPU context per probed GPU, on the order of 0.5 GiB of device memory, held only while the run probes that GPU (a few seconds) and released after the run. |

So the cost that matters on a busy fleet is the busy-mode run, not the idle one. Each PFC/drop trigger costs one such dip, at most once per `--busy-cooldown`; keep the cooldown at its default or longer on hosts whose fabric pauses routinely. `--busy-interval D` adds one dip per `D` on every loaded host even without a trigger, so set it in minutes and weigh a 1–2 s slowdown per `D` against how often you need a busy verdict. Avoid scripting `hostpingctl diagnose` against hosts that are running jobs: on a loaded host it runs in busy mode and costs the same dip. A job that sizes its GPU memory to the last few hundred MiB could still fail to allocate if it starts while an idle run holds a context on its GPU; this has not been measured.

A busy run that flags a path against its siblings re-reads it before reporting it (the paired busy recheck, see [Busy classification](design.md#busy-classification)): 3 rounds of the path and up to 4 other paths of its class on the same loaded RNIC, so at most 15 extra probes per flagged path (9 in a class of three) and 8 paths, 120 probes, per run. A busy run that flags nothing costs nothing extra. Like the sweep's probes, these share the loaded RNIC with the service, so a busy run with flagged paths holds its bandwidth dip a little longer.

A run that flags a path against its idle baseline (an idle run, or an unloaded RNIC in a busy run) re-reads it too (the paired bandwidth recheck, see [Bandwidth recheck](design.md#bandwidth-recheck)): one path per RNIC and distance class, 3 rounds of it and up to 2 references on other RNICs, so at most 9 extra probes per group and 12 groups, 108 probes, per run. Its probes land on RNICs that are not carrying service traffic.

Probe length itself is small: a loopback probe's setup (QP creation, memory registration) dominates its time even at the default 512 timed WRITE pairs, so `--iters` barely changes how long a run holds a path. `--mem-warmup D` does add time: `D` of CPU stores to one NUMA node per memory path, sequentially, so an idle run grows by about `D` × the number of memory paths (RNICs × NUMA nodes), and a busy run by `D` for each memory-path probe it takes. It is off by default; see below for when to turn it on.

### Hosts with dynamic uncore or memory power management

On some platforms the memory subsystem (uncore, memory controller) lowers its operating point while the host is idle and raises it under load. An idle-mode run is exactly the idle case, and a loopback probe is too short to move the operating point reliably, so on such hosts the memory paths (`local_mem`, `remote_mem`) can read at clearly different levels from one idle run to the next while GPU paths stay stable. Typical signs: calibration reports a wide spread or two levels for a memory distance, and repeated idle runs flip memory paths between `normal` and `abnormal` without any change on the host. More `--iters` or a larger `--large-msg` does not help, because the measurement is not short of samples; the hardware is in a different state.

Two ways to handle it:

1. **Fix the platform's uncore/memory frequency** in firmware or with the platform's tools, so idle and loaded hosts measure the same. This is the robust fix.
2. **Try `--mem-warmup`** (e.g. `--mem-warmup 50ms`): each memory-path probe writes its own NUMA-bound buffers from the CPU for that long right before measuring, so the memory subsystem is carrying traffic when the timed WRITEs start. Whether a given platform settles within the warm-up is a property of that platform: compare the spread of repeated idle runs (or `hostpingctl calibrate`) with and without it before relying on it, and calibrate baselines with the same setting the daemon will use. `verb_ns.mem_warmup` shows the time each probe actually spent.

The bandwidth verdict has a guard against the same drift over time. A run reads RNIC by RNIC, so a slow state over the batches of a few consecutive RNICs drops their memory paths below the baseline together, which reads as failed root ports (`cpu_root_port_failure`) and RNIC links (`rnic_pcie_failure`). Before such a verdict is kept, one path of each flagged RNIC and distance class is re-read interleaved with the same class on other RNICs that met the baseline, and the group stays `abnormal` only if the path still reads low against them; otherwise it is `normal` again, or `uncertain` with `host_shift` when it reads level with them but still below the baseline (see [Bandwidth recheck](design.md#bandwidth-recheck)). A `host_shift` that keeps recurring is the platform drifting, not a fault; fixing its power management, as above, removes it. The report's `bw_recheck` shows the numbers.

The latency verdict has its own guard against this drift. Without a `LAT` baseline a path's latency is judged against the median of its siblings, and a run probes RNIC by RNIC, so a slow window over one RNIC's batch would read as a slow RNIC (and an advisory `path_latency_abnormal`). Before such a verdict is kept, the run re-reads the path interleaved with siblings of its class, on other RNICs where it can, and keeps it only if the path still reads high against them; otherwise the path is `normal` with `latency_reason` `recheck_cleared` (see [Latency](design.md#latency)). That recheck costs 6 extra probes per flagged path, at most 16 paths per run, and only in idle runs that flag a path. Paths the locality check below clears anyway are not rechecked. On a host with many RNICs a drift can flag more paths than that: the run then rechecks them in turn across RNICs from a window that moves on by 16 every run, so each flagged path is rechecked within a few runs, and a path left out carries `latency_recheck_skipped` `cap`.

Some platforms drift per NUMA node rather than over time: the small-message latency of every RNIC attached to one node rises together, by about the sibling floor, and stays there for minutes, so the recheck confirms it (its siblings sit on other nodes). The affected node may move between runs, and loading CPU cores may move it rather than remove it. The analyzer therefore compares each sibling verdict the recheck kept (or did not reach) with the same-class paths of the other RNICs on its RNIC's NUMA node, without probing anything: a path that does not read high against them is `normal` with `latency_reason` `locality_shift`, and its `latency_locality` shows the node, the reference and the RNICs it came from. A per-RNIC fault raises one RNIC, not all of a node, so it stays flagged; so does a slow endpoint, which a strict majority of the other NUMA nodes also read as `abnormal`. A shift spanning several nodes at once (every node of one package, say) still leaves the remaining nodes reading the endpoint normally, so it is cleared. The check needs a second RNIC on the node: a host with one RNIC per NUMA node (sub-NUMA clustering often does this) keeps the plain sibling verdict, and so does an RNIC whose NUMA node is unknown. Fixing the platform's power management, as above, removes the drift itself.

What both checks leave is now and then a single path that reads high in one idle run, holds up in its recheck, and is not shared by its node, while the next run flags a different path or none. A per-path latency fault does not come and go between runs, so the daemon reports a sibling verdict only after the same path read `abnormal` in 3 consecutive idle runs, so the cause appears on the third of them, about two idle intervals after the first run that sees the rise: each path's `latency_streak` in the report counts them, a run that reads it normal or does not judge it resets it, and busy runs leave it alone. A run whose recheck cap left the path out holds its streak and reports nothing for it, since no recheck confirmed that run's reading; the rotating window reaches it in a later run. A path below 3 keeps `latency_status` `abnormal` (and `hostping_path_latency_status_code` 3) but gets no cause. A discarded idle run rolls the streaks back, and `--state-dir` keeps them across a restart. A one-shot run cannot see persistence, so it reports a sibling verdict only when the paired recheck confirmed it; a verdict against a `LAT` you set needs neither.

## Calibrating baselines

Idle runs judge each path against a per-distance bandwidth baseline. The Fig.4
defaults are scaled to `min(line rate, PCIe hop)`, but hosts of different
hardware classes (NUMA layout, PCIe switch tiers, GPU generation) reach
different levels, so a fleet should run on baselines measured on its own
hardware. `hostpingctl calibrate` measures them; nothing about it is verified
on RDMA hardware yet.

1. **Pick healthy hosts of one hardware class**, idle, with the daemon
   stopped: calibrate and the daemon do not coordinate, and each would read
   the other's probe traffic as load.
2. **Calibrate each host.** `hostpingctl calibrate --out $(hostname).json`
   performs `--runs` (default 5) idle-mode runs `--interval` (default 30 s)
   apart, with the same flags as `diagnose`. It refuses to start on a busy
   host or one whose idle state cannot be read. A run that the host became
   busy during is discarded and retried, as the daemon discards one; after
   more than `--max-discards` (default 3) discards it gives up. The file
   goes to `--out` or stdout; a table with the spread of the readings behind each number goes to
   stderr. Read the warnings before going on: a path more than
   `--abnormal-th` below its distance's calibrated value will diagnose
   `abnormal` against the file, which on a host believed healthy is either a
   fault nobody knew about or the wrong host to calibrate on. A distance
   that reads at two levels (a gap larger than `--abnormal-th` between its
   sorted per-path medians, at least two paths on each side) is reported
   one of two ways; see [Two levels in one distance](#two-levels-in-one-distance).
   RNICs that derive different Fig.4 tables are calibrated per table; see
   [RNICs that derive different Fig.4 tables](#rnics-that-derive-different-fig4-tables).
3. **Merge.** `hostpingctl calibrate merge --out class.json host*.json`
   takes the lower quartile of bandwidth (and the upper quartile of any
   latency) across hosts and warns about any host that would diagnose
   abnormal against the result, which usually means it belongs to another
   hardware class. Learned sub-classes are merged by name: a root-port pair
   is in the merged sub-class when at least half of the files that hold its
   distance list it, and every pair the files disagree on is named in a
   warning. Hosts of one board layout should agree; a disagreement is either
   a calibration the majority overrules or a host of another layout.
   Per-table sections are merged by key, and a section only some files
   have is named in a warning.
4. **Distribute** `class.json` to that class and pass it with
   `--baseline-file` (for example in `$HOSTPING_OPTS`). At start-up the
   daemon fails on a file that does not parse, and logs which distances take
   their bandwidth and latency from the file, which keep the Fig.4
   default, which sub-classes the file sets, and which of the host's Fig.4
   tables take a per-table section. A sub-class root-port pair
   the host does not have is ignored, and one start-up line lists every
   such pair.
5. **Recalibrate after hardware, firmware or BIOS changes** (RNIC or GPU
   swap, PCIe or NUMA settings, RNIC firmware): the file is absolute, and an
   out-of-date floor either hides a regression or reports one that is not
   there.

A distance no path of the host measured (Fig.2 has no `multi_switch` path,
for example) is left out of the file and keeps its Fig.4 default. Latency is
off unless `--latency` is given: a latency set in the file is a strict
reference without the 500 ns floor the sibling comparison has, so a
calibrated latency flags smaller increases. Prefer leaving it out until the
sibling comparison has proved too loose. Calibration writes each run's first
readings and skips the paired latency and bandwidth rechecks, which change
only verdicts calibration does not read. A latency it writes is drawn from those first
readings, slow time windows included, and a verdict against it, like any
`LAT` you set, is not rechecked.

### Two levels in one distance

On some platforms the P2P route between certain pairs of root ports costs
nothing while between others it halves the bandwidth, so `cross_root` (or
another distance between root ports) holds two populations on healthy
hardware, and nothing in sysfs tells them apart. One per-distance
baseline sits at one level, and when most paths are slow, a fast path that
degrades to the slow level stays `normal`. Calibrate handles this by keying
on the unordered pair of root ports each path crosses:

- **The split follows root-port pairs** — every pair's paths are all fast
  or all slow, and every fast path crosses two root ports. Calibrate writes
  the fast cluster as a learned sub-class `fast` of the distance, listing
  the fast pairs, and takes the distance's own value from the slow paths
  only. The table then shows a `cross_root/fast` row next to `cross_root`
  (each with the spread over its own paths), a `sub-class cross_root/fast:
  paths across root-port pairs …` line, and a `NOTE: … wrote sub-class`
  line. Against that file a fast path is judged against the fast level, so
  the drop is caught; the report names the sub-class in the path's
  `baseline_class`.
- **It does not** — a pair has paths at both levels, or a fast path crosses
  no pair of root ports. No sub-class is written and the distance keeps one
  value over all its paths. The `WARNING: … reads at two levels` names the
  fast paths, says which level the value sits at, and lists why the split
  could not be keyed (the mixed pairs, or the fast paths without a pair).
  The blind spot remains for that distance; check the fast paths by hand.

Root-port IDs are BDF-based (`rp.<BDF>` from discovery), so a sub-class is
portable between hosts with the same board layout and no further. Calibrate
and merge per hardware class as above; on a host whose layout differs, the
pairs it does not have are ignored (one start-up line lists them) and its
paths fall back to the distance's value. A hand-edited file may define
sub-classes too; see [Baselines](design.md#baselines) for the schema. Only the fast cluster
becomes a sub-class; the slow paths stay with the distance.

### RNICs that derive different Fig.4 tables

A host can carry RNICs that derive different Fig.4 tables (line rate and
PCIe hop), for example a lower-line-rate RNIC in a narrow slot next to faster
RNICs on wide links. The slower RNIC reads lower at every distance, so one
value per distance cannot serve both: calibrated on the majority, it puts
every path of the slower RNIC below its distance's baseline, and that RNIC is
diagnosed on every run.

Calibrate therefore groups the measured paths by their RNIC's table and
computes every distance (and sub-class) per group. The group with the most
measured RNICs (on a tie, the first table key in sort order) is written at
the top level of the file, exactly as on a host with one table, so a
single-table host's file does not change. Every other group goes under
`"tables"`, keyed by its table:

```json
{
  "local_mem": {"bandwidth_bps":198000000000},
  "remote_mem": {"bandwidth_bps":191000000000},
  "tables": {
    "line_rate=100Gbps,pcie_hop=64Gbps": {
      "local_mem": {"bandwidth_bps":61000000000},
      "remote_mem": {"bandwidth_bps":59000000000}
    }
  }
}
```

The key is `line_rate=<N>Gbps`, plus `,pcie_hop=<N>Gbps` when the hop
capacity is known, in Gb/s rounded to two decimals without trailing zeros —
the same pair the `hostping baseline line_rate=…` log line keys its tables
by. The loader accepts a key only in exactly that spelling. The calibrate
table prints the top level's table and RNICs, one table per section, and a
`NOTE` naming the tables; outliers and two-level warnings of a section are
prefixed with `tables <key>`.

An RNIC whose table has a section takes that section as a whole: a distance
the section leaves out keeps the RNIC's own Fig.4 default, not the top-level
value, which was measured on another table. Every other RNIC takes the top
level, as before. `--baseline` flags override the top level and every
section alike. The latency sibling comparison draws siblings only from RNICs
that take the same section. At start-up the agent logs one `baselines:` line
per section with the distances it sets, and one `baselines: per Fig.4 table:`
line naming each of this host's tables, its RNICs, and whether it takes the
top level or a section (and any section matching no RNIC, which is ignored);
each run's `hostping baseline` line for a table that uses a section says so.

`calibrate merge` merges each section by key, over the files that have it,
with the same quartile and sub-class rules as the top level, and warns about
a section only some files have: hosts of one hardware class should derive
the same tables.
