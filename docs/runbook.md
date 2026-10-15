# hostping runbook

One section per diagnosis code, plus one per agent-health alert in
[`contrib/prometheus/hostping-rules.yml`](../contrib/prometheus/hostping-rules.yml).
Each alert's `runbook_url` points at the section of the same name.

The "how it is derived" lines summarize the analyzer. The
[design document](design.md#diagnoses) is the reference. The checks and
remediations are generic. hostping does not know your hardware vendor, and
nothing below has been validated against a particular platform. Where a step is
only a reasonable first look and not something hostping's evidence implies, the
text says so.

## Contents

- [Before you start](#before-you-start)
- [Paging policy](#paging-policy)
- PCIe and intra-host buses: [`rnic_pcie_failure`](#rnic_pcie_failure),
  [`gpu_pcie_failure`](#gpu_pcie_failure),
  [`cpu_root_port_failure`](#cpu_root_port_failure),
  [`memory_channel_failure`](#memory_channel_failure),
  [`memory_channel_flapping`](#memory_channel_flapping),
  [`upi_failure`](#upi_failure), [`snc_mesh_failure`](#snc_mesh_failure)
- Configuration: [`acs_enabled`](#acs_enabled), [`ats_disabled`](#ats_disabled),
  [`rnic_slow_start`](#rnic_slow_start), [`rnic_tx_window`](#rnic_tx_window)
- Load: [`overloaded_rnic_pcie`](#overloaded_rnic_pcie),
  [`overloaded_memory_channel`](#overloaded_memory_channel),
  [`overloaded_intersocket_bus`](#overloaded_intersocket_bus)
- RNIC and fabric (mutual probe and counters):
  [`rnic_connectivity_failure`](#rnic_connectivity_failure),
  [`rnic_intermittent_timeout`](#rnic_intermittent_timeout),
  [`tor_switch_anomaly`](#tor_switch_anomaly),
  [`rnic_cross_rail_unreachable`](#rnic_cross_rail_unreachable),
  [`rnic_packet_drops`](#rnic_packet_drops)
- Advisory: [`path_latency_abnormal`](#path_latency_abnormal),
  [`gpu_mr_registration_slow`](#gpu_mr_registration_slow),
  [`rnic_pcie_undersized`](#rnic_pcie_undersized),
  [`unknown`](#unknown)
- Agent health: [HostpingTargetDown](#hostpingtargetdown),
  [HostpingNoSuccessfulRun](#hostpingnosuccessfulrun),
  [HostpingRunsFailing](#hostpingrunsfailing),
  [HostpingMonitorPollErrors](#hostpingmonitorpollerrors),
  [HostpingEndpointsSkipped](#hostpingendpointsskipped)

## Before you start

The metric `hostping_diagnosis_count_total{code,confidence}` says which code
fired, not on what. The subject is in the report and the log line:

```bash
# Diagnosis log lines
journalctl -u hostping --since -1h --grep 'hostping diagnosis'
# The causes of recent reports, with their links, RNICs, endpoints and evidence
journalctl -u hostping --since -1h -o cat \
  | jq -cR 'fromjson? | select(.causes | length > 0) | {started, mode, trigger, causes}'
# or, with --report-file
jq -c 'select(.causes | length > 0) | {started, mode, trigger, causes}' /var/log/hostping/reports.jsonl
```

Each path in the report also names the baseline it was judged against.
`baseline_class` is the learned sub-class (for example `cross_root/fast`, see
[Two levels in one distance](operations.md#two-levels-in-one-distance)). It is
absent when the path was judged against its distance's own value.

Each cause carries `links` (`pcie.<rnic>`, `pcie.<gpu>`, `rp.N`, `sw.N`,
`mem.N`, `upi.A-B`, `mesh.A-B`), `rnics`, `endpoints`, `confidence` and
`evidence`. `hostpingctl topology` maps those IDs to the RDMA device, netdev,
PCI address (`pcie_bdf`), socket (CPU package), NUMA node and root port. The
`lspci` and sysfs paths below take that address.

### Idle or busy

Before acting, check `mode` in the reports that carried the code.

- An `idle` report judged every path against its distance baseline in a full
  mesh.
- A `busy` report compared each loaded RNIC's paths with their siblings. It
  keeps a small per-run false-positive rate even after the median comparison
  and the paired busy recheck.

A code seen in idle runs, or in several busy runs on the same subject, is much
stronger evidence than one busy run.

In a busy report, an `abnormal` path carries `busy_recheck` when it was re-read
with its siblings.

- `confirmed: true` with `path_bps` clearly below `sibling_bps`: the path read
  low a second time, next to its siblings.
- No `busy_recheck`: the path was not rechecked. It was past the per-run cap of
  8, or a recheck probe failed (the journal logs it).
- A path the recheck cleared is `uncertain` with `rnic_busy`. It keeps its
  `busy_recheck` (`confirmed: false`) as a record.

An `abnormal` path judged against its idle baseline (an idle report, or an
unloaded RNIC in a busy report) carries `bw_recheck` when its group was re-read
next to the same class on other RNICs. The group is the RNIC's paths of one
distance class.

- `outcome: confirmed`: `path_bps` stayed more than `--abnormal-th` below
  `ref_bps`, so the drop is the path's own.
- No `bw_recheck`: the group was not rechecked. It was past the per-run cap of
  12 groups, no other RNIC met the baseline in that class, or a recheck probe
  failed (the journal logs it).
- `outcome: cleared`: the group is `normal` (or `uncertain` for the usual
  reasons).
- A group that read level with its references but still below `baseline_bps`
  is `uncertain` with `host_shift`, even when `ref_bps` itself just clears the
  baseline.

### `cpu_busy` and `host_shift`

`cpu_busy` names a path that read `abnormal` but transits a CPU package whose
CPUs were busy at or above `--cpu-busy-high` on the poll that started the run
(the report's `cpu_load`, and `hostping.cpu.busy`). CPU load contends for the
package's on-die fabric, which the path crosses. A constant load survives the
paired re-read, so such a drop is no evidence of a fault and raises no cause.
Paths through other packages are judged as usual. To test the paths of a
loaded package, re-run when its CPUs are idle.

`host_shift` names a path that read below its baseline while paths of the same
distance class on other RNICs, re-read next to it, read level with it. The
whole host was in a slower state than the one the baseline was measured in, so
neither side can be judged against it. It is not a fault and raises no cause.

- Occasional `host_shift` paths are the host's memory bandwidth drifting with
  power management (see [Hosts with dynamic uncore or memory power
  management](operations.md#hosts-with-dynamic-uncore-or-memory-power-management)).
- One in most idle runs means the baseline sits above what the host reaches in
  its slower state. Fix the platform's operating point, or recalibrate with the
  setting the daemon uses.

### Hosts without busy siblings

On some hosts each RNIC reaches only one endpoint per distance in the busy
probe set (for example, one memory node per side and one affinitive GPU). Busy
runs there have nothing to compare, and every busy-probed path stays
`uncertain` with `rnic_busy`. Busy reports from such hosts never carry a
bandwidth verdict. Rely on idle runs and on the PFC/drop counters (see
[Busy classification](design.md#busy-classification-and-hosts-without-siblings)).

### Re-testing on demand

1. Stop the daemon. `hostpingctl diagnose` and the daemon do not coordinate,
   and each reads the other's probe traffic as load.
2. Run `hostpingctl diagnose` with the daemon's flags.

On a loaded host the one-shot run runs in busy mode, which costs the workload a
short bandwidth dip (see
[Cost to running workloads](operations.md#cost-to-running-workloads)). Prefer a
drained or idle host.

### Confidence levels

Confidence, as hostping uses it:

- `high`: the bandwidth pattern isolates the component (Algorithm 1 marked the
  hop abnormal, or every affinitive path of an RNIC is abnormal and config
  space agrees), or the mutual probe evidence is unambiguous.
- `medium`: one line of evidence only (config space without a slow path, an
  RNIC-wide cap whose cause could be configuration), or a pattern that one
  shared component explains better than several faults.
- `low`: a finding hostping cannot distinguish from a by-design state, or
  abnormal paths it could not localize.

## Paging policy

[`contrib/prometheus/hostping-rules.yml`](../contrib/prometheus/hostping-rules.yml)
turns diagnoses into alerts as follows. Every diagnosis alert requires the
code in at least two separate engine runs within 30 minutes
(`hostping:diagnosis_runs:30m >= 2`). A single run never alerts.

| Alert | Severity | Codes |
| --- | --- | --- |
| `HostpingHardwareFault` | critical (page) | At confidence `high`: `rnic_pcie_failure`, `gpu_pcie_failure`, `memory_channel_failure`, `memory_channel_flapping`, `upi_failure`, `snc_mesh_failure`, `cpu_root_port_failure`, `rnic_connectivity_failure`, `rnic_intermittent_timeout` |
| `HostpingDiagnosis` | warning (ticket) | Every other code at `high` (`acs_enabled`, `ats_disabled`, `rnic_packet_drops`, `rnic_tx_window`, `overloaded_*`), and every non-advisory code at `medium` (including `tor_switch_anomaly` and `rnic_cross_rail_unreachable`) |
| `HostpingAdvisoryDiagnosis` | info | `path_latency_abnormal`, `gpu_mr_registration_slow`, `rnic_pcie_undersized`, `unknown`, at any confidence |
| none | | `rnic_cross_rail_unreachable` at `low` |

Why:

- **Persistence.** hostping reports what one run saw and does not require a
  finding to repeat. A busy-mode run judges a loaded RNIC's paths against each
  other. It keeps a small residual false-positive rate per run, even after the
  median comparison and the paired busy recheck, so a rule that pages on one
  run pages on healthy hosts. Two runs within 30 minutes cost a real fault
  little, since an idle host runs every `--idle-interval` (5 minutes by
  default). A host with frequent PFC/drop-triggered busy runs (up to one per
  `--busy-cooldown`, 30 s) can still meet that bar with two unrelated false
  positives of the same code. If that happens, raise the threshold to `>= 3` or
  shorten the window for such hosts.
- **Runs, not causes.** The recording rule counts `changes()` of the counter
  instead of its `increase()`. One run that names the same code for two GPUs
  adds 2 to the counter but is one run. A counter series exists only from the
  first run that emits its `(code, confidence)`, and `increase()` cannot see
  that first increment. The rule therefore counts a series that did not exist
  30 minutes earlier as one more run. The counter has no subject label, so two
  runs naming the same code on different subjects (say, two different GPUs)
  also count as two. Read the reports before acting.
- **Hardware faults page; configuration, load and fabric findings ticket.**
  `acs_enabled` and `ats_disabled` are `high`, but they are BIOS or kernel
  settings that will not change at night. `rnic_packet_drops` is `high` because
  the counter certainly moved. It does not mean the RNIC is faulty: the drop
  counters include receive-buffer exhaustion (`out_of_buffer`) and netdev
  drops, which a workload can cause. `overloaded_*` name load, not a broken
  part. `tor_switch_anomaly` and `rnic_cross_rail_unreachable` at `medium`
  point outside the host. Promote any of them if your operations treat them as
  urgent.
- **Advisory and topology-expected codes never page.** `path_latency_abnormal`,
  `gpu_mr_registration_slow` and `rnic_pcie_undersized` change no path or link
  status, and `unknown` names no component. `rnic_pcie_undersized` describes
  the slot, so it repeats on every run of a host built that way. Silence it for
  such hosts once the hardware layout is confirmed.
  `rnic_cross_rail_unreachable` at `low` is what a rail-optimized host whose
  rails are isolated by design reports on every run under
  `--mutual-scope=host`.

The rules use the counter and not the status gauges
(`hostping_path_status_code` and `hostping_link_status_code`) for three
reasons:

- The gauges hold the verdict of the last report until the next one, which may
  be hours away on a host that turned busy.
- A single busy run sets them.
- They carry no root cause, so an abnormal path explained by `acs_enabled`
  looks the same as a failed link.

Use the gauges to find the hop once an alert has fired:
`hostping_link_status_code == 3` for abnormal links,
`hostping_path_status_code == 3` for abnormal paths (see
[Status codes](operations.md#status-codes)).

## PCIe and intra-host buses

### `rnic_pcie_failure`

**Meaning.** An RNIC's PCIe link is degraded or failed (NSDI'23 failure #1).

**Confidence and derivation.** Three ways
([PCIe link checks](design.md#pcie-link-checks)):

- `high`, when any of these holds:
  - Every affinitive path of the RNIC is abnormal, the link trained below the
    lower of its device and upstream port maxima (whichever is readable), and
    the negotiated link (`current_link_speed` × width) no longer carries the
    RNIC's line rate.
  - The link trained below `min(device, upstream port)` maximum, no longer
    carries the line rate, and an affinitive path is also abnormal.
  - In a busy run, every affinitive path is uncertain and the link is
    down-trained the same way and below line rate.
- `medium`, "likely failed": every affinitive path is abnormal, but config
  space shows no down-train. The link is healthy, unread, at its maximum, or
  below line rate with both maxima unread. An RNIC-wide throttle (slow start,
  Tx window, NSDI'23 #9/#10) was not ruled out, and the evidence says so.
- `medium`, "down-trained": config space alone, no slow path yet.

**Check first.**

- The negotiated and capable link of the RNIC and of its upstream port:
  `lspci -vvv -s <bdf>` (`LnkCap` vs `LnkSta`), or
  `cat /sys/bus/pci/devices/<bdf>/{current,max}_link_{speed,width}`.
- Kernel messages for the device: `dmesg | grep -i -e <bdf> -e aer`.
- `ibv_devinfo -d <device>` and the port state and rate
  (`/sys/class/infiniband/<device>/ports/*/{state,rate}`).
- For `medium` without PCIe evidence: the RNIC's firmware congestion-control
  and transmit settings (vendor tool), which hostping cannot read.

**Remediation.**

1. Drain the host's RDMA workloads from that RNIC.
2. Reseat the card or its riser, or cold-boot. A link that trained narrow or
   slow often recovers this way.
3. Check the slot's BIOS link settings (forced generation, bifurcation).
4. If it persists after reseat, replace the RNIC or slot through the vendor.

For `medium` "likely failed" with a healthy link, rule out RNIC configuration
before replacing hardware.

**False positives.**

- A slow window over the RNIC's sweep batch can make every memory path of it
  abnormal at once. The paired bandwidth recheck re-reads such a group next to
  the same distance on other RNICs. A `medium` "likely failed" whose
  paths carry `bw_recheck` with `outcome: confirmed` read low twice, the second
  time next to healthy references.
- CPU load on the RNIC's package drops its memory paths the same way and
  survives the recheck. Those paths are `uncertain` with `cpu_busy` once a NUMA
  node of the package reaches `--cpu-busy-high` (check the report's
  `cpu_load`).
- A Gen5 slot that trained to Gen4 x16 under a lower-rate RNIC is not reported.
  The link still carries the line rate.
- A link trained at its maximum that is narrower than the line rate is a slot
  too small by design, not a fault. It is
  [`rnic_pcie_undersized`](#rnic_pcie_undersized), never `high`.
- The bandwidth rule needs all affinitive paths abnormal, which a busy-mode
  sibling comparison cannot produce on its own. `high` from bandwidth is
  therefore unlikely to be a busy artefact. Config-only is `medium` because
  config space may have been read in a transient state.

### `gpu_pcie_failure`

**Meaning.** A GPU's PCIe link is degraded (NSDI'23 failure #2).

**Confidence and derivation.**

- `high`, when any of these holds:
  - Algorithm 1 marked the `pcie.<gpu>` hop abnormal without a matching
    latency increase.
  - The GPU's affinitive path is abnormal while sibling affinitive paths of
    the same RNIC are not.
  - The link is narrower than its capability and an affinitive path is
    abnormal.
- `medium`, when either holds:
  - The link is narrower than `min(device, upstream port)` maximum width with
    no slow path yet.
  - The hop is abnormal while its utilization is at or above `--util-high`
    (summary "GPU PCIe link overloaded").

GPU link speed is never evidence, because GPUs lower it when idle. The
bandwidth verdicts are not reported when the host has an
`acs_enabled`/`ats_disabled` cause or when an RNIC-wide cause covers the GPU's
RNIC. The width check still reports a narrow link, at `medium` while an
`acs_enabled`/`ats_disabled` cause is present.

**Check first.**

- Link width: `lspci -vvv -s <bdf>` (`LnkCap` vs `LnkSta` width) or
  `/sys/bus/pci/devices/<bdf>/current_link_width`.
- `nvidia-smi -q -d PCIE` (or your vendor's tool) for link generation, width
  and replay counters.
- `dmesg` for AER and Xid messages.
- `nvidia-smi topo -m` to confirm the GPU sits under the RNIC hostping calls
  affinitive.
- For the overloaded variant: what was using the GPU's link at the time.

**Remediation.**

1. Drain the GPU.
2. Reseat the GPU or its riser/cable.
3. Re-check width after a cold boot.
4. Update GPU and system firmware if the vendor lists a link-training fix.
5. If it stays narrow, open a vendor ticket.

**False positives.** Busy-mode false positives most often produce this code.
One GPU path below the median of its siblings yields a `high` verdict on that
GPU.

- Busy false positives move between GPUs from run to run. A real fault names
  the same GPU.
- The path's `busy_recheck` shows whether it read low again when re-read next
  to its siblings (`confirmed`).
- Some GPUs power down lanes dynamically, which is why width alone is `medium`.

### `cpu_root_port_failure`

**Meaning.** A CPU root port degrades traffic that transits it (NSDI'23
failure #5).

**Confidence and derivation.** `high`. All of these hold:

- The root port link is abnormal.
- The GPUs local to RNICs under it test normal.
- Cross-root GPU paths through it are abnormal. Alternatively, memory on that
  socket is still normal; the evidence then names the abnormal paths through
  the root port instead of cross-root GPU paths.

Cross-root GPU paths, its main evidence, are probed only in the idle full mesh,
so this code mostly comes from idle runs. It suppresses
`memory_channel_failure` on the same socket.

**Check first.**

- The root port and its downstream links in `hostpingctl topology` and
  `lspci -tv`.
- `lspci -vvv` on the root port for link status and AER.
- `dmesg` for AER on that port.
- BIOS settings that affect the port (link generation, bifurcation, IOMMU /
  ACS).

The concrete cause is platform specific. hostping only localizes the hop.

**Remediation.**

1. Drain the host.
2. Update firmware/BIOS, or reset the affected port settings to defaults.
3. Open a vendor ticket. The root port is part of the CPU package or chipset.

**False positives.** The code requires agreement between several paths in the
same idle run, so it does not come from the busy comparison.

- Host-wide slow state. On a host whose memory bandwidth drifts with power
  management, a slow state over the sweep batches of a few consecutive RNICs
  drops their memory paths together, which would fire this code through the
  memory branch. The paired bandwidth recheck re-reads each such group next
  to the same distance on other RNICs and clears it, or marks it `uncertain`
  with `host_shift` when the whole host is slow. Check the abnormal paths'
  `bw_recheck`. `outcome: confirmed` means they read low again next to healthy
  references. No `bw_recheck` means they were not rechecked (cap, no reference,
  or a failed probe in the journal).
- Workload. A workload that started during an idle run is caught by the
  idle-mode hold, and the report is discarded.
- CPU load. Load on the root port's package can fire this code on a healthy
  host, because the paired re-read confirms a constant load. No RDMA traffic is
  needed: a host process streaming memory on every core of a socket is enough.
  Paths through a package at or above `--cpu-busy-high` are `uncertain` with
  `cpu_busy` instead. If the code fires
  while `cpu_load` shows the package busy just below `--cpu-busy-high`, lower
  the threshold or re-run with the host's CPUs idle.
- Mismatched baseline file. A `--baseline-file` whose learned sub-class
  (`baseline_class`, for example `cross_root/fast`) does not match this host's
  platform holds cross-root paths to a level they never reach and makes them
  abnormal. Check the abnormal paths' `baseline_class` and recalibrate if the
  file came from another board layout.

### `memory_channel_failure`

**Meaning.** The memory behind one NUMA node (`mem.N`) is slow for RDMA
(NSDI'23 failure #3).

**Confidence and derivation.** `high`. Algorithm 1 marked the `mem.N` link
abnormal, and its utilization is unknown or below `--util-high`. Suppressed when
a `cpu_root_port_failure` on the same socket explains it.

**Check first.**

- `dmesg` and the EDAC / machine-check log for corrected and uncorrected
  memory errors (`edac-util -v`, `ras-mc-ctl --errors`, if installed).
- The memory configuration the OS sees: `numactl -H`, and
  `dmidecode -t memory` for populated DIMMs and their speed.
- BIOS memory settings.

Which of these applies is platform specific and not something hostping's
evidence implies.

**Remediation.**

1. Drain the host.
2. Replace or reseat DIMMs that the error log names.
3. Open a vendor ticket.

**False positives.** Memory-path busy verdicts are usually `uncertain`, since
one memory endpoint per distance has no sibling. This code therefore rarely
comes from a busy run. On a host where `--uncore-util` is off, the utilization
is unknown, so a channel saturated by a workload reads as a failure and not as
`overloaded_memory_channel`. Idle runs avoid that.

### `memory_channel_flapping`

**Meaning.** A `mem.N` link was `gray` in `--flap-window` (3) consecutive
loopback tests (NSDI'23 §4.3).

**Confidence and derivation.** `high`. The streak lives in the daemon. It
resets on a run that tests the link without graying it. It also resets on
restart, unless `--state-dir` carried it over (same boot and topology, last run
recent enough; see
[Stopping, upgrading and rolling back](operations.md#stopping-upgrading-and-rolling-back)).
The code is reported only while the current run still sees the link gray
([Flapping](design.md#flapping)).

**Check first / remediation.** As for
[`memory_channel_failure`](#memory_channel_failure). Look first for
intermittent memory errors in the EDAC log.

**False positives.** Discarded idle runs roll their gray streaks back, so a
workload that keeps interrupting idle runs does not build a streak. The
opposite risk also exists. Without `--state-dir`, a fleet whose agents restart
often may never complete a streak for a link that flaps slowly.

### `upi_failure`

**Meaning.** The inter-socket bus (UPI, xGMI) is failed or flapping (NSDI'23
failure #4).

**Confidence and derivation.** `high`. The `upi.A-B` link is abnormal and not
explained by utilization, or gray in `--flap-window` consecutive tests.
Utilization is unknown unless `--uncore-util` and `--upi-capacity` read it. A
and B are CPU packages. On a host with sub-NUMA clustering, a fault between
NUMA nodes of one package is [`snc_mesh_failure`](#snc_mesh_failure) instead,
never this code.

**Check first.**

- BIOS inter-socket link settings (link speed, link count, power management of
  the links).
- `dmesg` / machine-check logs.

Whether the platform reports degraded inter-socket links to the OS, and how, is
vendor specific and not verified.

**Remediation.**

1. Drain the host.
2. Update or reset firmware/BIOS.
3. Open a vendor ticket.

**False positives.** Cross-socket GPU paths only appear in the idle full mesh.
Busy runs measure only the remote-memory paths across the link, so this code
mostly comes from idle runs.

### `snc_mesh_failure`

**Meaning.** On a host with sub-NUMA clustering (Intel SNC, AMD NPS > 1), the
on-package fabric between two NUMA nodes of one CPU package is failed or
flapping. No inter-socket bus is involved.

**Confidence and derivation.** `high`, by the
[`upi_failure`](#upi_failure) rules. The `mesh.A-B` link is abnormal, or gray
in `--flap-window` consecutive tests. A and B are NUMA node numbers of one
package. Paths across the link are the GPU paths between root ports on the two
nodes (`cross_root`) and the memory paths to the other node (`remote_mem`). No
source reads its utilization or capacity, so it is never explained as load.

**Check first.**

- The SNC / NPS setting and memory population of both nodes in the BIOS. An
  uneven or changed population changes the node layout.
- `numactl -H` against the expected layout.
- `dmesg` / machine-check logs.

How a platform reports a degraded on-package fabric, if at all, is vendor
specific and not verified.

**Remediation.**

1. Drain the host.
2. Update or reset firmware/BIOS.
3. Open a vendor ticket.

**False positives.** The paths across the mesh are not affinitive. Busy runs
measure only its memory paths, and the GPU paths come from the idle full mesh.
As for [`upi_failure`](#upi_failure), this code mostly comes from idle runs.

## Configuration

### `acs_enabled`

**Meaning.** PCIe Access Control Services redirect GPUDirect RDMA traffic
through the CPU root complex (NSDI'23 failure #6).

**Confidence and derivation.** `high`. An affinitive GPU path is abnormal, its
small-message latency exceeds the `same_switch` latency baseline by more than
`--abnormal-th`, and config space shows ACS enabled on the GPU, RNIC, or a
port or switch between them. The check needs root (config space beyond 64
bytes). The code is never reported for an unprivileged daemon.

**Check first.**

- `lspci -vvv` on the switch downstream ports and root ports between the GPU
  and RNIC: `ACSCtl` with `SrcValid+`, `ReqRedir+`, `CmpltRedir+`.
- `nvidia-smi topo -m` should show the GPU and RNIC as `PIX`/`PXB`.

**Remediation.** Disable ACS for those ports, through the BIOS (often an
"ACS" or "PCIe ACS" setting) or `setpci` at boot as your platform documents.
Keep in mind that ACS is what isolates devices for virtualization (IOMMU
groups). Ticket, not page.

**False positives.** On hosts that need ACS (device passthrough to VMs) this
is the intended state. Silence it for those hosts.

### `ats_disabled`

**Meaning.** Address Translation Services are off on the RNIC, so GPUDirect
RDMA address translation hairpins through the CPU (NSDI'23 failure #7).

**Confidence and derivation.** `high`. An affinitive GPU path is abnormal with
latency above the `same_switch` baseline, ACS is not the explanation, and the
RNIC's config space shows ATS present but disabled. Needs root.

**Check first.**

- `lspci -vvv -s <rnic bdf>`: `ATSCtl: Enable-`.
- The RNIC's firmware configuration for ATS (vendor tool).
- The platform IOMMU settings.

**Remediation.**

1. Enable ATS in the RNIC firmware configuration.
2. Where the platform requires it, also enable it in the BIOS / IOMMU setup.
3. Reboot.

Ticket.

**False positives.** None known beyond a deliberate configuration.

### `rnic_slow_start`

**Meaning.** An RNIC-wide bandwidth cap caused by the RNIC's slow-start
configuration (NSDI'23 #9).

**Confidence and derivation.** `medium`. Diagnosed only under `--sim`. It
needs the RNIC's device configuration, which hostping cannot read
vendor-neutrally on a live host. A live RNIC with the same pattern is reported
as `rnic_pcie_failure` (`medium`).

### `rnic_tx_window`

**Meaning.** An RNIC-wide bandwidth cap caused by a too-small Tx window
(NSDI'23 #10).

**Confidence and derivation.** `high`, `--sim` only, for the same reason as
[`rnic_slow_start`](#rnic_slow_start).

## Load

### `overloaded_rnic_pcie`

**Meaning.** Every affinitive path of an RNIC is abnormal while its PCIe link
utilization is at or above `--util-high` (90%). The link is busy, not broken.

**Confidence and derivation.** `high`. Utilization is the max(rx, tx) port byte
rate over the hop's capacity, which is a lower bound
([Bus utilization](design.md#bus-utilization)). The code is also reported in
busy runs where every affinitive path is uncertain and the link is saturated.

**Check first.** What the workload is doing on that RNIC
(`hostping_rnic_throughput_bit_per_second`). If the host should not be that
busy, look for unexpected traffic.

**Remediation.** None on the host. The code describes load. Ticket only if the
load is unexpected.

### `overloaded_memory_channel`

**Meaning.** A `mem.N` link is abnormal and its measured utilization is at or
above `--util-high`.

**Confidence and derivation.** `high`. Needs `--uncore-util` and
`--mem-channel-capacity`. Without them the utilization is unknown, and an
abnormal link is a [`memory_channel_failure`](#memory_channel_failure).

**Remediation.** None. The code describes load. A wrong
`--mem-channel-capacity` makes utilization wrong in both directions, so check
it.

### `overloaded_intersocket_bus`

**Meaning.** An abnormal inter-socket link at or above `--util-high`.

**Confidence and derivation.** `high`. Needs `--uncore-util` and
`--upi-capacity` on an Intel processor whose UPI flit size hostping has
measured (or `--upi-bytes-per-flit`). Without them the utilization is unknown,
and an abnormal link is an [`upi_failure`](#upi_failure).

**Remediation.** None. The code describes load. A wrong `--upi-capacity` or
`--upi-bytes-per-flit` makes utilization wrong in both directions, so check
them.

## RNIC and fabric

These codes come from the RNIC-to-RNIC mutual probe (ToN'24 §IV-C-2) and the
RNIC drop counters. With the default `--mutual-scope=rail`, a rail-optimized
host has no RNIC pair, and none of them can fire.

The default `rc` transport only sees sustained loss. `ud` has not been
validated on RDMA hardware. A bug in it would read as every probe timing out on
every host (false `rnic_connectivity_failure` / `tor_switch_anomaly`
fleet-wide). If those codes appear on many hosts at once right after enabling
`ud`, suspect the transport first
([Mutual probe](design.md#mutual-probe-transports-and-schedule)).

### `rnic_connectivity_failure`

**Meaning.** An RNIC times out on every one of its mutual pairs. It is down or
misrouted (ToN #13). A dead pair between two RNICs that both reach others is
reported on that pair, naming both.

**Confidence and derivation.** `high`. An RNIC counts as down only when it has
a same-rail pair and every pair times out.

**Check first.**

- Port state and link: `ibv_devinfo -d <device>`, `rdma link`,
  `ip link show <netdev>`, `ethtool <netdev>`.
- The RoCEv2 GID table (`/sys/class/infiniband/<device>/ports/1/gids/`).
- The cable and the switch port.
- `dmesg` for the driver.

**Remediation.**

1. Drain the RNIC.
2. Re-seat or replace the cable or optic.
3. Check the switch port.
4. Then check the RNIC.

**False positives.** See the `ud` caveat above. A fault on the switch side
looks the same from the host.

### `rnic_intermittent_timeout`

**Meaning.** The RNIC's mean timeout ratio exceeds `--timeout-th` (0.1) without
being total. The mean is over its pairs that answered at least one probe (pairs
with a down peer left out). The code covers sudden timeouts, such as a
flapping link (ToN #15/#16).

**Confidence and derivation.** `high`. `medium` when all of the RNIC's lossy
pairs cross rails (`--mutual-scope=host`). A peer whose only timeouts are with
a worse RNIC is not flagged.

**Check first.** Link flaps and error counters:

- `ethtool -S <netdev>` (symbol, FEC and link-down counters as your driver
  names them).
- `dmesg` for link up/down messages.
- The switch port's error counters.
- Optic diagnostics (`ethtool -m`).

**Remediation.**

1. Drain the RNIC.
2. Clean or replace the optic or cable.
3. Check the switch port.
4. Then check the RNIC.

**False positives.** See the `ud` caveat. With `rc`, a partial ratio appears
only after an outage long enough to exhaust retransmission.

### `tor_switch_anomaly`

**Meaning.** Every RNIC that has a pair times out on every pair, over at least
two same-rail pairs. One shared component (the ToR, or the host's uplinks)
explains that better than independent RNIC failures.

**Confidence and derivation.** `medium`.

**Check first.**

- Whether other hosts on the same ToR report the same code or
  `rnic_connectivity_failure` at the same time.
- The switch's state.
- The host's RoCEv2 GIDs and routes.

**Remediation.** Network team / switch vendor. Ticket by default. Promote it if
a whole host losing its fabric is a page for you.

**False positives.** The `ud` caveat applies most here. Rail pairs that span a
gateway also time out on every pair. Rail keys follow the netdev's IPv4
prefix, so RNICs in one /24 but on narrower subnets (e.g. two /26s) are not
paired ([Rail-optimized hosts](design.md#rail-optimized-hosts-and---mutual-scope)).
If the startup log says a prefix length was unreadable and /24 was assumed,
check that RNIC's netdev address first.

### `rnic_cross_rail_unreachable`

**Meaning.** Under `--mutual-scope=host`, RNICs on different rails cannot reach
each other.

**Confidence and derivation.**
([RNIC and fabric](design.md#rnic-and-fabric))

- `low`: no cross-rail pair answers. That is what rails isolated by design
  look like, and it is reported on every run of such a host. It is not
  alerted on.
- `medium`, when either holds:
  - Some cross-rail pairs answer and a group of RNICs reaches none outside it.
    Typical causes are an RNIC cabled to the wrong switch port or on an
    isolated subnet (ToN #14).
  - One cross-rail pair times out while both RNICs reach others. This points
    at the route between those two subnets.

**Check first.** For `medium`:

- The named RNICs' IP addresses and subnets (`ip -br addr`).
- Cabling against the intended rail plan, and the switch port they land on.
- The routes between the rails.

**Remediation.** Re-cable or correct the port/VLAN assignment. Fix fabric
routing. Ticket.

**False positives.** `low` on a by-design isolated fabric. Either ignore it (the
shipped rules do) or run that host with the default `--mutual-scope=rail`.

### `rnic_packet_drops`

**Meaning.** Packet drops on an RNIC. Either a hardware discard/OOB counter
delta is non-zero, or (in `--sim`) the mutual probe saw loss. Probe timeouts
are not drops.

**Confidence and derivation.** `high`, because the counter moved. A drop delta
also triggers a busy run itself. A host with steady drops therefore reports
this code on every drop-triggered run (at most one per `--busy-cooldown`). The
counters include `out_of_buffer` (no receive WQE posted), `rx_missed_errors`,
names containing `discard` and netdev `dropped` counters.

**Check first.** Which counter moved:

- `ethtool -S <netdev>` and
  `/sys/class/infiniband/<device>/ports/1/hw_counters/` before and after a
  minute.
- `hostping_rnic_drop_delta` over time.
- PFC configuration and pause counters
  (`hostping_rnic_pause_duration_ratio`).

**Remediation.**

- `out_of_buffer` usually means the receiving application does not post
  receives fast enough. That is a workload matter.
- Physical discards with PFC misconfiguration point at the fabric's lossless
  configuration (network team).

Ticket.

**False positives.** Most drops are caused by a workload and not by a fault.
That is why this code tickets and does not page.

## Advisory

Advisory codes change no path or link status and do not feed Algorithm 1. The
shipped rules raise them as `info`.

### `path_latency_abnormal`

**Meaning.** Idle paths whose small-message latency exceeds their reference by
more than `--abnormal-th` while their bandwidth is not abnormal and no ACS/ATS
cause covers them (NSDI'23 §3.2).

**Confidence and derivation.**

- `medium` when every path was judged against a `LAT` you set in `--baseline` /
  `--baseline-file`.
- `low` when any path rests on the sibling median, which also needs a
  `--latency-floor` margin (500 ns by default).

The findings are grouped by distance, then endpoint, then RNIC
([Latency](design.md#latency), [Latency increase](design.md#latency-increase)).

A sibling verdict goes through these steps before it is reported:

1. Recheck. After the sweep, the path is re-read interleaved with siblings of
   its class. The verdict is kept only if the path still reads high against
   them. The evidence then quotes the re-reads
   (`recheck <path>ns vs <siblings>ns`), and the path's `latency_recheck` holds
   them.
2. Locality check. A kept verdict that the other RNICs on its RNIC's NUMA node
   share (their same-class paths read as high) is cleared as a node-wide
   platform state. The path is `normal` with `latency_reason`
   `locality_shift`, and `latency_locality` holds that reference. This needs
   another RNIC on the node. The verdict stands with one RNIC per NUMA node or
   an unknown node. It also stands when a strict majority of the other nodes
   also read the endpoint `abnormal` (a slow endpoint).
3. Streak. A verdict that stands is reported only once the same path read
   `abnormal` in 3 consecutive idle runs. It is reported on the third of them,
   about two idle intervals after the first run that sees it. The path's
   `latency_streak` holds the count, and the evidence says
   `3 consecutive idle runs`. A run that reads the path normal or cannot judge
   it resets the streak. Busy runs keep it. A discarded idle run rolls it back.

The recheck has a per-run cap of 16. A run that flagged more paths rechecks
them in turn across RNICs, moving on every run. A path it left out carries
`latency_recheck_skipped` `cap`, holds its streak and is not reported in that
run.

**False positives.** On a platform whose healthy paths jitter by about a µs
between runs, `latency_status` flaps between `normal` and `abnormal` with
reason `siblings` on a different set of paths each run, while the streak keeps
it from being reported. Raise `--latency-floor` above the per-path p50 range
of healthy idle runs ([Latency](design.md#latency-verdict-and-reference)).

A one-shot run (`hostpingctl diagnose`) has no streak to carry. It reports a
sibling verdict only when the recheck confirmed it (evidence `one-shot run`).

A `LAT` verdict is reported on the first run.

**Check first.**

- Paths `abnormal` with `latency_streak` 1 or 2 and no cause are a reading
  that has not persisted yet. One that is gone in the next idle run was noise.
- A daemon finding has persisted for at least three idle runs. A fault that
  comes and goes faster than `--idle-interval` is not reported.
- A one-shot finding rests on one run. Before acting on it, repeat
  `hostpingctl diagnose` or watch the daemon's next idle runs.
- A path that keeps reappearing with `recheck_cleared` in between is
  time-window noise the recheck did not always catch. A recheck probe that
  failed keeps its first verdict.
- Many paths with `latency_recheck_skipped` `cap` in one run mean a drift
  flagged more than the cap. They are rechecked over the next runs.
- Paths with `locality_shift` in recent runs point at node-wide drift. On a
  host with one RNIC per NUMA node the locality check cannot see that drift,
  so a finding that names a different RNIC from run to run there may be it.
  See [operations](operations.md#hosts-with-dynamic-uncore-or-memory-power-management).
- A finding whose evidence quotes a recheck held up next to its siblings.
- If the group is a whole distance, the `LAT` you set may be below what this
  hardware measures. Re-check it with `hostpingctl calibrate
  --latency`.
- If the paths carry a `baseline_class`, the reference is that sub-class's
  latency (or, against siblings, the median of the same sub-class). A
  sub-class learned on another board layout is the first suspect.
- If it is one endpoint, look at that device's topology placement
  (`nvidia-smi topo -m`). For GPUs, also look at ACS/ATS, which hostping cannot
  read without root (see
  [operations](operations.md#running-as-a-dedicated-user)).

**Remediation.** Fix the baseline, or treat the finding as a lead for the
configuration codes above.

### `gpu_mr_registration_slow`

**Meaning.** A GPU's memory registration takes at least 4× and at least 1 ms
longer than the median of the other GPUs (ToN'24 §VI-C), in 2 consecutive idle
runs.

**Confidence and derivation.**

- `medium` when the GPU was slow from every one of at least two RNICs.
- `low` otherwise.

Idle runs only. The code needs three other GPUs
([Slow verbs](design.md#slow-verbs)).

Each GPU's value is the lower median of its `mr_reg` readings, one per RNIC
that probed it. For an even count it is the lower of the two middle values.
Healthy GPUs show single slow registrations now and then, and these cannot
move the value unless they are more than half of the readings. Hosts with few
RNICs per GPU have few readings per GPU.

The same GPU must cross in 2 consecutive idle runs.

- An idle run in which it does not cross resets the streak.
- Busy runs neither extend nor reset it.
- A discarded idle run is rolled back.
- `--state-dir` keeps the streak across a restart.
- A one-shot run never reports the code.

The evidence names the lower median, the sample count and the number of
consecutive idle runs.

**Check first.**

- `hostping_verb_duration_microseconds{verb="mr_reg"}` for that GPU over time.
  With `--metrics-verb-durations=false` the series is absent; read the
  paths' `verb_ns` in the report instead.
- The GPU's health in `nvidia-smi -q` and `dmesg` (Xid messages).
- The peer-memory / dma-buf path in use.

**Remediation.** Lead for a GPU vendor investigation; no direct action.

### `rnic_pcie_undersized`

**Meaning.** An RNIC's PCIe link trained at its maximum speed and width, but
that maximum cannot carry the RNIC's line rate. The slot (or the card's own
link) is narrower or slower than the RNIC by design. Nothing is degraded.
Advisory. It is routed to `HostpingAdvisoryDiagnosis` (info) and never paged.

**Confidence and derivation.** `low`, once per RNIC per run, from config space
alone. Each half of the negotiated link is compared with the lower of the
device and upstream port maxima that are readable. Both conditions must hold:

- Both halves are at that maximum.
- The effective bandwidth (`current_link_speed` × width, less line-code
  overhead) is below the line rate.

A link below a known maximum is a down-train and goes to
[`rnic_pcie_failure`](#rnic_pcie_failure) instead. A link whose maxima are
unread gets neither. The code changes no path or link status and never feeds
Algorithm 1 ([PCIe link checks](design.md#pcie-link-checks)).

**Check first.**

- The evidence names the negotiated link, the maxima and the effective
  bandwidth against the line rate.
- Confirm with `lspci -vvv -s <bdf>` (`LnkCap` vs `LnkSta`) for the RNIC and
  its upstream port.
- Compare with the platform's slot plan. Is the RNIC in the slot it was meant
  for?

**Remediation.** None needed if the layout is intended. The RNIC cannot reach
its line rate there. Otherwise move the RNIC to a slot that carries its line
rate. Silence the info alert for hosts built this way.

The device may not be a data-path NIC at all. Typically it is the HCA a host
manages its GPU fabric through: an InfiniBand device on a deliberately narrow
link whose subnet manager runs on the host itself. Discovery warns about such
a device at start. Do not silence the advisory for it. Exclude the device with
`--exclude-rnic` (every function of it) and restart. Otherwise hostping keeps
sending loopback probes into that fabric's management subnet. See
[Excluding RNICs](design.md#excluding-rnics---exclude-rnic) and
[operations](operations.md#install).

### `unknown`

**Meaning.** Some paths are abnormal but tomography left only gray or
uncertain links. No hop could be isolated, and no other cause fired from the
loopback bandwidth pattern.

**Confidence and derivation.** `low`.

**Check first.**

- `hostping_path_status_code == 3` for which paths, and whether they share a
  component in `hostpingctl topology`.
- In a busy run this is often the sibling comparison's residual noise.
- If it repeats in idle runs, compare with the other hosts of the same
  hardware class, check the abnormal paths' `baseline_class`, and consider
  calibrating baselines
  ([Calibrating baselines](operations.md#calibrating-baselines)).

## Agent health

See [Liveness and staleness alerts](operations.md#liveness-and-staleness-alerts)
for the series behind these.

### HostpingTargetDown

Prometheus cannot scrape `/metrics`.

1. Check `systemctl status hostping` and `journalctl -u hostping -n 50`. A
   start-up error restarts the unit every 30 s. Typical errors:
   - discovery before the RDMA driver registered its devices;
   - a flag the binary rejects;
   - a metrics address that cannot be bound;
   - a bad `--metrics-web-config`.
2. If the process runs, check the address, firewall and TLS/basic-auth
   settings against the scrape config.

Prometheus cannot check `/healthz` on its own. If you want the poll-loop stall
check, point a blackbox prober or the host's service monitor at it
([Metrics endpoint](operations.md#metrics-endpoint)).

### HostpingNoSuccessfulRun

The host has been idle for 15 minutes but no run was confirmed and written.
Look in the journal for `hostping` lines about:

- Discarded idle runs: a workload returns during each idle run, and
  `hostping_engine_run_discarded_total` rises.
- Failed runs (`hostping_engine_run_failed_total`).
- An RNIC whose line rate is unread. No idle run is started (see
  [What "unknown" means](operations.md#what-unknown-means-per-data-source)).

Adjust the 900 s / 15 m in the rule to 3 × your `--idle-interval`.

### HostpingRunsFailing

- `stage="engine"`: the engine run returned an error. The journal line says
  why (for example, discovery or probe library errors).
- `stage="report"`: the report could not be written to stdout or
  `--report-file` (disk full, permissions, removed directory).

### HostpingMonitorPollErrors

The monitor could not read the RNIC counters or GPU state, so no run is
triggered. The journal names the device or command (`nvidia-smi` timing out,
`monitor: no RNIC counters readable for <dev>`). See
[What "unknown" means](operations.md#what-unknown-means-per-data-source).

### HostpingEndpointsSkipped

Loopback probes to some GPU or memory endpoints are skipped, so paths to them
are never diagnosed. The journal line
(`hostping: endpoints skipped kind=gpu ...`) and the report's
`skipped_endpoints` carry the reason. The metric does not, because the reason
is free-form text.

Typical GPU reasons:

- No CUDA library or GPUDirect RDMA support (`NoGDR`). Install the driver's
  peer-memory module, or use a kernel and driver with dma-buf.
- `compute_mode: Exclusive_Process` / `Prohibited`. This is intended: hostping
  does not probe GPUs that a job owns exclusively. On hosts that run GPUs in an
  exclusive mode by policy, silence this alert.

Memory reasons: memlock limits, device nodes or NUMA binding
([Privileges and devices](operations.md#privileges-and-devices)).
