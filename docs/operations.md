# Operating hostping

How to run the `hostping` daemon as a service: what to install, what the host
must grant it, what each data source reports when it is not available, and what
a run costs the workload.

The systemd unit in [`contrib/systemd`](../contrib/systemd) has not yet been
run under systemd on RDMA hardware. The privilege statements below come from
the code unless they say otherwise. A statement marked "not verified" has not
been verified.

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

The commands below use the `bin/linux-amd64/` paths. From a tarball, take the
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
`/opt/hostping/current` on `PATH`. A copy elsewhere would not find the library.

Discovery reads `/sys/class/infiniband` and the PCI tree once, at start. A
daemon started before the RDMA driver has registered its devices exits with a
`sysfs discovery` error, and `Restart=on-failure` retries it every 30 s. An RNIC
or GPU that appears later, or a driver reload, needs a restart.

Before enabling the daemon, run `hostpingctl topology` to see which devices
discovery takes as RNICs. Every device under `/sys/class/infiniband` except
bonding devices is probed. That includes an HCA that is not a data-path NIC,
such as one through which a host manages its GPU fabric's switches over an
InfiniBand subnet of its own. Probing that HCA sends RDMA traffic into the
fabric's control plane.

Discovery logs a warning naming `--exclude-rnic` for an InfiniBand RNIC that
looks like such an HCA: its subnet manager runs on the host, or its IPoIB
netdev is administratively down while the port is ACTIVE. Exclude such a
device in `/etc/default/hostping` before `systemctl enable`. See
[Excluding RNICs](design.md#excluding-rnics---exclude-rnic).

RNIC IDs (`rnicN`) are positions in the sorted device list and shift when a
device is excluded, added or removed. Key dashboards and alerts on `device` or
`pcie_bdf` instead of the ID, by joining `hostping_rnic_info` (see
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
loopback probe registers two host buffers of `max(--large-msg, 4 KiB)` each:
512 KiB at the default and 32 MiB at the 16 MiB cap. Probes run one at a time.
The mutual probe registers a small buffer per RNIC, about 0.5 MiB at the
4 KiB `--mutual-msg` cap.

The kernel charges host MRs, and the queue buffers the verbs provider
registers, against the limit unless the process holds `CAP_IPC_LOCK`, which
root does. A refused registration fails that probe as a setup error, so the
endpoint shows up in `skipped_endpoints` with the probe's reason instead of as
a measurement. The unit sets `LimitMEMLOCK=infinity` so a non-root deployment
does not hit this. Whether GPU MRs are charged depends on the driver path and
is not verified.

### Running as a dedicated user

Verified: a user with access to the RDMA and NVIDIA device nodes can run memory
and GPUDirect RDMA loopback probes. To run that way, create the user and
uncomment `User=`/`Group=` in the unit, plus `SupplementaryGroups=` if your
distribution restricts the device nodes to a group. What is lost:

- **ACS / ATS become unknown.** For an unprivileged reader the kernel returns
  only the first 64 bytes of a device's `config` file, and ACS and ATS are
  PCIe extended capabilities (offset 0x100 and up). hostping then has
  `ACSKnown`/`ATSKnown` false for every device, so `acs_enabled` and
  `ats_disabled` are never reported. Nothing is logged about it. Slow
  GPUDirect paths are still classified and localized by Algorithm 1. An
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
  ignores the `$ORIGIN` rpath. In that case, register the directory holding
  `libhostping_probe.so` with `ldconfig` (`/etc/ld.so.conf.d/`), and set the
  capability again after replacing the binary.
- **Memlock** applies, since the user has no `CAP_IPC_LOCK`. Keep
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
`ProtectKernelLogs`. Enable any of them only after checking on your hardware
that memory loopback, GPU loopback, `nvidia-smi` and `ethtool -S` still work.

## Configuration

Everything is a flag, plus the standard `OTEL_EXPORTER_OTLP_*` variables (see
[Metrics endpoint](#metrics-endpoint)). The unit reads
`/etc/default/hostping` and passes `$HOSTPING_OPTS` on the command line; see
the commented example in
[`contrib/systemd/hostping.default`](../contrib/systemd/hostping.default).
systemd splits the variable on whitespace and does not parse quotes inside it,
so no value may contain a space. To disable a listener, write `--metrics=`, not
`--metrics ""`.

A flag the binary rejects exits non-zero, and `Restart=on-failure` retries it
every 30 s with the same flags. Check a new option set before restarting:

```bash
set -a; . /etc/default/hostping; set +a
/opt/hostping/current/hostpingctl topology $HOSTPING_OPTS >/dev/null
```

This parses every flag and runs discovery, including `--mem-channel-capacity`
link IDs, without probing. It does not read `--baseline-file` or check the
`--mutual-interval` / `--mutual-transport` combination.

`hostpingctl diagnose` and the daemon do not coordinate. Running a live
`diagnose` while the daemon is up puts both runs' probe traffic on the same
RNICs, and each reads the other's as host load.

## What "unknown" means per data source

A source that cannot be read is unknown, and unknown is never read as healthy
or idle. Where a source has no unknown state, the table says so.

| Source | Unavailable when | What hostping does |
| --- | --- | --- |
| RNIC discovery | No RDMA driver, no `/sys/class/infiniband` | Exits at start with a `sysfs discovery` error. The unit retries every 30 s. |
| RNIC line rate (`ports/N/rate`) | Unreadable | No idle run and no `--busy-interval` run (the loaded set cannot be classified). GPU paths of that RNIC are `uncertain` with `no_ceiling`. |
| RNIC byte counters | Neither IB port counters nor netdev statistics readable | Every poll fails with `monitor: no RNIC counters readable for <dev>` and no run starts. |
| PFC pause duration, drops: never found | InfiniBand link layer (no PFC; `ethtool` is not run), or no vendor `*pause*duration*` sysfs counter and no `ethtool -S` answer since start | **Not applicable**, logged once per RNIC: `hostping: rnic3 has no PFC pause-duration source (…); the PFC trigger cannot fire for it`. The reason says `ethtool not on PATH` when that is the fix. The trigger cannot fire for that RNIC and it does not block the idle verdict. `hostping.rnic.pause_duration_ratio` / `hostping.rnic.drop.delta` are absent for it, not 0. A source that appears later is logged and used from then on. |
| PFC pause duration, drops: lost | A source read before is gone: the file vanished or is unreadable, or `ethtool -S` fails, times out or is backed off | **Unknown**, logged once per change. The host is not idle: no idle run happens, and a live `hostpingctl diagnose` refuses. No ratio or delta is computed and the gauge is not written. The exporter keeps its last measured value, so the log line is the signal. PFC/drop runs from other RNICs and `--busy-interval` still happen. When the source reads again, the first sample only re-baselines, so the jump does not trigger a run. A counter that is present and reads 0 is known. |
| GPU utilization | No `nvidia-smi` on `PATH` and no DRM `gpu_busy_percent` | The GPU is unknown, so the host is never idle and no idle runs happen. PFC/drop busy runs still happen. A live `hostpingctl diagnose` refuses to run. |
| CPU load of a NUMA node | `/proc/stat` unreadable | The host is never idle while it is unread. No idle runs happen, a held idle-mode run is discarded (`idle_unknown`), and a live `hostpingctl diagnose` refuses to run. Logged once when it fails and once when it reads again; that first read only re-baselines. No `hostping.cpu.busy` sample is written, never a 0. The exporter keeps its last measured value. |
| GPU compute mode | No `nvidia-smi`, AMD GPU, `[N/A]` | The GPU is probed as if `Default`. |
| GPU loopback (GDR) | No GPU device nodes, CUDA/HIP library, or `nvidia-peermem`/dma-buf | GPU endpoints are skipped with the probe's reason, reported through one log line per change, `skipped_endpoints` and `hostping.endpoint.skipped`. Memory paths still diagnose. |
| Memory loopback | Device node, MR registration (memlock) or NUMA binding fails | Same skip channels, kind `memory`. |
| Loopback port address | RoCE: no active-port RoCEv2 GID. InfiniBand (`ports/N/link_layer`): no ACTIVE port, or an ACTIVE port with LID 0 (no subnet manager) | Every endpoint of that RNIC is skipped through the same channels, with reason `no active rocev2 gid`, `infiniband port not active` or `infiniband port has no lid`. The RNIC's fabric is `unknown`. The addressing a probe did use is logged once per RNIC and on change: `hostping: loopback addressing rnic=rnic0 device=… link_layer=InfiniBand mode=ib_lid port=1 lid=…`. The log shows `mode=ib_lid_grh` when the LID-only connect was refused or its WRITEs completed with a remote error, and `mode=rocev2_gid` on RoCE. The mode that worked is remembered per device and port. A WRITE that fails in every mode is skipped as `probe failed: Wc: <ibv_wc_status_str> (<status>)`, e.g. `transport retry counter exceeded (12)`. The same text names RoCE completion errors. Port-level failures on InfiniBand are described after the table. |
| Mutual probe | No active-port RoCEv2 GID or usable InfiniBand LID, or no RNIC pair in `--mutual-scope` | No pairs, logged once per fabric set, and no `hostping.mutual.*` series. InfiniBand RNICs are left out in every scope unless `--mutual-infiniband` is set (logged once: `hostping: mutual probe leaves out N InfiniBand RNIC(s) […]`). With it, they pair under `rail` by subnet prefix (`ib:` key). Separate fabrics left at the default prefix `fe80::/64` then look like one rail and time out as false connectivity failures, so opt in only when the host's InfiniBand ports share one fabric. RoCE and InfiniBand RNICs never pair. |
| PCIe link speed / width | Unreadable `current_link_speed` | No `rnic_pcie_failure` / `gpu_pcie_failure` from the negotiated link of that device. A hop whose maxima are unreadable is assumed Gen4 x16 and marked `capacity_assumed`. |
| ACS / ATS | Not root (see above), or the device has no such capability | `acs_enabled` / `ats_disabled` not reported. Not logged. |
| GPU PCIe utilization | No `nvidia-smi` | `pcie.<gpu>` utilization unknown. |
| Memory utilization | No `--uncore-util`, no `--mem-channel-capacity`, no perf access, AMD, sub-NUMA clustering whose start-up IMC mapping was not clear (a host loading memory hard while the agent starts) | `mem.N` unknown. The reason is logged at start. |
| Inter-socket utilization | No `--uncore-util`, no `--upi-capacity`, no perf access, AMD, a processor whose UPI flit size is not measured (without `--upi-bytes-per-flit`), more than two packages and a UPI link whose `die<N>` file is missing | `upi.A-B` unknown. The reason is logged at start. |
| CPU package of a NUMA node | A node's `cpulist` or a CPU's `topology/physical_package_id` unreadable | Every NUMA node is modeled as its own socket: no `mesh.A-B` links, and `upi.A-B` between every two nodes. Logged once at start. On a sub-NUMA-clustered host this puts the NUMA nodes of one package behind UPI links again. |
| Package mesh utilization and capacity (`mesh.A-B`) | Always | Unknown. An abnormal mesh link is `snc_mesh_failure`. |

An unknown utilization is absent from the report's `utilization`, writes no
`hostping.bus.utilization` sample and is never taken as overloaded.

A loopback WRITE that fails in every mode on an InfiniBand RNIC may fail with a
port-level error: retry counter exceeded, a remote error, a bad response or
response timeout. If it does, and none of that RNIC's probes has answered yet
in the run, the RNIC's remaining endpoints are
not probed in that run, because each would wait out the hardware retry budget
once per mode. They are skipped through the same channels with reason
`not probed: an earlier loopback on this RNIC failed: <first failure>`, so
`skipped_endpoints` and `hostping.endpoint.skipped` still count every
endpoint. The next run probes the port again. An RNIC that answered earlier in
the run, a local completion error, a setup failure and a RoCE port are still probed
endpoint by endpoint.

## Metrics endpoint

`--metrics` (default `:9464`, every interface) serves `/metrics` (Prometheus)
and `/healthz` (see [Liveness and staleness alerts](#liveness-and-staleness-alerts)).
By default it is plain HTTP with no authentication, so bind it to the
management address (`--metrics 10.0.0.10:9464`) or firewall the port. On a
host with data-plane and management networks, the default listens on both.

`--metrics-web-config FILE` adds TLS and/or basic authentication, using the
[Prometheus exporter-toolkit web configuration](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md)
(the same file format as `node_exporter --web.config.file`). The file is
checked at start. Certificates are re-read on each new TLS connection, so a
renewed certificate needs no restart.

`/healthz` sits behind the same TLS and basic auth as `/metrics`, because
exporter-toolkit wraps the whole listener. A health checker must present the
same credentials. One that cannot should check the process
(`systemctl is-active`) instead.

An example web configuration:

```yaml
# /etc/hostping/web.yml
tls_server_config:
  cert_file: /etc/hostping/tls.crt
  key_file: /etc/hostping/tls.key
basic_auth_users:
  # bcrypt hash, e.g. from `htpasswd -nBC 10 "" | tr -d ':\n'`
  prometheus: $2y$10$...
```

The listener is bound before the daemon starts working. An address that cannot
be bound (in use, or not configured on the host) or a web config that does not
load is a start-up error, and `Restart=on-failure` retries it every 30 s. A
slow client cannot hold a connection open. The server times out reading
headers (10 s), the request (30 s), writing the response (60 s) and idle
keep-alive connections (2 min).

`OTEL_EXPORTER_OTLP_ENDPOINT` (or `--otlp`) pushes the same metrics over OTLP
gRPC every 10 s. The endpoint is `host:port`, or a URL whose scheme decides the
transport (`http://` is plaintext, `https://` is TLS). For `host:port`, the
transport is chosen by the first of these that applies:

1. `--otlp-insecure` (`--otlp-insecure=false` forces TLS);
2. `OTEL_EXPORTER_OTLP_METRICS_INSECURE`, then `OTEL_EXPORTER_OTLP_INSECURE`;
3. TLS when a CA or client certificate is configured (below);
4. plaintext when the host is loopback (`localhost`, `127.0.0.0/8`, `::1`),
   TLS with the system roots otherwise.

The exporter also reads the standard `OTEL_EXPORTER_OTLP_CERTIFICATE` (CA
bundle), `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` / `_CLIENT_KEY` (mutual TLS),
`OTEL_EXPORTER_OTLP_HEADERS` (e.g. an `authorization` header for a hosted
collector), `OTEL_EXPORTER_OTLP_TIMEOUT` and `OTEL_EXPORTER_OTLP_COMPRESSION`,
and their `_METRICS_` variants. Settings that contradict each other, such as a
plaintext setting with a certificate, or `--otlp-insecure` against the env var
or the URL scheme, are a start-up error instead of one silently winning. The
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
  max(10 × `--metric-interval`, 30 s), which is 30 s at the default 1 s
  interval. A poll that returned an error still counts, because the loop is
  alive. The error is counted in `hostping_monitor_poll_errors_total`.
- **200** while an engine run is in progress, for up to 30 minutes. The loop
  runs the engine synchronously, so no poll completes during a run, and a full
  mesh whose probes time out one by one can take minutes. The end of a run
  counts as a tick.
- **503** with the reason otherwise, e.g. `no monitor poll completed for 41s
  (limit 30s)` or `engine run in progress for 31m0s (limit 30m0s)`.

Start-up counts as the first tick, which covers the warm-up before the first
poll. One-shot runs have no poll loop. Their `/healthz`, served only when
`--metrics` is given, is always 200.

`/healthz` does not depend on runs succeeding. A host that is never idle never
starts an idle run, and restarting the daemon does not fix a failing probe.
Whether hostping is still producing diagnoses is a question for alerting on
these series (names as Prometheus renders them):

| Series | Meaning |
| --- | --- |
| `hostping_engine_last_run_timestamp_seconds` | End of the last engine run, whatever its outcome |
| `hostping_engine_last_success_timestamp_seconds` | End of the last run whose report was confirmed and written |
| `hostping_engine_run_failed_total{stage}` | Runs that returned an error (`stage="engine"`) or whose report could not be written (`stage="report"`) |
| `hostping_engine_run_discarded_total{reason}` | Idle-mode runs discarded because the clean poll after them found the host busy (`reason="host_busy"`) or could not classify it (`reason="idle_unknown"`) |
| `hostping_engine_idle_gate_skipped_total{reason}` | Idle runs `--idle-gate-command` kept from starting: it exited 1 (`reason="denied"`), timed out (`reason="timeout"`), or exited 2 or higher or could not run (`reason="error"`). Exported only with a gate ([Keeping idle runs out of scheduler jobs](#keeping-idle-runs-out-of-scheduler-jobs)) |
| `hostping_monitor_poll_errors_total` | Monitor polls that returned an error |
| `hostping_monitor_last_poll_timestamp_seconds` | Last monitor poll that completed without error |
| `hostping_monitor_host_idle` | 1 when the last clean poll found the host idle, 0 when busy or unclassified |

The counters are exported as 0 from the first scrape. The timestamps are
absent until their first event, and again after a restart. If a host that
never succeeds even once matters to you, an alert on them should also cover
`absent()`.

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

Use `hostping_monitor_host_idle`, not `hostping_host_idle`, in the first
expression. `hostping_host_idle` is the classification of the last *report*,
so it keeps reading 1 on a host that turned busy after an idle run and has
not been diagnosed since.

A steady `hostping_engine_run_discarded_total` increase means a workload keeps
coming back during idle runs. That is expected on a host that pauses between
iterations. If it never stops, no idle diagnosis is being made.

With `--idle-gate-command`, a host whose gate keeps denying (exit 1) makes no idle run
and no diagnosis, by design. Leave such hosts out of the first expression with
`unless on (job, instance) increase(hostping_engine_idle_gate_skipped_total[15m]) > 0`,
as the shipped rules do.

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
has no `hostping_rnic_info` series. The values are fixed at start-up. A
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
mutual series name RNICs as `src` and `dst`. Join each side with
`label_replace`, for example `label_replace(hostping_rnic_info, "src", "$1",
"rnic", "(.*)")`. The diagnosis counter has no RNIC label, so the bundled
alerting rules do not join. The report names the devices of each cause.

## Status codes

Path and link states are exported as codes. Each path or link has one series,
and its value is the code of the current state. Every report rewrites the value
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

**The codes are fixed.** They come from a table in
`internal/telemetry/statuscode.go`, not from the position of a value in its Go
enum, and a test pins them. A state added later gets the next unused code of
its gauge. An existing code is never renumbered, and the code of a removed
state is not reused. Codes are names and do not form a scale. Compare them with
`==`, never with `<` or `>`, even though the three status gauges happen to
share 1 for healthy and 3 for abnormal.

**Zero.** A status always has a value, so status codes start at 1 and 0 is not
a status. A query that fills a missing series with 0 (`or vector(0)`, a
dashboard's null-as-zero), or a backend that defaults an unwritten gauge,
cannot make it read as a state. Reason codes use 0 for `none`, the reason of a
path that needs none, so `!= 0` reads "has a reason". A value of -1 means
hostping wrote a state its table has no code for. The tests rule that out, so
it is a bug to report.

`hostping_status_code_info` names every code. It has one series per
`(metric, code)` (28 in all), always 1 and exported from the first scrape.
`metric` is the OpenTelemetry instrument name with dots
(`hostping.path.status_code`), the same on every backend. Rows of a status
gauge carry a `status` label, and rows of a reason gauge carry `reason`:

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

A filter like `== 3` keeps the code as the value. Count the result
(`count by (instance) (...)`) instead of summing it.

To get names instead of numbers, turn the code into a label with
`count_values` and join it on the info gauge. `count_values` keeps only the
labels it groups by, so list the ones that identify a path or link:

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

The status gauges hold the verdict of the last report until the next one.
Use them to locate a hop or path, not to page. See
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

`hostping_link_status_code` adds `"4": {"text": "gray", "color": "orange"}`.
The reason gauges map their codes from the table above in the same way. A
mapping is copied into each dashboard and does not pick up a new code. A panel
that should name every state as states are added can query the `count_values`
join above instead and use `{{status}}` (or `{{reason}}`) as the legend.

### Migrating from 0.1.1

0.1.1 exported one 0/1 series per possible state (`hostping_path_status`,
`hostping_path_latency_status`, `hostping_link_status`). 0.1.2 replaces them
with the code gauges, and there is no option to keep the old series. Rewrite
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

The old expressions return 1 per matching path. The new filters return the
code, which matters only to `sum()`. 0.1.1 had no latency series for a few
(status, reason) pairs the classifier does not produce. With separate status
and reason gauges, every pair is representable. After the upgrade, the old
series stop being scraped and go stale after Prometheus's 5-minute lookback.

## Topology graph

Two more info gauges describe the host's intra-host topology as a graph, for
Grafana's built-in Node Graph panel. They are always 1, fixed at start-up from
the discovered graph (or the `--sim` / `--topology-file` one), and exported
from the first scrape. RNICs dropped by `--exclude-rnic` are not in it.

| Series | Labels |
| --- | --- |
| `hostping_topology_node_info` | `node`, `kind`, `title`, `socket`; `numa` where the node has one NUMA node; `pcie_bdf` on PCI nodes (root port, switch, RNIC, GPU) |
| `hostping_topology_edge_info` | `edge`, `link`, `source`, `target`, `kind`, `capacity_bps`, `negotiated_bps`, `capacity_assumed` |

Nodes, by `kind`:

| `kind` | `node` | `title` |
| --- | --- | --- |
| `socket` | `socketN` | `socket N` |
| `numa` | `numaN`, only on a package split by sub-NUMA clustering | `NUMA N` |
| `memory` | `memN`, as `hostping_endpoint_info` | `memN (NUMA N)` |
| `root_port` | `rp.<BDF>`, the root-port link's ID | `root port <BDF>` |
| `switch` | `sw.<BDF>` of the switch's upstream port, the switch link's ID | `switch <BDF>` |
| `rnic` | `rnicN`, as `hostping_rnic_info` | IB device (`mlx5_N`) |
| `gpu` | `gpuN`, as `hostping_endpoint_info` | `gpuN <BDF>` |

The simulator's root ports and switches have no PCI address (`rp.0`, `sw.0`).
Their `pcie_bdf` is `unknown`, and their title names the ID.

Each link of the graph is one edge, with `edge` and `link` both set to the
link ID that `hostping_link_status_code`, `hostping_link_abnormal_count` and
`hostping_bus_utilization` carry as `link`. An edge points from the CPU side
(`source`) to the device side (`target`), and between two packages or two NUMA
nodes from the lower number to the higher. `kind` is the link kind:

| `kind` | `source` → `target` |
| --- | --- |
| `root_port` | package (or NUMA node) → root port |
| `pcie_switch` | root port or upper switch → switch |
| `pcie_rnic`, `pcie_gpu` | switch, or root port when no switch is between them → RNIC or GPU |
| `memory` | package (or NUMA node) → memory node |
| `upi` | package → package |
| `snc_mesh` | NUMA node → NUMA node of the same package |
| `package` | package → NUMA node; a structural edge with `link="none"` and capacity 0 |

Switches follow the sysfs parent chain. A cascaded switch (two-level PEX890xx)
hangs off the upper switch, and devices on different downstream ports of one
switch share it as their source. Without a `pcie_type` attribute every bridge
port is its own switch node, chained the same way. A `--topology-file` written
before 0.1.4 has no `upstream` on its links. Its devices then hang off their
`switch` field, or their root port.

Consecutive links of a path share a node, so `link_ids` of a report's path
walk the edges from the RNIC to the endpoint. Under sub-NUMA clustering a path
that leaves the package also crosses a `package` edge it does not list. A
DMA between two devices under one switch turns at that switch and does not
cross the edges above it.

### Grafana Node Graph

The panel takes two data frames: nodes and edges. Use two queries against the
Prometheus data source, both **Format: Table** and **Type: Instant**:

- A (nodes): `hostping_topology_node_info{instance="$instance"}`
- B (edges): `hostping_topology_edge_info{instance="$instance"}`

The panel picks up a frame by its field names. Add an **Organize fields by
name** transformation per query (**Filter data by query refId** A, then B) that
hides `Time`, `Value`, `__name__`, `job`, `instance` and the `otel_scope_*`
labels and renames:

| Query | Field | Rename to |
| --- | --- | --- |
| A | `node` | `id` |
| A | `title` | `title` |
| A | `kind` | `subtitle` |
| A | `pcie_bdf` | `detail__pcie_bdf` |
| A | `numa` | `detail__numa` |
| B | `edge` | `id` |
| B | `source` | `source` |
| B | `target` | `target` |
| B | `kind` | `detail__kind` |
| B | `capacity_bps` | `secondarystat` |

To color the edges by link state and show it as their main stat, join the
edges to `hostping_link_status_code`. Make B the join instead of the raw info
series:

```promql
hostping_topology_edge_info{instance="$instance"}
  * on (instance, link) group_left
    hostping_link_status_code{instance="$instance"}
```

Its value is the link's status code. Structural edges and links no run has
reported yet drop out of the product. Rename `Value #B` to `mainstat` and apply
the value mappings of [Grafana](#grafana) (1 green, 2 yellow, 3 red, 4 orange)
to it. To color by utilization, join `hostping_bus_utilization` the same way;
only hops with a live source have one. The node frame can color by kind with a
`color` field from `label_replace` or a value mapping on `subtitle`. A table of
the raw info series is often enough to check a host:

```promql
# Every switch with what hangs off it.
count by (instance, source) (hostping_topology_edge_info{kind=~"pcie_.*"})
  * on (instance, source) group_left (title)
    label_replace(hostping_topology_node_info{kind="switch"}, "source", "$1", "node", "(.*)")
```

## Alerting rules

[`contrib/prometheus/hostping-rules.yml`](../contrib/prometheus/hostping-rules.yml)
packages the expressions above. It also has alerts on diagnoses and on
skipped endpoints, and per-host counts of path and link states by name
([Status codes](#status-codes)). Its unit tests are in
`hostping-rules.test.yml`:

```bash
promtool check rules contrib/prometheus/hostping-rules.yml
promtool test rules contrib/prometheus/hostping-rules.test.yml
```

The rules assume the scrape job is named `hostping`. A diagnosis alerts only
after it appears in two engine runs within 30 minutes. High-confidence
hardware faults page, other findings open a ticket, and advisory codes are
`info`. `rnic_cross_rail_unreachable` at `low` does not alert. The
[runbook](runbook.md) explains the policy
([Paging policy](runbook.md#paging-policy)) and has one section per code and
alert.

## Series cardinality

This section lists what one host exports. It is derived from the instruments
in `internal/telemetry` and checked against a scrape of the `--sim` Fig.2
host. Notation:

| Symbol | Meaning |
| --- | --- |
| R | RNICs |
| E | Loopback endpoints: GPUs (E_gpu) plus NUMA memory nodes (E_mem) |
| P | Measured RNIC→endpoint paths, at most R × E: P_gpu to GPUs, P_mem to memory |
| L | Topology links: one `pcie.<rnic>` per RNIC, one `pcie.<gpu>` per GPU, plus root ports, switches, `mem.N`, inter-socket links and, under sub-NUMA clustering, `mesh.A-B` links between NUMA nodes of one package (`hostpingctl topology` lists them) |
| K | Topology graph nodes besides RNICs and endpoints: CPU packages, root ports, switches and, under sub-NUMA clustering, NUMA nodes |
| S | NUMA nodes of packages split by sub-NUMA clustering (0 without it): one structural `package` edge each |
| M | Mutual pairs: unordered RNIC pairs on a shared rail under `--mutual-scope=rail` (0 on a rail-optimized host), R × (R − 1) / 2 under `--mutual-scope=host` |

| Series | Count | Labels that multiply |
| --- | --- | --- |
| `hostping_loopback_latency_microseconds`, `hostping_loopback_bandwidth_bit_per_second`, `hostping_path_abnormal` | 3 P | `rnic` × `endpoint` |
| `hostping_path_status_code`, `hostping_path_uncertain_reason_code`, `hostping_path_latency_status_code`, `hostping_path_latency_reason_code` | 4 P | `rnic` × `endpoint`; the state is the value, not a label |
| `hostping_path_latency_streak` | P | `rnic` × `endpoint` |
| `hostping_verb_duration_microseconds` | 3 P_gpu + 2 P_mem | `verb`: `gpu_alloc` on GPU paths only, `mr_reg`, `qp_setup`; `mem_warmup` on memory paths with `--mem-warmup`; none with `--metrics-verb-durations=false` |
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
| `hostping_topology_node_info` | R + E + K | `node` (the other labels are fixed per node) |
| `hostping_topology_edge_info` | L + S | `edge` (the other labels are fixed per edge) |
| `hostping_status_code_info` | 28 | `metric` × `code`: fixed by the code tables, whatever the host |
| `hostping_engine_idle_gate_skipped_total` | G | `reason`: 3 with `--idle-gate-command`, else 0 |
| Liveness: `hostping_host_idle`, `hostping_monitor_host_idle`, three `*_timestamp_seconds`, `hostping_engine_run_failed_total` (2 `stage`), `hostping_engine_run_discarded_total` (2 `reason`), `hostping_monitor_poll_errors_total`, `hostping_build_info` | 11 | |

Per host:

```
series ≈ 11 P_gpu + 10 P_mem + 3 L + U + 3 M + 5 R + 2 E + K + S + N + 41 + D + T + G
```

U ≤ L is the number of links with a utilization source. The Prometheus client
adds about 50 Go runtime, process and `target_info` series on top. The
constant 41 is the 11 liveness series, the 2 `hostping_endpoint_skipped`
series and the 28 `hostping_status_code_info` series.

The path terms dominate. Each RNIC→endpoint pair costs about 11 series, so a
host grows with R × E. `--metrics-verb-durations=false` drops
`hostping_verb_duration_microseconds`, the 3 P_gpu + 2 P_mem of those terms,
leaving 8 series per path (the Fig.2 sim host below scrapes 499 instead of
611). The probes still time the verbs, and the report's `verb_ns` and the
`gpu_slow_mr_reg` diagnosis keep using them. Prometheus adds its own `job` and `instance` labels, and
the exporter adds the constant `otel_scope_*` labels. None of those multiply.
The endpoint skip reason (free-form probe text), a path's baseline sub-class
and a cause's subjects (which GPU, which link) are not labels.

Worked example: the Fig.2 host of `--sim`.

- R = 4.
- E = 10, with 8 GPUs and 2 memory nodes, so P = 40 with P_gpu = 32 and
  P_mem = 8.
- L = 23: 4 RNIC PCIe, 8 GPU PCIe, 4 switch, 4 root port, 2 memory,
  1 inter-socket.
- K = 10: 2 packages, 4 root ports, 4 switches. S = 0.
- M = 6 under rail scope.
- U = 0, because there is no live bus utilization.
- N = 0, because no CPU load is read.
- G = 0, because no `--idle-gate-command` is set.

```
11 × 32 + 10 × 8 + 3 × 23 + 0 + 3 × 6 + 5 × 4 + 2 × 10 + 10 + 0 + 0 + 41 + D + T
= 352 + 80 + 69 + 18 + 20 + 20 + 10 + 41 + D + T = 610 + D + T
```

A healthy idle sim run scrapes 611 `hostping_*` series (D = 0, T = 1), 47 of
them the topology graph (24 nodes, 23 edges) and 40 the latency streaks; 0.1.4
scraped 571 and 0.1.3 524.
0.1.1, with one-hot status gauges at 22 series per path and 4 per link,
scraped 1285. The rail-optimized `--topology fig2-rails` host has M = 0 and
scrapes 593. `--topology fig2-snc` (K = 14 with 4 NUMA nodes, S = 4, L = 27)
has 30 nodes and 31 edges.

Each further diagnosis code and confidence, and each further trigger/mode
combination, adds one series for the life of the process. The same host
under `--mutual-scope=host` has the same M = 6. An 8-RNIC host would have
M = 28 there.

When series appear and disappear:

- **Present from the first scrape:** `hostping_build_info`,
  `hostping_rnic_info`, `hostping_endpoint_info`,
  `hostping_topology_node_info`, `hostping_topology_edge_info` and
  `hostping_status_code_info` (fixed at start-up), and
  the liveness counters (written as 0).
- **Absent until first written:**
  - path, link and verb series, until the first run reports them;
  - `hostping_diagnosis_count_total` per `(code, confidence)`, until the first
    run that emits it;
  - `hostping_engine_run_count_total` per `(trigger, mode)`, until the first
    such run;
  - the `*_timestamp_seconds` gauges, until their first event.

  Alerts that need a series must account for that (see
  [Liveness and staleness alerts](#liveness-and-staleness-alerts)).
- **Absent when unknown, never 0:**
  - `hostping_bus_utilization` for a hop no source read;
  - `hostping_rnic_pause_duration_ratio` and `hostping_rnic_drop_delta` for an
    RNIC with no source (InfiniBand, or no counter found);
  - `hostping_cpu_busy` for a NUMA node whose CPU load was not read;
  - `hostping_mutual_timeout_ratio` for a pair that reported no probe count;
  - every `hostping_mutual_*` series on a host with no qualifying pair;
  - path and verb series for a skipped endpoint (it is counted in
    `hostping_endpoint_skipped` instead);
  - verb series for a probe that reported no timings.
- **Kept until restart:** gauges keep their last value. A path measured once
  and skipped afterwards, a busy run that probes only the reduced set, or a
  bus whose source stopped answering leaves the earlier series in place with
  its old value. The report of the current run is authoritative.
- **State gauges never strand a series:** the `*_status_code` and
  `*_reason_code` gauges hold the state as their value, with one series per
  path or link. A state change rewrites that value and creates no new series.
  `hostping_endpoint_skipped` writes every `kind` on every report, so a
  recovered kind falls back to 0. A path or link missing from a later report
  keeps its last code, like every other path and link gauge (previous
  item).
- **Restart:** every series restarts. Counters begin again from 0 (`rate()`
  and `changes()` handle the reset), and the series that are absent until
  first written are absent again.

## Logs and reports under journald

The daemon writes log lines to stderr: start-up, skipped endpoints, PFC pause
and drop counter sources, discarded idle runs and diagnoses. It writes each
confirmed report to stdout as one compact JSON object per line
(`--report-format=json`, the daemon's default). Under the unit both go to the
journal with one entry per line, so every report is a single journal entry:

```bash
journalctl -u hostping -f
journalctl -u hostping --grep 'hostping diagnosis'
# reports only: the log lines are not JSON and are dropped by fromjson?
journalctl -u hostping -o cat | jq -cR 'fromjson? | select(.causes | length > 0)'
```

Each report line carries the report's own fields plus `reported_at`, `version`
and `commit`. `reported_at` is the UTC time the line was written. For a held
idle-mode run it can be up to one poll after `started`. A discarded idle-mode
run is written nowhere. Only its log line records that it happened.

To keep reports out of the journal, or alongside it, use `--report-file PATH`.
It appends every confirmed report to `PATH` as one JSON line, whatever
`--report-format` says about stdout. `--report-format=none` stops the stdout
copies. The file is created with mode 0640 and opened for each report, so
logrotate can move it away without signalling the daemon (no `copytruncate`
needed). A path that cannot be opened fails the start. The unit's
`LogsDirectory=hostping` provides `/var/log/hostping`:

```bash
# /etc/default/hostping
HOSTPING_OPTS="--report-file /var/log/hostping/reports.jsonl --report-format=none"
```

One-shot runs (`hostpingctl diagnose`, `hostping --once`) default to
`--report-format=pretty`, the indented JSON an operator reads in a terminal.
The default depends on the run kind, not on whether stdout is a terminal, so a
command behaves the same under ssh, a pipe or systemd. Pass
`--report-format=json` to pipe a one-shot report into `jq -c` or a collector.

## Stopping, upgrading and rolling back

`systemctl stop` sends SIGTERM. The daemon stops between probes, never inside
one. How long that takes depends on what is running:

- a loopback probe ends within `--timeout` (5 s) of a stalled transfer;
- a mutual burst against a pair that does not answer can take
  `--mutual-iters` × `--mutual-timeout` (32 s at the defaults);
- the last OTLP push is bounded by the exporter's timeout.

The unit's `TimeoutStopSec=90s` covers all of these. If systemd has to send
SIGKILL, the kernel releases the verbs resources and pinned memory with the
process. The daemon exits 0 on SIGTERM, so `Restart=on-failure` does not bring
it back.

Without `--state-dir`, a restart loses in-memory state: flapping streaks
(`--flap-window`), a held idle-mode report awaiting confirmation, and the
mutual probe window. The idle timer also restarts. On an idle host the first
idle full-mesh run then follows within seconds of the restart instead of
`--idle-interval` later. On a fleet whose agents restart regularly, a link that
flaps slowly relative to `--idle-interval` × `--flap-window` may never be
reported. The liveness counters restart from 0, and the `*_timestamp_seconds`
series are absent until their first event after the restart. `rate()` handles
the counter reset.

`--state-dir DIR` is opt-in. The unit's `StateDirectory=hostping` provides
`/var/lib/hostping` with mode 0700. With it, the daemon keeps four things across
a restart in `DIR/state.json` (mode 0600, replaced atomically):

- the per-link consecutive-gray counters behind flapping, with the end of the
  last run that updated them, so a streak started before the restart is
  completed by the runs after it;
- the per-GPU count of consecutive idle runs whose memory registration crossed
  the `gpu_mr_registration_slow` threshold (`slow_mr`). This field is
  optional, and a file without it restores with no such streaks;
- the per-path count of consecutive idle runs that read the path's latency
  `abnormal` against its siblings, behind `path_latency_abnormal`
  (`latency`, keyed `rnic->endpoint`). It is optional like `slow_mr`, and a
  path the graph no longer has is dropped on restore;
- the end of the last idle-trigger run (confirmed or discarded), so the next
  idle full mesh stays `--idle-interval` after it, or runs on the first idle
  poll when that is already past.

The file is written:

- after each idle-mode run is confirmed or discarded (a discarded run's
  streaks are rolled back first);
- after a busy-mode run that changed a streak;
- on shutdown (an idle-mode report still held at shutdown is left out of the
  streaks).

At start the daemon restores the file only if the schema version, the topology
fingerprint and the kernel boot ID all match, and its last run ended no more
than `--flap-window` × `--idle-interval` ago. Otherwise it logs one line saying
why and starts from empty state. The fingerprint covers RNIC IDs and verbs
devices, endpoint IDs and kinds, link IDs and kinds, and each path's link list
and distance. It ignores capacities, line rates and rail keys, which are
re-read on every start. A corrupt or unreadable file is ignored the same way.

A directory that cannot be created or written fails the start, like
`--report-file`. A host whose boot ID (`/proc/sys/kernel/random/boot_id`)
cannot be read runs without persistence and says so once. One-shot runs
(`hostpingctl diagnose`, `hostping --once`, `hostpingctl calibrate`) never read
or write the file.

Nothing survives a reboot. The boot ID changes, and links retrain on boot, so
streaks from before the reboot say nothing about the links now. A changed
topology (an RNIC or GPU added, removed or renamed) also starts from zero. The
held idle-mode report and the mutual probe window are never persisted.

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

To roll back, point `current` at the previous directory and restart. Keep the
previous release directory until the new one has run through at least one idle
run. The environment file is shared between releases. A rollback past a release
that added a flag you now use fails at start until you remove the flag.

## Cost to running workloads

Every run puts real traffic on the RNICs and PCIe links the service uses, and a
GPU endpoint costs GPU memory while it is probed. The following was measured
with a two-node all-to-all collective between two multi-GPU RDMA hosts:

| Situation | What the workload sees |
| --- | --- |
| Busy-mode run during service traffic (PFC/drop trigger, `--busy-interval`, or a manual `diagnose` on a loaded host) | Bus bandwidth 10–15% lower for 1–2 s per run, in every repetition. The loopback probes share the loaded RNICs and their PCIe hops with the service. |
| Idle-mode run already in flight when a job starts | Job launch about 0.4 s slower and the first ~2 s a few percent slower, then back to normal. hostping detects that the host became busy and discards the report (see the idle-mode hold in [Triggers](design.md#triggers)). An idle run that starts inside a quiet stretch of a job is not caught this way; see [Keeping idle runs out of scheduler jobs](#keeping-idle-runs-out-of-scheduler-jobs). |
| GPU endpoint being probed | One GPU context per probed GPU, on the order of 0.5 GiB of device memory, held only while the run probes that GPU (a few seconds) and released after the run. |

On a busy fleet, the cost that matters is the busy-mode run, not the idle one.
Each PFC/drop
trigger costs one such dip, at most once per `--busy-cooldown`. On hosts whose
fabric pauses routinely, keep the cooldown at its default or longer.
`--busy-interval D` adds one dip per `D` on every loaded host even without a
trigger. Set it in minutes, and weigh a 1–2 s slowdown per `D` against how often
you need a busy verdict.

Avoid scripting `hostpingctl diagnose` against hosts that are running jobs. On
a loaded host it runs in busy mode and costs the same dip.

A job that sizes its GPU memory to the last few hundred MiB could still fail to
allocate if it starts while an idle run holds a context on its GPU. This has not
been measured.

A busy run that flags a path against its siblings re-reads it before reporting
it. This is the paired busy recheck (see
[Busy classification](design.md#busy-classification)). It probes the path and
up to 4 other paths of its class on the same loaded RNIC for 3 rounds. That is
at most 15 extra probes per flagged path (9 in a class of three), and at most 8
paths, 120 probes, per run. A busy run that flags nothing costs nothing extra.
Like the sweep's probes, these share the loaded RNIC with the service, so a
busy run with flagged paths holds its bandwidth dip a little longer.

A run that flags a path against its idle baseline (an idle run, or an unloaded
RNIC in a busy run) re-reads it too. This is the paired bandwidth recheck (see
[Bandwidth recheck](design.md#bandwidth-recheck)). It takes one path per RNIC
and distance class and probes it for 3 rounds with up to 2 references on other
RNICs. That is at most 9 extra probes per group, and at most 12 groups, 108
probes, per run. Its probes land on RNICs that are not carrying service
traffic.

The probes themselves are short. A loopback probe's setup (QP creation, memory
registration) dominates its time even at the default 512 timed WRITE pairs, so
`--iters` barely changes how long a run holds a path.

`--mem-warmup D` does add time. Each memory path gets `D` of CPU stores to one
NUMA node, one path after another. An idle run grows by about `D` × the number
of memory paths (RNICs × NUMA nodes), and a busy run by `D` for each
memory-path probe it takes. It is off by default. See below for when to turn
it on.

### Keeping idle runs out of scheduler jobs

The idle verdict measures activity, not allocation. A scheduler job that is
loading data, preprocessing or writing a checkpoint can leave every GPU and
RNIC below the idle thresholds for minutes. hostping then calls the host idle
and starts the full mesh inside the job's allocation. The job pays the costs
above, and the run can misread the job's own traffic. Storage I/O on an RNIC
with no GPU path, for example, shows up as packet drops. The idle-mode hold
discards a run only when the job comes back before the run ends.

`--idle-gate-command PATH` hands that decision to the site. The daemon runs
`PATH` right before each idle-trigger run:

- **Exit 0**: the run starts.
- **Exit 1**: the gate denies the run, for example because a scheduler job
  holds the host (`reason="denied"`).
- **Exit 2 or higher, no exit within `--idle-gate-timeout`** (default `5s`, at
  most `20s`), **or a command that cannot be started**: the gate could not
  decide, for example because the scheduler could not be queried
  (`reason="error"` or `reason="timeout"`).

Every outcome but exit 0 skips the run. The next attempt follows
`--idle-interval`, as after a discarded run. The gate fails closed, so a
broken gate stops idle runs rather than probing inside a job. Exit 1 and the
others are counted apart so that a broken gate alerts
(`HostpingIdleGateFailing`) instead of reading as a host that is always
allocated. A gate script must therefore exit 1 only for "not now", and 2 for
any failure of its own query. A shell already exits 126 or 127 when it cannot
run a command.

Details:

- `PATH` runs without a shell and without arguments, as the daemon's user and
  inside the unit's sandboxing. Its stdin and stdout are `/dev/null`. On a
  timeout, its whole process group is killed.
- The gate runs only on the poll that would start an idle run, never on every
  poll. While it runs, the poll loop waits, which is why the timeout is capped
  below the 30 s `/healthz` stall limit.
- The outcome is logged when it changes, not on every attempt. A skip line
  names the reason and quotes the first 256 bytes of the gate's stderr.
- Every skip counts in
  `hostping_engine_idle_gate_skipped_total{reason}`: `denied` (exit 1),
  `timeout` or `error` (exit 2 or higher, or an exec failure or signal). The three series are exported as 0 from start-up when
  a gate is set, and not at all otherwise.
- Only the scheduled idle run is gated. PFC/drop, `--busy-interval` and
  mutual-probe alarm runs answer a symptom and keep their own rate limits.
  `hostpingctl diagnose`, `hostpingctl calibrate` and `--once` never run the
  gate: an operator runs them on a drained host.
- A skipped run measured nothing, so it does not restart `--busy-interval`.
  A child the gate leaves running when it exits is killed with its process
  group.
- A job can still start between the gate's answer and the end of the run.
  The idle-mode hold covers that case as before.

A Slurm gate that allows a run only when a query succeeds and finds no
running job on this node:

```sh
#!/bin/sh
# exit 0: a successful query found no RUNNING job on this node
# exit 1: a job is running here (denied)
# exit 2: the node name or the query failed (could not decide)
node=$(hostname -s) || exit 2
[ -n "$node" ] || exit 2
jobs=$(squeue -h -w "$node" -t R -o %i) || exit 2
[ -z "$jobs" ] || exit 1
```

Test the query's status before its output, as here. Testing only
`[ -z "$(squeue ...)" ]` allows a run whenever `squeue` fails with empty
output. Exiting 2 on a failed query keeps a slurmctld outage from reading as
"a job holds every host": the run is still skipped, but
`HostpingIdleGateFailing` fires instead of every idle diagnosis stopping
silently. `-t R` skips only running jobs. Add states such as `CF`
(configuring) or `CG` (completing) if an allocation in those states must not
be probed either. A check that does not depend on slurmctld, such as the
presence of `job_*` cgroups under the slurmstepd scope, also works. It fails
open, though, if the path is not where your Slurm creates them.

`hostname -s` must match the node's Slurm `NodeName` or `NodeHostname`
(`squeue -w` accepts either). Where it matches neither, `squeue -w` rejects
the name on every attempt, so the gate never allows a run. With `exit 2` it
shows as `reason="error"` and alerts. Pass the `NodeName` instead, for
example from a file the site writes at provisioning.

`HostpingNoSuccessfulRun` in the shipped rules leaves out a host whose gate
skipped a run in its window. `HostpingIdleGateFailing` alerts on `timeout`
and `error` instead ([Alerting rules](#alerting-rules)), so a gate that cannot
reach its scheduler must exit 2, not 1.

### Hosts with dynamic uncore or memory power management

On some platforms the memory subsystem (uncore, memory controller) lowers its
operating point while the host is idle and raises it under load. An idle-mode
run is the idle case, and a loopback probe is too short to move the operating
point reliably. On such hosts the memory paths (`local_mem`, `remote_mem`) can
read at clearly different levels from one idle run to the next while GPU paths
stay stable.

Typical signs:

- calibration reports a wide spread or two levels for a memory distance;
- repeated idle runs flip memory paths between `normal` and `abnormal` without
  any change on the host.

More `--iters` or a larger `--large-msg` does not help. The measurement is not
short of samples; the hardware is in a different state.

Two ways to handle it:

1. **Fix the platform's uncore/memory frequency** in firmware or with the
   platform's tools, so idle and loaded hosts measure the same. This is the
   robust fix.
2. **Try `--mem-warmup`** (e.g. `--mem-warmup 50ms`). Each memory-path probe
   writes its own NUMA-bound buffers from the CPU for that long right before
   measuring, so the memory subsystem is carrying traffic when the timed WRITEs
   start. Whether a platform settles within the warm-up is a property of that
   platform. Before relying on it, compare the spread of repeated idle runs (or
   `hostpingctl calibrate`) with and without it, and calibrate baselines with
   the same setting the daemon will use. `verb_ns.mem_warmup` shows the time
   each probe spent.

#### Bandwidth guard

The bandwidth verdict has a guard against the same drift over time. A run reads
RNIC by RNIC. A slow state over the batches of a few consecutive RNICs drops
their memory paths below the baseline together, which reads as failed root
ports (`cpu_root_port_failure`) and RNIC links (`rnic_pcie_failure`).

Before such a verdict is kept, one path of each flagged RNIC and distance class
is re-read, interleaved with the same class on other RNICs that met the
baseline. The group stays `abnormal` only if the path still reads low against
them. Otherwise it is `normal` again, or `uncertain` with `host_shift` when it
reads level with them but still below the baseline (see
[Bandwidth recheck](design.md#bandwidth-recheck)).

A `host_shift` that keeps recurring is the platform drifting, not a fault.
Fixing its power management, as above, removes it. The report's `bw_recheck`
shows the numbers.

#### Latency guards

The latency verdict has its own guard against this drift. Without a `LAT`
baseline, a path's latency is judged against the median of its siblings. A run
probes RNIC by RNIC, so a slow window over one RNIC's batch would read as a
slow RNIC (and an advisory `path_latency_abnormal`).

Before such a verdict is kept, the run re-reads the path interleaved with
siblings of its class, on other RNICs where it can. It keeps the verdict only
if the path still reads high against them. Otherwise the path is `normal` with
`latency_reason` `recheck_cleared` (see [Latency](design.md#latency)).

That recheck costs 6 extra probes per flagged path, at most 16 paths per run,
and only in idle runs that flag a path. Paths the locality check below clears
anyway are not rechecked. On a host with many RNICs a drift can flag more paths
than that. The run then rechecks them in turn across RNICs, from a window that
moves on by 16 every run, so each flagged path is rechecked within a few runs.
A path left out carries `latency_recheck_skipped` `cap`.

Some platforms drift per NUMA node instead of over time. The small-message
latency of every RNIC attached to one node rises together, by about the sibling
floor, and stays there for minutes. The recheck confirms such a rise, because
the path's siblings sit on other nodes. The affected node may move between
runs, and loading CPU cores may move it instead of removing it.

The analyzer therefore runs a locality check without probing anything. It
compares each sibling verdict the recheck kept (or did not reach) with the
same-class paths of the other RNICs on its RNIC's NUMA node. A path that does
not read high against them is `normal` with `latency_reason` `locality_shift`.
Its `latency_locality` shows the node, the reference and the RNICs the
reference came from.

- A per-RNIC fault raises one RNIC, not all of a node, so it stays flagged.
- A slow endpoint also stays flagged, because a strict majority of the other
  NUMA nodes also read it as `abnormal`.
- A shift spanning several nodes at once (every node of one package, say)
  still leaves the remaining nodes reading the endpoint normally, so it is
  cleared.

The check needs a second RNIC on the node. A host with one RNIC per NUMA node
(sub-NUMA clustering often does this) keeps the plain sibling verdict, and so
does an RNIC whose NUMA node is unknown. Fixing the platform's power
management, as above, removes the drift itself.

#### Latency streak

Both checks still leave, now and then, a single path that reads high in one
idle run, holds up in its recheck, and is not shared by its node, while the
next run flags a different path or none. A per-path latency fault does not come
and go between runs. The daemon therefore reports a sibling verdict only after
the same path read `abnormal` in 3 consecutive idle runs. The cause appears on
the third of them, about two idle intervals after the first run that sees the
rise.

- Each path's `latency_streak` in the report counts these runs.
- A run that reads the path normal, or does not judge it, resets the streak.
- Busy runs leave the streak alone.
- A run whose recheck cap left the path out holds its streak and reports
  nothing for it, since no recheck confirmed that run's reading. The rotating
  window reaches the path in a later run.

A path below 3 keeps `latency_status` `abnormal` (and
`hostping_path_latency_status_code` 3) but gets no cause.
`hostping_path_latency_streak` exports the count, so a dashboard or alert can
wait for the same persistence instead of following the per-run status code,
which one high run of a healthy path flips:

```promql
# Paths whose latency has read abnormal against their siblings for 3
# consecutive idle runs: the persistence the daemon needs to name a cause.
hostping_path_latency_streak >= 3
```

The gauge reads 0 in a busy run, which judges no latency, while the daemon
keeps the count; the next idle run continues it. A discarded idle run
rolls the streaks back, and `--state-dir` keeps them across a restart. A
one-shot run cannot see persistence, so it reports a sibling verdict only when
the paired recheck confirmed it. A verdict against a `LAT` you set needs
neither.

## Calibrating baselines

Idle runs judge each path against a per-distance bandwidth baseline. The Fig.4
defaults are scaled to `min(line rate, PCIe hop)`. Hosts of different hardware
classes (NUMA layout, PCIe switch tiers, GPU generation) still reach different
levels, so a fleet should run on baselines measured on its own hardware.
`hostpingctl calibrate` measures them. Nothing about it is verified on RDMA
hardware yet.

1. **Pick healthy hosts of one hardware class**, idle, with the daemon
   stopped. Calibrate and the daemon do not coordinate, and each would read
   the other's probe traffic as load.
2. **Calibrate each host.** `hostpingctl calibrate --out $(hostname).json`
   performs `--runs` (default 5) idle-mode runs `--interval` (default 30 s)
   apart, with the same flags as `diagnose`.
   - It refuses to start on a busy host or one whose idle state cannot be
     read.
   - A run during which the host became busy is discarded and retried, as the
     daemon discards one. After more than `--max-discards` (default 3)
     discards it gives up.
   - The file goes to `--out` or stdout. A table with the spread of the
     readings behind each number goes to stderr.

   Read the warnings before going on. A path more than `--abnormal-th` below
   its distance's calibrated value will diagnose `abnormal` against the file.
   On a host believed healthy, that is either a fault nobody knew about or the
   wrong host to calibrate on. A distance that reads at two levels (a gap
   larger than `--abnormal-th` between its sorted per-path medians, at least
   two paths on each side) is reported one of two ways. See
   [Two levels in one distance](#two-levels-in-one-distance). RNICs that
   derive different Fig.4 tables are calibrated per table. See
   [RNICs that derive different Fig.4 tables](#rnics-that-derive-different-fig4-tables).
3. **Merge.** `hostpingctl calibrate merge --out class.json host*.json`
   takes the lower quartile of bandwidth (and the upper quartile of any
   latency) across hosts. It warns about any host that would diagnose
   abnormal against the result, which usually means the host belongs to
   another hardware class.

   Learned sub-classes are merged by name. A root-port pair is in the merged
   sub-class when at least half of the files that hold its distance list it,
   and every pair the files disagree on is named in a warning. Hosts of one
   board layout should agree. A disagreement is either a calibration the
   majority overrules or a host of another layout. Per-table sections are
   merged by key, and a section only some files have is named in a warning.
4. **Distribute** `class.json` to that class and pass it with
   `--baseline-file` (for example in `$HOSTPING_OPTS`). At start-up the
   daemon fails on a file that does not parse. For a file that parses, it
   logs:
   - which distances take their bandwidth and latency from the file;
   - which keep the Fig.4 default;
   - which sub-classes the file sets;
   - which of the host's Fig.4 tables take a per-table section.

   A sub-class root-port pair the host does not have is ignored, and one
   start-up line lists every such pair.
5. **Recalibrate after hardware, firmware or BIOS changes** (RNIC or GPU
   swap, PCIe or NUMA settings, RNIC firmware). The file is absolute. An
   out-of-date floor either hides a regression or reports one that is not
   there.

A distance no path of the host measured (Fig.2 has no `multi_switch` path,
for example) is left out of the file and keeps its Fig.4 default.

Latency is off unless `--latency` is given. A latency set in the file is a
strict reference without the `--latency-floor` the sibling comparison has, so a
calibrated latency flags smaller increases. Prefer leaving it out until the
sibling comparison has proved too loose.

Calibration writes each run's first readings and skips the paired latency and
bandwidth rechecks, which change only verdicts calibration does not read. A
latency it writes is drawn from those first readings, slow time windows
included. A verdict against it, like any `LAT` you set, is not rechecked.

### Two levels in one distance

On some platforms the P2P route between certain pairs of root ports costs
nothing, while between others it halves the bandwidth. `cross_root` (or
another distance between root ports) then holds two populations on healthy
hardware, and nothing in sysfs tells them apart. One per-distance baseline sits
at one level. When most paths are slow, a fast path that degrades to the slow
level stays `normal`. Calibrate handles this by keying on the unordered pair of
root ports each path crosses:

- **The split follows root-port pairs.** Every pair's paths are all fast or
  all slow, and every fast path crosses two root ports. Calibrate writes the
  fast cluster as a learned sub-class `fast` of the distance, listing the fast
  pairs, and takes the distance's own value from the slow paths only. The
  table then shows a `cross_root/fast` row next to `cross_root` (each with the
  spread over its own paths), a `sub-class cross_root/fast:
  paths across root-port pairs …` line, and a `NOTE: … wrote sub-class`
  line. Against that file a fast path is judged against the fast level, so
  the drop is caught. The report names the sub-class in the path's
  `baseline_class`.
- **The split does not follow root-port pairs.** A pair has paths at both
  levels, or a fast path crosses no pair of root ports. No sub-class is
  written, and the distance keeps one value over all its paths. The
  `WARNING: … reads at two levels` names the fast paths, says which level the
  value sits at, and lists why the split could not be keyed (the mixed pairs,
  or the fast paths without a pair). The blind spot remains for that
  distance, so check the fast paths by hand.

Root-port IDs are BDF-based (`rp.<BDF>` from discovery), so a sub-class is
portable only between hosts with the same board layout. Calibrate and merge per
hardware class as above. On a host whose layout differs, the pairs it does not
have are ignored (one start-up line lists them), and its paths fall back to the
distance's value. A hand-edited file may define sub-classes too. See
[Baselines](design.md#baselines) for the schema. Only the fast cluster becomes
a sub-class. The slow paths stay with the distance.

### RNICs that derive different Fig.4 tables

A host can carry RNICs that derive different Fig.4 tables (line rate and
PCIe hop), for example a lower-line-rate RNIC in a narrow slot next to faster
RNICs on wide links. The slower RNIC reads lower at every distance, so one
value per distance cannot serve both. Calibrated on the majority, it puts
every path of the slower RNIC below its distance's baseline, and that RNIC is
diagnosed on every run.

Calibrate therefore groups the measured paths by their RNIC's table and
computes every distance (and sub-class) per group. The group with the most
measured RNICs (on a tie, the first table key in sort order) is written at
the top level of the file, the same way as on a host with one table, so a
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
capacity is known. Values are in Gb/s, rounded to two decimals without
trailing zeros. This is the same pair the `hostping baseline line_rate=…` log
line keys its tables by. The loader accepts a key only in exactly that
spelling.

The calibrate table prints the top level's table and RNICs, one table per
section, and a `NOTE` naming the tables. Outliers and two-level warnings of a
section are prefixed with `tables <key>`.

An RNIC whose table has a section takes that section as a whole. A distance
the section leaves out keeps the RNIC's own Fig.4 default, not the top-level
value, which was measured on another table. Every other RNIC takes the top
level. `--baseline` flags override the top level and every section
alike. The latency sibling comparison draws siblings only from RNICs that take
the same section.

At start-up the agent logs:

- one `baselines:` line per section, with the distances it sets;
- one `baselines: per Fig.4 table:` line naming each of this host's tables,
  its RNICs, and whether it takes the top level or a section (and any section
  matching no RNIC, which is ignored).

Each run's `hostping baseline` line for a table that uses a section says so.

`calibrate merge` merges each section by key, over the files that have it,
with the same quartile and sub-class rules as the top level. It warns about a
section only some files have, since hosts of one hardware class should derive
the same tables.
